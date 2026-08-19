-- Static queries for the SQLite backend, compiled by sqlc into the sqlite
-- package. The dynamic filtered queries (GetASTUnits/GetEdges over QueryOpts)
-- and the batched/transactional writers are hand-written in their .go files;
-- sqlc cannot express those.
--
-- Booleans are stored as INTEGER (0/1): the wrappers convert to/from Go bool.

-- --- Files ---

-- name: StoreFile :exec
INSERT INTO files (repo_id, path, language, hash, size, mod_time, indexed)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (repo_id, path) DO UPDATE SET
    language = excluded.language, hash = excluded.hash,
    size = excluded.size, mod_time = excluded.mod_time, indexed = excluded.indexed;

-- name: GetFile :one
SELECT repo_id, path, language, hash, size, mod_time, indexed
FROM files WHERE repo_id = ? AND path = ?;

-- name: GetFilesByRepo :many
SELECT repo_id, path, language, hash, size, mod_time, indexed
FROM files WHERE repo_id = ? ORDER BY path;

-- name: GetFilesByHash :many
SELECT repo_id, path, language, hash, size, mod_time, indexed
FROM files WHERE repo_id = ? AND hash = ?;

-- name: DeleteFile :exec
DELETE FROM files WHERE repo_id = ? AND path = ?;

-- name: DeleteFilesByRepo :exec
DELETE FROM files WHERE repo_id = ?;

-- --- AST units ---

-- name: InsertASTUnit :one
INSERT INTO ast_units (repo_id, file_path, language, kind, name, qualified, parent_id, start_line, end_line, start_byte, end_byte, signature, doc, hash, meta)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: GetASTUnitByID :one
SELECT id, repo_id, file_path, language, kind, name, qualified, parent_id, start_line, end_line, start_byte, end_byte, signature, doc, hash, meta
FROM ast_units WHERE id = ?;

-- name: GetASTUnitByQualifiedName :one
SELECT id, repo_id, file_path, language, kind, name, qualified, parent_id, start_line, end_line, start_byte, end_byte, signature, doc, hash, meta
FROM ast_units WHERE qualified = ? AND repo_id = ?;

-- name: DeleteASTUnitsByRepo :exec
DELETE FROM ast_units WHERE repo_id = ?;

-- name: DeleteASTUnitsByKind :exec
DELETE FROM ast_units WHERE repo_id = ? AND kind = ?;

-- name: CountASTUnitsByRepo :one
SELECT COUNT(*) FROM ast_units WHERE repo_id = ?;

-- name: CountASTUnits :one
SELECT COUNT(*) FROM ast_units;

-- --- Edges ---

-- name: InsertEdge :one
INSERT INTO edges (repo_id, src_id, dst_id, kind, dst_name, file_path, line, dst_repo_id, confidence, meta)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: GetEdge :one
SELECT id, repo_id, src_id, dst_id, kind, dst_name, file_path, line, dst_repo_id, confidence, meta
FROM edges WHERE id = ?;

-- name: DeleteEdgesByFile :exec
DELETE FROM edges WHERE repo_id = ? AND file_path = ?;

-- name: DeleteEdgesByRepo :exec
DELETE FROM edges WHERE repo_id = ?;

-- name: DeleteEdgesByKind :exec
DELETE FROM edges WHERE repo_id = ? AND kind = ?;

-- name: DeleteEdgesByKindGlobal :exec
DELETE FROM edges WHERE kind = ?;

-- name: DeleteEdgesByKindAndDst :exec
DELETE FROM edges WHERE kind = ? AND dst_name = ?;

-- name: UpdateEdgeResolution :execrows
UPDATE edges SET dst_id = ?, dst_repo_id = ?, confidence = ? WHERE id = ?;

-- name: UpdateEdgeDstName :execrows
UPDATE edges SET dst_name = ? WHERE id = ?;

-- name: UpdateEdgeMeta :execrows
UPDATE edges SET meta = ? WHERE id = ?;

-- name: ResolveEdges :exec
UPDATE edges SET dst_id = ? WHERE dst_id = 0 AND repo_id = ? AND dst_name = ? AND kind = ?;

-- --- Repos ---

-- name: StoreRepo :exec
INSERT INTO repos (id, name, source, url, path, branch, status, last_error, created_at, indexed_at, last_commit)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    name = excluded.name, source = excluded.source, url = excluded.url,
    path = excluded.path, branch = excluded.branch;

-- name: GetRepo :one
SELECT id, name, source, url, path, branch, status, last_error, created_at, indexed_at, last_commit, pending_commit, active
FROM repos WHERE id = ?;

-- name: ListRepos :many
SELECT id, name, source, url, path, branch, status, last_error, created_at, indexed_at, last_commit, pending_commit, active
FROM repos ORDER BY created_at;

-- name: ListActiveRepos :many
SELECT id, name, source, url, path, branch, status, last_error, created_at, indexed_at, last_commit, pending_commit, active
FROM repos WHERE active = 1 ORDER BY created_at;

-- name: DeleteRepo :exec
DELETE FROM repos WHERE id = ?;

-- name: UpdateRepoStatus :execrows
UPDATE repos SET status = ?, last_error = ?, indexed_at = ?,
    claimed_by = '', claim_expires_at = 0, pending_commit = ''
WHERE id = ?;

-- name: UpdateRepoLastCommit :execrows
UPDATE repos SET last_commit = ? WHERE id = ?;

-- name: SetRepoPendingCommit :execrows
UPDATE repos SET pending_commit = ? WHERE id = ?;

-- name: ClaimRepoForIndexing :execrows
UPDATE repos
SET status = ?, last_error = '', claimed_by = ?, claim_expires_at = ?
WHERE id = ? AND (status != ? OR claim_expires_at <= ?);

-- name: ResetIndexingRepos :execrows
UPDATE repos SET status = 'idle', claimed_by = '', claim_expires_at = 0,
    pending_commit = '', last_error = ?
WHERE status = 'indexing';

-- name: ResetExpiredIndexingRepos :execrows
UPDATE repos SET status = 'idle', claimed_by = '', claim_expires_at = 0,
    pending_commit = '', last_error = ?
WHERE status = 'indexing' AND claim_expires_at <= ?;

-- name: RepoExists :one
SELECT EXISTS(SELECT 1 FROM repos WHERE id = ?);

-- --- Index jobs ---

-- name: EnqueueCommitJob :one
INSERT INTO index_jobs (repo_id, kind, force, status, created_at, payload)
VALUES (?, 'commits', 0, 'pending', ?, ?)
RETURNING id, repo_id, kind, force, status, error, created_at, claimed_at, heartbeat_at, claimed_by;

-- name: GetIndexJob :one
SELECT id, repo_id, kind, force, status, error, created_at, claimed_at, heartbeat_at, claimed_by
FROM index_jobs WHERE id = ?;

-- name: ListIndexJobs :many
SELECT id, repo_id, kind, force, status, error, created_at, claimed_at, heartbeat_at, claimed_by
FROM index_jobs WHERE repo_id = ? ORDER BY id DESC LIMIT ?;

-- name: HasPendingCommitJobBefore :one
SELECT EXISTS(
    SELECT 1 FROM index_jobs
    WHERE repo_id = ? AND kind = 'commits' AND status IN ('pending', 'running') AND id < ?
);

-- name: HeartbeatIndexJob :execrows
UPDATE index_jobs SET heartbeat_at = ? WHERE id = ? AND status = 'running' AND claimed_by = ?;

-- name: CompleteIndexJob :execrows
UPDATE index_jobs SET status = ?, error = ?, payload = '' WHERE id = ? AND status = 'running' AND claimed_by = ?;

-- --- Coverage ---

-- name: InsertRepoCoverage :exec
INSERT INTO repo_coverage (repo_id, kind, candidates, edges, updated_at)
VALUES (?, ?, ?, ?, ?);

-- name: DeleteRepoCoverage :exec
DELETE FROM repo_coverage WHERE repo_id = ?;

-- name: GetRepoCoverage :many
SELECT kind, candidates, edges, updated_at FROM repo_coverage WHERE repo_id = ? ORDER BY kind;
