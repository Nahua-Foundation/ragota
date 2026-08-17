package lsp

import (
	"context"
	"errors"
	"log/slog"

	"github.com/Nahua-Foundation/ragota/internal/indexing"
)

// Compile-time interface assertion.
var _ indexing.Indexer = (*chained)(nil)

// Chain composes two indexers so that refiner always runs after primary
// within one Index call. The service iterates its indexer map in random
// order, so registering the LSP refiner as a separate map entry would let it
// run before the AST indexer (whose per-file cleanup would then discard the
// refinement); chaining under the primary's slot guarantees ordering.
//
// The refiner is best-effort: its errors are logged and folded into the
// result but never fail the indexing run.
func Chain(primary, refiner indexing.Indexer) indexing.Indexer {
	return &chained{primary: primary, refiner: refiner}
}

type chained struct {
	primary indexing.Indexer
	refiner indexing.Indexer
}

func (c *chained) Name() string { return c.primary.Name() + "+" + c.refiner.Name() }

func (c *chained) Type() indexing.IndexType { return c.primary.Type() }

func (c *chained) Init(ctx context.Context, config map[string]interface{}) error {
	if err := c.primary.Init(ctx, config); err != nil {
		return err
	}
	return c.refiner.Init(ctx, config)
}

func (c *chained) Index(ctx context.Context, req *indexing.IndexRequest) (*indexing.IndexResult, error) {
	res, err := c.primary.Index(ctx, req)
	if err != nil {
		return res, err
	}
	refRes, refErr := c.refiner.Index(ctx, req)
	if refErr != nil {
		slog.Warn("lsp: refinement pass failed", "repo", req.RepoID, "error", refErr)
		return res, nil
	}
	if refRes != nil {
		res.FilesFailed += refRes.FilesFailed
		res.Errors = append(res.Errors, refRes.Errors...)
	}
	return res, nil
}

func (c *chained) Remove(ctx context.Context, repoID string, paths []string) error {
	return errors.Join(
		c.primary.Remove(ctx, repoID, paths),
		c.refiner.Remove(ctx, repoID, paths),
	)
}

func (c *chained) Stats(ctx context.Context) (*indexing.IndexerStats, error) {
	return c.primary.Stats(ctx)
}

func (c *chained) Close() error {
	return errors.Join(c.primary.Close(), c.refiner.Close())
}
