package storage

import (
	"context"
	"errors"

	"github.com/Nahua-Foundation/ragota/internal/repos"
)

// ErrNotFound is returned when a requested entity does not exist.
var ErrNotFound = errors.New("not found")

// DefaultRepoClaimTTLSeconds bounds how long an indexing claim keeps a repo
// locked. It has to outlive a full index pass over a large repository, since
// the claim is not renewed while indexing runs; a stuck repo is otherwise
// recovered at startup or via POST /repos/{id}/reset.
const DefaultRepoClaimTTLSeconds int64 = 3600

// RepoResetMessage is recorded as last_error on repos whose indexing claim was
// recovered, so the reason a repo left "indexing" without finishing stays
// visible to clients.
const RepoResetMessage = "indexing interrupted; reset at startup"

// Lifecycle manages the storage connection.
type Lifecycle interface {
	// Init initializes the storage connection.
	Init(ctx context.Context) error

	// Close closes the storage connection.
	Close() error
}

// FileStore handles file metadata.
type FileStore interface {
	StoreFile(ctx context.Context, f *File) error
	// BatchStoreFiles upserts many file rows in one transaction. An index pass
	// writes one row per file, and one autocommit transaction per row is a
	// per-file WAL commit that dominates the bookkeeping cost on a repository
	// with tens of thousands of files.
	BatchStoreFiles(ctx context.Context, files []*File) error
	GetFile(ctx context.Context, repoID, path string) (*File, error)
	GetFilesByRepo(ctx context.Context, repoID string) ([]*File, error)
	DeleteFile(ctx context.Context, repoID, path string) error
	// DeleteFilesByPaths deletes the given paths of one repo in one statement
	// per chunk, rather than one round-trip per path.
	DeleteFilesByPaths(ctx context.Context, repoID string, paths []string) error
	DeleteFilesByRepo(ctx context.Context, repoID string) error
}

// UnitStore handles AST units.
type UnitStore interface {
	StoreASTUnit(ctx context.Context, u *ASTUnit) error
	BatchStoreASTUnits(ctx context.Context, units []*ASTUnit) error
	GetASTUnits(ctx context.Context, opts QueryOpts) ([]*ASTUnit, error)
	GetASTUnitsByIDs(ctx context.Context, ids []string) ([]*ASTUnit, error)
	// DeleteASTUnitsByFile deletes a file's units and, in the same
	// transaction, unresolves every edge that pointed at one of them (dst_id
	// and dst_repo_id are cleared). Re-indexing a file recreates its units with
	// new IDs, so an edge that kept the old ID would still look resolved to
	// every caller testing dst_id != "" while resolving to nothing; clearing it
	// hands the edge back to the linker, which re-resolves it on its next run.
	DeleteASTUnitsByFile(ctx context.Context, repoID, filePath string) error
	// DeleteASTUnitsByFiles is DeleteASTUnitsByFile for many paths of one repo,
	// unresolve included, in a single transaction. An index pass rewrites a
	// whole window of files at once, and one transaction per file is what the
	// store stage spends its time on rather than on the rows it writes.
	DeleteASTUnitsByFiles(ctx context.Context, repoID string, paths []string) error
	DeleteASTUnitsByRepo(ctx context.Context, repoID string) error
	DeleteASTUnitsByKind(ctx context.Context, repoID, kind string) error
}

// EdgeStore handles edges between AST units.
type EdgeStore interface {
	StoreEdge(ctx context.Context, e *Edge) error
	BatchStoreEdges(ctx context.Context, edges []*Edge) error
	GetEdges(ctx context.Context, opts QueryOpts) ([]*Edge, error)
	DeleteEdgesByFile(ctx context.Context, repoID, filePath string) error
	// DeleteEdgesByFiles is DeleteEdgesByFile for many paths of one repo, in a
	// single transaction.
	DeleteEdgesByFiles(ctx context.Context, repoID string, paths []string) error
	DeleteEdgesByRepo(ctx context.Context, repoID string) error
	DeleteEdgesByKindAndDst(ctx context.Context, kind, dstName string) error
}

// RepoStore handles repository records.
type RepoStore interface {
	// StoreRepo inserts a repository or, when the id already exists, updates
	// only its definition (name, source, url, path, branch). Lifecycle state
	// — status, last_error, indexed_at, last_commit, the indexing claim — is
	// owned by the indexing pipeline and is never reset by a re-registration.
	//
	// The active flag is not written either, in either direction: a newly
	// registered repository is active, an existing one keeps the membership
	// SetActiveRepos gave it. Registration would otherwise silently redefine
	// the working set — every startup re-registers what its source finds.
	StoreRepo(ctx context.Context, r *repos.Repo) error
	GetRepo(ctx context.Context, id string) (*repos.Repo, error)
	ListRepos(ctx context.Context) ([]*repos.Repo, error)
	// ListActiveRepos returns the active repositories, in ListRepos' order.
	ListActiveRepos(ctx context.Context) ([]*repos.Repo, error)
	// SetActiveRepos makes exactly the repositories named by ids active and
	// every other registered repository inactive, as one atomic write: a
	// half-applied switch would leave a working set nobody asked for. An id
	// that names no repository is ignored, and an empty list is a valid
	// request that leaves nothing active.
	//
	// It changes nothing else. An inactive repository keeps its files, units,
	// edges and coverage, so a later call naming it again restores it as it
	// was, up to whatever an index pass has caught up on since.
	SetActiveRepos(ctx context.Context, ids []string) error
	DeleteRepo(ctx context.Context, id string) error
	// UpdateRepoStatus writes a terminal status and releases the indexing
	// claim (owner, expiry and the in-flight commit SHA are cleared).
	UpdateRepoStatus(ctx context.Context, id string, status repos.Status, lastError string, indexedAt int64) error
	// ClaimRepoForIndexing atomically transitions a repo to the indexing
	// status on behalf of owner, for at most ttlSeconds. A claim whose expiry
	// has passed can be taken over, so a crashed owner cannot wedge the repo
	// in "indexing" forever. It returns false if a live claim is held by
	// someone else, and ErrNotFound if the repo does not exist.
	ClaimRepoForIndexing(ctx context.Context, id, owner string, ttlSeconds int64) (bool, error)
	// ResetStuckRepos moves repos left in the indexing status back to idle.
	// With force it resets every such repo (single-instance startup, where no
	// other process can legitimately hold a claim); otherwise only claims that
	// have expired. It returns how many repos were reset.
	ResetStuckRepos(ctx context.Context, force bool) (int, error)
	// UpdateRepoLastCommit records the SHA of the last commit applied via
	// the commit ingestion API.
	UpdateRepoLastCommit(ctx context.Context, id, sha string) error
	// SetRepoPendingCommit records the SHA a running commit batch is applying,
	// so /sync-state can tell "in flight" from "lost". Empty clears it.
	SetRepoPendingCommit(ctx context.Context, id, sha string) error
}

// JobStore handles the distributed indexing job queue.
type JobStore interface {
	// EnqueueIndexJob adds a pending indexing job for the repo. At most one
	// pending index job per repo exists (enforced by a partial unique index, so
	// the insert is atomic rather than a check-then-insert race); when one is
	// already queued it absorbs this request and its force flag is the OR of
	// both, so a forced reindex is never downgraded to a cheap one.
	EnqueueIndexJob(ctx context.Context, repoID string, force bool) (*IndexJob, error)

	// EnqueueCommitJob adds a pending commit-ingestion job carrying the encoded
	// commit batch. Unlike index jobs these never merge: every batch is a
	// distinct piece of history that has to be applied, so each enqueue is a
	// new row and the queue order (by id) is the order they must run in.
	EnqueueCommitJob(ctx context.Context, repoID, payload string) (*IndexJob, error)

	// GetIndexJob returns one job by ID, without its payload.
	GetIndexJob(ctx context.Context, jobID string) (*IndexJob, error)

	// ListIndexJobs returns the repo's jobs, newest first, without their
	// payloads. limit <= 0 selects the backend default.
	ListIndexJobs(ctx context.Context, repoID string, limit int) ([]*IndexJob, error)

	// HasPendingCommitJobBefore reports whether the repo has an unfinished
	// (pending or running) commit job queued ahead of jobID. A commit batch
	// that does not continue the repo's cursor is only worth waiting for while
	// that is true; otherwise the chain is broken and waiting would block the
	// queue forever.
	HasPendingCommitJobBefore(ctx context.Context, repoID, jobID string) (bool, error)

	// ClaimNextIndexJob atomically claims the oldest pending job for the
	// given worker (pending -> running), payload included. It returns
	// ErrNotFound when the queue is empty.
	ClaimNextIndexJob(ctx context.Context, workerID string) (*IndexJob, error)

	// HeartbeatIndexJob refreshes the heartbeat of a running job. It returns
	// ErrNotFound if the job is not running anymore or was re-claimed by a
	// different worker.
	HeartbeatIndexJob(ctx context.Context, jobID, workerID string) error

	// CompleteIndexJob finishes a job: status becomes "done" when jobErr is
	// empty and "error" otherwise. The update is scoped to the claiming
	// worker, so a worker whose job was requeued and picked up elsewhere
	// cannot overwrite the new owner's result; it returns ErrNotFound then.
	CompleteIndexJob(ctx context.Context, jobID, workerID string, jobErr string) error

	// ReleaseIndexJob returns a claimed job to the pending queue (used when
	// the repo is busy, which is a retry condition rather than a failure).
	// It is scoped to the claiming worker.
	//
	// An index job is dropped as superseded when another pending index job for
	// the same repo appeared meanwhile (they are interchangeable); a commit job
	// always goes back to the queue, because no other job carries its batch.
	ReleaseIndexJob(ctx context.Context, jobID, workerID string) error

	// RequeueStaleIndexJobs moves running jobs whose heartbeat is older than
	// olderThanSec seconds back to pending and returns how many were requeued.
	// At most one index job per repo becomes pending; surplus index jobs are
	// failed as superseded, keeping the one-pending-index-job-per-repo
	// invariant. Commit jobs are never superseded — dropping one would drop a
	// piece of the repository's history.
	RequeueStaleIndexJobs(ctx context.Context, olderThanSec int64) (int, error)
}

// CoverageStore persists per-repo contract coverage summaries.
type CoverageStore interface {
	// StoreRepoCoverage replaces the repo's summary with c. Kinds absent from
	// c are removed, so a re-index never leaves counters from a previous
	// pass behind.
	StoreRepoCoverage(ctx context.Context, c *RepoCoverage) error
	// GetRepoCoverage returns the repo's summary, or ErrNotFound when no
	// coverage-reporting pass has run for it.
	GetRepoCoverage(ctx context.Context, repoID string) (*RepoCoverage, error)
	// DeleteRepoCoverage drops the repo's summary.
	DeleteRepoCoverage(ctx context.Context, repoID string) error
}

// GraphStore supports the graph/linker layer.
type GraphStore interface {
	GetASTUnitByID(ctx context.Context, id string) (*ASTUnit, error)
	UpdateEdgeResolution(ctx context.Context, edgeID, dstID, dstRepoID string, confidence float32) error
	UpdateEdgeDstName(ctx context.Context, edgeID, dstName string) error
	// UpdateEdgeMeta rewrites an edge's metadata in place, so annotating an
	// edge no longer requires deleting and re-inserting its whole group.
	UpdateEdgeMeta(ctx context.Context, edgeID, meta string) error
	DeleteEdgesByKind(ctx context.Context, repoID, kind string) error
}

// EdgeResolutionBatcher applies many edge resolutions per transaction. Linking
// a repository the size of Elasticsearch resolves millions of edges, and one
// autocommit UPDATE each serializes the whole pass on the writer.
//
// It is deliberately not part of Storage: it is a performance capability, not
// a requirement, and callers must fall back to UpdateEdgeResolution for
// backends (and test doubles) that do not implement it.
//
// A nil error means every resolution not named in the returned failures was
// applied and committed. A failure never aborts the rest of the batch.
//
// One batch must not name the same edge twice: the Postgres implementation
// joins on edge id and would apply an arbitrary one of the two.
type EdgeResolutionBatcher interface {
	BatchUpdateEdgeResolutions(ctx context.Context, res []EdgeResolution) ([]EdgeResolutionFailure, error)
}

// Storage is the main storage interface for ragota-core.
// It handles metadata (files, AST units, edges) and delegates vector storage.
type Storage interface {
	Lifecycle
	FileStore
	UnitStore
	EdgeStore
	RepoStore
	GraphStore
	JobStore
	CoverageStore

	// Vector storage delegate
	VectorStore() VectorStorage

	// Counters
	CountASTUnitsByRepo(ctx context.Context, repoID string) (int64, error)
	CountASTUnits(ctx context.Context) (int64, error)
}

// VectorStorage handles vector embeddings and search.
type VectorStorage interface {
	// Init initializes the vector storage.
	Init(ctx context.Context) error

	// Close closes the vector storage connection.
	Close() error

	// Upsert inserts or updates vectors.
	Upsert(ctx context.Context, points []*VectorPoint) error

	// Search performs vector similarity search.
	Search(ctx context.Context, opts VectorSearchOpts) ([]*VectorResult, error)

	// Delete removes vectors by repo and/or file.
	Delete(ctx context.Context, repoID, filePath string) error

	// Stats returns vector storage statistics.
	Stats(ctx context.Context) (*VectorStats, error)
}
