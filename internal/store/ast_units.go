package store

// Файл реализует CRUD AST units (таблица ast_units): Replace/List/Get/
// Find/UpdateParents/ChildrenOf и общие helper'ы scanASTUnit + astUnitColumns.

import (
	"context"
	"database/sql"
	"strings"
)

// scanASTUnit — общий код для чтения строки ast_units.
func scanASTUnit(rows interface {
	Scan(dest ...any) error
}) (ASTUnit, error) {
	var u ASTUnit
	err := rows.Scan(
		&u.ID, &u.Repo, &u.FilePath, &u.Language, &u.Kind, &u.Name, &u.Qualified,
		&u.ParentID, &u.StartLine, &u.EndLine, &u.StartByte, &u.EndByte,
		&u.NameStartLine, &u.NameStartCol,
		&u.Signature, &u.Doc, &u.Hash,
	)
	return u, err
}

const astUnitColumns = `id, repo, file_path, language, kind, name, qualified, parent_id, start_line, end_line, start_byte, end_byte, name_start_line, name_start_col, signature, doc, hash`

// ReplaceASTUnits атомарно заменяет все AST-единицы файла. Возвращает map
// "name -> id" для свежезаписанных юнитов — удобно при последующей записи
// рёбер.
//
// units.ParentID может ссылаться по индексу (отрицательное значение -1*idx-1
// в IDColumn ParentID.Int64), но проще: ParentID.Valid=false для корня,
// либо пересохранять рёбра contains после реальной вставки. Здесь мы
// сохраняем как есть и предполагаем, что вызывающий уже корректно
// проставил parent_id (или nil) — что делает индексатор поверх параса.
func (s *SQLite) ReplaceASTUnits(ctx context.Context, filePath string, units []ASTUnit) (map[string]int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM ast_units WHERE file_path = ?`, filePath); err != nil {
		return nil, err
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO ast_units(repo, file_path, language, kind, name, qualified, parent_id,
			start_line, end_line, start_byte, end_byte, name_start_line, name_start_col, signature, doc, hash)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	ids := make(map[string]int, len(units))
	for _, u := range units {
		res, err := stmt.ExecContext(ctx,
			u.Repo, filePath, u.Language, u.Kind, u.Name, u.Qualified, u.ParentID,
			u.StartLine, u.EndLine, u.StartByte, u.EndByte,
			u.NameStartLine, u.NameStartCol,
			u.Signature, u.Doc, u.Hash)
		if err != nil {
			return nil, err
		}
		id64, err := res.LastInsertId()
		if err != nil {
			return nil, err
		}
		id := int(id64)
		// Сохраняем последний id для каждого имени (используется в edges-resolver).
		key := u.Qualified
		if key == "" {
			key = u.Name
		}
		if key != "" {
			ids[key] = id
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// ListASTUnitsByFile возвращает все AST units файла в порядке появления.
func (s *SQLite) ListASTUnitsByFile(ctx context.Context, filePath string) ([]ASTUnit, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+astUnitColumns+` FROM ast_units WHERE file_path = ? ORDER BY start_byte`, filePath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ASTUnit{}
	for rows.Next() {
		u, err := scanASTUnit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetASTUnit возвращает unit по id (nil если не найден).
func (s *SQLite) GetASTUnit(ctx context.Context, id int) (*ASTUnit, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+astUnitColumns+` FROM ast_units WHERE id = ?`, id)
	u, err := scanASTUnit(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// FindASTUnits ищет AST units по имени/qualified (LIKE), опционально
// фильтруя по kind/language/repo.
//
// repo:
//   - ""    — без фильтра (поиск по всем репо workspace);
//   - имя репы — точное совпадение по полю repo;
//   - "*"   — эквивалентно "" (специальное значение для совместимости с
//     MCP-параметром «все репо»).
func (s *SQLite) FindASTUnits(ctx context.Context, query, kind, language, repo string, limit int) ([]ASTUnit, error) {
	if limit <= 0 {
		limit = 50
	}
	kind = strings.ToLower(kind)
	language = strings.ToLower(language)

	// Сначала пробуем точное совпадение (по индексам idx_ast_units_name или idx_ast_units_qualified).
	// Используем COLLATE NOCASE для соответствия индексам.
	qExact := `SELECT ` + astUnitColumns + ` FROM ast_units WHERE (name = ? COLLATE NOCASE OR qualified = ? COLLATE NOCASE)`
	argsExact := []any{query, query}
	if kind != "" {
		qExact += ` AND kind = ?`
		argsExact = append(argsExact, kind)
	}
	if language != "" {
		qExact += ` AND language = ?`
		argsExact = append(argsExact, language)
	}
	if repo != "" && repo != "*" {
		qExact += ` AND repo = ?`
		argsExact = append(argsExact, repo)
	}
	qExact += ` LIMIT ?`
	argsExact = append(argsExact, limit)

	rows, err := s.db.QueryContext(ctx, qExact, argsExact...)
	if err != nil {
		return nil, err
	}

	out := []ASTUnit{}
	for rows.Next() {
		u, err := scanASTUnit(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, u)
	}
	rows.Close()

	// Если нашли достаточно точных совпадений, возвращаем их.
	if len(out) >= limit {
		return out, nil
	}

	// Иначе добираем через LIKE.
	// LIKE в SQLite по умолчанию case-insensitive для ASCII, но не использует обычные индексы.
	// Мы исключаем точные совпадения, так как они уже в out.
	remaining := limit - len(out)
	qLike := `SELECT ` + astUnitColumns + ` FROM ast_units WHERE (name LIKE ? OR qualified LIKE ?) AND name <> ? COLLATE NOCASE AND qualified <> ? COLLATE NOCASE`
	argsLike := []any{"%" + query + "%", "%" + query + "%", query, query}
	if kind != "" {
		qLike += ` AND kind = ?`
		argsLike = append(argsLike, kind)
	}
	if language != "" {
		qLike += ` AND language = ?`
		argsLike = append(argsLike, language)
	}
	if repo != "" && repo != "*" {
		qLike += ` AND repo = ?`
		argsLike = append(argsLike, repo)
	}
	qLike += ` ORDER BY length(name) ASC LIMIT ?`
	argsLike = append(argsLike, remaining)

	rows, err = s.db.QueryContext(ctx, qLike, argsLike...)
	if err != nil {
		return out, nil // Возвращаем то, что уже нашли
	}
	defer rows.Close()

	for rows.Next() {
		u, err := scanASTUnit(rows)
		if err != nil {
			return out, nil
		}
		out = append(out, u)
	}

	return out, rows.Err()
}

// UpdateASTParents массово проставляет parent_id (map childID -> parentID).
// Используется во второй фазе индексации, когда реальные id уже известны.
func (s *SQLite) UpdateASTParents(ctx context.Context, updates map[int]int) error {
	if len(updates) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `UPDATE ast_units SET parent_id = ? WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for childID, parentID := range updates {
		if childID == parentID {
			continue
		}
		if _, err := stmt.ExecContext(ctx, parentID, childID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FindModuleUnits ищет модульные AST units, чей FilePath содержит указанный путь.
// Используется для резолва модулей по относительному пути.
func (s *SQLite) FindModuleUnits(ctx context.Context, pathPattern string, limit int) ([]ASTUnit, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+astUnitColumns+` FROM ast_units WHERE kind = 'module' AND file_path LIKE ? LIMIT ?`,
		"%"+pathPattern+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ASTUnit{}
	for rows.Next() {
		u, err := scanASTUnit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ChildrenOf возвращает прямых детей AST unit.
func (s *SQLite) ChildrenOf(ctx context.Context, id int) ([]ASTUnit, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+astUnitColumns+` FROM ast_units WHERE parent_id = ? ORDER BY start_byte`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ASTUnit{}
	for rows.Next() {
		u, err := scanASTUnit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ReplaceFileGraph атомарно заменяет AST units и edges файла в одной транзакции.
// EdgeRef описывает ребро через индексы в units slice (для SrcID) и dstName для резолва.
type EdgeRef struct {
	SrcUnitIdx    int
	ResolvedSrcID int // уже разрешённый src_id (0 = resolve via SrcUnitIdx)
	DstName       string
	DstUnitIdx    int  // -1 если не resolved по индексу (local file)
	ResolvedDstID int // уже разрешённый dst_id из global map (0 = unresolved)
	DstRepo       string // целевой репо (для cross-repo edges)
	Confidence    float64 // уверенность (1.0 = default)
	Kind          string
	Line          int
}

// ReplaceFileGraph атомарно заменяет AST units и edges файла в одной транзакции.
func (s *SQLite) ReplaceFileGraph(ctx context.Context, filePath string, units []ASTUnit, edgeRefs []EdgeRef, repo string) (map[string]int, []int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	// Delete old data (только units если переданы)
	if len(units) > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM ast_units WHERE file_path = ?`, filePath); err != nil {
			return nil, nil, err
		}
	}
	// Delete edges всегда (для перезаписи)
	if _, err := tx.ExecContext(ctx, `DELETE FROM edges WHERE src_id IN (SELECT id FROM ast_units WHERE file_path = ?)`, filePath); err != nil {
		return nil, nil, err
	}

	// Insert units и запоминаем id по индексу
	unitIDs := make([]int, len(units))
	ids := make(map[string]int, len(units))
	if len(units) > 0 {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT INTO ast_units(repo, file_path, language, kind, name, qualified, parent_id,
				start_line, end_line, start_byte, end_byte, name_start_line, name_start_col, signature, doc, hash)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return nil, nil, err
		}
		defer stmt.Close()

		for i, u := range units {
			res, err := stmt.ExecContext(ctx,
				u.Repo, filePath, u.Language, u.Kind, u.Name, u.Qualified, u.ParentID,
				u.StartLine, u.EndLine, u.StartByte, u.EndByte,
				u.NameStartLine, u.NameStartCol,
				u.Signature, u.Doc, u.Hash)
			if err != nil {
				return nil, nil, err
			}
			id64, _ := res.LastInsertId()
			unitIDs[i] = int(id64)
			key := u.Qualified
			if key == "" {
				key = u.Name
			}
			if key != "" {
				ids[key] = int(id64)
			}
		}
	}

	// Insert edges с разрешением src_id по индексу и dst_id по имени
	if len(edgeRefs) > 0 {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT INTO edges(repo, src_id, dst_id, kind, dst_name, file_path, line, dst_repo, confidence) VALUES(?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return nil, nil, err
		}
		defer stmt.Close()

		for _, e := range edgeRefs {
			// Приоритет: ResolvedSrcID → SrcUnitIdx через unitIDs
			srcID := 0
			if e.ResolvedSrcID > 0 {
				srcID = e.ResolvedSrcID
			} else if e.SrcUnitIdx >= 0 && e.SrcUnitIdx < len(unitIDs) {
				srcID = unitIDs[e.SrcUnitIdx]
			}
			// Приоритет: уже разрешённый ResolvedDstID → локальный DstUnitIdx → ids по имени
			dstID := 0
			if e.ResolvedDstID > 0 {
				dstID = e.ResolvedDstID
			} else if e.DstUnitIdx >= 0 && e.DstUnitIdx < len(unitIDs) {
				dstID = unitIDs[e.DstUnitIdx]
			} else if id, ok := ids[e.DstName]; ok {
				dstID = id
			}
			if srcID == 0 {
				// Skip edges with unresolved src — should never happen for valid edges
				continue
			}
			confidence := e.Confidence
			if confidence == 0 {
				confidence = 1.0
			}
			// Write edge even if dst is unresolved (dst_id = 0) — ResolvePendingEdges will fix it later
			if _, err := stmt.ExecContext(ctx, repo, srcID, dstID, e.Kind, e.DstName, filePath, e.Line, e.DstRepo, confidence); err != nil {
				return nil, nil, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return ids, unitIDs, nil
}

// FindASTUnitsByFileLine находит AST unit, содержащий указанную линию в файле.
func (s *SQLite) FindASTUnitsByFileLine(ctx context.Context, filePath string, line int) ([]ASTUnit, error) {
	q := `SELECT ` + astUnitColumns + ` FROM ast_units WHERE file_path = ? AND start_line <= ? AND end_line >= ? ORDER BY start_line DESC LIMIT 1`
	rows, err := s.db.QueryContext(ctx, q, filePath, line, line)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ASTUnit
	for rows.Next() {
		u, err := scanASTUnit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
