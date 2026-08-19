package search

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/index"
)

// mockSearcher implements index.Searcher for testing.
type mockSearcher struct {
	name  index.IndexType
	hits  []*index.Hit
	total int
}

func (m *mockSearcher) Search(ctx context.Context, q *index.SearchQuery) (*index.SearchResult, error) {
	return &index.SearchResult{
		Hits:  m.hits,
		Total: m.total,
		Query: q.Query,
	}, nil
}

func hitKey(repoID, filePath string, line int) string {
	return (&index.Hit{RepoID: repoID, FilePath: filePath, Line: line}).Key()
}

func TestRRFFormula(t *testing.T) {
	ctx := context.Background()

	vectorA := &index.Hit{RepoID: "r1", FilePath: "/a.go", Line: 1, Score: 0.9}
	vectorB := &index.Hit{RepoID: "r1", FilePath: "/b.go", Line: 5, Score: 0.8}
	bm25B := &index.Hit{RepoID: "r1", FilePath: "/b.go", Line: 5, Score: 0.7}
	bm25C := &index.Hit{RepoID: "r1", FilePath: "/c.go", Line: 10, Score: 0.6}

	vector := &mockSearcher{
		name: index.IndexTypeVector,
		hits: []*index.Hit{vectorA, vectorB},
	}

	bm25 := &mockSearcher{
		name: index.IndexTypeBM25,
		hits: []*index.Hit{bm25B, bm25C},
	}

	svc := New(map[index.IndexType]index.Searcher{
		index.IndexTypeVector: vector,
		index.IndexTypeBM25:   bm25,
	}, nil)

	result, err := svc.Search(ctx, &index.SearchQuery{Query: "test", Limit: 10}, true)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	if len(result.Hits) != 3 {
		t.Fatalf("expected 3 hits, got %d", len(result.Hits))
	}

	if result.Hits[0].Key() != hitKey("r1", "/b.go", 5) {
		t.Errorf("expected first hit to be B, got %s", result.Hits[0].Key())
	}

	if result.Hits[1].Key() != hitKey("r1", "/a.go", 1) {
		t.Errorf("expected second hit to be A, got %s", result.Hits[1].Key())
	}

	if result.Hits[2].Key() != hitKey("r1", "/c.go", 10) {
		t.Errorf("expected third hit to be C, got %s", result.Hits[2].Key())
	}

	// Both sources carry weight 1.0: RRF interleaves by rank alone (see
	// DefaultConfig for why a skewed default breaks interleaving).
	const tol = 1e-5
	if want := 1.0/62.0 + 1.0/61.0; math.Abs(float64(result.Hits[0].Score)-want) > tol {
		t.Errorf("B score = %f, want ≈ %f", result.Hits[0].Score, want)
	}
	if math.Abs(float64(result.Hits[1].Score)-1.0/61.0) > tol {
		t.Errorf("A score = %f, want ≈ %f", result.Hits[1].Score, 1.0/61.0)
	}
	if math.Abs(float64(result.Hits[2].Score)-1.0/62.0) > tol {
		t.Errorf("C score = %f, want ≈ %f", result.Hits[2].Score, 1.0/62.0)
	}
}

func TestRRFDedup(t *testing.T) {
	ctx := context.Background()

	sameHit := &index.Hit{RepoID: "r1", FilePath: "/same.go", Line: 1, Score: 0.5}

	vector := &mockSearcher{
		name: index.IndexTypeVector,
		hits: []*index.Hit{sameHit},
	}

	bm25 := &mockSearcher{
		name: index.IndexTypeBM25,
		hits: []*index.Hit{sameHit},
	}

	svc := New(map[index.IndexType]index.Searcher{
		index.IndexTypeVector: vector,
		index.IndexTypeBM25:   bm25,
	}, nil)

	result, err := svc.Search(ctx, &index.SearchQuery{Query: "test", Limit: 10}, true)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	if len(result.Hits) != 1 {
		t.Fatalf("expected 1 deduplicated hit, got %d", len(result.Hits))
	}

	if result.Hits[0].Key() != hitKey("r1", "/same.go", 1) {
		t.Errorf("expected hit key 'r1:/same.go:1', got %s", result.Hits[0].Key())
	}

	expectedScore := float32(2.0 / 61.0)
	if math.Abs(float64(result.Hits[0].Score-expectedScore)) > 1e-5 {
		t.Errorf("score = %f, want ≈ %f", result.Hits[0].Score, expectedScore)
	}
}

func TestRRFTieBreak(t *testing.T) {
	ctx := context.Background()

	hits := make([]*index.Hit, 5)
	for i := 0; i < 5; i++ {
		hits[i] = &index.Hit{
			Score:    0.5,
			RepoID:   "r1",
			FilePath: "/f" + string(rune('a'+i)) + ".go",
			Line:     1,
		}
	}

	svc := New(map[index.IndexType]index.Searcher{
		index.IndexTypeVector: &mockSearcher{name: index.IndexTypeVector, hits: hits},
		index.IndexTypeBM25:   &mockSearcher{name: index.IndexTypeBM25, hits: hits},
	}, nil)

	var lastOrder []string
	for run := 0; run < 20; run++ {
		result, err := svc.Search(ctx, &index.SearchQuery{Query: "test", Limit: 10}, true)
		if err != nil {
			t.Fatalf("Search() error = %v", err)
		}

		order := make([]string, len(result.Hits))
		for i, h := range result.Hits {
			order[i] = h.Key()
		}

		if run > 0 {
			for i := range order {
				if order[i] != lastOrder[i] {
					t.Errorf("run %d: ordering differs from run 0 at index %d", run, i)
					break
				}
			}
		} else {
			lastOrder = order
		}
	}
}

func TestRRFLimit(t *testing.T) {
	ctx := context.Background()

	hits := []*index.Hit{
		{RepoID: "r1", FilePath: "/a.go", Line: 1, Score: 0.9},
		{RepoID: "r1", FilePath: "/b.go", Line: 5, Score: 0.8},
		{RepoID: "r1", FilePath: "/c.go", Line: 10, Score: 0.7},
	}

	svc := New(map[index.IndexType]index.Searcher{
		index.IndexTypeVector: &mockSearcher{name: index.IndexTypeVector, hits: hits},
		index.IndexTypeBM25:   &mockSearcher{name: index.IndexTypeBM25, hits: hits},
	}, nil)

	result, err := svc.Search(ctx, &index.SearchQuery{Query: "test", Limit: 2}, true)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	if len(result.Hits) != 2 {
		t.Fatalf("expected exactly 2 hits with limit=2, got %d", len(result.Hits))
	}

	if result.Hits[0].Key() != hitKey("r1", "/a.go", 1) {
		t.Errorf("expected first hit to be A, got %s", result.Hits[0].Key())
	}

	if result.Hits[1].Key() != hitKey("r1", "/b.go", 5) {
		t.Errorf("expected second hit to be B, got %s", result.Hits[1].Key())
	}
}

func TestRRFSingleSource(t *testing.T) {
	ctx := context.Background()

	hits := []*index.Hit{
		{RepoID: "r1", FilePath: "/a.go", Line: 1, Score: 0.9},
		{RepoID: "r1", FilePath: "/b.go", Line: 5, Score: 0.8},
	}

	svc := New(map[index.IndexType]index.Searcher{
		index.IndexTypeBM25: &mockSearcher{name: index.IndexTypeBM25, hits: hits},
	}, nil)

	result, err := svc.Search(ctx, &index.SearchQuery{Query: "test", Limit: 10}, true)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	if len(result.Hits) != 2 {
		t.Fatalf("expected 2 hits from single source, got %d", len(result.Hits))
	}

	if result.Hits[0].Key() != hitKey("r1", "/a.go", 1) {
		t.Errorf("expected first hit to be A, got %s", result.Hits[0].Key())
	}
}

func TestRRFEmpty(t *testing.T) {
	ctx := context.Background()

	svc := New(map[index.IndexType]index.Searcher{}, nil)

	result, err := svc.Search(ctx, &index.SearchQuery{Query: "test", Limit: 10}, true)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	if len(result.Hits) != 0 {
		t.Errorf("expected empty result with no searchers, got %d hits", len(result.Hits))
	}

	if result.Total != 0 {
		t.Errorf("expected total=0, got %d", result.Total)
	}
}

func TestSemanticSearch(t *testing.T) {
	ctx := context.Background()

	expectedHits := []*index.Hit{
		{RepoID: "r1", FilePath: "/a.go", Line: 1, Score: 0.9},
	}

	svc := New(map[index.IndexType]index.Searcher{
		index.IndexTypeVector: &mockSearcher{name: index.IndexTypeVector, hits: expectedHits},
		index.IndexTypeBM25:   &mockSearcher{name: index.IndexTypeBM25, hits: nil},
	}, nil)

	result, err := svc.SemanticSearch(ctx, &index.SearchQuery{Query: "test"})
	if err != nil {
		t.Fatalf("SemanticSearch() error = %v", err)
	}

	if len(result.Hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(result.Hits))
	}

	if result.Hits[0].Key() != hitKey("r1", "/a.go", 1) {
		t.Errorf("expected hit A, got %s", result.Hits[0].Key())
	}
}

func TestKeywordSearch(t *testing.T) {
	ctx := context.Background()

	expectedHits := []*index.Hit{
		{RepoID: "r1", FilePath: "/b.go", Line: 5, Score: 0.7},
	}

	svc := New(map[index.IndexType]index.Searcher{
		index.IndexTypeVector: &mockSearcher{name: index.IndexTypeVector, hits: nil},
		index.IndexTypeBM25:   &mockSearcher{name: index.IndexTypeBM25, hits: expectedHits},
	}, nil)

	result, err := svc.KeywordSearch(ctx, &index.SearchQuery{Query: "test"})
	if err != nil {
		t.Fatalf("KeywordSearch() error = %v", err)
	}

	if len(result.Hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(result.Hits))
	}

	if result.Hits[0].Key() != hitKey("r1", "/b.go", 5) {
		t.Errorf("expected hit B, got %s", result.Hits[0].Key())
	}
}

// TestPrimaryFailure keeps a damaged index visible when every searcher failed.
// Failures are collected in searchOrder, so a searcher that failed for some
// ordinary reason can be first; reporting that one would drop the damaged
// index out of the error chain and cost the caller the one status it can act on.
func TestPrimaryFailure(t *testing.T) {
	damaged := fmt.Errorf("search: %w", index.ErrIndexDamaged)
	ordinary := errors.New("context canceled")

	tests := []struct {
		name     string
		failures []searcherFailure
		want     error
	}{
		{
			name: "damaged wins over an ordinary failure collected first",
			failures: []searcherFailure{
				{source: index.IndexTypeVector, err: ordinary},
				{source: index.IndexTypeBM25, err: damaged},
			},
			want: damaged,
		},
		{
			name:     "the only failure is reported as is",
			failures: []searcherFailure{{source: index.IndexTypeBM25, err: ordinary}},
			want:     ordinary,
		},
		{
			name: "no damage falls back to the first failure",
			failures: []searcherFailure{
				{source: index.IndexTypeVector, err: ordinary},
				{source: index.IndexTypeBM25, err: errors.New("dial tcp: refused")},
			},
			want: ordinary,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := primaryFailure(tt.failures); got != tt.want {
				t.Errorf("primaryFailure() = %v, want %v", got, tt.want)
			}
		})
	}
}
