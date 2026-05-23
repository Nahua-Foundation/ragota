// Package store — SQLite-хранилище метаданных файлов и символов tree-sitter.
// Используется pure-Go драйвер modernc.org/sqlite (без CGO).
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

// FileRow — строка таблицы files.
type FileRow struct {
	Path      string
	Language  string
	Hash      string
	Size      int64
	ModTime   time.Time
	IndexedAt time.Time
	Symbols   int
	VecHash   string
}

// SymbolRow — строка таблицы symbols.
type SymbolRow struct {
	ID         int64
	FilePath   string
	Name       string
	Kind       string // function/method/class/struct/interface/var/const/...
	StartLine  int
	EndLine    int
	StartByte  int
	EndByte    int
	ParentName string
	Signature  string
}

// SQLite — хранилище.
type SQLite struct {
	db *sql.DB
}

// Open открывает БД (создаёт файл и схему при необходимости).
func Open(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	db.SetMaxOpenConns(1) // упрощает жизнь с WAL и pure-go драйвером
	s := &SQLite{db: db}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close закрывает БД.
func (s *SQLite) Close() error { return s.db.Close() }

func (s *SQLite) init() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS files (
			path        TEXT PRIMARY KEY,
			language    TEXT NOT NULL,
			hash        TEXT NOT NULL,
			size        INTEGER NOT NULL,
			mod_time    INTEGER NOT NULL,
			indexed_at  INTEGER NOT NULL,
			symbol_cnt  INTEGER NOT NULL DEFAULT 0,
			vec_hash    TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS symbols (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			file_path   TEXT NOT NULL,
			name        TEXT NOT NULL,
			kind        TEXT NOT NULL,
			start_line  INTEGER NOT NULL,
			end_line    INTEGER NOT NULL,
			start_byte  INTEGER NOT NULL,
			end_byte    INTEGER NOT NULL,
			parent_name TEXT NOT NULL DEFAULT '',
			signature   TEXT NOT NULL DEFAULT '',
			FOREIGN KEY (file_path) REFERENCES files(path) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_symbols_file ON symbols(file_path)`,
		`CREATE INDEX IF NOT EXISTS idx_symbols_name ON symbols(name)`,
		`CREATE INDEX IF NOT EXISTS idx_symbols_kind ON symbols(kind)`,

		// AST units — самостоятельные единицы кода: function, method, class,
		// interface, module/file, struct, enum и т.п. Используются для
		// hybrid retrieval (vector + BM25) и parent-child навигации.
		`CREATE TABLE IF NOT EXISTS ast_units (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			file_path   TEXT NOT NULL,
			language    TEXT NOT NULL,
			kind        TEXT NOT NULL,           -- function|method|class|interface|module|struct|enum|...
			name        TEXT NOT NULL,
			qualified   TEXT NOT NULL DEFAULT '', -- pkg.Class.method или эквивалент
			parent_id   INTEGER,                  -- родительская AST-единица (для parent-child)
			start_line  INTEGER NOT NULL,
			end_line    INTEGER NOT NULL,
			start_byte  INTEGER NOT NULL,
			end_byte    INTEGER NOT NULL,
			signature   TEXT NOT NULL DEFAULT '',
			doc         TEXT NOT NULL DEFAULT '',
			hash        TEXT NOT NULL DEFAULT '', -- хэш содержимого юнита для инкрементальной индексации
			FOREIGN KEY (file_path) REFERENCES files(path) ON DELETE CASCADE,
			FOREIGN KEY (parent_id) REFERENCES ast_units(id) ON DELETE SET NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_ast_units_file ON ast_units(file_path)`,
		`CREATE INDEX IF NOT EXISTS idx_ast_units_name ON ast_units(name)`,
		`CREATE INDEX IF NOT EXISTS idx_ast_units_kind ON ast_units(kind)`,
		`CREATE INDEX IF NOT EXISTS idx_ast_units_qualified ON ast_units(qualified)`,
		`CREATE INDEX IF NOT EXISTS idx_ast_units_parent ON ast_units(parent_id)`,

		// Edges — направленные связи между AST-единицами для graph expansion.
		// kind: call | import | implements | reference | extends | contains
		`CREATE TABLE IF NOT EXISTS edges (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			src_id    INTEGER NOT NULL,
			dst_id    INTEGER NOT NULL,
			kind      TEXT NOT NULL,
			-- Если dst пока неразрешён (forward-reference / внешний символ),
			-- то dst_id = 0 и используется dst_name для отложенного резолва.
			dst_name  TEXT NOT NULL DEFAULT '',
			file_path TEXT NOT NULL DEFAULT '',
			line      INTEGER NOT NULL DEFAULT 0,
			FOREIGN KEY (src_id) REFERENCES ast_units(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_src  ON edges(src_id, kind)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_dst  ON edges(dst_id, kind)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_kind ON edges(kind)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_dst_name ON edges(dst_name) WHERE dst_id = 0`,

		// embed_meta — метаданные эмбеддингов по коллекциям. Используется,
		// чтобы при смене модели/размерности эмбеддингов автоматически
		// триггерить full reindex соответствующей коллекции.
		`CREATE TABLE IF NOT EXISTS embed_meta (
			collection TEXT PRIMARY KEY,
			model      TEXT NOT NULL,
			dim        INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
	}
	// ALTER для существующих баз
	_, _ = s.db.Exec(`ALTER TABLE files ADD COLUMN vec_hash TEXT NOT NULL DEFAULT ''`)

	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("sqlite init: %w", err)
		}
	}
	return nil
}

// GetFile возвращает строку файла или nil, если не найдена.
func (s *SQLite) GetFile(ctx context.Context, path string) (*FileRow, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT path, language, hash, size, mod_time, indexed_at, symbol_cnt, vec_hash FROM files WHERE path = ?`,
		path)
	var fr FileRow
	var modTime, indexedAt int64
	err := row.Scan(&fr.Path, &fr.Language, &fr.Hash, &fr.Size, &modTime, &indexedAt, &fr.Symbols, &fr.VecHash)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	fr.ModTime = time.Unix(modTime, 0)
	fr.IndexedAt = time.Unix(indexedAt, 0)
	return &fr, nil
}

// UpsertFile добавляет/обновляет запись файла и заменяет его символы атомарно.
func (s *SQLite) UpsertFile(ctx context.Context, f FileRow, symbols []SymbolRow) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO files(path, language, hash, size, mod_time, indexed_at, symbol_cnt, vec_hash)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(path) DO UPDATE SET
		   language=excluded.language,
		   hash=excluded.hash,
		   size=excluded.size,
		   mod_time=excluded.mod_time,
		   indexed_at=excluded.indexed_at,
		   symbol_cnt=excluded.symbol_cnt,
		   vec_hash=CASE WHEN excluded.vec_hash != '' THEN excluded.vec_hash ELSE files.vec_hash END`,
		f.Path, f.Language, f.Hash, f.Size, f.ModTime.Unix(), time.Now().Unix(), len(symbols), f.VecHash); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM symbols WHERE file_path = ?`, f.Path); err != nil {
		return err
	}
	if len(symbols) > 0 {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT INTO symbols(file_path, name, kind, start_line, end_line, start_byte, end_byte, parent_name, signature)
			 VALUES(?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, sym := range symbols {
			if _, err := stmt.ExecContext(ctx,
				f.Path, sym.Name, sym.Kind, sym.StartLine, sym.EndLine,
				sym.StartByte, sym.EndByte, sym.ParentName, sym.Signature); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// EnsureFile гарантирует наличие записи файла в таблице files.
// Если файла нет, создает минимальную запись.
func (s *SQLite) EnsureFile(ctx context.Context, path, lang string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO files(path, language, hash, size, mod_time, indexed_at, symbol_cnt, vec_hash)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(path) DO NOTHING`,
		path, lang, "", 0, 0, time.Now().Unix(), 0, "")
	return err
}

// UpdateVectorHash обновляет только хэш векторного индекса.
func (s *SQLite) UpdateVectorHash(ctx context.Context, path, hash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE files SET vec_hash = ? WHERE path = ?`, hash, path)
	if err != nil {
		return err
	}
	// Если файла еще нет в таблице files (например, tree-sitter выключен),
	// нужно его вставить. Но для этого нужны language, size и т.д.
	// Поэтому безопаснее использовать UpsertFile с пустыми символами.
	return nil
}

// UpsertVectorHash — специальный апсерт для векторного индекса.
func (s *SQLite) UpsertVectorHash(ctx context.Context, path, hash, lang string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO files(path, language, hash, size, mod_time, indexed_at, symbol_cnt, vec_hash)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(path) DO UPDATE SET
		   vec_hash=excluded.vec_hash,
		   language=excluded.language,
		   size=excluded.size,
		   mod_time=excluded.mod_time`,
		path, lang, "", info.Size(), info.ModTime().Unix(), time.Now().Unix(), 0, hash)
	return err
}

// DeleteFile удаляет файл и его символы.
func (s *SQLite) DeleteFile(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM files WHERE path = ?`, path)
	return err
}

// SearchSymbols ищет символы по подстроке имени, опционально фильтруя по kind и языку.
func (s *SQLite) SearchSymbols(ctx context.Context, query, kind, language string, limit int) ([]SymbolRow, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT s.id, s.file_path, s.name, s.kind, s.start_line, s.end_line, s.start_byte, s.end_byte, s.parent_name, s.signature
	      FROM symbols s JOIN files f ON f.path = s.file_path
	      WHERE s.name LIKE ?`
	args := []any{"%" + query + "%"}
	if kind != "" {
		q += ` AND s.kind = ?`
		args = append(args, kind)
	}
	if language != "" {
		q += ` AND f.language = ?`
		args = append(args, language)
	}
	q += ` ORDER BY length(s.name) ASC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SymbolRow{}
	for rows.Next() {
		var r SymbolRow
		if err := rows.Scan(&r.ID, &r.FilePath, &r.Name, &r.Kind, &r.StartLine, &r.EndLine, &r.StartByte, &r.EndByte, &r.ParentName, &r.Signature); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SymbolsByFile возвращает все символы из файла, упорядоченные по позиции.
func (s *SQLite) SymbolsByFile(ctx context.Context, path string) ([]SymbolRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, file_path, name, kind, start_line, end_line, start_byte, end_byte, parent_name, signature
		 FROM symbols WHERE file_path = ? ORDER BY start_byte`, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SymbolRow{}
	for rows.Next() {
		var r SymbolRow
		if err := rows.Scan(&r.ID, &r.FilePath, &r.Name, &r.Kind, &r.StartLine, &r.EndLine, &r.StartByte, &r.EndByte, &r.ParentName, &r.Signature); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Stats — общая статистика индекса.
type Stats struct {
	Files   int
	Symbols int
}

// GraphStats — статистика графового индекса.
type GraphStats struct {
	Units int
	Edges int
}

// Stats возвращает количество файлов и символов.
func (s *SQLite) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files`).Scan(&st.Files); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM symbols`).Scan(&st.Symbols); err != nil {
		return st, err
	}
	return st, nil
}

// GraphStats возвращает количество AST юнитов и ребер.
func (s *SQLite) GraphStats(ctx context.Context) (GraphStats, error) {
	var st GraphStats
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ast_units`).Scan(&st.Units); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM edges`).Scan(&st.Edges); err != nil {
		return st, err
	}
	return st, nil
}
