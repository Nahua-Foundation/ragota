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
		&u.Signature, &u.Doc, &u.Hash,
	)
	return u, err
}

const astUnitColumns = `id, repo, file_path, language, kind, name, qualified, parent_id, start_line, end_line, start_byte, end_byte, signature, doc, hash`

// ReplaceASTUnits атомарно заменяет все AST-единицы файла. Возвращает map
// "name -> id" для свежезаписанных юнитов — удобно при последующей записи
// рёбер.
//
// units.ParentID может ссылаться по индексу (отрицательное значение -1*idx-1
// в IDColumn ParentID.Int64), но проще: ParentID.Valid=false для корня,
// либо пересохранять рёбра contains после реальной вставки. Здесь мы
// сохраняем как есть и предполагаем, что вызывающий уже корректно
// проставил parent_id (или nil) — что делает индексатор поверх параса.
func (s *SQLite) ReplaceASTUnits(ctx context.Context, filePath string, units []ASTUnit) (map[string]int64, error) {
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
			start_line, end_line, start_byte, end_byte, signature, doc, hash)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	ids := make(map[string]int64, len(units))
	for _, u := range units {
		res, err := stmt.ExecContext(ctx,
			u.Repo, filePath, u.Language, u.Kind, u.Name, u.Qualified, u.ParentID,
			u.StartLine, u.EndLine, u.StartByte, u.EndByte,
			u.Signature, u.Doc, u.Hash)
		if err != nil {
			return nil, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, err
		}
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
func (s *SQLite) GetASTUnit(ctx context.Context, id int64) (*ASTUnit, error) {
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

	q := `SELECT ` + astUnitColumns + ` FROM ast_units WHERE (name = ? COLLATE NOCASE OR qualified = ? COLLATE NOCASE OR name LIKE ? OR qualified LIKE ?)`
	args := []any{query, query, "%" + query + "%", "%" + query + "%"}
	if kind != "" {
		q += ` AND kind = ?`
		args = append(args, kind)
	}
	if language != "" {
		q += ` AND language = ?`
		args = append(args, language)
	}
	if repo != "" && repo != "*" {
		q += ` AND repo = ?`
		args = append(args, repo)
	}
	// Точные совпадения раньше LIKE.
	q += ` ORDER BY CASE WHEN name = ? COLLATE NOCASE OR qualified = ? COLLATE NOCASE THEN 0 ELSE 1 END, length(name) ASC LIMIT ?`
	args = append(args, query, query, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
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

// UpdateASTParents массово проставляет parent_id (map childID -> parentID).
// Используется во второй фазе индексации, когда реальные id уже известны.
func (s *SQLite) UpdateASTParents(ctx context.Context, updates map[int64]int64) error {
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

// ChildrenOf возвращает прямых детей AST unit.
func (s *SQLite) ChildrenOf(ctx context.Context, id int64) ([]ASTUnit, error) {
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
