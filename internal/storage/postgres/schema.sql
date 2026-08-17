-- Final PostgreSQL schema for ragota metadata storage.
--
-- This file is the sqlc source of truth and must always match the result of
-- applying every migration in postgres.go (schema_migrations mechanism). At
-- runtime the schema is created by those migrations, not by this file.

CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY
);

CREATE TABLE files (
    repo_id    TEXT NOT NULL,
    path       TEXT NOT NULL,
    language   TEXT NOT NULL,
    hash       TEXT NOT NULL,
    size       BIGINT NOT NULL,
    mod_time   BIGINT NOT NULL,
    indexed    BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (repo_id, path)
);

CREATE INDEX idx_files_repo ON files(repo_id);

CREATE TABLE ast_units (
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
    hash        TEXT NOT NULL DEFAULT '',
    meta        TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_ast_units_repo_file ON ast_units(repo_id, file_path);
CREATE INDEX idx_ast_units_name ON ast_units(name);
CREATE INDEX idx_ast_units_qualified ON ast_units(qualified);
CREATE INDEX idx_ast_units_kind ON ast_units(kind);
CREATE INDEX idx_ast_units_name_lower ON ast_units(LOWER(name));
CREATE INDEX idx_ast_units_qualified_lower ON ast_units(LOWER(qualified));

CREATE TABLE edges (
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
);

CREATE INDEX idx_edges_src ON edges(src_id, kind);
CREATE INDEX idx_edges_dst ON edges(dst_id, kind);
CREATE INDEX idx_edges_kind ON edges(kind);
CREATE INDEX idx_edges_repo ON edges(repo_id);
CREATE INDEX idx_edges_repo_file ON edges(repo_id, file_path);
CREATE INDEX idx_edges_dst_name ON edges(dst_name);

CREATE TABLE index_jobs (
    id           BIGSERIAL PRIMARY KEY,
    repo_id      TEXT NOT NULL,
    force        BOOLEAN NOT NULL DEFAULT FALSE,
    status       TEXT NOT NULL DEFAULT 'pending',
    error        TEXT NOT NULL DEFAULT '',
    created_at   BIGINT NOT NULL,
    claimed_at   BIGINT NOT NULL DEFAULT 0,
    heartbeat_at BIGINT NOT NULL DEFAULT 0,
    claimed_by   TEXT NOT NULL DEFAULT '',
    kind         TEXT NOT NULL DEFAULT 'index',
    -- Body of a commit job: the encoded commit batch, file contents included.
    -- This row is the only durable copy of the batch between the request that
    -- accepted it and the worker that applies it.
    payload      TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_index_jobs_status ON index_jobs(status);
CREATE INDEX idx_index_jobs_repo ON index_jobs(repo_id, id);

-- At most one pending *index* job per repo: enqueueing is an atomic upsert
-- against this index rather than a SELECT-then-INSERT race between instances.
-- Commit jobs are exempt — several batches may queue up and are applied in id
-- order.
CREATE UNIQUE INDEX idx_index_jobs_pending_repo ON index_jobs(repo_id)
    WHERE status = 'pending' AND kind = 'index';

CREATE TABLE repos (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    source     TEXT NOT NULL,
    url        TEXT NOT NULL DEFAULT '',
    path       TEXT NOT NULL,
    branch     TEXT NOT NULL DEFAULT '',
    status     TEXT NOT NULL DEFAULT 'idle',
    last_error TEXT NOT NULL DEFAULT '',
    created_at BIGINT NOT NULL,
    indexed_at BIGINT NOT NULL DEFAULT 0,
    last_commit TEXT NOT NULL DEFAULT '',
    claimed_by TEXT NOT NULL DEFAULT '',
    claim_expires_at BIGINT NOT NULL DEFAULT 0,
    pending_commit TEXT NOT NULL DEFAULT '',
    -- Whether the repository belongs to the working set the current run is
    -- about. Defaulting to TRUE is what keeps a registration (which never
    -- writes the column) and an upgrade of an existing database from hiding
    -- repositories nobody asked to hide.
    active BOOLEAN NOT NULL DEFAULT TRUE
);

-- Contract coverage of the last full index pass, one row per contract kind.
-- The candidates that produced no edge leave no trace in edges, so the
-- summary is stored rather than derived: it is what separates "this repo has
-- nothing to find" from "we did not find it".
CREATE TABLE repo_coverage (
    repo_id    TEXT NOT NULL,
    kind       TEXT NOT NULL,
    candidates BIGINT NOT NULL DEFAULT 0,
    edges      BIGINT NOT NULL DEFAULT 0,
    updated_at BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (repo_id, kind)
);
