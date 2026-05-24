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
// ast_units.name. Резолв выполняется в пределах одного языка — это
// предотвращает кросс-языковые ложные привязки (например, TS-вызов
// log() матчится с Go-функцией log в другом пакете).
//
// Алгоритм:
//  1. Определяем язык каждого ребра по его src (через JOIN с ast_units).
//  2. Ищем кандидата в ast_units, у которого тот же language и совпадает
//     qualified или name (qualified имеет приоритет).
//  3. Сначала прогон с qualified-матчем, затем — с name-матчем для
//     оставшихся (двухпроходный резолв проще и стабильнее, чем единый
//     UPDATE с коррелированным CASE ORDER BY в SQLite).
//
// Возвращает суммарное количество разрешённых рёбер.
func (s *SQLite) ResolvePendingEdges(ctx context.Context) (int, error) {
	// Pass 1: qualified-матч (точный, кросс-файловый, но одноязычный).
	res1, err := s.db.ExecContext(ctx, `
		UPDATE edges SET dst_id = (
			SELECT dst.id FROM ast_units AS dst
			 JOIN ast_units AS src ON src.id = edges.src_id
			 WHERE dst.qualified = edges.dst_name
			   AND dst.language = src.language
			 LIMIT 1
		)
		WHERE dst_id = 0 AND dst_name <> '' AND EXISTS (
			SELECT 1 FROM ast_units AS dst
			 JOIN ast_units AS src ON src.id = edges.src_id
			 WHERE dst.qualified = edges.dst_name
			   AND dst.language = src.language
		)`)
	if err != nil {
		return 0, err
	}
	n1, _ := res1.RowsAffected()

	// Pass 2: name-матч для всё ещё неразрешённых (всё так же в пределах
	// одного языка). qualified уже был разрешён в первом проходе и сюда не
	// попадёт благодаря условию dst_id = 0.
	res2, err := s.db.ExecContext(ctx, `
		UPDATE edges SET dst_id = (
			SELECT dst.id FROM ast_units AS dst
			 JOIN ast_units AS src ON src.id = edges.src_id
			 WHERE dst.name = edges.dst_name
			   AND dst.language = src.language
			 LIMIT 1
		)
		WHERE dst_id = 0 AND dst_name <> '' AND EXISTS (
			SELECT 1 FROM ast_units AS dst
			 JOIN ast_units AS src ON src.id = edges.src_id
			 WHERE dst.name = edges.dst_name
			   AND dst.language = src.language
		)`)
	if err != nil {
		return 0, err
	}
	n2, _ := res2.RowsAffected()

	return int(n1 + n2), nil
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
// Если name не содержит точек, ищет также по суффиксу .name.
//
// ВНИМАНИЕ: не фильтрует по языку — может вернуть кросс-языковые ложные
// совпадения. Для языко-чувствительных сценариев (find_callers,
// find_references, find_implementations) используйте EdgesByDstNameForLang.
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

// EdgesByDstNameForLang — то же, что EdgesByDstName, но дополнительно
// фильтрует рёбра по языку источника: учитываются только рёбра, у которых
// исходный ast_unit (edges.src_id) имеет ast_units.language = lang.
// Если lang == "" — поведение совпадает с EdgesByDstName.
//
// Цель: при поиске callers/references/implementations не подмешивать
// совпадения имён из других языков (например, TS-вызов log() не должен
// возвращать Go-определения log).
func (s *SQLite) EdgesByDstNameForLang(ctx context.Context, name, kind, lang string) ([]Edge, error) {
	if lang == "" {
		return s.EdgesByDstName(ctx, name, kind)
	}
	// Явные алиасы в SELECT обязательны: после JOIN с ast_units колонка
	// `id` становится неоднозначной (есть и в edges, и в ast_units).
	q := `SELECT edges.id, edges.src_id, edges.dst_id, edges.kind, edges.dst_name, edges.file_path, edges.line
	      FROM edges
	      JOIN ast_units AS src ON src.id = edges.src_id
	      WHERE (edges.dst_name = ? OR edges.dst_name LIKE ?)
	        AND src.language = ?`
	args := []any{name, "%." + name, lang}
	if kind != "" {
		q += ` AND edges.kind = ?`
		args = append(args, kind)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return scanEdges(rows)
}
