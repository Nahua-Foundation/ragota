package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// The repository catalog: what is registered, what gets forgotten. Indexing a
// repository lives in indexing.go; queue mechanics live in jobs.go.

// AddRepo adds a repository from the given source type.
//
// Repository IDs are derived from name+path, so re-posting the same repo is
// idempotent — and must stay that way: the existing row's lifecycle state
// (status, cursor, indexed_at) is preserved rather than reset, otherwise a
// re-registration would clear an in-progress claim (allowing two concurrent
// index passes) and wipe the commit cursor that the gap check depends on.
func (s *Service) AddRepo(ctx context.Context, sourceType repos.SourceType, req *repos.AddRequest) (*repos.Repo, error) {
	src, ok := s.sources[sourceType]
	if !ok {
		return nil, fmt.Errorf("%w: source type %s not supported", ErrBadRequest, sourceType)
	}

	repo, err := src.Add(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("add repo: %w", err)
	}
	repo.Status = repos.StatusIdle
	repo.CreatedAt = time.Now().Unix()
	// A repository joins the working set when it is first registered, and an
	// existing one keeps the membership SetActiveRepos gave it (below).
	// StoreRepo writes the flag in neither case; setting it here only makes the
	// object handed back to the caller agree with the row.
	repo.Active = true

	existing, err := s.storage.GetRepo(ctx, repo.ID)
	switch {
	case err == nil && existing != nil:
		repo.Status = existing.Status
		repo.LastError = existing.LastError
		repo.CreatedAt = existing.CreatedAt
		repo.IndexedAt = existing.IndexedAt
		repo.LastCommit = existing.LastCommit
		repo.PendingCommit = existing.PendingCommit
		repo.Active = existing.Active
	case err != nil && !errors.Is(err, storage.ErrNotFound):
		return nil, fmt.Errorf("get repo: %w", err)
	}

	if err := s.storage.StoreRepo(ctx, repo); err != nil {
		return nil, fmt.Errorf("store repo: %w", err)
	}

	return repo, nil
}

// ResetRepo clears a stuck indexing claim and returns the repo to idle. It is
// the manual counterpart of the startup recovery, exposed as
// POST /repos/{id}/reset for the case where a claim outlives its holder.
func (s *Service) ResetRepo(ctx context.Context, repoID string) (*repos.Repo, error) {
	repo, err := s.storage.GetRepo(ctx, repoID)
	if err != nil {
		return nil, err
	}
	if err := s.storage.UpdateRepoStatus(ctx, repoID, repos.StatusIdle, "", repo.IndexedAt); err != nil {
		return nil, err
	}
	return s.storage.GetRepo(ctx, repoID)
}

// GetRepo retrieves a repository by ID.
func (s *Service) GetRepo(ctx context.Context, repoID string) (*repos.Repo, error) {
	return s.storage.GetRepo(ctx, repoID)
}

// ListRepos returns all known repositories.
func (s *Service) ListRepos(ctx context.Context) ([]*repos.Repo, error) {
	return s.storage.ListRepos(ctx)
}

// DeleteRepo removes a repository and all its indexed data.
func (s *Service) DeleteRepo(ctx context.Context, repoID string) error {
	repo, err := s.storage.GetRepo(ctx, repoID)
	if err != nil {
		return err
	}

	var errs []error
	if source, ok := s.sources[repo.Source]; ok {
		if err := source.Remove(ctx, repoID); err != nil {
			errs = append(errs, fmt.Errorf("remove from source: %w", err))
		}
	}
	for _, idx := range s.indexers {
		if err := idx.Remove(ctx, repoID, nil); err != nil {
			errs = append(errs, fmt.Errorf("remove from indexer %s: %w", idx.Name(), err))
		}
	}
	if err := s.storage.DeleteFilesByRepo(ctx, repoID); err != nil {
		errs = append(errs, fmt.Errorf("delete files: %w", err))
	}
	if err := s.storage.DeleteASTUnitsByRepo(ctx, repoID); err != nil {
		errs = append(errs, fmt.Errorf("delete ast units: %w", err))
	}
	if err := s.storage.DeleteEdgesByRepo(ctx, repoID); err != nil {
		errs = append(errs, fmt.Errorf("delete edges: %w", err))
	}
	if err := s.storage.DeleteRepoCoverage(ctx, repoID); err != nil {
		errs = append(errs, fmt.Errorf("delete coverage: %w", err))
	}
	if err := s.storage.DeleteRepo(ctx, repoID); err != nil {
		errs = append(errs, fmt.Errorf("delete repo: %w", err))
	}

	// A deleted repository's documents stop counting the moment they are
	// removed, but the words they contributed stay in the term dictionary
	// until the segments holding them are rewritten. That gap is what the
	// keyword indexer measures its corpus with, so leaving it skews the
	// scores of every repository that remains.
	s.compactIndexes(ctx)

	return errors.Join(errs...)
}
