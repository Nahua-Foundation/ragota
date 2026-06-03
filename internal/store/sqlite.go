// Package store — SQLite-хранилище метаданных файлов и символов tree-sitter.
// Используется pure-Go драйвер modernc.org/sqlite (без CGO).
//
// Структура файлов:
//   - sqlite.go   — SQLite struct, Open, OpenFresh, Close, init (миграции).
//   - files.go    — FileRow + CRUD файлов (GetFile, UpsertFile, EnsureFile, hash ops).
//   - symbols.go  — SymbolRow + SearchSymbols, SymbolsByFile.
//   - ast_units.go — CRUD AST units.
//   - edges.go    — CRUD рёбер + отложенный резолв.
//   - neighbors.go — BFS-обход графа.
//   - embed_meta.go — EmbedMeta + Get/SetEmbedMeta.
//   - graph.go    — доменные типы (ASTUnit, Edge).
//   - stats.go    — Stats, GraphStats.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	"ragota/pkg/logger"

	_ "modernc.org/sqlite"
)

// SQLite — хранилище.
type SQLite struct {
	db *sql.DB
}

// Open открывает БД (создаёт файл и схему при необходимости).
func Open(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(30000)")
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	db.SetMaxOpenConns(4)
	s := &SQLite{db: db}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// OpenFresh открывает БД, предварительно удаляя файлы, если текущий
// workspace-signature не совпадает с сохранённым.
func OpenFresh(path, signature string) (*SQLite, error) {
	if signature != "" {
		if prev, err := readSignature(path); err == nil && prev != "" && prev != signature {
			removeDBFiles(path)
		}
	}
	s, err := Open(path)
	if err != nil {
		return nil, err
	}
	if signature != "" {
		if err := s.setSignature(signature); err != nil {
			_ = s.Close()
			return nil, err
		}
	}
	return s, nil
}

func removeDBFiles(path string) {
	for _, suf := range []string{"", "-wal", "-shm", "-journal"} {
		_ = os.Remove(path + suf)
	}
}

// readSignature читает workspace-signature напрямую через временное открытие БД.
func readSignature(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return "", err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var sig string
	err = db.QueryRow(`SELECT value FROM meta WHERE key = 'workspace_signature'`).Scan(&sig)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return sig, err
}

func (s *SQLite) setSignature(sig string) error {
	_, err := s.db.Exec(
		`INSERT INTO meta(key, value) VALUES('workspace_signature', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, sig)
	return err
}

// Close закрывает БД.
func (s *SQLite) Close() error { return s.db.Close() }

// GetDBForTests возвращает raw *sql.DB для тестов.
// Не используйте в production-коде — это нарушает инкапсуляцию store.
func (s *SQLite) GetDBForTests() *sql.DB {
	return s.db
}

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
		`CREATE TABLE IF NOT EXISTS ast_units (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			repo        TEXT NOT NULL DEFAULT '',
			file_path   TEXT NOT NULL,
			language    TEXT NOT NULL,
			kind        TEXT NOT NULL,
			name        TEXT NOT NULL,
			qualified   TEXT NOT NULL DEFAULT '',
			parent_id   INTEGER,
			start_line  INTEGER NOT NULL,
			end_line    INTEGER NOT NULL,
			start_byte  INTEGER NOT NULL,
			end_byte    INTEGER NOT NULL,
			name_start_line INTEGER NOT NULL DEFAULT 0,
			name_start_col  INTEGER NOT NULL DEFAULT 0,
			signature   TEXT NOT NULL DEFAULT '',
			doc         TEXT NOT NULL DEFAULT '',
			hash        TEXT NOT NULL DEFAULT '',
			FOREIGN KEY (file_path) REFERENCES files(path) ON DELETE CASCADE,
			FOREIGN KEY (parent_id) REFERENCES ast_units(id) ON DELETE SET NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_ast_units_file ON ast_units(file_path)`,
		`CREATE INDEX IF NOT EXISTS idx_ast_units_name ON ast_units(name COLLATE NOCASE)`,
		`CREATE INDEX IF NOT EXISTS idx_ast_units_kind ON ast_units(kind)`,
		`CREATE INDEX IF NOT EXISTS idx_ast_units_qualified ON ast_units(qualified COLLATE NOCASE)`,
		`CREATE INDEX IF NOT EXISTS idx_ast_units_parent ON ast_units(parent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_ast_units_repo_qualified ON ast_units(repo, qualified COLLATE NOCASE)`,
		`CREATE INDEX IF NOT EXISTS idx_ast_units_repo_name ON ast_units(repo, name COLLATE NOCASE)`,
		`CREATE TABLE IF NOT EXISTS edges (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			repo      TEXT NOT NULL DEFAULT '',
			src_id    INTEGER NOT NULL,
			dst_id    INTEGER NOT NULL,
			kind      TEXT NOT NULL,
			dst_name  TEXT NOT NULL DEFAULT '',
			file_path TEXT NOT NULL DEFAULT '',
			line      INTEGER NOT NULL DEFAULT 0,
			FOREIGN KEY (src_id) REFERENCES ast_units(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_src  ON edges(src_id, kind)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_dst  ON edges(dst_id, kind)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_kind ON edges(kind)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_unresolved ON edges(dst_id, dst_name) WHERE dst_id = 0`,
		`CREATE INDEX IF NOT EXISTS idx_edges_dst_name ON edges(dst_name) WHERE dst_id = 0`,
		`CREATE INDEX IF NOT EXISTS idx_edges_repo_dst_name_kind ON edges(repo, dst_name, kind)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_dst_name_kind_src ON edges(dst_name, kind, src_id)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_dst_name_full ON edges(dst_name, kind, src_id, repo)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_src_id ON edges(src_id)`,
		`CREATE INDEX IF NOT EXISTS idx_ast_units_qualified_lang_repo ON ast_units(qualified COLLATE NOCASE, language, repo)`,
		`CREATE INDEX IF NOT EXISTS idx_ast_units_name_lang_repo ON ast_units(name COLLATE NOCASE, language, repo)`,
		`CREATE TABLE IF NOT EXISTS meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS embed_meta (
			collection TEXT PRIMARY KEY,
			model      TEXT NOT NULL,
			dim        INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("sqlite init: %w", err)
		}
	}

	alters := []string{
		`ALTER TABLE files ADD COLUMN vec_hash TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE ast_units ADD COLUMN repo TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE edges ADD COLUMN repo TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE ast_units ADD COLUMN name_start_line INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE ast_units ADD COLUMN name_start_col INTEGER NOT NULL DEFAULT 0`,
	}
	for _, q := range alters {
		if _, err := s.db.Exec(q); err != nil {
			errStr := err.Error()
			if strings.Contains(errStr, "duplicate column") ||
				strings.Contains(errStr, "duplicate column name") {
				continue
			}
			logger.Log().Warn().Err(err).Msg("store: ALTER TABLE ignored (expected for existing schema)")
		}
	}

	postIdx := []string{
		`CREATE INDEX IF NOT EXISTS idx_ast_units_repo_qualified ON ast_units(repo, qualified)`,
		`CREATE INDEX IF NOT EXISTS idx_ast_units_repo_name ON ast_units(repo, name)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_repo_dst_name_kind ON edges(repo, dst_name, kind)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_dst_name_kind_src ON edges(dst_name, kind, src_id)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_dst_name_full ON edges(dst_name, kind, src_id, repo)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_src_id ON edges(src_id)`,
		`CREATE INDEX IF NOT EXISTS idx_ast_units_qualified_lang_repo ON ast_units(qualified COLLATE NOCASE, language, repo)`,
		`CREATE INDEX IF NOT EXISTS idx_ast_units_name_lang_repo ON ast_units(name COLLATE NOCASE, language, repo)`,
	}
	for _, q := range postIdx {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("sqlite init: %w", err)
		}
	}
	return nil
}
