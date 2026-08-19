package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// StoreRepo inserts a repository, or updates only the definition of an
// existing one. Re-registering a repo (the id is derived from name+path, so
// POST /repos is naturally idempotent) must not reset status, indexed_at or
// last_commit: that would start a second concurrent index pass over the same
// repo and disable the commit-gap guard.
//
// The active column is absent from both halves on purpose: an insert takes the
// schema's default (active), and a re-registration leaves the membership
// SetActiveRepos decided.
func (s *SQLite) StoreRepo(ctx context.Context, r *domain.Repo) error {
	if err := s.q.StoreRepo(ctx, StoreRepoParams{
		ID: r.ID, Name: r.Name, Source: string(r.Source), Url: r.URL,
		Path: r.Path, Branch: r.Branch, Status: string(r.Status),
		LastError: r.LastError, CreatedAt: r.CreatedAt, IndexedAt: r.IndexedAt,
		LastCommit: r.LastCommit,
	}); err != nil {
		return fmt.Errorf("store repo: %w", err)
	}
	return nil
}

// GetRepo gets a repository by ID.
func (s *SQLite) GetRepo(ctx context.Context, id string) (*domain.Repo, error) {
	r, err := s.q.GetRepo(ctx, id)
	if err != nil {
		return nil, notFound(err, "get repo")
	}
	return repoFromRow(r), nil
}

// ListRepos lists all repositories.
func (s *SQLite) ListRepos(ctx context.Context) ([]*domain.Repo, error) {
	rows, err := s.q.ListRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}
	out := make([]*domain.Repo, 0, len(rows))
	for _, r := range rows {
		out = append(out, repoFromRow(GetRepoRow(r)))
	}
	return out, nil
}

// ListActiveRepos lists the repositories in the active set.
func (s *SQLite) ListActiveRepos(ctx context.Context) ([]*domain.Repo, error) {
	rows, err := s.q.ListActiveRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("list active repos: %w", err)
	}
	out := make([]*domain.Repo, 0, len(rows))
	for _, r := range rows {
		out = append(out, repoFromRow(GetRepoRow(r)))
	}
	return out, nil
}

// SetActiveRepos makes exactly the named repositories active.
//
// Clearing every row and raising the named ones are two statements — the second
// is chunked, since SQLite binds a bounded number of parameters per statement —
// so they share a transaction: between them nothing is active at all, and a
// search that read the working set in that window would answer with nothing.
func (s *SQLite) SetActiveRepos(ctx context.Context, ids []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set active repos: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `UPDATE repos SET active = 0`); err != nil {
		return fmt.Errorf("set active repos: %w", err)
	}
	err = eachPathChunk(ids, func(batch []string) error {
		args := make([]any, 0, len(batch))
		for _, id := range batch {
			args = append(args, id)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE repos SET active = 1 WHERE id IN (`+placeholders(len(batch))+`)`, args...,
		); err != nil {
			return fmt.Errorf("set active repos: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set active repos: %w", err)
	}
	return nil
}

// DeleteRepo deletes a repository.
func (s *SQLite) DeleteRepo(ctx context.Context, id string) error {
	if err := s.q.DeleteRepo(ctx, id); err != nil {
		return fmt.Errorf("delete repo: %w", err)
	}
	return nil
}

// ClaimRepoForIndexing atomically transitions a repo to the indexing status
// for owner, for at most ttlSeconds. An expired claim is taken over, so a
// crashed indexer cannot wedge the repo. It returns false if a live claim is
// held, and ErrNotFound if the repo does not exist.
func (s *SQLite) ClaimRepoForIndexing(ctx context.Context, id, owner string, ttlSeconds int64) (bool, error) {
	if ttlSeconds <= 0 {
		ttlSeconds = store.DefaultRepoClaimTTLSeconds
	}
	now := time.Now().Unix()
	n, err := s.q.ClaimRepoForIndexing(ctx, ClaimRepoForIndexingParams{
		Status:           string(domain.StatusIndexing),
		ClaimedBy:        owner,
		ClaimExpiresAt:   now + ttlSeconds,
		ID:               id,
		Status_2:         string(domain.StatusIndexing),
		ClaimExpiresAt_2: now,
	})
	if err != nil {
		return false, fmt.Errorf("claim repo for indexing: %w", err)
	}
	if n > 0 {
		return true, nil
	}
	// Not claimed: either a live claim is held or the repo is missing.
	exists, err := s.q.RepoExists(ctx, id)
	if err != nil {
		return false, fmt.Errorf("claim repo for indexing: %w", err)
	}
	if !exists {
		return false, store.ErrNotFound
	}
	return false, nil
}

// ResetStuckRepos moves repos left in the indexing status back to idle.
func (s *SQLite) ResetStuckRepos(ctx context.Context, force bool) (int, error) {
	var (
		n   int64
		err error
	)
	if force {
		n, err = s.q.ResetIndexingRepos(ctx, store.RepoResetMessage)
	} else {
		n, err = s.q.ResetExpiredIndexingRepos(ctx, ResetExpiredIndexingReposParams{
			LastError:      store.RepoResetMessage,
			ClaimExpiresAt: time.Now().Unix(),
		})
	}
	if err != nil {
		return 0, fmt.Errorf("reset stuck repos: %w", err)
	}
	return int(n), nil
}

// SetRepoPendingCommit records the SHA a running commit batch is applying.
func (s *SQLite) SetRepoPendingCommit(ctx context.Context, id, sha string) error {
	n, err := s.q.SetRepoPendingCommit(ctx, SetRepoPendingCommitParams{PendingCommit: sha, ID: id})
	if err != nil {
		return fmt.Errorf("set repo pending commit: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// UpdateRepoLastCommit records the SHA of the last applied commit.
func (s *SQLite) UpdateRepoLastCommit(ctx context.Context, id, sha string) error {
	n, err := s.q.UpdateRepoLastCommit(ctx, UpdateRepoLastCommitParams{LastCommit: sha, ID: id})
	if err != nil {
		return fmt.Errorf("update repo last commit: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// UpdateRepoStatus updates repository status and releases the indexing claim.
func (s *SQLite) UpdateRepoStatus(ctx context.Context, id string, status domain.Status, lastError string, indexedAt int64) error {
	n, err := s.q.UpdateRepoStatus(ctx, UpdateRepoStatusParams{
		Status:    string(status),
		LastError: lastError,
		IndexedAt: indexedAt,
		ID:        id,
	})
	if err != nil {
		return fmt.Errorf("update repo status: %w", err)
	}
	// A status write that matched no repo is reported, not swallowed: the repo
	// was deleted under a running pass, and the caller logging "status update
	// failed" is the only trace that is left of it. Postgres already answered
	// this way, so the two backends now agree (see storage/storagetest).
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}
