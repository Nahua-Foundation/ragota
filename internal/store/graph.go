package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// ASTUnit — самостоятельная AST-единица: function/method/class/interface/module/...
// Используется для hybrid retrieval, parent-child навигации и graph expansion.
type ASTUnit struct {
	ID        int64
	FilePath  string
	Language  string
	Kind      string
	Name      string
	Qualified string
	ParentID  sql.NullInt64
	StartLine int
	EndLine   int
	StartByte int
	EndByte   int
	Signature string
	Doc       string
	Hash      string
}

// Edge — направленная связь между AST units.
//
// Kind:
//   - "call"        : src вызывает dst
//   - "import"      : src импортирует dst (модуль/файл)
//   - "implements"  : src реализует интерфейс dst
//   - "extends"     : src наследует от dst
//   - "reference"   : src ссылается на dst (поле/переменная/тип)
//   - "contains"    : src содержит dst (parent-child, дублирует ParentID, опционально)
type Edge struct {
	ID       int64
	SrcID    int64
	DstID    int64  // 0 если ещё не разрешено — тогда используется DstName
	Kind     string
	DstName  string
	FilePath string
	Line     int
}

// EmbedMeta — метаданные коллекции эмбеддингов (модель + размерность).
type EmbedMeta struct {
	Collection string
	Model      string
	Dim        int
	UpdatedAt  time.Time
}

// GetEmbedMeta возвращает текущие метаданные коллекции, либо nil если запись отсутствует.
func (s *SQLite) GetEmbedMeta(ctx context.Context, collection string) (*EmbedMeta, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT collection, model, dim, updated_at FROM embed_meta WHERE collection = ?`, collection)
	var m EmbedMeta
	var ts int64
	if err := row.Scan(&m.Collection, &m.Model, &m.Dim, &ts); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	m.UpdatedAt = time.Unix(ts, 0)
	return &m, nil
}

// SetEmbedMeta сохраняет/обновляет метаданные эмбеддингов коллекции.
func (s *SQLite) SetEmbedMeta(ctx context.Context, m EmbedMeta) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO embed_meta(collection, model, dim, updated_at)
		 VALUES(?,?,?,?)
		 ON CONFLICT(collection) DO UPDATE SET
		   model=excluded.model,
		   dim=excluded.dim,
		   updated_at=excluded.updated_at`,
		m.Collection, m.Model, m.Dim, time.Now().Unix())
	return err
}

// scanASTUnit — общий код для чтения строки ast_units.
func scanASTUnit(rows interface {
	Scan(dest ...any) error
}) (ASTUnit, error) {
	var u ASTUnit
	err := rows.Scan(
		&u.ID, &u.FilePath, &u.Language, &u.Kind, &u.Name, &u.Qualified,
		&u.ParentID, &u.StartLine, &u.EndLine, &u.StartByte, &u.EndByte,
		&u.Signature, &u.Doc, &u.Hash,
	)
	return u, err
}

const astUnitColumns = `id, file_path, language, kind, name, qualified, parent_id, start_line, end_line, start_byte, end_byte, signature, doc, hash`

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
		`INSERT INTO ast_units(file_path, language, kind, name, qualified, parent_id,
			start_line, end_line, start_byte, end_byte, signature, doc, hash)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	ids := make(map[string]int64, len(units))
	for _, u := range units {
		res, err := stmt.ExecContext(ctx,
			filePath, u.Language, u.Kind, u.Name, u.Qualified, u.ParentID,
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
// фильтруя по kind/language.
func (s *SQLite) FindASTUnits(ctx context.Context, query, kind, language string, limit int) ([]ASTUnit, error) {
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

// ExpandNeighbors — BFS вокруг nodeID с глубиной depth. kinds — фильтр
// рёбер; пусто = все. Возвращает множества узлов и рёбер, без дубликатов.
// Обход и по исходящим, и по входящим рёбрам — для симметричного «contextual neighbourhood».
func (s *SQLite) ExpandNeighbors(ctx context.Context, nodeID int64, depth int, kinds []string) ([]ASTUnit, []Edge, error) {
	if depth <= 0 {
		depth = 1
	}
	visited := map[int64]bool{nodeID: true}
	frontier := []int64{nodeID}
	allNodes := []ASTUnit{}
	allEdges := []Edge{}

	root, err := s.GetASTUnit(ctx, nodeID)
	if err != nil {
		return nil, nil, err
	}
	if root != nil {
		allNodes = append(allNodes, *root)
	}

	for d := 0; d < depth && len(frontier) > 0; d++ {
		var next []int64
		for _, id := range frontier {
			outE, err := s.edgesAround(ctx, id, kinds)
			if err != nil {
				return nil, nil, err
			}
			for _, e := range outE {
				allEdges = append(allEdges, e)
				var other int64
				if e.SrcID == id {
					other = e.DstID
				} else {
					other = e.SrcID
				}
				if other == 0 || visited[other] {
					continue
				}
				visited[other] = true
				u, err := s.GetASTUnit(ctx, other)
				if err != nil {
					return nil, nil, err
				}
				if u != nil {
					allNodes = append(allNodes, *u)
					next = append(next, other)
				}
			}
		}
		frontier = next
	}
	return allNodes, allEdges, nil
}

func (s *SQLite) edgesAround(ctx context.Context, id int64, kinds []string) ([]Edge, error) {
	q := `SELECT ` + edgeColumns + ` FROM edges WHERE (src_id = ? OR dst_id = ?)`
	args := []any{id, id}
	if len(kinds) > 0 {
		q += ` AND kind IN (`
		for i, k := range kinds {
			if i > 0 {
				q += ","
			}
			q += "?"
			args = append(args, k)
		}
		q += `)`
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return scanEdges(rows)
}
