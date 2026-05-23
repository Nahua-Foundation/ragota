package store

// Файл реализует CRUD рёбер графа (таблица edges) и отложенный резолв
// dst_id по dst_name: Resolve/Replace/EdgesFrom/EdgesTo/EdgesByDstName +
// общие helper'ы scanEdges + edgeColumns.

import (
	"context"
	"database/sql"
)

// ResolvePendingEdges пытается разрешить dst_id для всех edges с dst_id=0,
// используя точное совпадение dst_name с ast_units.qualified или
// ast_units.name. Возвращает количество разрешённых рёбер.
func (s *SQLite) ResolvePendingEdges(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE edges SET dst_id = (
			SELECT id FROM ast_units
			 WHERE ast_units.qualified = edges.dst_name OR ast_units.name = edges.dst_name
			 ORDER BY CASE WHEN ast_units.qualified = edges.dst_name THEN 0 ELSE 1 END
			 LIMIT 1
		)
		WHERE dst_id = 0 AND dst_name <> '' AND EXISTS (
			SELECT 1 FROM ast_units WHERE ast_units.qualified = edges.dst_name OR ast_units.name = edges.dst_name
		)`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ReplaceEdges атомарно заменяет рёбра, исходящие из любого ast_unit'а
// данного srcFile. (Файл — естественная граница инкрементальной
// переиндексации.)
func (s *SQLite) ReplaceEdges(ctx context.Context, srcFile string, edges []Edge) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM edges WHERE src_id IN (SELECT id FROM ast_units WHERE file_path = ?)`, srcFile); err != nil {
		return err
	}
	if len(edges) == 0 {
		return tx.Commit()
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO edges(src_id, dst_id, kind, dst_name, file_path, line) VALUES(?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range edges {
		if _, err := stmt.ExecContext(ctx, e.SrcID, e.DstID, e.Kind, e.DstName, e.FilePath, e.Line); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func scanEdges(rows *sql.Rows) ([]Edge, error) {
	defer rows.Close()
	out := []Edge{}
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.ID, &e.SrcID, &e.DstID, &e.Kind, &e.DstName, &e.FilePath, &e.Line); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

const edgeColumns = `id, src_id, dst_id, kind, dst_name, file_path, line`

// EdgesFrom возвращает все исходящие рёбра из srcID. kind="" — без фильтра.
func (s *SQLite) EdgesFrom(ctx context.Context, srcID int64, kind string) ([]Edge, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if kind == "" {
		rows, err = s.db.QueryContext(ctx, `SELECT `+edgeColumns+` FROM edges WHERE src_id = ?`, srcID)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT `+edgeColumns+` FROM edges WHERE src_id = ? AND kind = ?`, srcID, kind)
	}
	if err != nil {
		return nil, err
	}
	return scanEdges(rows)
}

// EdgesTo возвращает все входящие рёбра в dstID. kind="" — без фильтра.
func (s *SQLite) EdgesTo(ctx context.Context, dstID int64, kind string) ([]Edge, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if kind == "" {
		rows, err = s.db.QueryContext(ctx, `SELECT `+edgeColumns+` FROM edges WHERE dst_id = ?`, dstID)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT `+edgeColumns+` FROM edges WHERE dst_id = ? AND kind = ?`, dstID, kind)
	}
	if err != nil {
		return nil, err
	}
	return scanEdges(rows)
}

// EdgesByDstName возвращает рёбра по имени dst (для unresolved или внешних).
// Если name не содержит точек, ищет также по суффиксу .name
func (s *SQLite) EdgesByDstName(ctx context.Context, name, kind string) ([]Edge, error) {
	var (
		rows *sql.Rows
		err  error
	)
	q := `SELECT ` + edgeColumns + ` FROM edges WHERE (dst_name = ? OR dst_name LIKE ?)`
	args := []any{name, "%." + name}

	if kind == "" {
		rows, err = s.db.QueryContext(ctx, q, args...)
	} else {
		q += ` AND kind = ?`
		args = append(args, kind)
		rows, err = s.db.QueryContext(ctx, q, args...)
	}
	if err != nil {
		return nil, err
	}
	return scanEdges(rows)
}
