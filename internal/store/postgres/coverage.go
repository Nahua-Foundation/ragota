package postgres

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
//
// The statements are written here rather than in queries.sql because the
// summary is a whole-row replacement of a variable set of kinds, which sqlc's
// static queries do not express.
func (p *Postgres) StoreRepoCoverage(ctx context.Context, c *domain.RepoCoverage) error {
	if c == nil || c.RepoID == "" {
		return fmt.Errorf("store coverage: repo id is required")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store coverage: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM repo_coverage WHERE repo_id = $1`, c.RepoID); err != nil {
		return fmt.Errorf("store coverage: %w", err)
	}
	for kind, counts := range c.Kinds {
		_, err := tx.Exec(ctx,
			`INSERT INTO repo_coverage (repo_id, kind, candidates, edges, updated_at)
			 VALUES ($1, $2, $3, $4, $5)`,
			c.RepoID, kind, int64(counts.Candidates), int64(counts.Edges), c.UpdatedAt)
		if err != nil {
			return fmt.Errorf("store coverage %s: %w", kind, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store coverage: %w", err)
	}
	return nil
}

// GetRepoCoverage returns a repo's coverage summary.
func (p *Postgres) GetRepoCoverage(ctx context.Context, repoID string) (*domain.RepoCoverage, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT kind, candidates, edges, updated_at FROM repo_coverage WHERE repo_id = $1 ORDER BY kind`, repoID)
	if err != nil {
		return nil, fmt.Errorf("get coverage: %w", err)
	}
	defer rows.Close()

	cov := &domain.RepoCoverage{RepoID: repoID, Kinds: map[string]domain.CoverageCounts{}}
	for rows.Next() {
		var kind string
		var candidates, edges, updatedAt int64
		if err := rows.Scan(&kind, &candidates, &edges, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan coverage: %w", err)
		}
		cov.Kinds[kind] = domain.CoverageCounts{Candidates: int(candidates), Edges: int(edges)}
		if updatedAt > cov.UpdatedAt {
			cov.UpdatedAt = updatedAt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get coverage: %w", err)
	}
	if len(cov.Kinds) == 0 {
		return nil, store.ErrNotFound
	}
	return cov, nil
}

// DeleteRepoCoverage drops a repo's coverage summary.
func (p *Postgres) DeleteRepoCoverage(ctx context.Context, repoID string) error {
	if _, err := p.pool.Exec(ctx, `DELETE FROM repo_coverage WHERE repo_id = $1`, repoID); err != nil {
		return fmt.Errorf("delete coverage: %w", err)
	}
	return nil
}
