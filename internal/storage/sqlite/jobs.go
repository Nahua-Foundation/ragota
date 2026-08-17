package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// --- Index jobs (distributed indexing queue) ---

// jobColumns omits the payload: it can be tens of megabytes and only the
// claiming worker has any use for it (see jobClaimColumns).
const (
	jobColumns      = `id, repo_id, kind, force, status, error, created_at, claimed_at, heartbeat_at, claimed_by`
	jobClaimColumns = jobColumns + `, payload`
)

// defaultJobListLimit bounds an unqualified job listing; the queue keeps every
// terminal job, so a repo's history is unbounded.
const defaultJobListLimit = 50

func scanJobRow(sc scanner, withPayload bool) (*storage.IndexJob, error) {
	var j storage.IndexJob
	dest := []any{&j.ID, &j.RepoID, &j.Kind, &j.Force, &j.Status, &j.Error,
		&j.CreatedAt, &j.ClaimedAt, &j.HeartbeatAt, &j.ClaimedBy}
	if withPayload {
		dest = append(dest, &j.Payload)
	}
	err := sc.Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

func scanJob(row *sql.Row) (*storage.IndexJob, error) { return scanJobRow(row, false) }

// EnqueueIndexJob adds a pending indexing job for the repo. The upsert targets
// the partial unique index over pending index rows, so two instances
// enqueueing at the same moment cannot both insert; the surviving row's force
// flag is the OR of both requests, so a forced reindex is never swallowed by a
// queued cheap one.
func (s *SQLite) EnqueueIndexJob(ctx context.Context, repoID string, force bool) (*storage.IndexJob, error) {
	job, err := scanJob(s.db.QueryRowContext(ctx,
		`INSERT INTO index_jobs (repo_id, kind, force, status, created_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (repo_id) WHERE status = 'pending' AND kind = 'index'
		 DO UPDATE SET force = index_jobs.force OR excluded.force
		 RETURNING `+jobColumns,
		repoID, storage.JobKindIndex, force, storage.JobStatusPending, time.Now().Unix(),
	))
	if err != nil {
		return nil, fmt.Errorf("enqueue index job: %w", err)
	}
	return job, nil
}

// EnqueueCommitJob queues a commit batch. It never merges with a queued job:
// each batch is a distinct span of history and dropping one would leave a hole
// the client is never told about.
func (s *SQLite) EnqueueCommitJob(ctx context.Context, repoID, payload string) (*storage.IndexJob, error) {
	job, err := scanJob(s.db.QueryRowContext(ctx,
		`INSERT INTO index_jobs (repo_id, kind, force, status, created_at, payload)
		 VALUES (?, ?, 0, ?, ?, ?)
		 RETURNING `+jobColumns,
		repoID, storage.JobKindCommits, storage.JobStatusPending, time.Now().Unix(), payload,
	))
	if err != nil {
		return nil, fmt.Errorf("enqueue commit job: %w", err)
	}
	return job, nil
}

// GetIndexJob returns one job by ID.
func (s *SQLite) GetIndexJob(ctx context.Context, jobID string) (*storage.IndexJob, error) {
	job, err := scanJob(s.db.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM index_jobs WHERE id = ?`, jobID))
	if errors.Is(err, storage.ErrNotFound) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get index job: %w", err)
	}
	return job, nil
}

// ListIndexJobs returns the repo's jobs, newest first.
func (s *SQLite) ListIndexJobs(ctx context.Context, repoID string, limit int) ([]*storage.IndexJob, error) {
	if limit <= 0 {
		limit = defaultJobListLimit
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM index_jobs WHERE repo_id = ? ORDER BY id DESC LIMIT ?`, repoID, limit)
	if err != nil {
		return nil, fmt.Errorf("list index jobs: %w", err)
	}
	defer rows.Close()

	var jobs []*storage.IndexJob
	for rows.Next() {
		j, err := scanJobRow(rows, false)
		if err != nil {
			return nil, fmt.Errorf("scan index job: %w", err)
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// HasPendingCommitJobBefore reports whether an unfinished commit job for the
// repo is queued ahead of jobID. Served by idx_index_jobs_repo.
func (s *SQLite) HasPendingCommitJobBefore(ctx context.Context, repoID, jobID string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM index_jobs
		  WHERE repo_id = ? AND kind = 'commits' AND status IN (?, ?) AND id < ?)`,
		repoID, storage.JobStatusPending, storage.JobStatusRunning, jobID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check earlier commit jobs: %w", err)
	}
	return exists != 0, nil
}

// ClaimNextIndexJob atomically claims the oldest pending job for the worker
// (pending -> running). SQLite has a single writer, so the UPDATE with a
// nested SELECT is atomic. It returns ErrNotFound when the queue is empty.
func (s *SQLite) ClaimNextIndexJob(ctx context.Context, workerID string) (*storage.IndexJob, error) {
	now := time.Now().Unix()
	job, err := scanJobRow(s.db.QueryRowContext(ctx,
		`UPDATE index_jobs SET status = ?, claimed_by = ?, claimed_at = ?, heartbeat_at = ?
		 WHERE id = (SELECT id FROM index_jobs WHERE status = ? ORDER BY id LIMIT 1)
		 RETURNING `+jobClaimColumns,
		storage.JobStatusRunning, workerID, now, now, storage.JobStatusPending,
	), true)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("claim next index job: %w", err)
	}
	return job, nil
}

// HeartbeatIndexJob refreshes the heartbeat of a running job held by workerID.
func (s *SQLite) HeartbeatIndexJob(ctx context.Context, jobID, workerID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE index_jobs SET heartbeat_at = ? WHERE id = ? AND status = ? AND claimed_by = ?`,
		time.Now().Unix(), jobID, storage.JobStatusRunning, workerID,
	)
	if err != nil {
		return fmt.Errorf("heartbeat index job: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("heartbeat index job: %w", err)
	}
	if n == 0 {
		return storage.ErrNotFound
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
func (s *SQLite) CompleteIndexJob(ctx context.Context, jobID, workerID string, jobErr string) error {
	status := storage.JobStatusDone
	if jobErr != "" {
		status = storage.JobStatusError
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE index_jobs SET status = ?, error = ?, payload = '' WHERE id = ? AND status = ? AND claimed_by = ?`,
		status, jobErr, jobID, storage.JobStatusRunning, workerID,
	)
	if err != nil {
		return fmt.Errorf("complete index job: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete index job: %w", err)
	}
	if n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// ReleaseIndexJob returns a job claimed by workerID to the pending queue. An
// index job whose slot was taken by another pending index job meanwhile is
// dropped as superseded rather than violating the one-pending-per-repo index;
// a commit job is always requeued, since no other job carries its batch.
func (s *SQLite) ReleaseIndexJob(ctx context.Context, jobID, workerID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE index_jobs SET status = ?, claimed_by = '', claimed_at = 0, heartbeat_at = 0
		 WHERE id = ? AND status = ? AND claimed_by = ?
		   AND (kind <> 'index'
		        OR NOT EXISTS (SELECT 1 FROM index_jobs p
		                       WHERE p.repo_id = index_jobs.repo_id AND p.status = 'pending' AND p.kind = 'index'))`,
		storage.JobStatusPending, jobID, storage.JobStatusRunning, workerID,
	)
	if err != nil {
		return fmt.Errorf("release index job: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("release index job: %w", err)
	}
	if n > 0 {
		return nil
	}
	return s.CompleteIndexJob(ctx, jobID, workerID, "superseded by a pending job for the same repo")
}

// RequeueStaleIndexJobs moves running jobs whose heartbeat is older than
// olderThanSec seconds back to pending. Only one index job per repo may become
// pending (the partial unique index), so the surplus is failed as superseded
// first and the survivor requeued afterwards. Commit jobs are exempt: they are
// not interchangeable, so every stale one is requeued.
func (s *SQLite) RequeueStaleIndexJobs(ctx context.Context, olderThanSec int64) (int, error) {
	cutoff := time.Now().Unix() - olderThanSec
	if _, err := s.db.ExecContext(ctx,
		`UPDATE index_jobs SET status = ?, error = 'superseded by a pending job for the same repo'
		 WHERE status = ? AND heartbeat_at < ? AND kind = 'index'
		   AND (EXISTS (SELECT 1 FROM index_jobs p
		                WHERE p.repo_id = index_jobs.repo_id AND p.status = 'pending' AND p.kind = 'index')
		        OR id <> (SELECT MIN(j.id) FROM index_jobs j
		                  WHERE j.repo_id = index_jobs.repo_id AND j.status = ?
		                    AND j.heartbeat_at < ? AND j.kind = 'index'))`,
		storage.JobStatusError, storage.JobStatusRunning, cutoff, storage.JobStatusRunning, cutoff,
	); err != nil {
		return 0, fmt.Errorf("requeue stale index jobs: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE index_jobs SET status = ?, claimed_by = '', claimed_at = 0, heartbeat_at = 0
		 WHERE status = ? AND heartbeat_at < ?`,
		storage.JobStatusPending, storage.JobStatusRunning, cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("requeue stale index jobs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("requeue stale index jobs: %w", err)
	}
	return int(n), nil
}
