package postgres

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// --- Index jobs (distributed indexing queue) ---

// defaultJobListLimit bounds an unqualified job listing; the queue keeps every
// terminal job, so a repo's history is unbounded.
const defaultJobListLimit = 50

// jobFromRow converts any of the payload-less job projections. sqlc emits a
// distinct row type per query; their field layouts are identical, so the
// callers convert to one of them rather than repeating this mapping.
func jobFromRow(r GetIndexJobRow) *domain.IndexJob {
	return &domain.IndexJob{
		ID:          strconv.FormatInt(r.ID, 10),
		RepoID:      r.RepoID,
		Kind:        r.Kind,
		Force:       r.Force,
		Status:      r.Status,
		Error:       r.Error,
		CreatedAt:   r.CreatedAt,
		ClaimedAt:   r.ClaimedAt,
		HeartbeatAt: r.HeartbeatAt,
		ClaimedBy:   r.ClaimedBy,
	}
}

// EnqueueIndexJob adds a pending indexing job for the repo. The upsert targets
// the partial unique index over pending index rows, so two instances
// enqueueing at the same moment cannot both insert; the surviving row's force
// flag is the OR of both requests, so a forced reindex is never swallowed by a
// queued cheap one.
func (p *Postgres) EnqueueIndexJob(ctx context.Context, repoID string, force bool) (*domain.IndexJob, error) {
	row, err := p.q.InsertIndexJob(ctx, InsertIndexJobParams{
		RepoID:    repoID,
		Force:     force,
		CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		return nil, fmt.Errorf("enqueue index job: %w", err)
	}
	return jobFromRow(GetIndexJobRow(row)), nil
}

// EnqueueCommitJob queues a commit batch. It never merges with a queued job:
// each batch is a distinct span of history and dropping one would leave a hole
// the client is never told about.
func (p *Postgres) EnqueueCommitJob(ctx context.Context, repoID, payload string) (*domain.IndexJob, error) {
	row, err := p.q.InsertCommitJob(ctx, InsertCommitJobParams{
		RepoID:    repoID,
		CreatedAt: time.Now().Unix(),
		Payload:   payload,
	})
	if err != nil {
		return nil, fmt.Errorf("enqueue commit job: %w", err)
	}
	return jobFromRow(GetIndexJobRow(row)), nil
}

// GetIndexJob returns one job by ID.
func (p *Postgres) GetIndexJob(ctx context.Context, jobID string) (*domain.IndexJob, error) {
	row, err := p.q.GetIndexJob(ctx, intOrZero(jobID))
	if err != nil {
		return nil, notFoundErr(err)
	}
	return jobFromRow(row), nil
}

// ListIndexJobs returns the repo's jobs, newest first.
func (p *Postgres) ListIndexJobs(ctx context.Context, repoID string, limit int) ([]*domain.IndexJob, error) {
	if limit <= 0 {
		limit = defaultJobListLimit
	}
	rows, err := p.q.ListIndexJobs(ctx, ListIndexJobsParams{RepoID: repoID, Limit: int32(limit)})
	if err != nil {
		return nil, fmt.Errorf("list index jobs: %w", err)
	}
	jobs := make([]*domain.IndexJob, 0, len(rows))
	for _, r := range rows {
		jobs = append(jobs, jobFromRow(GetIndexJobRow(r)))
	}
	return jobs, nil
}

// HasPendingCommitJobBefore reports whether an unfinished commit job for the
// repo is queued ahead of jobID.
func (p *Postgres) HasPendingCommitJobBefore(ctx context.Context, repoID, jobID string) (bool, error) {
	ok, err := p.q.HasPendingCommitJobBefore(ctx, HasPendingCommitJobBeforeParams{
		RepoID: repoID, ID: intOrZero(jobID),
	})
	if err != nil {
		return false, fmt.Errorf("check earlier commit jobs: %w", err)
	}
	return ok, nil
}

// ClaimNextIndexJob atomically claims the oldest pending job for the worker
// (pending -> running, via FOR UPDATE SKIP LOCKED), payload included. It
// returns ErrNotFound when the queue is empty.
func (p *Postgres) ClaimNextIndexJob(ctx context.Context, workerID string) (*domain.IndexJob, error) {
	row, err := p.q.ClaimNextIndexJob(ctx, ClaimNextIndexJobParams{
		ClaimedBy: workerID,
		ClaimedAt: time.Now().Unix(),
	})
	if err != nil {
		return nil, notFoundErr(err)
	}
	job := jobFromRow(GetIndexJobRow{
		ID: row.ID, RepoID: row.RepoID, Kind: row.Kind, Force: row.Force,
		Status: row.Status, Error: row.Error, CreatedAt: row.CreatedAt,
		ClaimedAt: row.ClaimedAt, HeartbeatAt: row.HeartbeatAt, ClaimedBy: row.ClaimedBy,
	})
	job.Payload = row.Payload
	return job, nil
}

// HeartbeatIndexJob refreshes the heartbeat of a running job held by workerID.
func (p *Postgres) HeartbeatIndexJob(ctx context.Context, jobID, workerID string) error {
	n, err := p.q.HeartbeatIndexJob(ctx, HeartbeatIndexJobParams{
		HeartbeatAt: time.Now().Unix(),
		ID:          intOrZero(jobID),
		ClaimedBy:   workerID,
	})
	if err != nil {
		return fmt.Errorf("heartbeat index job: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// CompleteIndexJob finishes a running job held by workerID: status becomes
// "done" when jobErr is empty and "error" otherwise. A worker whose job was
// requeued and re-claimed elsewhere gets ErrNotFound instead of overwriting
// the new owner's result.
//
// The payload is dropped with the result. A terminal job is never re-run, and
// keeping every applied commit batch would grow the queue table by the size of
// the repository's history.
func (p *Postgres) CompleteIndexJob(ctx context.Context, jobID, workerID string, jobErr string) error {
	status := domain.JobStatusDone
	if jobErr != "" {
		status = domain.JobStatusError
	}
	n, err := p.q.CompleteIndexJob(ctx, CompleteIndexJobParams{
		Status:    status,
		Error:     jobErr,
		ID:        intOrZero(jobID),
		ClaimedBy: workerID,
	})
	if err != nil {
		return fmt.Errorf("complete index job: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ReleaseIndexJob returns a job claimed by workerID to the pending queue. An
// index job whose slot was taken by another pending index job meanwhile is
// dropped as superseded rather than violating the one-pending-per-repo index;
// a commit job is always requeued, since no other job carries its batch.
func (p *Postgres) ReleaseIndexJob(ctx context.Context, jobID, workerID string) error {
	n, err := p.q.ReleaseIndexJob(ctx, ReleaseIndexJobParams{
		ID:        intOrZero(jobID),
		ClaimedBy: workerID,
	})
	if err != nil {
		return fmt.Errorf("release index job: %w", err)
	}
	if n > 0 {
		return nil
	}
	return p.CompleteIndexJob(ctx, jobID, workerID, "superseded by a pending job for the same repo")
}

// RequeueStaleIndexJobs moves running jobs whose heartbeat is older than
// olderThanSec seconds back to pending. Only one index job per repo may become
// pending (the partial unique index), so the surplus is failed as superseded
// first and the survivor requeued afterwards. Commit jobs are exempt: they are
// not interchangeable, so every stale one is requeued.
func (p *Postgres) RequeueStaleIndexJobs(ctx context.Context, olderThanSec int64) (int, error) {
	cutoff := time.Now().Unix() - olderThanSec
	if _, err := p.q.SupersedeStaleIndexJobs(ctx, cutoff); err != nil {
		return 0, fmt.Errorf("requeue stale index jobs: %w", err)
	}
	n, err := p.q.RequeueStaleIndexJobs(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("requeue stale index jobs: %w", err)
	}
	return int(n), nil
}
