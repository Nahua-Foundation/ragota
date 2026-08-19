-- SQLite schema for ragota metadata storage, sqlc's source of truth for the
-- generated queries. It mirrors the final state of the migrations in
-- sqlite.go, which are what actually create the database at runtime; this file
-- exists only so sqlc can infer column types. Keep the two in sync: booleans
-- are stored as INTEGER (0/1), which the hand-written wrappers convert to/from
-- Go bools.

CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY
);

CREATE TABLE files (
    repo_id    TEXT NOT NULL,
    path       TEXT NOT NULL,
    language   TEXT NOT NULL,
    hash       TEXT NOT NULL,
    size       INTEGER NOT NULL,
    mod_time   INTEGER NOT NULL,
    indexed    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (repo_id, path)
);

CREATE INDEX idx_files_repo ON files(repo_id);
CREATE INDEX idx_files_language ON files(language);

CREATE TABLE ast_units (
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
    meta        TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_ast_units_repo_file ON ast_units(repo_id, file_path);
CREATE INDEX idx_ast_units_name ON ast_units(name);
CREATE INDEX idx_ast_units_qualified ON ast_units(qualified);
CREATE INDEX idx_ast_units_kind ON ast_units(kind);
CREATE INDEX idx_ast_units_parent ON ast_units(parent_id);
CREATE INDEX idx_ast_units_name_lower ON ast_units(LOWER(name));
CREATE INDEX idx_ast_units_qualified_lower ON ast_units(LOWER(qualified));

CREATE TABLE edges (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id     TEXT NOT NULL,
    src_id      INTEGER NOT NULL,
    dst_id      INTEGER NOT NULL,
    kind        TEXT NOT NULL,
    dst_name    TEXT NOT NULL DEFAULT '',
    file_path   TEXT NOT NULL DEFAULT '',
    line        INTEGER NOT NULL DEFAULT 0,
    dst_repo_id TEXT NOT NULL DEFAULT '',
    confidence  REAL NOT NULL DEFAULT 1.0,
    meta        TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_edges_src ON edges(src_id, kind);
CREATE INDEX idx_edges_dst ON edges(dst_id, kind);
CREATE INDEX idx_edges_kind ON edges(kind);
CREATE INDEX idx_edges_dst_name ON edges(dst_name);
CREATE INDEX idx_edges_repo_file ON edges(repo_id, file_path);
CREATE INDEX idx_edges_unresolved ON edges(dst_name, kind) WHERE dst_id = 0;
CREATE INDEX idx_edges_repo_kind ON edges(repo_id, kind);

CREATE TABLE index_jobs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id      TEXT NOT NULL,
    force        INTEGER NOT NULL DEFAULT 0,
    status       TEXT NOT NULL DEFAULT 'pending',
    error        TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    claimed_at   INTEGER NOT NULL DEFAULT 0,
    heartbeat_at INTEGER NOT NULL DEFAULT 0,
    claimed_by   TEXT NOT NULL DEFAULT '',
    kind         TEXT NOT NULL DEFAULT 'index',
    payload      TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_index_jobs_status ON index_jobs(status);
CREATE INDEX idx_index_jobs_repo ON index_jobs(repo_id, id);
CREATE UNIQUE INDEX idx_index_jobs_pending_repo ON index_jobs(repo_id)
    WHERE status = 'pending' AND kind = 'index';

CREATE TABLE repos (
    id                TEXT PRIMARY KEY,
    name              TEXT NOT NULL,
    source            TEXT NOT NULL,
    url               TEXT NOT NULL DEFAULT '',
    path              TEXT NOT NULL,
    branch            TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL DEFAULT 'idle',
    last_error        TEXT NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL,
    indexed_at        INTEGER NOT NULL DEFAULT 0,
    last_commit       TEXT NOT NULL DEFAULT '',
    claimed_by        TEXT NOT NULL DEFAULT '',
    claim_expires_at  INTEGER NOT NULL DEFAULT 0,
    pending_commit    TEXT NOT NULL DEFAULT '',
    active            INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE repo_coverage (
    repo_id    TEXT NOT NULL,
    kind       TEXT NOT NULL,
    candidates INTEGER NOT NULL DEFAULT 0,
    edges      INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (repo_id, kind)
);
