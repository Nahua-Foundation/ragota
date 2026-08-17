package service

import (
	"context"
	"fmt"

	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// IndexJobState reports the queue state of one indexing job. It backs the
// job_id a distributed-mode /index response hands out: without it a client
// only ever sees "202 indexing" and cannot tell a queued job from a running
// one.
func (s *Service) IndexJobState(ctx context.Context, jobID string) (*storage.IndexJob, error) {
	if s.storage == nil {
		return nil, fmt.Errorf("storage not available")
	}
	return s.storage.GetIndexJob(ctx, jobID)
}

// maxJobListLimit caps a job listing: the queue keeps terminal jobs, so an
// unbounded page would grow with the repository's push history.
const maxJobListLimit = 200

// RepoJobs lists the queue entries of one repository, newest first. The repo
// is read first so an unknown id is a 404 rather than an empty list, which a
// client cannot tell from "nothing queued".
func (s *Service) RepoJobs(ctx context.Context, repoID string, limit int) ([]*storage.IndexJob, error) {
	if s.storage == nil {
		return nil, fmt.Errorf("storage not available")
	}
	if _, err := s.storage.GetRepo(ctx, repoID); err != nil {
		return nil, err
	}
	if limit > maxJobListLimit {
		limit = maxJobListLimit
	}
	return s.storage.ListIndexJobs(ctx, repoID, limit)
}

// RepoJob returns one job of a repository. A job belonging to another repo is
// reported as missing: the id alone must not be a way to read across repos.
func (s *Service) RepoJob(ctx context.Context, repoID, jobID string) (*storage.IndexJob, error) {
	if s.storage == nil {
		return nil, fmt.Errorf("storage not available")
	}
	job, err := s.storage.GetIndexJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job.RepoID != repoID {
		return nil, storage.ErrNotFound
	}
	return job, nil
}

// Ready probes the dependencies a request actually needs, so /ready can fail
// while /health (pure liveness) still succeeds. It touches the metadata store
// and, when configured, the vector store — a cheap round-trip each, not a full
// query.
func (s *Service) Ready(ctx context.Context) error {
	if s.storage == nil {
		return fmt.Errorf("storage not available")
	}
	if err := s.storage.Init(ctx); err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	if _, err := s.storage.CountASTUnits(ctx); err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	if vs := s.storage.VectorStore(); vs != nil {
		if _, err := vs.Stats(ctx); err != nil {
			return fmt.Errorf("vector store: %w", err)
		}
	}
	return nil
}
