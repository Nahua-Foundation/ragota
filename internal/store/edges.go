package store

// Файл реализует CRUD рёбер графа (таблица edges) и отложенный резолв
// dst_id по dst_name: Resolve/Replace/EdgesFrom/EdgesTo/EdgesByDstName +
// общие helper'ы scanEdges + edgeColumns.

import (
	"context"
	"database/sql"
	"fmt"
)

// resolveProgressFn вызывается между чанками для обновления прогресса.
type resolveProgressFn func(pass int, resolved int64, remaining int64)

// CrossRepoResolver — интерфейс для разрешения import paths → repo names.
// Реализуется manifests.Registry из crossrepo пакета.
type CrossRepoResolver interface {
	ResolveImport(importPath string) string
}

// countUnresolved возвращает количество edges с dst_id = 0.
func (s *SQLite) CountUnresolvedEdges(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM edges WHERE dst_id = 0 AND dst_name <> ''`).Scan(&n)
	return n, err
}

// resolveChunkSize — максимальное количество рёбер, разрешаемых
// за один pass. Большие кодовые базы (>100K unresolved edges)
// обрабатываются чанками, чтобы не создавать гигантские временные
// таблицы и не блокировать БД на минуты.
const resolveChunkSize = 50000

// ResolvePendingEdges пытается разрешить dst_id для всех edges с dst_id=0,
// используя точное совпадение dst_name с ast_units.qualified или
// ast_units.name. Резолв выполняется в пределах одного языка И одной репы:
//
//   - Языковая граница: предотвращает кросс-языковые ложные привязки
//     (например, TS-вызов log() не должен матчиться с Go-функцией log).
//   - Repo-граница (multi-repo workspace): функция Save в репо A никогда
//     не должна резолвиться в Save из репо B — графы репо независимы.
//
// Алгоритм (4 pass, чанки по 50K, early termination):
//  1. Qualified-матч (dst.qualified = e.dst_name)
//  2. Local name-матч (dst.name = e.dst_name, same file)
//  3. Import-матч (LIKE file_path для relative paths)
//  4. Global name-матч (fallback)
//
// После каждого pass и чанка проверяется количество оставшихся unresolved edges —
// если 0, оставшиеся pass пропускаются (early termination).
// Каждый pass обрабатывается чанками по 50K рёбер для стабильности.
// onProgress вызывается между чанками для обновления UI.
//
// Возвращает суммарное количество разрешённых рёбер.
func (s *SQLite) ResolvePendingEdges(ctx context.Context, onProgress resolveProgressFn) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// Временная таблица для сбора разрешённых ID.
	if _, err := tx.ExecContext(ctx, `CREATE TEMPORARY TABLE IF NOT EXISTS tmp_resolved (row_id INTEGER PRIMARY KEY, dst_id INTEGER)`); err != nil {
		return 0, err
	}
	defer func() {
		_, _ = tx.ExecContext(ctx, `DELETE FROM tmp_resolved`)
	}()

	var total int64

	// countUnresolved возвращает количество edges с dst_id = 0.
	countUnresolved := func() (int64, error) {
		var n int64
		err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM edges WHERE dst_id = 0 AND dst_name <> ''`).Scan(&n)
		return n, err
	}

	// Helper: resolve one pass with chunking + progress reporting.
	resolvePass := func(query string, pass int) (int64, error) {
		passTotal := int64(0)
		for {
			// Report progress BEFORE chunk execution so UI shows "still working"
			// even if remaining is 0 from previous chunk.
			if onProgress != nil {
				rem, _ := countUnresolved()
				onProgress(pass, passTotal, rem)
			}

			chunked := query + fmt.Sprintf(` LIMIT %d`, resolveChunkSize)
			if _, err := tx.ExecContext(ctx, `INSERT INTO tmp_resolved (row_id, dst_id) `+chunked); err != nil {
				return 0, err
			}
			res, err := tx.ExecContext(ctx, `UPDATE edges SET dst_id = (SELECT dst_id FROM tmp_resolved WHERE row_id = edges.rowid) WHERE rowid IN (SELECT row_id FROM tmp_resolved)`)
			if err != nil {
				return 0, err
			}
			n, _ := res.RowsAffected()
			passTotal += n
			total += n
			if _, err := tx.ExecContext(ctx, `DELETE FROM tmp_resolved`); err != nil {
				return 0, err
			}

			if n < resolveChunkSize {
				// Чанк не заполнен — значит все рёбра этого pass обработаны.
				break
			}
		}
		return passTotal, nil
	}

	// ── Pass 1: qualified-матч (точный, кросс-файловый, одноязычный, одна репа).
	q1 := `SELECT e.rowid, MIN(dst.id)
		FROM edges e
		JOIN ast_units src ON src.id = e.src_id
		JOIN ast_units dst ON dst.qualified = e.dst_name
		                  AND dst.language = src.language
		                  AND dst.repo = src.repo
		WHERE e.dst_id = 0 AND e.dst_name <> ''
		GROUP BY e.rowid`
	n1, err := resolvePass(q1, 1)
	if err != nil {
		return 0, err
	}
	// Early termination
	if rem, _ := countUnresolved(); rem == 0 {
		return int(total), nil
	}

	// ── Pass 2: Локальный name-матч (в пределах того же файла).
	q2 := `SELECT e.rowid, MIN(dst.id)
		FROM edges e
		JOIN ast_units src ON src.id = e.src_id
		JOIN ast_units dst ON dst.name = e.dst_name
		                  AND dst.language = src.language
		                  AND dst.file_path = src.file_path
		                  AND dst.repo = src.repo
		WHERE e.dst_id = 0 AND e.dst_name <> ''
		GROUP BY e.rowid`
	n2, err := resolvePass(q2, 2)
	if err != nil {
		return 0, err
	}
	if rem, _ := countUnresolved(); rem == 0 {
		return int(total), nil
	}

	// ── Pass 3: Import-матч (JS/TS) — relative path resolution.
	// Нормализуем "./foo" → "foo", "../bar/baz" → "bar/baz" и ищем
	// module unit, чей file_path содержит нормализованный путь.
	// LIKE неизбеен, но chunking + early termination делают его терпимым.
	q3 := `SELECT e.rowid, MIN(dst.id)
		FROM edges e
		JOIN ast_units src ON src.id = e.src_id
		JOIN ast_units dst ON dst.kind = 'module'
		                  AND dst.language = src.language
		                  AND dst.repo = src.repo
		                  AND dst.file_path LIKE '%' || REPLACE(REPLACE(e.dst_name, './', ''), '../', '') || '%'
		WHERE e.dst_id = 0 AND e.kind = 'import'
		  AND (e.dst_name LIKE './%' OR e.dst_name LIKE '../%')
		GROUP BY e.rowid`
	n3, err := resolvePass(q3, 3)
	if err != nil {
		return 0, err
	}
	if rem, _ := countUnresolved(); rem == 0 {
		return int(total), nil
	}

	// ── Pass 4: Глобальный name-матч для всё ещё неразрешённых.
	q4 := `SELECT e.rowid, MIN(dst.id)
		FROM edges e
		JOIN ast_units src ON src.id = e.src_id
		JOIN ast_units dst ON dst.name = e.dst_name
		                  AND dst.language = src.language
		                  AND dst.repo = src.repo
		WHERE e.dst_id = 0 AND e.dst_name <> ''
		GROUP BY e.rowid`
	n4, err := resolvePass(q4, 4)
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	_ = n1
	_ = n2
	_ = n3
	_ = n4
	return int(total), nil
}

// ResolvePendingEdgesCrossRepo — Pass 5: cross-repo import resolution.
// Использует resolver для маппинга import_path → repo_name.
// Для resolved edges ставит dst_repo и confidence = 1.0.
// dst_id остаётся 0 если target module не найден в целевой репе.
func (s *SQLite) ResolvePendingEdgesCrossRepo(ctx context.Context, resolver CrossRepoResolver, onProgress resolveProgressFn) (int, error) {
	if resolver == nil {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// Получаем все unresolved import edges
	rows, err := tx.QueryContext(ctx,
		`SELECT `+edgeColumns+` FROM edges WHERE kind = 'import' AND dst_id = 0 AND dst_name <> ''`)
	if err != nil {
		return 0, err
	}
	edges, err := scanEdges(rows)
	if err != nil {
		return 0, err
	}

	if len(edges) == 0 {
		return 0, nil
	}

	var totalResolved int64
	for _, e := range edges {
		targetRepo := resolver.ResolveImport(e.DstName)
		if targetRepo == "" {
			continue
		}

		// Update edge with dst_repo
		_, err := tx.ExecContext(ctx,
			`UPDATE edges SET dst_repo = ?, confidence = 1.0 WHERE id = ?`,
			targetRepo, e.ID)
		if err != nil {
			return 0, err
		}
		totalResolved++

		if onProgress != nil {
			onProgress(5, totalResolved, int64(len(edges))-totalResolved)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	return int(totalResolved), nil
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
		`INSERT INTO edges(repo, src_id, dst_id, kind, dst_name, file_path, line, dst_repo, confidence) VALUES(?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range edges {
		conf := e.Confidence
		if conf == 0 {
			conf = 1.0
		}
		if _, err := stmt.ExecContext(ctx, e.Repo, e.SrcID, e.DstID, e.Kind, e.DstName, e.FilePath, e.Line, e.DstRepo, conf); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RemoveFileGraph атомарно удаляет AST units и edges файла в одной транзакции.
func (s *SQLite) RemoveFileGraph(ctx context.Context, filePath string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// CASCADE удаляет edges при удалении units, но делаем явно для консистентности.
	if _, err := tx.ExecContext(ctx, `DELETE FROM edges WHERE src_id IN (SELECT id FROM ast_units WHERE file_path = ?)`, filePath); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ast_units WHERE file_path = ?`, filePath); err != nil {
		return err
	}
	return tx.Commit()
}

func scanEdges(rows *sql.Rows) ([]Edge, error) {
	defer rows.Close()
	out := []Edge{}
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.ID, &e.Repo, &e.SrcID, &e.DstID, &e.Kind, &e.DstName, &e.FilePath, &e.Line, &e.DstRepo, &e.Confidence); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

const edgeColumns = `id, repo, src_id, dst_id, kind, dst_name, file_path, line, dst_repo, confidence`

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

// DeleteEdgesByFilePath удаляет edges указанного вида для данного файла.
func (s *SQLite) DeleteEdgesByFilePath(ctx context.Context, filePath, kind string) error {
	if kind == "" {
		_, err := s.db.ExecContext(ctx,
			`DELETE FROM edges WHERE src_id IN (SELECT id FROM ast_units WHERE file_path = ?)`, filePath)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM edges WHERE src_id IN (SELECT id FROM ast_units WHERE file_path = ?) AND kind = ?`, filePath, kind)
	return err
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

// AllEdgesByKind возвращает все edges указанного вида.
// kind="" — все edges.
func (s *SQLite) AllEdgesByKind(ctx context.Context, kind string) ([]Edge, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if kind == "" {
		rows, err = s.db.QueryContext(ctx, `SELECT `+edgeColumns+` FROM edges`)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT `+edgeColumns+` FROM edges WHERE kind = ?`, kind)
	}
	if err != nil {
		return nil, err
	}
	return scanEdges(rows)
}

// InsertEdge вставляет одно ребро.
func (s *SQLite) InsertEdge(ctx context.Context, e Edge) (int, error) {
	conf := e.Confidence
	if conf == 0 {
		conf = 1.0
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO edges(repo, src_id, dst_id, kind, dst_name, file_path, line, dst_repo, confidence) VALUES(?,?,?,?,?,?,?,?,?)`,
		e.Repo, e.SrcID, e.DstID, e.Kind, e.DstName, e.FilePath, e.Line, e.DstRepo, conf)
	if err != nil {
		return 0, err
	}
	id64, _ := res.LastInsertId()
	return int(id64), nil
}

// AllCrossRepoEdges возвращает все edges с dst_repo != "" (cross-repo связи).
func (s *SQLite) AllCrossRepoEdges(ctx context.Context) ([]Edge, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+edgeColumns+` FROM edges WHERE dst_repo <> ''`)
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
	q := `SELECT edges.id, edges.repo, edges.src_id, edges.dst_id, edges.kind, edges.dst_name, edges.file_path, edges.line, edges.dst_repo, edges.confidence
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
