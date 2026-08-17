package sqlite

import (
	"context"
	"fmt"

	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// StoreRepoCoverage replaces a repo's contract coverage summary. The delete
// and the inserts share one transaction: a half-written summary would read as
// a repo whose HTTP calls are covered and whose RPC calls were never looked
// at.
func (s *SQLite) StoreRepoCoverage(ctx context.Context, c *storage.RepoCoverage) error {
	if c == nil || c.RepoID == "" {
		return fmt.Errorf("store coverage: repo id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store coverage: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM repo_coverage WHERE repo_id = ?`, c.RepoID); err != nil {
		return fmt.Errorf("store coverage: %w", err)
	}
	for kind, counts := range c.Kinds {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO repo_coverage (repo_id, kind, candidates, edges, updated_at) VALUES (?, ?, ?, ?, ?)`,
			c.RepoID, kind, counts.Candidates, counts.Edges, c.UpdatedAt)
		if err != nil {
			return fmt.Errorf("store coverage %s: %w", kind, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store coverage: %w", err)
	}
	return nil
}

// GetRepoCoverage returns a repo's coverage summary.
func (s *SQLite) GetRepoCoverage(ctx context.Context, repoID string) (*storage.RepoCoverage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, candidates, edges, updated_at FROM repo_coverage WHERE repo_id = ? ORDER BY kind`, repoID)
	if err != nil {
		return nil, fmt.Errorf("get coverage: %w", err)
	}
	defer rows.Close()

	cov := &storage.RepoCoverage{RepoID: repoID, Kinds: map[string]storage.CoverageCounts{}}
	for rows.Next() {
		var kind string
		var counts storage.CoverageCounts
		var updatedAt int64
		if err := rows.Scan(&kind, &counts.Candidates, &counts.Edges, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan coverage: %w", err)
		}
		cov.Kinds[kind] = counts
		if updatedAt > cov.UpdatedAt {
			cov.UpdatedAt = updatedAt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get coverage: %w", err)
	}
	if len(cov.Kinds) == 0 {
		return nil, storage.ErrNotFound
	}
	return cov, nil
}

// DeleteRepoCoverage drops a repo's coverage summary.
func (s *SQLite) DeleteRepoCoverage(ctx context.Context, repoID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM repo_coverage WHERE repo_id = ?`, repoID); err != nil {
		return fmt.Errorf("delete coverage: %w", err)
	}
	return nil
}
