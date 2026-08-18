package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/Nahua-Foundation/ragota/internal/storage"
	_ "modernc.org/sqlite"
)

type migration struct {
	version int
	stmts   []string
}

var migrations = []migration{
	{
		version: 1,
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS files (
				repo_id    TEXT NOT NULL,
				path       TEXT NOT NULL,
				language   TEXT NOT NULL,
				hash       TEXT NOT NULL,
				size       INTEGER NOT NULL,
				mod_time   INTEGER NOT NULL,
				indexed    INTEGER NOT NULL DEFAULT 0,
				PRIMARY KEY (repo_id, path)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_files_repo ON files(repo_id)`,
			`CREATE INDEX IF NOT EXISTS idx_files_language ON files(language)`,

			`CREATE TABLE IF NOT EXISTS ast_units (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				repo_id     TEXT NOT NULL,
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
				signature   TEXT NOT NULL DEFAULT '',
				doc         TEXT NOT NULL DEFAULT '',
				hash        TEXT NOT NULL DEFAULT '',
				FOREIGN KEY (parent_id) REFERENCES ast_units(id) ON DELETE SET NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_ast_units_repo_file ON ast_units(repo_id, file_path)`,
			`CREATE INDEX IF NOT EXISTS idx_ast_units_name ON ast_units(name)`,
			`CREATE INDEX IF NOT EXISTS idx_ast_units_qualified ON ast_units(qualified)`,
			`CREATE INDEX IF NOT EXISTS idx_ast_units_kind ON ast_units(kind)`,
			`CREATE INDEX IF NOT EXISTS idx_ast_units_parent ON ast_units(parent_id)`,

			`CREATE TABLE IF NOT EXISTS edges (
				id         INTEGER PRIMARY KEY AUTOINCREMENT,
				repo_id    TEXT NOT NULL,
				src_id     INTEGER NOT NULL,
				dst_id     INTEGER NOT NULL,
				kind       TEXT NOT NULL,
				dst_name   TEXT NOT NULL DEFAULT '',
				file_path  TEXT NOT NULL DEFAULT '',
				line       INTEGER NOT NULL DEFAULT 0,
				dst_repo_id TEXT NOT NULL DEFAULT '',
				confidence REAL NOT NULL DEFAULT 1.0,
				FOREIGN KEY (src_id) REFERENCES ast_units(id) ON DELETE CASCADE
			)`,
			`CREATE INDEX IF NOT EXISTS idx_edges_src ON edges(src_id, kind)`,
			`CREATE INDEX IF NOT EXISTS idx_edges_dst ON edges(dst_id, kind)`,
			`CREATE INDEX IF NOT EXISTS idx_edges_kind ON edges(kind)`,
			`CREATE INDEX IF NOT EXISTS idx_edges_repo ON edges(repo_id)`,
			`CREATE INDEX IF NOT EXISTS idx_edges_unresolved ON edges(dst_name, kind) WHERE dst_id = 0`,
			`CREATE TABLE IF NOT EXISTS repos (
				id         TEXT PRIMARY KEY,
				name       TEXT NOT NULL,
				source     TEXT NOT NULL,
				url        TEXT NOT NULL DEFAULT '',
				path       TEXT NOT NULL,
				branch     TEXT NOT NULL DEFAULT '',
				status     TEXT NOT NULL DEFAULT 'idle',
				last_error TEXT NOT NULL DEFAULT '',
				created_at INTEGER NOT NULL,
				indexed_at INTEGER NOT NULL DEFAULT 0
			)`,
		},
	},
	{
		version: 2,
		stmts: []string{
			`ALTER TABLE edges ADD COLUMN meta TEXT NOT NULL DEFAULT ''`,
			`CREATE INDEX IF NOT EXISTS idx_edges_dst_name ON edges(dst_name)`,
		},
	},
	{
		version: 3,
		stmts: []string{
			`ALTER TABLE ast_units ADD COLUMN meta TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		version: 4,
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS index_jobs (
				id           INTEGER PRIMARY KEY AUTOINCREMENT,
				repo_id      TEXT NOT NULL,
				force        INTEGER NOT NULL DEFAULT 0,
				status       TEXT NOT NULL DEFAULT 'pending',
				error        TEXT NOT NULL DEFAULT '',
				created_at   INTEGER NOT NULL,
				claimed_at   INTEGER NOT NULL DEFAULT 0,
				heartbeat_at INTEGER NOT NULL DEFAULT 0,
				claimed_by   TEXT NOT NULL DEFAULT ''
			)`,
			`CREATE INDEX IF NOT EXISTS idx_index_jobs_status ON index_jobs(status)`,
		},
	},
	{
		version: 5,
		stmts: []string{
			`ALTER TABLE repos ADD COLUMN last_commit TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		version: 6,
		stmts: []string{
			// Indexing claim: without an owner and an expiry a crashed indexer
			// leaves the repo in "indexing" forever and every later request is
			// rejected as busy.
			`ALTER TABLE repos ADD COLUMN claimed_by TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE repos ADD COLUMN claim_expires_at INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE repos ADD COLUMN pending_commit TEXT NOT NULL DEFAULT ''`,
			// One pending job per repo, enforced by the database so enqueueing
			// is a single atomic upsert instead of SELECT-then-INSERT.
			`DELETE FROM index_jobs WHERE status = 'pending' AND id NOT IN (
				SELECT MIN(id) FROM index_jobs WHERE status = 'pending' GROUP BY repo_id
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_index_jobs_pending_repo
				ON index_jobs(repo_id) WHERE status = 'pending'`,
		},
	},
	{
		version: 7,
		stmts: []string{
			// Commit batches are queued like index passes so a crashed instance
			// loses no work. The payload carries the batch itself (file
			// contents included) — the queue row is the only durable copy
			// between the accepting request and the claiming worker.
			`ALTER TABLE index_jobs ADD COLUMN kind TEXT NOT NULL DEFAULT 'index'`,
			`ALTER TABLE index_jobs ADD COLUMN payload TEXT NOT NULL DEFAULT ''`,
			// The one-pending-job-per-repo rule only ever meant index jobs:
			// several commit batches may legitimately queue up for one repo and
			// are applied in id order.
			`DROP INDEX IF EXISTS idx_index_jobs_pending_repo`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_index_jobs_pending_repo
				ON index_jobs(repo_id) WHERE status = 'pending' AND kind = 'index'`,
			`CREATE INDEX IF NOT EXISTS idx_index_jobs_repo ON index_jobs(repo_id, id)`,
		},
	},
	{
		version: 8,
		stmts: []string{
			// Re-indexing a file deletes its edges first, and without this index
			// that DELETE scans every edge of the repository. On a repo the size
			// of Kafka (~6.7k files, ~700k edges) the cost is quadratic and the
			// pass effectively never finishes; ast_units already had the
			// equivalent index.
			`CREATE INDEX IF NOT EXISTS idx_edges_repo_file ON edges(repo_id, file_path)`,
		},
	},
	{
		version: 9,
		stmts: []string{
			// Contract coverage: how many call sites of each contract kind were
			// seen against how many produced an edge. It is a summary of the
			// last pass rather than something derivable from the edges — the
			// candidates that produced nothing leave no row behind anywhere
			// else, and they are what tells "nothing to find" from "missed it".
			`CREATE TABLE IF NOT EXISTS repo_coverage (
				repo_id    TEXT NOT NULL,
				kind       TEXT NOT NULL,
				candidates INTEGER NOT NULL DEFAULT 0,
				edges      INTEGER NOT NULL DEFAULT 0,
				updated_at INTEGER NOT NULL DEFAULT 0,
				PRIMARY KEY (repo_id, kind)
			)`,
		},
	},
	{
		version: 10,
		stmts: []string{
			// "The contract edges of this repository" is a query the search
			// intents make per request (see service/contractusers.go), and the
			// contract kinds are a fraction of a percent of the edges: on
			// elasticsearch, 2 660 rpc_call rows among 2.8 M. With only
			// idx_edges_repo to work from, SQLite walked all 2.8 M to find them
			// and one /search took 3.9 s.
			`CREATE INDEX IF NOT EXISTS idx_edges_repo_kind ON edges(repo_id, kind)`,
			// (repo_id) is a prefix of (repo_id, kind), so the old index buys
			// nothing and every edge written would pay for both.
			`DROP INDEX IF EXISTS idx_edges_repo`,
		},
	},
	{
		version: 11,
		stmts: []string{
			// Symbol lookups match name and qualified case-insensitively, and
			// LOWER(name) = ? cannot use an index over name. Without these the
			// lookup an agent makes most often reads the whole unit table. The
			// case-sensitive indexes stay: GetASTUnitByName and
			// GetASTUnitByQualifiedName still compare as written.
			`CREATE INDEX IF NOT EXISTS idx_ast_units_name_lower ON ast_units(LOWER(name))`,
			`CREATE INDEX IF NOT EXISTS idx_ast_units_qualified_lower ON ast_units(LOWER(qualified))`,
		},
	},
	{
		version: 12,
		stmts: []string{
			// The working set: which repositories this run is about. DEFAULT 1
			// is what upgrades an existing database — every repository a user
			// already has stays visible, since nothing has yet said otherwise —
			// and it is also the value a newly registered repository gets,
			// because StoreRepo does not write the column at all.
			`ALTER TABLE repos ADD COLUMN active INTEGER NOT NULL DEFAULT 1`,
		},
	},
}

func (s *SQLite) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}

	var current int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.version, err)
		}
		for _, stmt := range m.stmts {
			if _, err := tx.Exec(stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migration %d: %w", m.version, err)
			}
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, m.version); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.version, err)
		}
	}
	return nil
}

// SQLite implements storage.Storage with SQLite backend.
// See the matching assertion in postgres: conformance is a build-time fact.
var _ storage.Storage = (*SQLite)(nil)

type SQLite struct {
	db          *sql.DB
	path        string
	vectorStore storage.VectorStorage
}

// Config is the SQLite storage configuration.
type Config struct {
	Path     string
	PoolSize int
	// CacheSizeKB is the per-connection page cache in kibibytes; 0 selects
	// defaultCacheKB.
	CacheSizeKB int
	// WALAutoCheckpointPages is the WAL length, in pages, that triggers an
	// automatic checkpoint; 0 selects defaultWALCheckpointPages.
	WALAutoCheckpointPages int
}

const (
	// defaultCacheKB is the per-connection page cache. SQLite's own default is
	// 2 MiB, which is smaller than the interior levels of the edge indexes once
	// a repository the size of Elasticsearch is in the database: every insert
	// then re-reads index pages from disk, and the index pass spends its time
	// in pread rather than anywhere interesting. Sized so the whole pool still
	// fits comfortably inside the process' memory budget.
	defaultCacheKB = 32 * 1024
	// defaultWALCheckpointPages is when the WAL is folded back into the
	// database. An index pass commits several transactions per file, so at
	// SQLite's default of 1000 pages the checkpoint runs continuously on the
	// writing connection and rewrites the same hot index pages over and over —
	// it cost 6% of the CPU of an Elasticsearch pass. A longer WAL coalesces
	// those repeated updates into one write per page per checkpoint, at the
	// price of ~80 MiB of WAL between checkpoints.
	defaultWALCheckpointPages = 20000
)

// Open opens the SQLite database.
func Open(cfg *Config) (*SQLite, error) {
	if cfg.Path == "" {
		return nil, errors.New("sqlite: path is required")
	}

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0755); err != nil {
		return nil, fmt.Errorf("create directory: %w", err)
	}

	// Open with pragmas for performance
	cacheKB := cfg.CacheSizeKB
	if cacheKB <= 0 {
		cacheKB = defaultCacheKB
	}
	checkpoint := cfg.WALAutoCheckpointPages
	if checkpoint <= 0 {
		checkpoint = defaultWALCheckpointPages
	}
	// A negative cache_size is a size in kibibytes rather than a page count,
	// which is what makes it independent of page_size.
	dsn := cfg.Path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(30000)" +
		"&_pragma=cache_size(-" + strconv.Itoa(cacheKB) + ")" +
		"&_pragma=temp_store(memory)" +
		"&_pragma=wal_autocheckpoint(" + strconv.Itoa(checkpoint) + ")"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}

	if cfg.PoolSize > 0 {
		db.SetMaxOpenConns(cfg.PoolSize)
	}

	s := &SQLite{
		db:   db,
		path: cfg.Path,
	}

	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite migrate: %w", err)
	}

	return s, nil
}

// SetVectorStore sets the vector storage delegate.
func (s *SQLite) SetVectorStore(vs storage.VectorStorage) {
	s.vectorStore = vs
}

// Init initializes the storage connection.
func (s *SQLite) Init(ctx context.Context) error {
	// Already initialized in Open()
	return nil
}

// Close closes the storage connection.
func (s *SQLite) Close() error {
	var vsErr error
	if s.vectorStore != nil {
		vsErr = s.vectorStore.Close()
	}
	return errors.Join(vsErr, s.db.Close())
}

// VectorStore returns the vector storage delegate.
func (s *SQLite) VectorStore() storage.VectorStorage {
	return s.vectorStore
}

// Compile-time check that *SQLite implements storage.Storage and, since the
// linker only reaches the batched path through a type assertion, the optional
// EdgeResolutionBatcher as well.
var (
	_ storage.Storage               = (*SQLite)(nil)
	_ storage.EdgeResolutionBatcher = (*SQLite)(nil)
)
