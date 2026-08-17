package search

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/indexing"
)

// fakeReranker inverts the candidate order: the last document gets the
// highest score. err, when set, is returned instead.
type fakeReranker struct {
	err  error
	seen []string
}

func (f *fakeReranker) Name() string { return "fake" }

func (f *fakeReranker) Rerank(ctx context.Context, query string, documents []string) ([]float64, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.seen = documents
	scores := make([]float64, len(documents))
	for i := range documents {
		scores[i] = float64(i) // ascending → reversed order after sort
	}
	return scores, nil
}

func rerankHits() []*indexing.Hit {
	return []*indexing.Hit{
		{RepoID: "r1", FilePath: "/a.go", Path: "/a.go", Line: 1, Score: 0.9, Snippet: "snippet a", Reason: "keyword"},
		{RepoID: "r1", FilePath: "/b.go", Path: "/b.go", Line: 5, Score: 0.8, Snippet: "snippet b", Reason: "keyword"},
		{RepoID: "r1", FilePath: "/c.go", Path: "/c.go", Line: 9, Symbol: "SymC", Score: 0.7, Reason: "keyword"},
	}
}

func TestRerankReordersTopN(t *testing.T) {
	ctx := context.Background()
	svc := New(map[indexing.IndexType]indexing.Searcher{
		indexing.IndexTypeBM25: &mockSearcher{name: indexing.IndexTypeBM25, hits: rerankHits()},
	}, nil)
	rr := &fakeReranker{}
	svc.SetReranker(rr, 0) // default topN

	result, err := svc.KeywordSearch(ctx, &indexing.SearchQuery{Query: "test", Limit: 10})
	if err != nil {
		t.Fatalf("KeywordSearch() error = %v", err)
	}
	if len(result.Hits) != 3 {
		t.Fatalf("expected 3 hits, got %d", len(result.Hits))
	}

	// Inverted order: c, b, a.
	wantOrder := []string{"/c.go", "/b.go", "/a.go"}
	for i, want := range wantOrder {
		if result.Hits[i].FilePath != want {
			t.Errorf("hit[%d] = %s, want %s", i, result.Hits[i].FilePath, want)
		}
	}

	// Rerank scores are blended into the fusion range rather than served raw,
	// so a client sorting by score reproduces the served order.
	for i := 1; i < len(result.Hits); i++ {
		if result.Hits[i-1].Score < result.Hits[i].Score {
			t.Errorf("scores not monotone with the served order: %v > %v at %d",
				result.Hits[i].Score, result.Hits[i-1].Score, i)
		}
	}
	if result.Hits[0].Score > 0.9 || result.Hits[0].Score < 0.7 {
		t.Errorf("top hit score = %f, want it inside the candidates' score range [0.7, 0.9]",
			result.Hits[0].Score)
	}
	for i, h := range result.Hits {
		if !strings.Contains(h.Reason, "rerank") {
			t.Errorf("hit[%d].Reason = %q, want it to contain rerank", i, h.Reason)
		}
	}

	// Documents: snippet, with Path+Symbol fallback for the snippet-less hit.
	if len(rr.seen) != 3 {
		t.Fatalf("reranker saw %d documents, want 3", len(rr.seen))
	}
	if rr.seen[0] != "snippet a" || rr.seen[2] != "SymC /c.go" {
		t.Errorf("reranker documents = %v", rr.seen)
	}
}

func TestRerankTopNKeepsTail(t *testing.T) {
	ctx := context.Background()
	svc := New(map[indexing.IndexType]indexing.Searcher{
		indexing.IndexTypeBM25: &mockSearcher{name: indexing.IndexTypeBM25, hits: rerankHits()},
	}, nil)
	svc.SetReranker(&fakeReranker{}, 2)

	result, err := svc.KeywordSearch(ctx, &indexing.SearchQuery{Query: "test", Limit: 10})
	if err != nil {
		t.Fatalf("KeywordSearch() error = %v", err)
	}
	// First two reranked (inverted), tail untouched.
	wantOrder := []string{"/b.go", "/a.go", "/c.go"}
	for i, want := range wantOrder {
		if result.Hits[i].FilePath != want {
			t.Errorf("hit[%d] = %s, want %s", i, result.Hits[i].FilePath, want)
		}
	}
	if strings.Contains(result.Hits[2].Reason, "rerank") {
		t.Errorf("tail hit must not be marked reranked, got Reason=%q", result.Hits[2].Reason)
	}
	if result.Hits[2].Score != 0.7 {
		t.Errorf("tail hit score = %f, want original 0.7", result.Hits[2].Score)
	}
}

func TestRerankErrorFallsBack(t *testing.T) {
	ctx := context.Background()
	svc := New(map[indexing.IndexType]indexing.Searcher{
		indexing.IndexTypeBM25: &mockSearcher{name: indexing.IndexTypeBM25, hits: rerankHits()},
	}, nil)
	svc.SetReranker(&fakeReranker{err: fmt.Errorf("upstream down")}, 50)

	result, err := svc.KeywordSearch(ctx, &indexing.SearchQuery{Query: "test", Limit: 10})
	if err != nil {
		t.Fatalf("KeywordSearch() must not fail when the reranker fails, got %v", err)
	}
	wantOrder := []string{"/a.go", "/b.go", "/c.go"}
	for i, want := range wantOrder {
		if result.Hits[i].FilePath != want {
			t.Errorf("hit[%d] = %s, want original order %v", i, result.Hits[i].FilePath, wantOrder)
		}
		if strings.Contains(result.Hits[i].Reason, "rerank") {
			t.Errorf("hit[%d] marked reranked despite failure", i)
		}
	}
}

func TestRerankAppliedToHybridFusion(t *testing.T) {
	ctx := context.Background()
	svc := New(map[indexing.IndexType]indexing.Searcher{
		indexing.IndexTypeVector: &mockSearcher{name: indexing.IndexTypeVector, hits: rerankHits()},
		indexing.IndexTypeBM25:   &mockSearcher{name: indexing.IndexTypeBM25, hits: rerankHits()},
	}, nil)
	svc.SetReranker(&fakeReranker{}, 50)

	result, err := svc.Search(ctx, &indexing.SearchQuery{Query: "test", Limit: 10}, true)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(result.Hits) != 3 {
		t.Fatalf("expected 3 fused hits, got %d", len(result.Hits))
	}
	// RRF ranks a, b, c; the inverting reranker flips that.
	wantOrder := []string{"/c.go", "/b.go", "/a.go"}
	for i, want := range wantOrder {
		if result.Hits[i].FilePath != want {
			t.Errorf("hit[%d] = %s, want %s", i, result.Hits[i].FilePath, want)
		}
	}
}

func TestRerankSingleHitSkipped(t *testing.T) {
	ctx := context.Background()
	one := []*indexing.Hit{{RepoID: "r1", FilePath: "/a.go", Line: 1, Score: 0.9, Reason: "keyword"}}
	svc := New(map[indexing.IndexType]indexing.Searcher{
		indexing.IndexTypeBM25: &mockSearcher{name: indexing.IndexTypeBM25, hits: one},
	}, nil)
	rr := &fakeReranker{}
	svc.SetReranker(rr, 50)

	result, err := svc.KeywordSearch(ctx, &indexing.SearchQuery{Query: "test", Limit: 10})
	if err != nil {
		t.Fatalf("KeywordSearch() error = %v", err)
	}
	if len(result.Hits) != 1 || result.Hits[0].Reason != "keyword" {
		t.Errorf("single hit must pass through unchanged, got %+v", result.Hits)
	}
	if rr.seen != nil {
		t.Errorf("reranker must not be called for a single hit")
	}
}
