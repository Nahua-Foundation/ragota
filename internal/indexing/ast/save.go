package astindex

import (
	"context"
	"database/sql"
	"fmt"

	"ragota/internal/store"
)

// saveUnitsAndEdges — общее ядро записи AST units и edges в БД.
// Использует ReplaceFileGraph для атомарной записи units+edges в одной транзакции.
func (i *Indexer) saveUnitsAndEdges(
	ctx context.Context,
	path string,
	units []store.ASTUnit,
	edges []pendingEdge,
	repo string,
	idMap map[string]int,
) error {
	idxToParent := make(map[int]int, len(units))
	for idx, u := range units {
		if u.ParentID.Valid {
			idxToParent[idx] = int(u.ParentID.Int64)
			units[idx].ParentID = sql.NullInt64{}
		}
	}

	lang := detectLang(path)
	if err := i.st.EnsureFile(ctx, path, lang); err != nil {
		return fmt.Errorf("astindex: ensure file: %w", err)
	}

	// Атомарная замена units + edges в одной транзакции.
	edgeRefs := make([]store.EdgeRef, 0, len(edges))
	for _, e := range edges {
		edgeRefs = append(edgeRefs, store.EdgeRef{
			SrcUnitIdx: e.srcIdx,
			DstName:    e.dstName,
			DstUnitIdx: e.dstIdx,
			Kind:       e.kind,
			Line:       e.line,
		})
	}

	idMap2, _, err := i.st.ReplaceFileGraph(ctx, path, units, edgeRefs, repo)
	if err != nil {
		return fmt.Errorf("astindex: replace file graph: %w", err)
	}
	// Merge idMap
	if idMap == nil {
		idMap = idMap2
	} else {
		for k, v := range idMap2 {
			idMap[k] = v
		}
	}

	persisted, err := i.st.ListASTUnitsByFile(ctx, path)
	if err != nil {
		return err
	}

	// Обновляем parent_id (отдельная транзакция — допустимая деградация)
	if len(persisted) == len(units) {
		updates := make(map[int]int, len(idxToParent))
		for idx, parentIdx := range idxToParent {
			if parentIdx < 0 || parentIdx >= len(persisted) {
				continue
			}
			updates[persisted[idx].ID] = persisted[parentIdx].ID
		}
		if err := i.st.UpdateASTParents(ctx, updates); err != nil {
			return err
		}
	}

	return nil
}

// parseFile парсит файл и возвращает units + edges.
func (i *Indexer) parseFile(ctx context.Context, path string, src []byte) ([]store.ASTUnit, []pendingEdge, error) {
	lang := detectLang(path)
	var (
		units []store.ASTUnit
		edges []pendingEdge
		err   error
	)
	switch lang {
	case "go":
		units, edges, err = i.parseGo(path, src)
	case "java", "typescript", "javascript":
		units, edges, err = i.parseWithTreeSitter(ctx, lang, path, src)
		for k := range units {
			if units[k].FilePath == "" {
				units[k].FilePath = path
			}
		}
	default:
		units, err = i.parseGeneric(ctx, lang, path, src)
	}
	if err != nil {
		return nil, nil, err
	}

	var repo string
	if i.resolv != nil {
		repo = i.resolv.For(path)
	}
	for k := range units {
		units[k].Repo = repo
	}

	return units, edges, nil
}
