package sqlite

import (
	"context"
	"fmt"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// StoreRepoCoverage replaces a repo's contract coverage summary. The delete
// and the inserts share one transaction: a half-written summary would read as
// a repo whose HTTP calls are covered and whose RPC calls were never looked
// at.
func (s *SQLite) StoreRepoCoverage(ctx context.Context, c *domain.RepoCoverage) error {
	if c == nil || c.RepoID == "" {
		return fmt.Errorf("store coverage: repo id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store coverage: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	q := s.q.WithTx(tx)
	if err := q.DeleteRepoCoverage(ctx, c.RepoID); err != nil {
		return fmt.Errorf("store coverage: %w", err)
	}
	for kind, counts := range c.Kinds {
		if err := q.InsertRepoCoverage(ctx, InsertRepoCoverageParams{
			RepoID: c.RepoID, Kind: kind, Candidates: int64(counts.Candidates),
			Edges: int64(counts.Edges), UpdatedAt: c.UpdatedAt,
		}); err != nil {
			return fmt.Errorf("store coverage %s: %w", kind, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store coverage: %w", err)
	}
	return nil
}

// GetRepoCoverage returns a repo's coverage summary.
func (s *SQLite) GetRepoCoverage(ctx context.Context, repoID string) (*domain.RepoCoverage, error) {
	rows, err := s.q.GetRepoCoverage(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("get coverage: %w", err)
	}
	cov := &domain.RepoCoverage{RepoID: repoID, Kinds: map[string]domain.CoverageCounts{}}
	for _, r := range rows {
		cov.Kinds[r.Kind] = domain.CoverageCounts{Candidates: int(r.Candidates), Edges: int(r.Edges)}
		if r.UpdatedAt > cov.UpdatedAt {
			cov.UpdatedAt = r.UpdatedAt
		}
	}
	if len(cov.Kinds) == 0 {
		return nil, store.ErrNotFound
	}
	return cov, nil
}

// DeleteRepoCoverage drops a repo's coverage summary.
func (s *SQLite) DeleteRepoCoverage(ctx context.Context, repoID string) error {
	if err := s.q.DeleteRepoCoverage(ctx, repoID); err != nil {
		return fmt.Errorf("delete coverage: %w", err)
	}
	return nil
}
