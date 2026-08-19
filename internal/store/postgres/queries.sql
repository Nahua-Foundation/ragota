-- Static queries for the PostgreSQL backend, compiled by sqlc into the pgdb
-- package. Dynamic filtered queries (GetASTUnits/GetEdges over QueryOpts) are
-- built with sqlutil in postgres.go instead; sqlc cannot express them.

-- --- Files ---

-- name: StoreFile :exec
INSERT INTO files (repo_id, path, language, hash, size, mod_time, indexed)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (repo_id, path) DO UPDATE SET
    language = EXCLUDED.language, hash = EXCLUDED.hash,
    size = EXCLUDED.size, mod_time = EXCLUDED.mod_time, indexed = EXCLUDED.indexed;

-- name: GetFile :one
SELECT repo_id, path, language, hash, size, mod_time, indexed
FROM files WHERE repo_id = $1 AND path = $2;

-- name: GetFilesByRepo :many
SELECT repo_id, path, language, hash, size, mod_time, indexed
FROM files WHERE repo_id = $1;

-- name: DeleteFile :exec
DELETE FROM files WHERE repo_id = $1 AND path = $2;

-- name: DeleteFilesByRepo :exec
DELETE FROM files WHERE repo_id = $1;

-- --- AST units ---

-- name: InsertASTUnit :one
INSERT INTO ast_units (repo_id, file_path, language, kind, name, qualified, parent_id, start_line, end_line, start_byte, end_byte, signature, doc, hash, meta)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
RETURNING id;

-- name: BatchInsertASTUnits :batchone
INSERT INTO ast_units (repo_id, file_path, language, kind, name, qualified, parent_id, start_line, end_line, start_byte, end_byte, signature, doc, hash, meta)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
RETURNING id;

-- name: GetASTUnitByID :one
SELECT id, repo_id, file_path, language, kind, name, qualified, parent_id, start_line, end_line, start_byte, end_byte, signature, doc, hash, meta
FROM ast_units WHERE id = $1;

-- name: GetASTUnitsByIDs :many
SELECT id, repo_id, file_path, language, kind, name, qualified, parent_id, start_line, end_line, start_byte, end_byte, signature, doc, hash, meta
FROM ast_units WHERE id = ANY(sqlc.arg(ids)::bigint[]);

-- name: DeleteASTUnitsByFile :exec
DELETE FROM ast_units WHERE repo_id = $1 AND file_path = $2;

-- name: DeleteASTUnitsByFiles :exec
DELETE FROM ast_units WHERE repo_id = @repo_id AND file_path = ANY(@paths::text[]);

-- name: DeleteASTUnitsByRepo :exec
DELETE FROM ast_units WHERE repo_id = $1;

-- name: DeleteASTUnitsByKind :exec
DELETE FROM ast_units WHERE repo_id = $1 AND kind = $2;

-- name: CountASTUnitsByRepo :one
SELECT COUNT(*) FROM ast_units WHERE repo_id = $1;

-- name: CountASTUnits :one
SELECT COUNT(*) FROM ast_units;

-- --- Edges ---

-- name: InsertEdge :one
INSERT INTO edges (repo_id, src_id, dst_id, kind, dst_name, file_path, line, dst_repo_id, confidence, meta)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING id;

-- name: BatchInsertEdges :batchone
INSERT INTO edges (repo_id, src_id, dst_id, kind, dst_name, file_path, line, dst_repo_id, confidence, meta)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING id;

-- name: DeleteEdgesByFile :exec
DELETE FROM edges WHERE repo_id = $1 AND file_path = $2;

-- name: DeleteEdgesByFiles :exec
DELETE FROM edges WHERE repo_id = @repo_id AND file_path = ANY(@paths::text[]);

-- name: DeleteEdgesByRepo :exec
DELETE FROM edges WHERE repo_id = $1;

-- name: DeleteEdgesByKind :exec
DELETE FROM edges WHERE repo_id = $1 AND kind = $2;

-- name: DeleteEdgesByKindGlobal :exec
DELETE FROM edges WHERE kind = $1;

-- name: DeleteEdgesByKindAndDst :exec
DELETE FROM edges WHERE kind = $1 AND dst_name = $2;

-- UnresolveEdgesByDstFile clears the resolution of every edge pointing at a
-- unit of the given file. Re-indexing recreates the file's units with new IDs,
-- so an edge keeping the old one reads as resolved while resolving to nothing.
-- The subquery is served by idx_ast_units_repo_file and the update by
-- idx_edges_dst, so the cost follows the file's units, not the edge table.
-- name: UnresolveEdgesByDstFile :execrows
UPDATE edges SET dst_id = 0, dst_repo_id = ''
WHERE dst_id <> 0
  AND dst_id IN (
      SELECT u.id FROM ast_units u WHERE u.repo_id = $1 AND u.file_path = $2
  );

-- UnresolveEdgesByDstFiles is UnresolveEdgesByDstFile for a whole window of
-- files, so re-indexing one window costs one statement rather than one per file.
-- name: UnresolveEdgesByDstFiles :execrows
UPDATE edges SET dst_id = 0, dst_repo_id = ''
WHERE dst_id <> 0
  AND dst_id IN (
      SELECT u.id FROM ast_units u
      WHERE u.repo_id = @repo_id AND u.file_path = ANY(@paths::text[])
  );

-- name: UpdateEdgeResolution :execrows
UPDATE edges SET dst_id = $1, dst_repo_id = $2, confidence = $3 WHERE id = $4;

-- BatchUpdateEdgeResolutions applies a whole batch of resolutions in one
-- statement, joined against the arrays as if against a temp table: linking a
-- large repository resolves millions of edges and one UPDATE each serializes
-- the pass on the writer. It returns the ids it actually updated, so the
-- caller can attribute the missing ones to the edges that no longer exist.
-- name: BatchUpdateEdgeResolutions :many
UPDATE edges e
SET dst_id = v.dst_id, dst_repo_id = v.dst_repo_id, confidence = v.confidence
FROM (
    SELECT unnest(@ids::bigint[]) AS id,
           unnest(@dst_ids::bigint[]) AS dst_id,
           unnest(@dst_repo_ids::text[]) AS dst_repo_id,
           unnest(@confidences::real[]) AS confidence
) AS v
WHERE e.id = v.id
RETURNING e.id;

-- name: UpdateEdgeDstName :execrows
UPDATE edges SET dst_name = $1 WHERE id = $2;

-- --- Repos ---

-- StoreRepo only writes the repo's definition on conflict. Re-registering an
-- existing repo must not reset status/indexed_at/last_commit: that would allow
-- a second concurrent index pass and disable the commit-gap guard. The active
-- column is absent from both halves for the same kind of reason: an insert
-- takes the schema's default (active) and a re-registration leaves the working
-- set SetActiveRepos decided.
-- name: StoreRepo :exec
INSERT INTO repos (id, name, source, url, path, branch, status, last_error, created_at, indexed_at, last_commit)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name, source = EXCLUDED.source, url = EXCLUDED.url,
    path = EXCLUDED.path, branch = EXCLUDED.branch;

-- name: GetRepo :one
SELECT id, name, source, url, path, branch, status, last_error, created_at, indexed_at, last_commit, pending_commit, active
FROM repos WHERE id = $1;

-- name: ListRepos :many
SELECT id, name, source, url, path, branch, status, last_error, created_at, indexed_at, last_commit, pending_commit, active
FROM repos ORDER BY created_at;

-- name: ListActiveRepos :many
SELECT id, name, source, url, path, branch, status, last_error, created_at, indexed_at, last_commit, pending_commit, active
FROM repos WHERE active ORDER BY created_at;

-- SetActiveRepos replaces the whole working set in one statement: every row is
-- raised or cleared by the same UPDATE, so no reader can observe a half-applied
-- switch. Ids naming no repository simply match nothing, and an empty array
-- leaves nothing active.
-- name: SetActiveRepos :exec
UPDATE repos SET active = (id = ANY(sqlc.arg(ids)::text[]));

-- name: DeleteRepo :exec
DELETE FROM repos WHERE id = $1;

-- name: UpdateRepoStatus :execrows
UPDATE repos SET status = $1, last_error = $2, indexed_at = $3,
    claimed_by = '', claim_expires_at = 0, pending_commit = ''
WHERE id = $4;

-- name: UpdateRepoLastCommit :execrows
UPDATE repos SET last_commit = $1 WHERE id = $2;

-- name: SetRepoPendingCommit :execrows
UPDATE repos SET pending_commit = $1 WHERE id = $2;

-- ClaimRepoForIndexing takes the claim when the repo is not indexing or when
-- the previous owner's claim has expired, so a crashed indexer cannot wedge
-- the repo in "indexing" forever.
-- name: ClaimRepoForIndexing :execrows
UPDATE repos
SET status = sqlc.arg(status), last_error = '',
    claimed_by = sqlc.arg(claimed_by), claim_expires_at = sqlc.arg(claim_expires_at)
WHERE id = sqlc.arg(id)
  AND (status <> sqlc.arg(status) OR claim_expires_at <= sqlc.arg(now));

-- name: ResetIndexingRepos :execrows
UPDATE repos SET status = 'idle', claimed_by = '', claim_expires_at = 0,
    pending_commit = '', last_error = sqlc.arg(last_error)
WHERE status = 'indexing';

-- name: ResetExpiredIndexingRepos :execrows
UPDATE repos SET status = 'idle', claimed_by = '', claim_expires_at = 0,
    pending_commit = '', last_error = sqlc.arg(last_error)
WHERE status = 'indexing' AND claim_expires_at <= sqlc.arg(now);

-- name: RepoExists :one
SELECT EXISTS(SELECT 1 FROM repos WHERE id = $1);

-- --- Index jobs ---

-- The payload (a commit job's batch, file contents included) is deliberately
-- absent from every projection but ClaimNextIndexJob's: only the worker that
-- runs the job needs it, and it can be tens of megabytes.

-- InsertIndexJob upserts against the partial unique index over pending index
-- rows, so concurrent enqueues cannot create duplicates and a queued job
-- absorbs the request instead of dropping its force flag.
-- name: InsertIndexJob :one
INSERT INTO index_jobs (repo_id, kind, force, status, created_at)
VALUES ($1, 'index', $2, 'pending', $3)
ON CONFLICT (repo_id) WHERE status = 'pending' AND kind = 'index'
DO UPDATE SET force = index_jobs.force OR EXCLUDED.force
RETURNING id, repo_id, kind, force, status, error, created_at, claimed_at, heartbeat_at, claimed_by;

-- InsertCommitJob never merges with a queued job: each batch is a distinct
-- span of history, and dropping one would leave a hole nothing reports.
-- name: InsertCommitJob :one
INSERT INTO index_jobs (repo_id, kind, force, status, created_at, payload)
VALUES ($1, 'commits', FALSE, 'pending', $2, $3)
RETURNING id, repo_id, kind, force, status, error, created_at, claimed_at, heartbeat_at, claimed_by;

-- name: GetIndexJob :one
SELECT id, repo_id, kind, force, status, error, created_at, claimed_at, heartbeat_at, claimed_by
FROM index_jobs WHERE id = $1;

-- name: ListIndexJobs :many
SELECT id, repo_id, kind, force, status, error, created_at, claimed_at, heartbeat_at, claimed_by
FROM index_jobs WHERE repo_id = $1 ORDER BY id DESC LIMIT $2;

-- HasPendingCommitJobBefore backs the "wait or give up" decision of a commit
-- batch that does not continue the repo's cursor. Served by idx_index_jobs_repo.
-- name: HasPendingCommitJobBefore :one
SELECT EXISTS(
    SELECT 1 FROM index_jobs
    WHERE repo_id = $1 AND kind = 'commits' AND status IN ('pending', 'running') AND id < $2
);

-- ClaimNextIndexJob atomically claims the oldest pending job. FOR UPDATE SKIP
-- LOCKED lets concurrent instances claim different jobs without blocking each
-- other, which is what makes the queue safe for multiple workers.
-- name: ClaimNextIndexJob :one
UPDATE index_jobs
SET status = 'running', claimed_by = $1, claimed_at = $2, heartbeat_at = $2
WHERE id = (
    SELECT id FROM index_jobs WHERE status = 'pending'
    ORDER BY id LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING id, repo_id, kind, force, status, error, created_at, claimed_at, heartbeat_at, claimed_by, payload;

-- Heartbeat/Complete/Release are scoped to the claiming worker: a worker whose
-- job was requeued and re-claimed elsewhere must not overwrite the new owner's
-- result (that is how a successful run got recorded as "repo busy").
-- name: HeartbeatIndexJob :execrows
UPDATE index_jobs SET heartbeat_at = $1 WHERE id = $2 AND status = 'running' AND claimed_by = $3;

-- The payload dies with the result: a terminal job is never re-run, and keeping
-- every applied commit batch would grow this table by the size of the
-- repository's history.
-- name: CompleteIndexJob :execrows
UPDATE index_jobs SET status = $1, error = $2, payload = '' WHERE id = $3 AND status = 'running' AND claimed_by = $4;

-- A released index job whose slot was taken meanwhile is dropped as superseded
-- by the caller; a commit job is always requeued, since no other job carries
-- its batch.
-- name: ReleaseIndexJob :execrows
UPDATE index_jobs AS ij
SET status = 'pending', claimed_by = '', claimed_at = 0, heartbeat_at = 0
WHERE ij.id = $1 AND ij.status = 'running' AND ij.claimed_by = $2
  AND (
      ij.kind <> 'index'
      OR NOT EXISTS (
          SELECT 1 FROM index_jobs p
          WHERE p.repo_id = ij.repo_id AND p.status = 'pending' AND p.kind = 'index'
      )
  );

-- Only one index job per repo may become pending, so stale running ones that
-- would collide are failed as superseded before the survivor is requeued.
-- Commit jobs are exempt: they are not interchangeable.
-- name: SupersedeStaleIndexJobs :execrows
UPDATE index_jobs AS ij
SET status = 'error', error = 'superseded by a pending job for the same repo'
WHERE ij.status = 'running' AND ij.heartbeat_at < sqlc.arg(cutoff) AND ij.kind = 'index'
  AND (
      EXISTS (SELECT 1 FROM index_jobs p
              WHERE p.repo_id = ij.repo_id AND p.status = 'pending' AND p.kind = 'index')
      OR ij.id <> (
          SELECT MIN(j.id) FROM index_jobs j
          WHERE j.repo_id = ij.repo_id AND j.status = 'running'
            AND j.heartbeat_at < sqlc.arg(cutoff) AND j.kind = 'index'
      )
  );

-- name: RequeueStaleIndexJobs :execrows
UPDATE index_jobs
SET status = 'pending', claimed_by = '', claimed_at = 0, heartbeat_at = 0
WHERE status = 'running' AND heartbeat_at < $1;
