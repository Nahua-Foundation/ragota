package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/indexing"
	"github.com/Nahua-Foundation/ragota/internal/repos"
)

// compactableIndexer is a mockIndexer that also settles a storage layout, as
// the BM25 indexer does.
type compactableIndexer struct {
	mockIndexer
	compacted atomic.Int32
	err       error
}

func (c *compactableIndexer) Compact(context.Context) error {
	c.compacted.Add(1)
	return c.err
}

func newCompactable(typ indexing.IndexType) *compactableIndexer {
	return &compactableIndexer{mockIndexer: mockIndexer{name: string(typ), indexType: typ}}
}

// A full pass has to settle the layout before anything queries it, or two
// builds of the same sources score them differently. See
// internal/indexing/bm25 (Compact) for what the layout does to a score.
func TestDoIndexCompacts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "good.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	keyword := newCompactable(indexing.IndexTypeBM25)
	plain := &mockIndexer{name: "vector", indexType: indexing.IndexTypeVector}
	svc := newLocalTestService(t, &mockStorage{}, map[indexing.IndexType]indexing.Indexer{
		indexing.IndexTypeBM25:   keyword,
		indexing.IndexTypeVector: plain,
	})
	repo := &repos.Repo{ID: "r1", Name: "t", Source: repos.SourceTypeLocal, Path: dir}

	if err := svc.doIndex(context.Background(), repo, false); err != nil {
		t.Fatalf("doIndex() error = %v", err)
	}
	if got := keyword.compacted.Load(); got != 1 {
		t.Errorf("compacted %d times, want 1", got)
	}
}

// A pass that indexed nothing wrote nothing, so there is no layout to settle
// and no reason to pay for a rewrite.
func TestDoIndexSkipsCompactionWhenNothingWasIndexed(t *testing.T) {
	keyword := newCompactable(indexing.IndexTypeBM25)
	svc := newLocalTestService(t, &mockStorage{}, map[indexing.IndexType]indexing.Indexer{
		indexing.IndexTypeBM25: keyword,
	})
	repo := &repos.Repo{ID: "r1", Name: "t", Source: repos.SourceTypeLocal, Path: t.TempDir()}

	if err := svc.doIndex(context.Background(), repo, false); err != nil {
		t.Fatalf("doIndex() error = %v", err)
	}
	if got := keyword.compacted.Load(); got != 0 {
		t.Errorf("compacted %d times over an empty repository, want 0", got)
	}
}

// Compaction is an optimisation of reproducibility, not of correctness: an
// index that would not compact is still a correct index, so the pass that
// produced it must not fail, and the other indexers must still be asked.
func TestCompactIndexesSurvivesAFailure(t *testing.T) {
	broken := newCompactable(indexing.IndexTypeBM25)
	broken.err = errors.New("force merge already in progress")
	other := newCompactable(indexing.IndexTypeAST)
	svc := newTestService(&mockStorage{}, map[indexing.IndexType]indexing.Indexer{
		indexing.IndexTypeBM25: broken,
		indexing.IndexTypeAST:  other,
	})

	svc.compactIndexes(context.Background())

	if got := broken.compacted.Load(); got != 1 {
		t.Errorf("the failing indexer was compacted %d times, want 1", got)
	}
	if got := other.compacted.Load(); got != 1 {
		t.Errorf("a failure on one indexer skipped another: compacted %d times, want 1", got)
	}
}

// An indexer with no layout to settle is passed over rather than asked and
// failed: most indexers do not implement Compactor and never will.
func TestCompactIndexesSkipsIndexersThatCannot(t *testing.T) {
	plain := &mockIndexer{name: "vector", indexType: indexing.IndexTypeVector}
	keyword := newCompactable(indexing.IndexTypeBM25)
	svc := newTestService(&mockStorage{}, map[indexing.IndexType]indexing.Indexer{
		indexing.IndexTypeVector: plain,
		indexing.IndexTypeBM25:   keyword,
	})

	svc.compactIndexes(context.Background())

	if got := keyword.compacted.Load(); got != 1 {
		t.Errorf("the compactable indexer was compacted %d times, want 1", got)
	}
}
