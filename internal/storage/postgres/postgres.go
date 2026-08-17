// Package postgres implements storage.Storage on PostgreSQL. It is the
// primary relational backend for ragota-core: static queries are compiled by
// sqlc into the pgdb subpackage and executed over native pgx (pgxpool);
// dynamic filtered queries (QueryOpts) are built with sqlutil. Schema and
// semantics mirror the SQLite backend, which remains a lightweight
// embedded/dev option.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/storage"
	"github.com/Nahua-Foundation/ragota/internal/storage/postgres/pgdb"
	"github.com/Nahua-Foundation/ragota/internal/storage/sqlutil"
)

// migrations is the incremental schema history. Existing databases are
// upgraded step by step via the schema_migrations table; the final state must
// always match schema.sql (the sqlc source of truth).
var migrations = []struct {
	version int
	stmts   []string
}{
	{
		version: 1,
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS files (
				repo_id    TEXT NOT NULL,
				path       TEXT NOT NULL,
				language   TEXT NOT NULL,
				hash       TEXT NOT NULL,
				size       BIGINT NOT NULL,
				mod_time   BIGINT NOT NULL,
				indexed    BOOLEAN NOT NULL DEFAULT FALSE,
				PRIMARY KEY (repo_id, path)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_files_repo ON files(repo_id)`,

			`CREATE TABLE IF NOT EXISTS ast_units (
				id          BIGSERIAL PRIMARY KEY,
				repo_id     TEXT NOT NULL,
				file_path   TEXT NOT NULL,
				language    TEXT NOT NULL,
				kind        TEXT NOT NULL,
				name        TEXT NOT NULL,
				qualified   TEXT NOT NULL DEFAULT '',
				parent_id   BIGINT,
				start_line  INTEGER NOT NULL,
				end_line    INTEGER NOT NULL,
				start_byte  INTEGER NOT NULL,
				end_byte    INTEGER NOT NULL,
				signature   TEXT NOT NULL DEFAULT '',
				doc         TEXT NOT NULL DEFAULT '',
				hash        TEXT NOT NULL DEFAULT ''
			)`,
			`CREATE INDEX IF NOT EXISTS idx_ast_units_repo_file ON ast_units(repo_id, file_path)`,
			`CREATE INDEX IF NOT EXISTS idx_ast_units_name ON ast_units(name)`,
			`CREATE INDEX IF NOT EXISTS idx_ast_units_qualified ON ast_units(qualified)`,
			`CREATE INDEX IF NOT EXISTS idx_ast_units_kind ON ast_units(kind)`,

			`CREATE TABLE IF NOT EXISTS edges (
				id          BIGSERIAL PRIMARY KEY,
				repo_id     TEXT NOT NULL,
				src_id      BIGINT NOT NULL,
				dst_id      BIGINT NOT NULL DEFAULT 0,
				kind        TEXT NOT NULL,
				dst_name    TEXT NOT NULL DEFAULT '',
				file_path   TEXT NOT NULL DEFAULT '',
				line        INTEGER NOT NULL DEFAULT 0,
				dst_repo_id TEXT NOT NULL DEFAULT '',
				confidence  REAL NOT NULL DEFAULT 1.0,
				meta        TEXT NOT NULL DEFAULT ''
			)`,
			`CREATE INDEX IF NOT EXISTS idx_edges_src ON edges(src_id, kind)`,
			`CREATE INDEX IF NOT EXISTS idx_edges_dst ON edges(dst_id, kind)`,
			`CREATE INDEX IF NOT EXISTS idx_edges_kind ON edges(kind)`,
			`CREATE INDEX IF NOT EXISTS idx_edges_repo ON edges(repo_id)`,
			`CREATE INDEX IF NOT EXISTS idx_edges_dst_name ON edges(dst_name)`,

			`CREATE TABLE IF NOT EXISTS repos (
				id         TEXT PRIMARY KEY,
				name       TEXT NOT NULL,
				source     TEXT NOT NULL,
				url        TEXT NOT NULL DEFAULT '',
				path       TEXT NOT NULL,
				branch     TEXT NOT NULL DEFAULT '',
				status     TEXT NOT NULL DEFAULT 'idle',
				last_error TEXT NOT NULL DEFAULT '',
				created_at BIGINT NOT NULL,
				indexed_at BIGINT NOT NULL DEFAULT 0
			)`,
		},
	},
	{
		version: 2,
		stmts: []string{
			`ALTER TABLE ast_units ADD COLUMN meta TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		version: 3,
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS index_jobs (
				id           BIGSERIAL PRIMARY KEY,
				repo_id      TEXT NOT NULL,
				force        BOOLEAN NOT NULL DEFAULT FALSE,
				status       TEXT NOT NULL DEFAULT 'pending',
				error        TEXT NOT NULL DEFAULT '',
				created_at   BIGINT NOT NULL,
				claimed_at   BIGINT NOT NULL DEFAULT 0,
				heartbeat_at BIGINT NOT NULL DEFAULT 0,
				claimed_by   TEXT NOT NULL DEFAULT ''
			)`,
			`CREATE INDEX IF NOT EXISTS idx_index_jobs_status ON index_jobs(status)`,
		},
	},
	{
		version: 4,
		stmts: []string{
			`ALTER TABLE repos ADD COLUMN last_commit TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		version: 5,
		stmts: []string{
			// Indexing claim: without an owner and an expiry a crashed indexer
			// leaves the repo in "indexing" forever and every later request is
			// rejected as busy.
			`ALTER TABLE repos ADD COLUMN IF NOT EXISTS claimed_by TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE repos ADD COLUMN IF NOT EXISTS claim_expires_at BIGINT NOT NULL DEFAULT 0`,
			`ALTER TABLE repos ADD COLUMN IF NOT EXISTS pending_commit TEXT NOT NULL DEFAULT ''`,
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
		version: 6,
		stmts: []string{
			// Commit batches are queued like index passes so a crashed instance
			// loses no work. The payload carries the batch itself (file
			// contents included) — the queue row is the only durable copy
			// between the accepting request and the claiming worker.
			`ALTER TABLE index_jobs ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'index'`,
			`ALTER TABLE index_jobs ADD COLUMN IF NOT EXISTS payload TEXT NOT NULL DEFAULT ''`,
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
		version: 7,
		stmts: []string{
			// See the SQLite migration: without it the per-file edge delete
			// scans the whole repository's edges on every re-index.
			`CREATE INDEX IF NOT EXISTS idx_edges_repo_file ON edges(repo_id, file_path)`,
		},
	},
	{
		version: 8,
		stmts: []string{
			// Contract coverage: how many call sites of each contract kind were
			// seen against how many produced an edge. The candidates that
			// produced no edge leave no row anywhere else, so the summary is
			// stored rather than derived.
			`CREATE TABLE IF NOT EXISTS repo_coverage (
				repo_id    TEXT NOT NULL,
				kind       TEXT NOT NULL,
				candidates BIGINT NOT NULL DEFAULT 0,
				edges      BIGINT NOT NULL DEFAULT 0,
				updated_at BIGINT NOT NULL DEFAULT 0,
				PRIMARY KEY (repo_id, kind)
			)`,
		},
	},
	{
		version: 9,
		stmts: []string{
			// "The contract edges of this repository" is a query the search
			// intents make per request (see service/contractusers.go), and the
			// contract kinds are a fraction of a percent of the edges — on
			// elasticsearch, 2 660 rpc_call rows among 2.8 M. Without this
			// index the lookup reads the repository's whole edge table.
			`CREATE INDEX IF NOT EXISTS idx_edges_repo_kind ON edges(repo_id, kind)`,
			// (repo_id) is a prefix of (repo_id, kind), so the old index buys
			// nothing and every edge written would pay for both.
			`DROP INDEX IF EXISTS idx_edges_repo`,
		},
	},
	{
		version: 10,
		stmts: []string{
			// See the SQLite migration: symbol lookups match name and qualified
			// case-insensitively, and LOWER(name) = ? cannot use an index over
			// name. The case-sensitive indexes stay for the lookups that still
			// compare as written.
			`CREATE INDEX IF NOT EXISTS idx_ast_units_name_lower ON ast_units(LOWER(name))`,
			`CREATE INDEX IF NOT EXISTS idx_ast_units_qualified_lower ON ast_units(LOWER(qualified))`,
		},
	},
	{
		version: 11,
		stmts: []string{
			// The working set: which repositories this run is about. DEFAULT
			// TRUE is what upgrades an existing database — every repository a
			// user already has stays visible, since nothing has yet said
			// otherwise — and it is also the value a newly registered
			// repository gets, because StoreRepo does not write the column.
			`ALTER TABLE repos ADD COLUMN IF NOT EXISTS active BOOLEAN NOT NULL DEFAULT TRUE`,
		},
	},
}

// Postgres implements storage.Storage.
type Postgres struct {
	pool        *pgxpool.Pool
	q           *pgdb.Queries
	vectorStore storage.VectorStorage
}

// Config is the Postgres storage configuration.
type Config struct {
	DSN      string
	PoolSize int
}

// Open connects to PostgreSQL and applies migrations.
func Open(cfg *Config) (*Postgres, error) {
	if cfg.DSN == "" {
		return nil, errors.New("postgres: dsn is required")
	}
	pcfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres parse dsn: %w", err)
	}
	if cfg.PoolSize > 0 {
		pcfg.MaxConns = int32(cfg.PoolSize)
	}
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("postgres open: %w", err)
	}
	p := &Postgres{pool: pool, q: pgdb.New(pool)}
	if err := p.migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres migrate: %w", err)
	}
	return p, nil
}

func (p *Postgres) migrate(ctx context.Context) error {
	if _, err := p.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		return err
	}
	var current int
	if err := p.pool.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		tx, err := p.pool.Begin(ctx)
		if err != nil {
			return err
		}
		for _, stmt := range m.stmts {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("migration %d: %w", m.version, err)
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, m.version); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// SetVectorStore sets the vector storage delegate.
func (p *Postgres) SetVectorStore(vs storage.VectorStorage) { p.vectorStore = vs }

// Init initializes the storage connection.
func (p *Postgres) Init(ctx context.Context) error { return p.pool.Ping(ctx) }

// Close closes the storage connection.
func (p *Postgres) Close() error {
	var vsErr error
	if p.vectorStore != nil {
		vsErr = p.vectorStore.Close()
	}
	p.pool.Close()
	return vsErr
}

// VectorStore returns the vector storage delegate.
func (p *Postgres) VectorStore() storage.VectorStorage { return p.vectorStore }

func intOrZero(s string) int64 {
	if s == "" {
		return 0
	}
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return i
}

func notFoundErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return storage.ErrNotFound
	}
	return err
}

// --- Files ---

// StoreFile stores file metadata.
func (p *Postgres) StoreFile(ctx context.Context, f *storage.File) error {
	return p.q.StoreFile(ctx, pgdb.StoreFileParams{
		RepoID:   f.RepoID,
		Path:     f.Path,
		Language: f.Language,
		Hash:     f.Hash,
		Size:     f.Size,
		ModTime:  f.ModTime,
		Indexed:  f.Indexed,
	})
}

// BatchStoreFiles upserts many file rows in one round-trip.
func (p *Postgres) BatchStoreFiles(ctx context.Context, files []*storage.File) error {
	if len(files) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, f := range files {
		batch.Queue(`
			INSERT INTO files (repo_id, path, language, hash, size, mod_time, indexed)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (repo_id, path) DO UPDATE SET
				language = excluded.language,
				hash = excluded.hash,
				size = excluded.size,
				mod_time = excluded.mod_time,
				indexed = excluded.indexed`,
			f.RepoID, f.Path, f.Language, f.Hash, f.Size, f.ModTime, f.Indexed)
	}
	if err := p.pool.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("batch store files: %w", err)
	}
	return nil
}

// DeleteFilesByPaths removes the given paths of one repo.
func (p *Postgres) DeleteFilesByPaths(ctx context.Context, repoID string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	if _, err := p.pool.Exec(ctx,
		`DELETE FROM files WHERE repo_id = $1 AND path = ANY($2)`, repoID, paths,
	); err != nil {
		return fmt.Errorf("delete files by paths: %w", err)
	}
	return nil
}

func fileFromRow(r pgdb.File) *storage.File {
	return &storage.File{
		RepoID:   r.RepoID,
		Path:     r.Path,
		Language: r.Language,
		Hash:     r.Hash,
		Size:     r.Size,
		ModTime:  r.ModTime,
		Indexed:  r.Indexed,
	}
}

// GetFile returns one file's metadata.
func (p *Postgres) GetFile(ctx context.Context, repoID, path string) (*storage.File, error) {
	r, err := p.q.GetFile(ctx, pgdb.GetFileParams{RepoID: repoID, Path: path})
	if err != nil {
		return nil, notFoundErr(err)
	}
	return fileFromRow(r), nil
}

// GetFilesByRepo lists a repo's files.
func (p *Postgres) GetFilesByRepo(ctx context.Context, repoID string) ([]*storage.File, error) {
	rows, err := p.q.GetFilesByRepo(ctx, repoID)
	if err != nil {
		return nil, err
	}
	var out []*storage.File
	for _, r := range rows {
		out = append(out, fileFromRow(r))
	}
	return out, nil
}

// DeleteFile removes one file row.
func (p *Postgres) DeleteFile(ctx context.Context, repoID, path string) error {
	return p.q.DeleteFile(ctx, pgdb.DeleteFileParams{RepoID: repoID, Path: path})
}

// DeleteFilesByRepo removes all file rows of a repo.
func (p *Postgres) DeleteFilesByRepo(ctx context.Context, repoID string) error {
	return p.q.DeleteFilesByRepo(ctx, repoID)
}

// --- AST units ---

func unitInsertParams(u *storage.ASTUnit) pgdb.InsertASTUnitParams {
	var parent *int64
	if u.ParentID != "" {
		id := intOrZero(u.ParentID)
		parent = &id
	}
	return pgdb.InsertASTUnitParams{
		RepoID:    u.RepoID,
		FilePath:  u.FilePath,
		Language:  u.Language,
		Kind:      u.Kind,
		Name:      u.Name,
		Qualified: u.Qualified,
		ParentID:  parent,
		StartLine: int32(u.StartLine),
		EndLine:   int32(u.EndLine),
		StartByte: int32(u.StartByte),
		EndByte:   int32(u.EndByte),
		Signature: u.Signature,
		Doc:       u.Doc,
		Hash:      u.Hash,
		Meta:      u.Meta,
	}
}

func unitFromRow(r pgdb.AstUnit) *storage.ASTUnit {
	u := &storage.ASTUnit{
		ID:        strconv.FormatInt(r.ID, 10),
		RepoID:    r.RepoID,
		FilePath:  r.FilePath,
		Language:  r.Language,
		Kind:      r.Kind,
		Name:      r.Name,
		Qualified: r.Qualified,
		StartLine: int(r.StartLine),
		EndLine:   int(r.EndLine),
		StartByte: int(r.StartByte),
		EndByte:   int(r.EndByte),
		Signature: r.Signature,
		Doc:       r.Doc,
		Hash:      r.Hash,
		Meta:      r.Meta,
	}
	if r.ParentID != nil {
		u.ParentID = strconv.FormatInt(*r.ParentID, 10)
	}
	return u
}

// StoreASTUnit stores an AST unit and assigns its ID.
func (p *Postgres) StoreASTUnit(ctx context.Context, u *storage.ASTUnit) error {
	id, err := p.q.InsertASTUnit(ctx, unitInsertParams(u))
	if err != nil {
		return fmt.Errorf("store ast unit: %w", err)
	}
	u.ID = strconv.FormatInt(id, 10)
	return nil
}

// BatchStoreASTUnits stores multiple AST units in one pgx batch inside a
// single transaction, assigning IDs.
func (p *Postgres) BatchStoreASTUnits(ctx context.Context, units []*storage.ASTUnit) error {
	if len(units) == 0 {
		return nil
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	params := make([]pgdb.BatchInsertASTUnitsParams, len(units))
	for i, u := range units {
		params[i] = pgdb.BatchInsertASTUnitsParams(unitInsertParams(u))
	}
	var batchErr error
	p.q.WithTx(tx).BatchInsertASTUnits(ctx, params).QueryRow(func(i int, id int64, err error) {
		if err != nil {
			if batchErr == nil {
				batchErr = fmt.Errorf("batch store ast unit: %w", err)
			}
			return
		}
		units[i].ID = strconv.FormatInt(id, 10)
	})
	if batchErr != nil {
		return batchErr
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// GetASTUnits queries units with the same filters, tiering and ranking as the
// SQLite backend: both go through sqlutil, and only running the statement is
// this backend's own.
func (p *Postgres) GetASTUnits(ctx context.Context, opts storage.QueryOpts) ([]*storage.ASTUnit, error) {
	return sqlutil.QueryUnits(sqlutil.PostgresDialect{}, opts,
		func(id string) any { return intOrZero(id) },
		func(query string, args []any) ([]*storage.ASTUnit, error) {
			rows, err := p.pool.Query(ctx, query, args...)
			if err != nil {
				return nil, fmt.Errorf("query ast units: %w", err)
			}
			defer rows.Close()
			var out []*storage.ASTUnit
			for rows.Next() {
				var r pgdb.AstUnit
				if err := rows.Scan(&r.ID, &r.RepoID, &r.FilePath, &r.Language, &r.Kind, &r.Name, &r.Qualified,
					&r.ParentID, &r.StartLine, &r.EndLine, &r.StartByte, &r.EndByte, &r.Signature, &r.Doc, &r.Hash, &r.Meta); err != nil {
					return nil, err
				}
				out = append(out, unitFromRow(r))
			}
			return out, rows.Err()
		})
}

// GetASTUnitByID returns one unit by ID.
func (p *Postgres) GetASTUnitByID(ctx context.Context, id string) (*storage.ASTUnit, error) {
	r, err := p.q.GetASTUnitByID(ctx, intOrZero(id))
	if err != nil {
		return nil, notFoundErr(err)
	}
	return unitFromRow(r), nil
}

// GetASTUnitsByIDs returns units matching the given IDs in a single query.
func (p *Postgres) GetASTUnitsByIDs(ctx context.Context, ids []string) ([]*storage.ASTUnit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	intIDs := make([]int64, 0, len(ids))
	for _, id := range ids {
		intIDs = append(intIDs, intOrZero(id))
	}
	rows, err := p.q.GetASTUnitsByIDs(ctx, intIDs)
	if err != nil {
		return nil, fmt.Errorf("query ast units by ids: %w", err)
	}
	var out []*storage.ASTUnit
	for _, r := range rows {
		out = append(out, unitFromRow(r))
	}
	return out, nil
}

// DeleteASTUnitsByFile deletes a file's units and unresolves the edges that
// pointed at them.
//
// Re-indexing a file deletes and recreates its units with new IDs. An edge
// pointing *into* the file keeps the old dst_id, which resolves to nothing yet
// still reads as resolved to every caller testing dst_id != "" — a call edge
// that silently targets a symbol that no longer exists. Clearing the
// resolution hands the edge back to the linker, which re-resolves it against
// the new units on its next run.
//
// Both statements run in one transaction: an unresolve without the delete (or
// the other way round) is a worse state than either.
func (p *Postgres) DeleteASTUnitsByFile(ctx context.Context, repoID, filePath string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("delete ast units by file: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := p.q.WithTx(tx)
	if _, err := q.UnresolveEdgesByDstFile(ctx, pgdb.UnresolveEdgesByDstFileParams{
		RepoID: repoID, FilePath: filePath,
	}); err != nil {
		return fmt.Errorf("unresolve edges into file: %w", err)
	}
	if err := q.DeleteASTUnitsByFile(ctx, pgdb.DeleteASTUnitsByFileParams{
		RepoID: repoID, FilePath: filePath,
	}); err != nil {
		return fmt.Errorf("delete ast units by file: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("delete ast units by file: %w", err)
	}
	return nil
}

// DeleteASTUnitsByFiles is DeleteASTUnitsByFile for many paths, in one
// transaction: an index pass rewrites a whole window of files, and the round
// trips rather than the deleted rows are what that used to cost. The paths go
// out as one array parameter, so no chunking is needed.
func (p *Postgres) DeleteASTUnitsByFiles(ctx context.Context, repoID string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("delete ast units by files: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := p.q.WithTx(tx)
	if _, err := q.UnresolveEdgesByDstFiles(ctx, pgdb.UnresolveEdgesByDstFilesParams{
		RepoID: repoID, Paths: paths,
	}); err != nil {
		return fmt.Errorf("unresolve edges into files: %w", err)
	}
	if err := q.DeleteASTUnitsByFiles(ctx, pgdb.DeleteASTUnitsByFilesParams{
		RepoID: repoID, Paths: paths,
	}); err != nil {
		return fmt.Errorf("delete ast units by files: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("delete ast units by files: %w", err)
	}
	return nil
}

// DeleteASTUnitsByRepo deletes a repo's units.
func (p *Postgres) DeleteASTUnitsByRepo(ctx context.Context, repoID string) error {
	return p.q.DeleteASTUnitsByRepo(ctx, repoID)
}

// DeleteASTUnitsByKind deletes a repo's units of one kind.
func (p *Postgres) DeleteASTUnitsByKind(ctx context.Context, repoID, kind string) error {
	return p.q.DeleteASTUnitsByKind(ctx, pgdb.DeleteASTUnitsByKindParams{RepoID: repoID, Kind: kind})
}

// CountASTUnitsByRepo counts a repo's units.
func (p *Postgres) CountASTUnitsByRepo(ctx context.Context, repoID string) (int64, error) {
	return p.q.CountASTUnitsByRepo(ctx, repoID)
}

// CountASTUnits counts all units.
func (p *Postgres) CountASTUnits(ctx context.Context) (int64, error) {
	return p.q.CountASTUnits(ctx)
}

// --- Edges ---

func edgeInsertParams(e *storage.Edge) pgdb.InsertEdgeParams {
	return pgdb.InsertEdgeParams{
		RepoID:     e.RepoID,
		SrcID:      intOrZero(e.SrcID),
		DstID:      intOrZero(e.DstID),
		Kind:       e.Kind,
		DstName:    e.DstName,
		FilePath:   e.FilePath,
		Line:       int32(e.Line),
		DstRepoID:  e.DstRepoID,
		Confidence: e.Confidence,
		Meta:       e.Meta,
	}
}

func edgeFromRow(r pgdb.Edge) *storage.Edge {
	e := &storage.Edge{
		ID:         strconv.FormatInt(r.ID, 10),
		RepoID:     r.RepoID,
		SrcID:      strconv.FormatInt(r.SrcID, 10),
		Kind:       r.Kind,
		DstName:    r.DstName,
		FilePath:   r.FilePath,
		Line:       int(r.Line),
		DstRepoID:  r.DstRepoID,
		Confidence: r.Confidence,
		Meta:       r.Meta,
	}
	if r.DstID != 0 {
		e.DstID = strconv.FormatInt(r.DstID, 10)
	}
	return e
}

// StoreEdge stores an edge and assigns its ID.
func (p *Postgres) StoreEdge(ctx context.Context, e *storage.Edge) error {
	id, err := p.q.InsertEdge(ctx, edgeInsertParams(e))
	if err != nil {
		return fmt.Errorf("store edge: %w", err)
	}
	e.ID = strconv.FormatInt(id, 10)
	return nil
}

// BatchStoreEdges stores multiple edges in one pgx batch inside a single
// transaction, assigning IDs.
func (p *Postgres) BatchStoreEdges(ctx context.Context, edges []*storage.Edge) error {
	if len(edges) == 0 {
		return nil
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	params := make([]pgdb.BatchInsertEdgesParams, len(edges))
	for i, e := range edges {
		params[i] = pgdb.BatchInsertEdgesParams(edgeInsertParams(e))
	}
	var batchErr error
	p.q.WithTx(tx).BatchInsertEdges(ctx, params).QueryRow(func(i int, id int64, err error) {
		if err != nil {
			if batchErr == nil {
				batchErr = fmt.Errorf("batch store edge: %w", err)
			}
			return
		}
		edges[i].ID = strconv.FormatInt(id, 10)
	})
	if batchErr != nil {
		return batchErr
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// GetEdges queries edges with the same filters as the SQLite backend.
func (p *Postgres) GetEdges(ctx context.Context, opts storage.QueryOpts) ([]*storage.Edge, error) {
	b := sqlutil.NewBuilder(sqlutil.PostgresDialect{})
	sqlutil.EdgeFilters(b, opts, func(id string) any { return intOrZero(id) })
	var limit string
	if opts.Limit > 0 {
		limit = b.Limit(opts.Limit)
	}
	where, args := b.Where()
	query := "SELECT " + sqlutil.EdgeColumns + " FROM edges WHERE TRUE" + where + sqlutil.EdgeOrder + limit

	rows, err := p.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query edges: %w", err)
	}
	defer rows.Close()
	var out []*storage.Edge
	for rows.Next() {
		var r pgdb.Edge
		if err := rows.Scan(&r.ID, &r.RepoID, &r.SrcID, &r.DstID, &r.Kind, &r.DstName,
			&r.FilePath, &r.Line, &r.DstRepoID, &r.Confidence, &r.Meta); err != nil {
			return nil, err
		}
		out = append(out, edgeFromRow(r))
	}
	return out, rows.Err()
}

// DeleteEdgesByKindAndDst deletes edges of a kind pointing at a destination name.
func (p *Postgres) DeleteEdgesByKindAndDst(ctx context.Context, kind, dstName string) error {
	return p.q.DeleteEdgesByKindAndDst(ctx, pgdb.DeleteEdgesByKindAndDstParams{Kind: kind, DstName: dstName})
}

// DeleteEdgesByFile deletes a file's edges.
func (p *Postgres) DeleteEdgesByFile(ctx context.Context, repoID, filePath string) error {
	return p.q.DeleteEdgesByFile(ctx, pgdb.DeleteEdgesByFileParams{RepoID: repoID, FilePath: filePath})
}

// DeleteEdgesByFiles deletes the edges of many files of one repo in one
// statement.
func (p *Postgres) DeleteEdgesByFiles(ctx context.Context, repoID string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	return p.q.DeleteEdgesByFiles(ctx, pgdb.DeleteEdgesByFilesParams{RepoID: repoID, Paths: paths})
}

// DeleteEdgesByRepo deletes a repo's edges.
func (p *Postgres) DeleteEdgesByRepo(ctx context.Context, repoID string) error {
	return p.q.DeleteEdgesByRepo(ctx, repoID)
}

// DeleteEdgesByKind deletes edges of a kind (repoID empty = all repos).
func (p *Postgres) DeleteEdgesByKind(ctx context.Context, repoID, kind string) error {
	if repoID == "" {
		return p.q.DeleteEdgesByKindGlobal(ctx, kind)
	}
	return p.q.DeleteEdgesByKind(ctx, pgdb.DeleteEdgesByKindParams{RepoID: repoID, Kind: kind})
}

// DeleteEdgesByKindAndFiles deletes edges of a kind that originate in the
// given files. An incremental pass regenerates only the files it was handed,
// so deleting the whole repo's edges of that kind would drop data the pass
// never rebuilds. It runs on the pool directly: the path list is variadic,
// which sqlc's generated signatures do not express as cleanly as ANY().
func (p *Postgres) DeleteEdgesByKindAndFiles(ctx context.Context, repoID, kind string, filePaths []string) error {
	if len(filePaths) == 0 {
		return nil
	}
	_, err := p.pool.Exec(ctx,
		`DELETE FROM edges WHERE repo_id = $1 AND kind = $2 AND file_path = ANY($3)`,
		repoID, kind, filePaths)
	if err != nil {
		return fmt.Errorf("delete edges by kind and files: %w", err)
	}
	return nil
}

// UpdateEdgeResolution sets an edge's destination after linking.
func (p *Postgres) UpdateEdgeResolution(ctx context.Context, edgeID, dstID, dstRepoID string, confidence float32) error {
	n, err := p.q.UpdateEdgeResolution(ctx, pgdb.UpdateEdgeResolutionParams{
		DstID:      intOrZero(dstID),
		DstRepoID:  dstRepoID,
		Confidence: confidence,
		ID:         intOrZero(edgeID),
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// resolutionBatchRows is how many resolutions one statement carries. The
// arrays are bound as four parameters whatever the size, so this only bounds
// the memory of a single round trip and the cost of the row-by-row retry a
// failed statement falls back to.
const resolutionBatchRows = 1000

// BatchUpdateEdgeResolutions applies resolutions in statements of
// resolutionBatchRows rows each, one array-join UPDATE per batch.
func (p *Postgres) BatchUpdateEdgeResolutions(ctx context.Context, res []storage.EdgeResolution) ([]storage.EdgeResolutionFailure, error) {
	var failures []storage.EdgeResolutionFailure
	for start := 0; start < len(res); start += resolutionBatchRows {
		chunk := res[start:min(start+resolutionBatchRows, len(res))]
		got, err := p.resolutionBatch(ctx, start, chunk)
		if err != nil {
			if ctx.Err() != nil {
				return failures, err
			}
			// The statement failed as a whole, which says nothing about which
			// row broke it. Retry the chunk row by row so one bad resolution
			// costs only itself.
			got = p.resolutionRows(ctx, start, chunk)
		}
		failures = append(failures, got...)
	}
	return failures, nil
}

// resolutionBatch applies one chunk with a single UPDATE and reports the rows
// that matched no edge.
func (p *Postgres) resolutionBatch(ctx context.Context, offset int, chunk []storage.EdgeResolution) ([]storage.EdgeResolutionFailure, error) {
	arg := pgdb.BatchUpdateEdgeResolutionsParams{
		Ids:         make([]int64, len(chunk)),
		DstIds:      make([]int64, len(chunk)),
		DstRepoIds:  make([]string, len(chunk)),
		Confidences: make([]float32, len(chunk)),
	}
	for i, r := range chunk {
		arg.Ids[i] = intOrZero(r.EdgeID)
		arg.DstIds[i] = intOrZero(r.DstID)
		arg.DstRepoIds[i] = r.DstRepoID
		arg.Confidences[i] = r.Confidence
	}
	updated, err := p.q.BatchUpdateEdgeResolutions(ctx, arg)
	if err != nil {
		return nil, fmt.Errorf("batch update edge resolutions: %w", err)
	}
	if len(updated) == len(chunk) {
		return nil, nil
	}
	matched := make(map[int64]struct{}, len(updated))
	for _, id := range updated {
		matched[id] = struct{}{}
	}
	var failures []storage.EdgeResolutionFailure
	for i, r := range chunk {
		if _, ok := matched[arg.Ids[i]]; !ok {
			failures = append(failures, storage.EdgeResolutionFailure{
				Index: offset + i, EdgeID: r.EdgeID, Err: storage.ErrNotFound,
			})
		}
	}
	return failures, nil
}

// resolutionRows applies a chunk one statement at a time, the way linking used
// to, and reports every row that failed.
func (p *Postgres) resolutionRows(ctx context.Context, offset int, chunk []storage.EdgeResolution) []storage.EdgeResolutionFailure {
	var failures []storage.EdgeResolutionFailure
	for i, r := range chunk {
		if err := p.UpdateEdgeResolution(ctx, r.EdgeID, r.DstID, r.DstRepoID, r.Confidence); err != nil {
			failures = append(failures, storage.EdgeResolutionFailure{
				Index: offset + i, EdgeID: r.EdgeID, Err: err,
			})
		}
	}
	return failures
}

// UpdateEdgeDstName rewrites an edge's destination join key.
func (p *Postgres) UpdateEdgeDstName(ctx context.Context, edgeID, dstName string) error {
	n, err := p.q.UpdateEdgeDstName(ctx, pgdb.UpdateEdgeDstNameParams{DstName: dstName, ID: intOrZero(edgeID)})
	if err != nil {
		return err
	}
	if n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// UpdateEdgeMeta rewrites an edge's metadata in place.
func (p *Postgres) UpdateEdgeMeta(ctx context.Context, edgeID, meta string) error {
	tag, err := p.pool.Exec(ctx, `UPDATE edges SET meta = $1 WHERE id = $2`, meta, intOrZero(edgeID))
	if err != nil {
		return fmt.Errorf("update edge meta: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// --- Repos ---

// StoreRepo inserts a repository, or updates only the definition of an
// existing one; lifecycle state is preserved (see RepoStore.StoreRepo).
func (p *Postgres) StoreRepo(ctx context.Context, r *repos.Repo) error {
	return p.q.StoreRepo(ctx, pgdb.StoreRepoParams{
		ID:         r.ID,
		Name:       r.Name,
		Source:     string(r.Source),
		Url:        r.URL,
		Path:       r.Path,
		Branch:     r.Branch,
		Status:     string(r.Status),
		LastError:  r.LastError,
		CreatedAt:  r.CreatedAt,
		IndexedAt:  r.IndexedAt,
		LastCommit: r.LastCommit,
	})
}

func repoFromRow(r pgdb.GetRepoRow) *repos.Repo {
	return &repos.Repo{
		ID:            r.ID,
		Name:          r.Name,
		Source:        repos.SourceType(r.Source),
		URL:           r.Url,
		Path:          r.Path,
		Branch:        r.Branch,
		Status:        repos.Status(r.Status),
		LastError:     r.LastError,
		CreatedAt:     r.CreatedAt,
		IndexedAt:     r.IndexedAt,
		LastCommit:    r.LastCommit,
		PendingCommit: r.PendingCommit,
		Active:        r.Active,
	}
}

// GetRepo returns one repository.
func (p *Postgres) GetRepo(ctx context.Context, id string) (*repos.Repo, error) {
	r, err := p.q.GetRepo(ctx, id)
	if err != nil {
		return nil, notFoundErr(err)
	}
	return repoFromRow(r), nil
}

// ListRepos returns all repositories.
func (p *Postgres) ListRepos(ctx context.Context) ([]*repos.Repo, error) {
	rows, err := p.q.ListRepos(ctx)
	if err != nil {
		return nil, err
	}
	var out []*repos.Repo
	for _, r := range rows {
		out = append(out, repoFromRow(pgdb.GetRepoRow(r)))
	}
	return out, nil
}

// ListActiveRepos returns the repositories in the active set.
func (p *Postgres) ListActiveRepos(ctx context.Context) ([]*repos.Repo, error) {
	rows, err := p.q.ListActiveRepos(ctx)
	if err != nil {
		return nil, err
	}
	var out []*repos.Repo
	for _, r := range rows {
		out = append(out, repoFromRow(pgdb.GetRepoRow(r)))
	}
	return out, nil
}

// SetActiveRepos makes exactly the named repositories active. One UPDATE
// raises and clears every row at once, so no reader sees a half-applied switch.
func (p *Postgres) SetActiveRepos(ctx context.Context, ids []string) error {
	// A nil slice reaches Postgres as NULL rather than as an empty array, and
	// `id = ANY(NULL)` is NULL — which would write NULL into a NOT NULL column
	// and fail the statement instead of leaving nothing active.
	if ids == nil {
		ids = []string{}
	}
	if err := p.q.SetActiveRepos(ctx, ids); err != nil {
		return fmt.Errorf("set active repos: %w", err)
	}
	return nil
}

// DeleteRepo removes a repository row.
func (p *Postgres) DeleteRepo(ctx context.Context, id string) error {
	return p.q.DeleteRepo(ctx, id)
}

// ClaimRepoForIndexing atomically transitions a repo to the indexing status
// for owner, for at most ttlSeconds. An expired claim is taken over, so a
// crashed indexer cannot wedge the repo. It returns false if a live claim is
// held, and ErrNotFound if the repo does not exist.
func (p *Postgres) ClaimRepoForIndexing(ctx context.Context, id, owner string, ttlSeconds int64) (bool, error) {
	if ttlSeconds <= 0 {
		ttlSeconds = storage.DefaultRepoClaimTTLSeconds
	}
	now := time.Now().Unix()
	n, err := p.q.ClaimRepoForIndexing(ctx, pgdb.ClaimRepoForIndexingParams{
		Status:         string(repos.StatusIndexing),
		ClaimedBy:      owner,
		ClaimExpiresAt: now + ttlSeconds,
		ID:             id,
		Now:            now,
	})
	if err != nil {
		return false, fmt.Errorf("claim repo for indexing: %w", err)
	}
	if n > 0 {
		return true, nil
	}
	// Not claimed: either a live claim is held or the repo is missing.
	exists, err := p.q.RepoExists(ctx, id)
	if err != nil {
		return false, fmt.Errorf("claim repo for indexing: %w", err)
	}
	if !exists {
		return false, storage.ErrNotFound
	}
	return false, nil
}

// ResetStuckRepos moves repos left in the indexing status back to idle.
func (p *Postgres) ResetStuckRepos(ctx context.Context, force bool) (int, error) {
	var n int64
	var err error
	if force {
		n, err = p.q.ResetIndexingRepos(ctx, storage.RepoResetMessage)
	} else {
		n, err = p.q.ResetExpiredIndexingRepos(ctx, pgdb.ResetExpiredIndexingReposParams{
			LastError: storage.RepoResetMessage,
			Now:       time.Now().Unix(),
		})
	}
	if err != nil {
		return 0, fmt.Errorf("reset stuck repos: %w", err)
	}
	return int(n), nil
}

// SetRepoPendingCommit records the SHA a running commit batch is applying.
func (p *Postgres) SetRepoPendingCommit(ctx context.Context, id, sha string) error {
	n, err := p.q.SetRepoPendingCommit(ctx, pgdb.SetRepoPendingCommitParams{PendingCommit: sha, ID: id})
	if err != nil {
		return fmt.Errorf("set repo pending commit: %w", err)
	}
	if n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// UpdateRepoStatus updates status fields of a repository.
func (p *Postgres) UpdateRepoStatus(ctx context.Context, id string, status repos.Status, lastError string, indexedAt int64) error {
	n, err := p.q.UpdateRepoStatus(ctx, pgdb.UpdateRepoStatusParams{
		Status:    string(status),
		LastError: lastError,
		IndexedAt: indexedAt,
		ID:        id,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// UpdateRepoLastCommit records the SHA of the last applied commit.
func (p *Postgres) UpdateRepoLastCommit(ctx context.Context, id, sha string) error {
	n, err := p.q.UpdateRepoLastCommit(ctx, pgdb.UpdateRepoLastCommitParams{LastCommit: sha, ID: id})
	if err != nil {
		return err
	}
	if n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// Compile-time check.
var (
	_ storage.Storage               = (*Postgres)(nil)
	_ storage.EdgeResolutionBatcher = (*Postgres)(nil)
)
