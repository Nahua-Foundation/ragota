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
// ast_units.name. Резолв выполняется в пределах одного языка И одной репы:
//
//   - Языковая граница: предотвращает кросс-языковые ложные привязки
//     (например, TS-вызов log() не должен матчиться с Go-функцией log).
//   - Repo-граница (multi-repo workspace): функция Save в репо A никогда
//     не должна резолвиться в Save из репо B — графы репо независимы.
//
// Алгоритм:
//  1. Определяем язык/репо каждого ребра по его src (JOIN с ast_units).
//  2. Ищем кандидата в ast_units того же language И того же repo.
//  3. Сначала qualified-матч, затем name-матч для оставшихся.
//
// Возвращает суммарное количество разрешённых рёбер.
func (s *SQLite) ResolvePendingEdges(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// Используем временную таблицу для сбора разрешенных ID.
	// Это НАМНОГО быстрее, чем коррелированные подзапросы в UPDATE.
	if _, err := tx.ExecContext(ctx, `CREATE TEMPORARY TABLE IF NOT EXISTS tmp_resolved (row_id INTEGER PRIMARY KEY, dst_id INTEGER)`); err != nil {
		return 0, err
	}
	defer func() {
		_, _ = tx.ExecContext(ctx, `DELETE FROM tmp_resolved`)
	}()

	var total int64

	// Pass 1: qualified-матч (точный, кросс-файловый, одноязычный, одна репа).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tmp_resolved (row_id, dst_id)
		SELECT e.rowid, MIN(dst.id)
		FROM edges e
		JOIN ast_units src ON src.id = e.src_id
		JOIN ast_units dst ON dst.qualified = e.dst_name 
		                  AND dst.language = src.language 
		                  AND dst.repo = src.repo
		WHERE e.dst_id = 0 AND e.dst_name <> ''
		GROUP BY e.rowid`); err != nil {
		return 0, err
	}
	res1, err := tx.ExecContext(ctx, `UPDATE edges SET dst_id = (SELECT dst_id FROM tmp_resolved WHERE row_id = edges.rowid) WHERE rowid IN (SELECT row_id FROM tmp_resolved)`)
	if err != nil {
		return 0, err
	}
	n1, _ := res1.RowsAffected()
	total += n1
	if _, err := tx.ExecContext(ctx, `DELETE FROM tmp_resolved`); err != nil {
		return 0, err
	}

	// Pass 2: Локальный name-матч (в пределах того же файла).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tmp_resolved (row_id, dst_id)
		SELECT e.rowid, MIN(dst.id)
		FROM edges e
		JOIN ast_units src ON src.id = e.src_id
		JOIN ast_units dst ON dst.name = e.dst_name 
		                  AND dst.language = src.language 
		                  AND dst.file_path = src.file_path
		                  AND dst.repo = src.repo
		WHERE e.dst_id = 0 AND e.dst_name <> ''
		GROUP BY e.rowid`); err != nil {
		return 0, err
	}
	res2, err := tx.ExecContext(ctx, `UPDATE edges SET dst_id = (SELECT dst_id FROM tmp_resolved WHERE row_id = edges.rowid) WHERE rowid IN (SELECT row_id FROM tmp_resolved)`)
	if err != nil {
		return 0, err
	}
	n2, _ := res2.RowsAffected()
	total += n2
	if _, err := tx.ExecContext(ctx, `DELETE FROM tmp_resolved`); err != nil {
		return 0, err
	}

	// Pass 3: Относительный путь для импортов (JS/TS).
	// Pass 3 оставляем как есть или тоже через tmp, но там LIKE, он медленный сам по себе.
	// Для консистентности сделаем через tmp.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tmp_resolved (row_id, dst_id)
		SELECT e.rowid, MIN(dst.id)
		FROM edges e
		JOIN ast_units src ON src.id = e.src_id
		JOIN ast_units dst ON dst.kind = 'module'
		                  AND dst.language = src.language
		                  AND dst.repo = src.repo
		                  AND dst.file_path LIKE '%' || REPLACE(REPLACE(e.dst_name, './', ''), '../', '') || '%'
		WHERE e.dst_id = 0 AND e.kind = 'import' AND (e.dst_name LIKE './%' OR e.dst_name LIKE '../%')
		GROUP BY e.rowid`); err != nil {
		return 0, err
	}
	res3, err := tx.ExecContext(ctx, `UPDATE edges SET dst_id = (SELECT dst_id FROM tmp_resolved WHERE row_id = edges.rowid) WHERE rowid IN (SELECT row_id FROM tmp_resolved)`)
	if err != nil {
		return 0, err
	}
	n3, _ := res3.RowsAffected()
	total += n3
	if _, err := tx.ExecContext(ctx, `DELETE FROM tmp_resolved`); err != nil {
		return 0, err
	}

	// Pass 4: Глобальный name-матч для всё ещё неразрешённых.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tmp_resolved (row_id, dst_id)
		SELECT e.rowid, MIN(dst.id)
		FROM edges e
		JOIN ast_units src ON src.id = e.src_id
		JOIN ast_units dst ON dst.name = e.dst_name 
		                  AND dst.language = src.language 
		                  AND dst.repo = src.repo
		WHERE e.dst_id = 0 AND e.dst_name <> ''
		GROUP BY e.rowid`); err != nil {
		return 0, err
	}
	res4, err := tx.ExecContext(ctx, `UPDATE edges SET dst_id = (SELECT dst_id FROM tmp_resolved WHERE row_id = edges.rowid) WHERE rowid IN (SELECT row_id FROM tmp_resolved)`)
	if err != nil {
		return 0, err
	}
	n4, _ := res4.RowsAffected()
	total += n4

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	return int(total), nil
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
		`INSERT INTO edges(repo, src_id, dst_id, kind, dst_name, file_path, line) VALUES(?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range edges {
		if _, err := stmt.ExecContext(ctx, e.Repo, e.SrcID, e.DstID, e.Kind, e.DstName, e.FilePath, e.Line); err != nil {
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
		if err := rows.Scan(&e.ID, &e.Repo, &e.SrcID, &e.DstID, &e.Kind, &e.DstName, &e.FilePath, &e.Line); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

const edgeColumns = `id, repo, src_id, dst_id, kind, dst_name, file_path, line`

// EdgesFrom возвращает все исходящие рёбра из srcID. kind="" — без фильтра.
func (s *SQLite) EdgesFrom(ctx context.Context, srcID int, kind string) ([]Edge, error) {
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
func (s *SQLite) EdgesTo(ctx context.Context, dstID int, kind string) ([]Edge, error) {
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
// фильтрует рёбра по языку источника. Repo-фильтр не применяется (поиск
// идёт по всем репо). Эквивалентен EdgesByDstNameForLangRepo с repo="".
//
// Цель: при поиске callers/references/implementations не подмешивать
// совпадения имён из других языков (например, TS-вызов log() не должен
// возвращать Go-определения log).
func (s *SQLite) EdgesByDstNameForLang(ctx context.Context, name, kind, lang string) ([]Edge, error) {
	return s.EdgesByDstNameForLangRepo(ctx, name, kind, lang, "")
}

// EdgesByDstNameForLangRepo — расширение EdgesByDstNameForLang с
// фильтром по репе источника. В multi-repo workspace используется для
// гарантии, что граф не пересекает границы репы: callers/references для
// функции Save из репо A не должны вернуть совпадения dst_name="Save"
// из репо B.
//
// Параметры:
//   - name — имя или qualified-имя dst;
//   - kind — фильтр edges.kind ("" = без фильтра);
//   - lang — фильтр src.language ("" = без фильтра);
//   - repo — фильтр src.repo ("" или "*" — без фильтра).
func (s *SQLite) EdgesByDstNameForLangRepo(ctx context.Context, name, kind, lang, repo string) ([]Edge, error) {
	if lang == "" && (repo == "" || repo == "*") {
		return s.EdgesByDstName(ctx, name, kind)
	}
	// Явные алиасы edges.* обязательны: после JOIN с ast_units такие
	// колонки как `id`, `repo` становятся неоднозначными.
	q := `SELECT edges.id, edges.repo, edges.src_id, edges.dst_id, edges.kind, edges.dst_name, edges.file_path, edges.line
	      FROM edges
	      JOIN ast_units AS src ON src.id = edges.src_id
	      WHERE (edges.dst_name = ? OR edges.dst_name LIKE ?)`
	args := []any{name, "%." + name}
	if lang != "" {
		q += ` AND src.language = ?`
		args = append(args, lang)
	}
	if repo != "" && repo != "*" {
		q += ` AND src.repo = ?`
		args = append(args, repo)
	}
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
