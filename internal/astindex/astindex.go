// Package astindex — индексатор AST units и code-graph edges.
//
// Для Go используется стандартная библиотека go/parser + go/ast: это даёт
// точную информацию о функциях/методах/типах/импортах/вызовах в пределах
// одного файла без построения полного go/packages-графа (что было бы
// тяжеловесно и требовало рабочего go-окружения).
//
// Для остальных языков (TS/JS/Python/Java) AST units заполняются на основе
// существующего tree-sitter symbols (см. internal/parser): edges пока не
// извлекаются, что соответствует согласованной стратегии «Go-first».
//
// Сохранение в БД — через store.ReplaceASTUnits + store.ReplaceEdges
// (атомарно по файлу). Разрешение dst_id у edges выполняется отложенно
// через store.ResolvePendingEdges после полного скана.
package astindex

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aitools/internal/config"
	"aitools/internal/fileutil"
	pkgparser "aitools/internal/parser"
	"aitools/internal/state"
	"aitools/internal/store"
)

// Indexer — индексатор AST units и edges.
type Indexer struct {
	cfg     *config.Config
	st      *store.SQLite
	ts      *pkgparser.Parser
	bus     *state.Bus
	matcher *fileutil.Matcher
}

// New создаёт индексатор.
func New(cfg *config.Config, st *store.SQLite) *Indexer {
	return &Indexer{
		cfg:     cfg,
		st:      st,
		ts:      pkgparser.New(),
		matcher: fileutil.NewMatcher(cfg.Ignore),
	}
}

// SetBus устанавливает шину событий для статистики.
func (i *Indexer) SetBus(bus *state.Bus) {
	i.bus = bus
}

// IndexFile парсит файл, извлекает AST units + edges и сохраняет в SQLite.
// path должен быть абсолютным.
func (i *Indexer) IndexFile(ctx context.Context, path string) error {
	if i == nil || i.st == nil {
		return nil
	}
	start := time.Now()
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lang := detectLang(path)

	var (
		units []store.ASTUnit
		edges []pendingEdge
	)
	switch lang {
	case "go":
		units, edges, err = i.parseGo(path, src)
	case "java", "typescript", "javascript":
		units, edges, err = i.parseWithTreeSitter(ctx, lang, path, src)
		// fileFromNode возвращает пусто — заполним FilePath здесь.
		for k := range units {
			if units[k].FilePath == "" {
				units[k].FilePath = path
			}
		}
	default:
		units, err = i.parseGeneric(ctx, lang, path, src)
	}
	if err != nil {
		return err
	}

	// Запись units. parent_id здесь — индексный (0-based) reference на
	// предыдущие units в этом же файле; пересчитаем после получения
	// реальных ids.
	rel, _ := filepath.Rel(i.cfg.Root, path)
	_ = rel

	// Сначала сохраняем все units без parent_id (parent проставим вторым
	// проходом), чтобы получить реальные ids.
	idxToParent := make(map[int]int, len(units))
	for i, u := range units {
		if u.ParentID.Valid {
			idxToParent[i] = int(u.ParentID.Int64)
			units[i].ParentID = sql.NullInt64{} // сбрасываем — это «индекс», не id
		}
	}

	// Гарантируем наличие файла в таблице files (внешний ключ ast_units.file_path).
	if err := i.st.EnsureFile(ctx, path, lang); err != nil {
		return fmt.Errorf("astindex: ensure file: %w", err)
	}

	idMap, err := i.st.ReplaceASTUnits(ctx, path, units)
	if err != nil {
		return fmt.Errorf("astindex: replace units: %w", err)
	}

	// Второй проход: проставляем parent_id по реальным ids.
	// Узнаём текущие ids (порядок такой же, как при вставке).
	persisted, err := i.st.ListASTUnitsByFile(ctx, path)
	if err != nil {
		return err
	}
	if len(persisted) == len(units) {
		// Соберём апдейты parent_id одной транзакцией: проще выгрузить
		// заново. Здесь — fallback на чистый exec из store, через простой
		// проход (доп. метод в store: UpdateASTParents).
		updates := make(map[int64]int64, len(idxToParent))
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

	// Теперь — edges. У edges src указывается по «локальному индексу»
	// исходного unit'а, dst либо по индексу (если внутри файла), либо по
	// имени (qualified) для отложенного резолва.
	resolvedEdges := make([]store.Edge, 0, len(edges))
	for _, e := range edges {
		if e.srcIdx < 0 || e.srcIdx >= len(persisted) {
			continue
		}
		ed := store.Edge{
			SrcID:    persisted[e.srcIdx].ID,
			Kind:     e.kind,
			DstName:  e.dstName,
			FilePath: path,
			Line:     e.line,
		}
		if e.dstIdx >= 0 && e.dstIdx < len(persisted) {
			ed.DstID = persisted[e.dstIdx].ID
		} else if e.dstName != "" {
			// dst_id=0 — будет разрешён ResolvePendingEdges.
			if id, ok := idMap[e.dstName]; ok {
				ed.DstID = id
			}
		}
		resolvedEdges = append(resolvedEdges, ed)
	}
	if err := i.st.ReplaceEdges(ctx, path, resolvedEdges); err != nil {
		return fmt.Errorf("astindex: replace edges: %w", err)
	}

	if i.bus != nil {
		i.bus.AddRecent(state.FileEntry{
			Path:       path,
			Kind:       "graph",
			Symbols:    len(units),
			DurationMs: time.Since(start).Milliseconds(),
		})
	}

	return nil
}

// FullScan индексирует все подходящие файлы под cfg.Root и затем разрешает
// отложенные dst_id у edges.
func (i *Indexer) FullScan(ctx context.Context) error {
	if i.bus != nil {
		i.bus.SetIndexer("graph", func(st *state.Indexer) {
			st.Status = "scanning"
			st.LastError = ""
		})
	}

	var allFiles []string
	_ = fileutil.WalkFiles(i.cfg.Root, i.matcher, i.cfg.Extensions, func(abs, rel string, _ os.FileInfo) error {
		allFiles = append(allFiles, abs)
		return nil
	})

	total := len(allFiles)
	var indexed int
	var lastErr error

	for idx, abs := range allFiles {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := i.IndexFile(ctx, abs); err == nil {
			indexed++
		} else {
			lastErr = err
		}

		if i.bus != nil {
			i.bus.SetIndexer("graph", func(st *state.Indexer) {
				st.FilesTotal = total
				st.FilesIndexed = idx + 1
				st.Status = "indexing"
				if lastErr != nil {
					st.LastError = lastErr.Error()
				}
			})
		}
	}

	if i.bus != nil {
		i.bus.SetIndexer("graph", func(st *state.Indexer) {
			st.Status = "resolving"
		})
	}
	if _, err := i.st.ResolvePendingEdges(ctx); err != nil {
		lastErr = err
	}

	if i.bus != nil {
		i.bus.SetIndexer("graph", func(st *state.Indexer) {
			if lastErr != nil && indexed < total {
				st.Status = "error"
				st.LastError = lastErr.Error()
			} else {
				st.Status = "idle"
			}
			st.FilesTotal = total
			st.FilesIndexed = total
		})
		if gs, err := i.st.GraphStats(ctx); err == nil {
			i.bus.SetIndexer("graph", func(st *state.Indexer) {
				st.Symbols = gs.Units
				st.Chunks = gs.Edges // Используем Chunks для Edges
			})
		}
	}
	return lastErr
}

// RemoveFile удаляет AST units и edges для файла.
func (i *Indexer) RemoveFile(ctx context.Context, path string) error {
	if _, err := i.st.ReplaceASTUnits(ctx, path, nil); err != nil {
		return err
	}
	return i.st.ReplaceEdges(ctx, path, nil)
}

// ---------------- Go-specific extractor ----------------

type pendingEdge struct {
	srcIdx  int    // индекс src в массиве units (внутри одного файла)
	dstIdx  int    // -1 если dst не в этом файле
	dstName string // qualified или name для отложенного резолва
	kind    string
	line    int
}

func (i *Indexer) parseGo(path string, src []byte) ([]store.ASTUnit, []pendingEdge, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, nil, err
	}

	pkgName := ""
	if f.Name != nil {
		pkgName = f.Name.Name
	}

	var units []store.ASTUnit
	var edges []pendingEdge

	// 1. module unit — представляет файл (или package) целиком.
	moduleIdx := len(units)
	units = append(units, store.ASTUnit{
		FilePath:  path,
		Language:  "go",
		Kind:      "module",
		Name:      pkgName,
		Qualified: pkgName,
		StartLine: 1,
		EndLine:   fset.Position(f.End()).Line,
		StartByte: 0,
		EndByte:   len(src),
		Signature: "package " + pkgName,
		Hash:      hashBytes(src),
	})

	// 2. imports — рёбра module --import--> "<import path>".
	for _, imp := range f.Imports {
		if imp.Path == nil {
			continue
		}
		ipath := strings.Trim(imp.Path.Value, "\"`")
		edges = append(edges, pendingEdge{
			srcIdx:  moduleIdx,
			dstIdx:  -1,
			dstName: ipath,
			kind:    "import",
			line:    fset.Position(imp.Pos()).Line,
		})
	}

	// 3. top-level declarations.
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			i.addFuncDecl(fset, src, d, pkgName, moduleIdx, &units, &edges)
		case *ast.GenDecl:
			i.addGenDecl(fset, src, d, pkgName, moduleIdx, &units, &edges)
		}
	}
	return units, edges, nil
}

func (i *Indexer) addFuncDecl(fset *token.FileSet, src []byte, d *ast.FuncDecl, pkg string, moduleIdx int, units *[]store.ASTUnit, edges *[]pendingEdge) {
	if d.Name == nil {
		return
	}
	name := d.Name.Name
	kind := "function"
	parentIdx := moduleIdx
	qualified := pkg + "." + name

	// Метод?
	if d.Recv != nil && len(d.Recv.List) > 0 {
		kind = "method"
		recvType := exprName(d.Recv.List[0].Type)
		if recvType != "" {
			qualified = pkg + "." + recvType + "." + name
		}
	}

	// Расширяем byte-range на doc-комментарий, чтобы chunker и
	// embedder увидели сопроводительные комментарии вместе с телом.
	declStart := d.Pos()
	if d.Doc != nil && d.Doc.Pos().IsValid() {
		declStart = d.Doc.Pos()
	}
	startPos := fset.Position(declStart)
	endPos := fset.Position(d.End())
	startByte := int(declStart - 1) // token positions are 1-based
	endByte := int(d.End() - 1)
	if startByte < 0 {
		startByte = 0
	}
	if endByte > len(src) {
		endByte = len(src)
	}

	body := ""
	if startByte < endByte {
		body = string(src[startByte:endByte])
	}

	idx := len(*units)
	*units = append(*units, store.ASTUnit{
		FilePath:  fset.Position(d.Pos()).Filename,
		Language:  "go",
		Kind:      kind,
		Name:      name,
		Qualified: qualified,
		ParentID:  sql.NullInt64{Int64: int64(parentIdx), Valid: true},
		StartLine: startPos.Line,
		EndLine:   endPos.Line,
		StartByte: startByte,
		EndByte:   endByte,
		Signature: signatureOf(src, d),
		Doc:       commentText(d.Doc),
		Hash:      hashBytes([]byte(body)),
	})

	// Call edges внутри тела.
	if d.Body != nil {
		ast.Inspect(d.Body, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			callee := exprName(ce.Fun)
			if callee == "" {
				return true
			}
			*edges = append(*edges, pendingEdge{
				srcIdx:  idx,
				dstIdx:  -1,
				dstName: callee,
				kind:    "call",
				line:    fset.Position(ce.Pos()).Line,
			})
			return true
		})
	}
}

func (i *Indexer) addGenDecl(fset *token.FileSet, src []byte, d *ast.GenDecl, pkg string, moduleIdx int, units *[]store.ASTUnit, edges *[]pendingEdge) {
	for _, spec := range d.Specs {
		switch ts := spec.(type) {
		case *ast.TypeSpec:
			name := ts.Name.Name
			qualified := pkg + "." + name
			kind := "type"

			switch ts.Type.(type) {
			case *ast.StructType:
				kind = "struct"
			case *ast.InterfaceType:
				kind = "interface"
			}

			declStart := d.Pos()
			if d.Doc != nil && d.Doc.Pos().IsValid() {
				declStart = d.Doc.Pos()
			}
			startPos := fset.Position(declStart)
			endPos := fset.Position(d.End())
			startByte := int(declStart - 1)
			endByte := int(d.End() - 1)
			if startByte < 0 {
				startByte = 0
			}
			if endByte > len(src) {
				endByte = len(src)
			}
			body := ""
			if startByte < endByte {
				body = string(src[startByte:endByte])
			}

			idx := len(*units)
			*units = append(*units, store.ASTUnit{
				FilePath:  fset.Position(d.Pos()).Filename,
				Language:  "go",
				Kind:      kind,
				Name:      name,
				Qualified: qualified,
				ParentID:  sql.NullInt64{Int64: int64(moduleIdx), Valid: true},
				StartLine: startPos.Line,
				EndLine:   endPos.Line,
				StartByte: startByte,
				EndByte:   endByte,
				Signature: firstLine(body),
				Doc:       commentText(d.Doc),
				Hash:      hashBytes([]byte(body)),
			})

			// Embedded типы у struct → reference; embedded interfaces → extends.
			switch tt := ts.Type.(type) {
			case *ast.StructType:
				if tt.Fields != nil {
					for _, f := range tt.Fields.List {
						if len(f.Names) == 0 { // embedded
							if nm := exprName(f.Type); nm != "" {
								*edges = append(*edges, pendingEdge{
									srcIdx:  idx,
									dstIdx:  -1,
									dstName: nm,
									kind:    "reference",
									line:    fset.Position(f.Pos()).Line,
								})
							}
						}
					}
				}
			case *ast.InterfaceType:
				if tt.Methods != nil {
					for _, f := range tt.Methods.List {
						if len(f.Names) == 0 {
							if nm := exprName(f.Type); nm != "" {
								*edges = append(*edges, pendingEdge{
									srcIdx:  idx,
									dstIdx:  -1,
									dstName: nm,
									kind:    "extends",
									line:    fset.Position(f.Pos()).Line,
								})
							}
						}
					}
				}
			}

		case *ast.ValueSpec:
			kind := "variable"
			if d.Tok == token.CONST {
				kind = "constant"
			}
			for _, nameIdent := range ts.Names {
				name := nameIdent.Name
				qualified := pkg + "." + name

				startPos := fset.Position(ts.Pos())
				endPos := fset.Position(ts.End())
				startByte := int(ts.Pos() - 1)
				endByte := int(ts.End() - 1)
				if startByte < 0 {
					startByte = 0
				}
				if endByte > len(src) {
					endByte = len(src)
				}
				body := string(src[startByte:endByte])

				*units = append(*units, store.ASTUnit{
					FilePath:  fset.Position(ts.Pos()).Filename,
					Language:  "go",
					Kind:      kind,
					Name:      name,
					Qualified: qualified,
					ParentID:  sql.NullInt64{Int64: int64(moduleIdx), Valid: true},
					StartLine: startPos.Line,
					EndLine:   endPos.Line,
					StartByte: startByte,
					EndByte:   endByte,
					Signature: firstLine(body),
					Doc:       commentText(ts.Doc),
					Hash:      hashBytes([]byte(body)),
				})
			}
		}
	}
}

// ---------------- Generic (non-Go) extractor ----------------

// parseGeneric делегирует tree-sitter (через internal/parser.Parser) и
// превращает его symbols в AST units. Edges не извлекаются.
func (i *Indexer) parseGeneric(ctx context.Context, lang, path string, src []byte) ([]store.ASTUnit, error) {
	if lang == "" {
		return nil, nil
	}
	syms, err := i.ts.Parse(ctx, lang, path, src)
	if err != nil {
		return nil, err
	}

	moduleName := filepath.Base(path)
	units := []store.ASTUnit{{
		FilePath:  path,
		Language:  lang,
		Kind:      "module",
		Name:      moduleName,
		Qualified: moduleName,
		StartLine: 1,
		EndLine:   1 + strings.Count(string(src), "\n"),
		StartByte: 0,
		EndByte:   len(src),
		Hash:      hashBytes(src),
	}}

	// parent_id всех unit'ов в этом файле = module (idx 0). Класс/интерфейс
	// тоже привязываем к module, а методы — к соответствующему классу
	// если он встретился (упрощённая 1-level вложенность).
	classIdx := map[string]int{}
	for _, sym := range syms {
		if sym.Name == "" {
			continue
		}
		parent := 0
		qualified := moduleName + "." + sym.Name
		if sym.Parent != "" {
			if ci, ok := classIdx[sym.Parent]; ok {
				parent = ci
			}
			qualified = moduleName + "." + sym.Parent + "." + sym.Name
		}
		body := ""
		if sym.StartByte >= 0 && sym.EndByte <= len(src) && sym.StartByte < sym.EndByte {
			body = string(src[sym.StartByte:sym.EndByte])
		}
		idx := len(units)
		units = append(units, store.ASTUnit{
			FilePath:  path,
			Language:  lang,
			Kind:      sym.Kind,
			Name:      sym.Name,
			Qualified: qualified,
			ParentID:  sql.NullInt64{Int64: int64(parent), Valid: true},
			StartLine: sym.StartLine,
			EndLine:   sym.EndLine,
			StartByte: sym.StartByte,
			EndByte:   sym.EndByte,
			Signature: sym.Signature,
			Hash:      hashBytes([]byte(body)),
		})
		if sym.Kind == "class" || sym.Kind == "interface" {
			classIdx[sym.Name] = idx
		}
	}
	return units, nil
}

// ---------------- helpers ----------------

func detectLang(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".py":
		return "python"
	case ".java":
		return "java"
	}
	return ""
}

// exprName выводит «имя» из ast.Expr (идентификатор, селектор, указатель).
func exprName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		base := exprName(v.X)
		if base == "" {
			return v.Sel.Name
		}
		return base + "." + v.Sel.Name
	case *ast.StarExpr:
		return exprName(v.X)
	case *ast.IndexExpr:
		return exprName(v.X)
	case *ast.IndexListExpr:
		return exprName(v.X)
	}
	return ""
}

func signatureOf(src []byte, d *ast.FuncDecl) string {
	start := int(d.Pos() - 1)
	if start < 0 {
		start = 0
	}
	end := start
	if d.Body != nil {
		end = int(d.Body.Pos() - 1)
	} else {
		end = int(d.End() - 1)
	}
	if end > len(src) {
		end = len(src)
	}
	if start >= end {
		return ""
	}
	s := strings.TrimSpace(string(src[start:end]))
	if nl := strings.Index(s, "\n"); nl >= 0 {
		s = s[:nl]
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

func firstLine(s string) string {
	if nl := strings.Index(s, "\n"); nl >= 0 {
		s = s[:nl]
	}
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

func commentText(g *ast.CommentGroup) string {
	if g == nil {
		return ""
	}
	return strings.TrimSpace(g.Text())
}

func hashBytes(b []byte) string {
	h := sha1.Sum(b)
	return hex.EncodeToString(h[:])
}
