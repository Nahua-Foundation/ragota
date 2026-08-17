package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

const repoColumns = `id, name, source, url, path, branch, status, last_error, created_at, indexed_at, last_commit, pending_commit, active`

func scanRepo(sc scanner) (*repos.Repo, error) {
	var r repos.Repo
	err := sc.Scan(
		&r.ID, &r.Name, &r.Source, &r.URL, &r.Path, &r.Branch, &r.Status,
		&r.LastError, &r.CreatedAt, &r.IndexedAt, &r.LastCommit, &r.PendingCommit, &r.Active,
	)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// StoreRepo inserts a repository, or updates only the definition of an
// existing one. Re-registering a repo (the id is derived from name+path, so
// POST /repos is naturally idempotent) must not reset status, indexed_at or
// last_commit: that would start a second concurrent index pass over the same
// repo and disable the commit-gap guard.
//
// The active column is absent from both halves on purpose: an insert takes the
// schema's default (active), and a re-registration leaves the membership
// SetActiveRepos decided.
func (s *SQLite) StoreRepo(ctx context.Context, r *repos.Repo) error {
	query := `
		INSERT INTO repos (id, name, source, url, path, branch, status, last_error, created_at, indexed_at, last_commit)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			name = excluded.name,
			source = excluded.source,
			url = excluded.url,
			path = excluded.path,
			branch = excluded.branch
	`
	_, err := s.db.ExecContext(ctx, query,
		r.ID, r.Name, r.Source, r.URL, r.Path, r.Branch, r.Status, r.LastError, r.CreatedAt, r.IndexedAt, r.LastCommit,
	)
	if err != nil {
		return fmt.Errorf("store repo: %w", err)
	}
	return nil
}

// GetRepo gets a repository by ID.
func (s *SQLite) GetRepo(ctx context.Context, id string) (*repos.Repo, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+repoColumns+` FROM repos WHERE id = ?`, id)
	r, err := scanRepo(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}
	return r, nil
}

// ListRepos lists all repositories.
func (s *SQLite) ListRepos(ctx context.Context) ([]*repos.Repo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+repoColumns+` FROM repos ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}
	defer rows.Close()

	var result []*repos.Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, fmt.Errorf("scan repo: %w", err)
		}
		result = append(result, r)
	}

	return result, rows.Err()
}

// ListActiveRepos lists the repositories in the active set.
func (s *SQLite) ListActiveRepos(ctx context.Context) ([]*repos.Repo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+repoColumns+` FROM repos WHERE active = 1 ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list active repos: %w", err)
	}
	defer rows.Close()

	var result []*repos.Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, fmt.Errorf("scan repo: %w", err)
		}
		result = append(result, r)
	}

	return result, rows.Err()
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
	_, err := s.db.ExecContext(ctx, "DELETE FROM repos WHERE id = ?", id)
	if err != nil {
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
		ttlSeconds = storage.DefaultRepoClaimTTLSeconds
	}
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE repos SET status = ?, last_error = '', claimed_by = ?, claim_expires_at = ?
		 WHERE id = ? AND (status != ? OR claim_expires_at <= ?)`,
		string(repos.StatusIndexing), owner, now+ttlSeconds,
		id, string(repos.StatusIndexing), now,
	)
	if err != nil {
		return false, fmt.Errorf("claim repo for indexing: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim repo for indexing: %w", err)
	}
	if n > 0 {
		return true, nil
	}
	// Not claimed: either a live claim is held or the repo is missing.
	var exists int
	err = s.db.QueryRowContext(ctx, `SELECT 1 FROM repos WHERE id = ?`, id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, storage.ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("claim repo for indexing: %w", err)
	}
	return false, nil
}

// ResetStuckRepos moves repos left in the indexing status back to idle.
func (s *SQLite) ResetStuckRepos(ctx context.Context, force bool) (int, error) {
	query := `UPDATE repos SET status = ?, claimed_by = '', claim_expires_at = 0, pending_commit = '',
			  last_error = ?
			  WHERE status = ?`
	args := []any{string(repos.StatusIdle), storage.RepoResetMessage, string(repos.StatusIndexing)}
	if !force {
		query += ` AND claim_expires_at <= ?`
		args = append(args, time.Now().Unix())
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("reset stuck repos: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reset stuck repos: %w", err)
	}
	return int(n), nil
}

// SetRepoPendingCommit records the SHA a running commit batch is applying.
func (s *SQLite) SetRepoPendingCommit(ctx context.Context, id, sha string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE repos SET pending_commit = ? WHERE id = ?`, sha, id)
	if err != nil {
		return fmt.Errorf("set repo pending commit: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set repo pending commit: %w", err)
	}
	if n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// UpdateRepoLastCommit records the SHA of the last applied commit.
func (s *SQLite) UpdateRepoLastCommit(ctx context.Context, id, sha string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE repos SET last_commit = ? WHERE id = ?`, sha, id)
	if err != nil {
		return fmt.Errorf("update repo last commit: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update repo last commit: %w", err)
	}
	if n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// UpdateRepoStatus updates repository status and releases the indexing claim.
func (s *SQLite) UpdateRepoStatus(ctx context.Context, id string, status repos.Status, lastError string, indexedAt int64) error {
	query := `
		UPDATE repos
		SET status = ?,
			last_error = ?,
			indexed_at = ?,
			claimed_by = '',
			claim_expires_at = 0,
			pending_commit = ''
		WHERE id = ?
	`
	res, err := s.db.ExecContext(ctx, query, status, lastError, indexedAt, id)
	if err != nil {
		return fmt.Errorf("update repo status: %w", err)
	}
	// A status write that matched no repo is reported, not swallowed: the repo
	// was deleted under a running pass, and the caller logging "status update
	// failed" is the only trace that is left of it. Postgres already answered
	// this way, so the two backends now agree (see storage/storagetest).
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return storage.ErrNotFound
	}
	return nil
}
