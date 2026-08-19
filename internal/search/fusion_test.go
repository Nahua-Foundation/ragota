package search

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/index"
)

// recordingSearcher remembers the query it was asked and can fail or stall.
type recordingSearcher struct {
	hits    []*index.Hit
	err     error
	delay   time.Duration
	mu      sync.Mutex
	gotLimt int
	calls   int
}

func (r *recordingSearcher) Search(ctx context.Context, q *index.SearchQuery) (*index.SearchResult, error) {
	if r.delay > 0 {
		select {
		case <-time.After(r.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	r.mu.Lock()
	r.gotLimt = q.Limit
	r.calls++
	r.mu.Unlock()

	if r.err != nil {
		return nil, r.err
	}
	hits := r.hits
	if q.Limit > 0 && len(hits) > q.Limit {
		hits = hits[:q.Limit]
	}
	return &index.SearchResult{Hits: hits, Total: len(hits), Query: q.Query}, nil
}

func (r *recordingSearcher) limit() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gotLimt
}

// countingReranker scores documents by their position, highest last, and
// records how many documents it was given.
type countingReranker struct {
	docs int
}

func (c *countingReranker) Name() string { return "counting" }

func (c *countingReranker) Rerank(ctx context.Context, query string, documents []string) ([]float64, error) {
	c.docs = len(documents)
	scores := make([]float64, len(documents))
	for i := range documents {
		// Negative, logit-style scores: the last document wins.
		scores[i] = float64(i) - float64(len(documents))
	}
	return scores, nil
}

func manyHits(n int) []*index.Hit {
	hits := make([]*index.Hit, n)
	for i := range hits {
		hits[i] = &index.Hit{
			RepoID:   "r1",
			FilePath: fmt.Sprintf("/f%02d.go", i),
			Path:     fmt.Sprintf("/f%02d.go", i),
			Line:     1,
			EndLine:  10,
			Score:    float32(n - i),
			Snippet:  fmt.Sprintf("snippet %02d", i),
			Reason:   "keyword",
		}
	}
	return hits
}

// TestRerankSeesMoreCandidatesThanLimit pins the top_n fix: fusion used to
// truncate to the caller's limit before reranking, so with limit=3 the
// reranker only ever saw 3 documents and could not change what was returned.
func TestRerankSeesMoreCandidatesThanLimit(t *testing.T) {
	ctx := context.Background()

	for _, mode := range []string{"hybrid", "keyword", "semantic"} {
		t.Run(mode, func(t *testing.T) {
			srch := &recordingSearcher{hits: manyHits(20)}
			searchers := map[index.IndexType]index.Searcher{}
			switch mode {
			case "keyword":
				searchers[index.IndexTypeBM25] = srch
			case "semantic":
				searchers[index.IndexTypeVector] = srch
			default:
				searchers[index.IndexTypeBM25] = srch
			}

			svc := New(searchers, nil)
			rr := &countingReranker{}
			svc.SetReranker(rr, 10)

			query := &index.SearchQuery{Query: "test", Limit: 3}
			var (
				result *index.SearchResult
				err    error
			)
			switch mode {
			case "keyword":
				result, err = svc.KeywordSearch(ctx, query)
			case "semantic":
				result, err = svc.SemanticSearch(ctx, query)
			default:
				result, err = svc.Search(ctx, query, true)
			}
			if err != nil {
				t.Fatalf("search error = %v", err)
			}

			if got := srch.limit(); got != 10 {
				t.Errorf("searcher asked for limit %d, want the rerank window 10", got)
			}
			if rr.docs != 10 {
				t.Errorf("reranker saw %d documents, want 10", rr.docs)
			}
			if len(result.Hits) != 3 {
				t.Fatalf("returned %d hits, want the caller's limit 3", len(result.Hits))
			}
			// The inverting reranker puts the last candidate first, so a hit
			// that fusion ranked outside the top 3 must now be returned.
			if result.Hits[0].FilePath != "/f09.go" {
				t.Errorf("top hit = %s, want /f09.go (reranked from outside the limit)",
					result.Hits[0].FilePath)
			}
		})
	}
}

func TestRerankWindowNotAppliedWithoutReranker(t *testing.T) {
	srch := &recordingSearcher{hits: manyHits(20)}
	svc := New(map[index.IndexType]index.Searcher{index.IndexTypeBM25: srch}, nil)

	if _, err := svc.KeywordSearch(context.Background(), &index.SearchQuery{Query: "q", Limit: 3}); err != nil {
		t.Fatalf("KeywordSearch() error = %v", err)
	}
	if got := srch.limit(); got != 3 {
		t.Errorf("searcher asked for limit %d, want 3 when no reranker is configured", got)
	}
}

// TestFuseMergesOverlappingRegions covers indexes that chunk differently:
// BM25 emits 60-line windows while the vector index emits symbol cards, so the
// same code arrives under different line ranges and used to be counted twice.
func TestFuseMergesOverlappingRegions(t *testing.T) {
	ctx := context.Background()

	card := &index.Hit{RepoID: "r1", FilePath: "/a.go", Line: 55, EndLine: 58, Reason: "semantic"}
	window := &index.Hit{RepoID: "r1", FilePath: "/a.go", Line: 1, EndLine: 60, Reason: "keyword"}
	other := &index.Hit{RepoID: "r1", FilePath: "/b.go", Line: 1, EndLine: 60, Reason: "keyword"}

	svc := New(map[index.IndexType]index.Searcher{
		index.IndexTypeVector: &mockSearcher{hits: []*index.Hit{card}},
		index.IndexTypeBM25:   &mockSearcher{hits: []*index.Hit{window, other}},
	}, nil)

	result, err := svc.Search(ctx, &index.SearchQuery{Query: "test", Limit: 10}, true)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(result.Hits) != 2 {
		for _, h := range result.Hits {
			t.Logf("hit %s:%d-%d reason=%s", h.FilePath, h.Line, h.EndLine, h.Reason)
		}
		t.Fatalf("got %d hits, want 2 (the overlapping pair merged)", len(result.Hits))
	}

	top := result.Hits[0]
	if top.FilePath != "/a.go" {
		t.Fatalf("top hit = %s, want the merged /a.go", top.FilePath)
	}
	// Both retrievers agreed on /a.go, so it must outscore the single-source hit.
	if top.Score <= result.Hits[1].Score {
		t.Errorf("merged hit score %v must exceed the single-source hit %v",
			top.Score, result.Hits[1].Score)
	}
	// Provenance: a hit found by both indexes reports both.
	if !strings.Contains(top.Reason, "semantic") || !strings.Contains(top.Reason, "keyword") {
		t.Errorf("Reason = %q, want both contributing sources", top.Reason)
	}
	// The higher-ranked hit (rank 0 in both) stays the representative; the
	// vector card is fused first, so its range is kept.
	if top.Line != 55 || top.EndLine != 58 {
		t.Errorf("representative range = %d-%d, want the higher-ranked 55-58", top.Line, top.EndLine)
	}
}

// TestFuseKeepsOverlappingChunksOfOneSearcher guards against over-merging:
// window chunks of one file overlap by design, and collapsing them would turn
// a whole file into a single hit.
func TestFuseKeepsOverlappingChunksOfOneSearcher(t *testing.T) {
	hits := []*index.Hit{
		{RepoID: "r1", FilePath: "/a.go", Line: 1, EndLine: 60, Reason: "keyword"},
		{RepoID: "r1", FilePath: "/a.go", Line: 51, EndLine: 110, Reason: "keyword"},
		{RepoID: "r1", FilePath: "/a.go", Line: 101, EndLine: 160, Reason: "keyword"},
	}
	svc := New(map[index.IndexType]index.Searcher{
		index.IndexTypeBM25: &mockSearcher{hits: hits},
	}, nil)

	result, err := svc.Search(context.Background(), &index.SearchQuery{Query: "test", Limit: 10}, true)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(result.Hits) != 3 {
		t.Errorf("got %d hits, want the 3 overlapping windows kept apart", len(result.Hits))
	}
}

// TestSingleSourceReasonKeepsIndexerLabel checks provenance is not lost when
// only one index contributes.
func TestSingleSourceReasonKeepsIndexerLabel(t *testing.T) {
	svc := New(map[index.IndexType]index.Searcher{
		index.IndexTypeVector: &mockSearcher{hits: []*index.Hit{
			{RepoID: "r1", FilePath: "/a.go", Line: 1, EndLine: 4, Reason: "semantic"},
		}},
	}, nil)

	result, err := svc.Search(context.Background(), &index.SearchQuery{Query: "test", Limit: 10}, true)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if result.Hits[0].Reason != "semantic" {
		t.Errorf("Reason = %q, want semantic", result.Hits[0].Reason)
	}
}

// TestSearchDegradesWithOneFailingSearcher: a failed searcher used to be
// dropped silently along with its error, so a half-empty result looked normal.
func TestSearchDegradesWithOneFailingSearcher(t *testing.T) {
	hits := []*index.Hit{{RepoID: "r1", FilePath: "/a.go", Line: 1, Reason: "keyword"}}
	svc := New(map[index.IndexType]index.Searcher{
		index.IndexTypeVector: &recordingSearcher{err: fmt.Errorf("embedder down")},
		index.IndexTypeBM25:   &recordingSearcher{hits: hits},
	}, nil)

	result, err := svc.Search(context.Background(), &index.SearchQuery{Query: "test", Limit: 10}, true)
	if err != nil {
		t.Fatalf("Search() must succeed while one searcher still answers, got %v", err)
	}
	if len(result.Hits) != 1 {
		t.Fatalf("got %d hits, want the surviving searcher's 1", len(result.Hits))
	}
	if result.Metadata["degraded"] != true {
		t.Errorf("Metadata = %v, want degraded=true", result.Metadata)
	}
	failed, _ := result.Metadata["failed_searchers"].([]string)
	if len(failed) != 1 || failed[0] != string(index.IndexTypeVector) {
		t.Errorf("failed_searchers = %v, want [vector]", failed)
	}
	if _, ok := result.Metadata["searcher_errors"]; !ok {
		t.Errorf("Metadata = %v, want the failure recorded", result.Metadata)
	}
}

func TestSearchFailsWhenEverySearcherFails(t *testing.T) {
	svc := New(map[index.IndexType]index.Searcher{
		index.IndexTypeVector: &recordingSearcher{err: fmt.Errorf("embedder down")},
		index.IndexTypeBM25:   &recordingSearcher{err: fmt.Errorf("index closed")},
	}, nil)

	if _, err := svc.Search(context.Background(), &index.SearchQuery{Query: "test", Limit: 10}, true); err == nil {
		t.Fatal("Search() must fail when no searcher answered")
	}
}

// TestSearchersRunConcurrently: hybrid latency used to be the sum of both
// searchers, one of which makes a synchronous embedding call.
func TestSearchersRunConcurrently(t *testing.T) {
	const delay = 200 * time.Millisecond
	svc := New(map[index.IndexType]index.Searcher{
		index.IndexTypeVector: &recordingSearcher{delay: delay, hits: manyHits(2)},
		index.IndexTypeBM25:   &recordingSearcher{delay: delay, hits: manyHits(2)},
	}, nil)

	start := time.Now()
	if _, err := svc.Search(context.Background(), &index.SearchQuery{Query: "test", Limit: 10}, true); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed >= 2*delay {
		t.Errorf("hybrid search took %s for two %s searchers; they must run concurrently", elapsed, delay)
	}
}

// TestRerankScoresStayComparableWithTheTail pins the score-scale fix: the
// reranked head used to carry cross-encoder logits (negative) while the tail
// kept RRF scores, so sorting the response by score reordered it.
func TestRerankScoresStayComparableWithTheTail(t *testing.T) {
	svc := New(map[index.IndexType]index.Searcher{
		index.IndexTypeBM25: &mockSearcher{hits: manyHits(10)},
	}, nil)
	svc.SetReranker(&countingReranker{}, 4)

	result, err := svc.KeywordSearch(context.Background(), &index.SearchQuery{Query: "test", Limit: 10})
	if err != nil {
		t.Fatalf("KeywordSearch() error = %v", err)
	}
	if len(result.Hits) != 10 {
		t.Fatalf("got %d hits, want 10", len(result.Hits))
	}

	served := make([]string, len(result.Hits))
	for i, h := range result.Hits {
		served[i] = h.FilePath
		if h.Score < 0 {
			t.Errorf("hit %s kept a raw negative rerank score %v", h.FilePath, h.Score)
		}
	}

	byScore := append([]*index.Hit(nil), result.Hits...)
	sort.SliceStable(byScore, func(i, j int) bool { return byScore[i].Score > byScore[j].Score })
	for i, h := range byScore {
		if h.FilePath != served[i] {
			t.Fatalf("sorting by score gives %s at %d but the response served %s",
				h.FilePath, i, served[i])
		}
	}
}

func TestSearchMetadataRecordsRerankFailure(t *testing.T) {
	svc := New(map[index.IndexType]index.Searcher{
		index.IndexTypeBM25: &mockSearcher{hits: manyHits(4)},
	}, nil)
	svc.SetReranker(&fakeReranker{err: fmt.Errorf("upstream down")}, 4)

	result, err := svc.Search(context.Background(), &index.SearchQuery{Query: "test", Limit: 4}, true)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if result.Metadata["reranked"] != false {
		t.Errorf("Metadata = %v, want reranked=false", result.Metadata)
	}
}

// TestTiedScoresDoNotFollowSearcherOrder covers the second way two builds of
// one corpus used to disagree.
//
// A searcher returns hits in score order and says nothing about the ones that
// tie: the BM25 leg hands back bleve's internal document order, which is the
// order the indexing goroutines wrote the documents in. That would cost nothing
// if a tie stayed a tie, but fuseRRF scores a hit by its *position* in the
// searcher's list, so the tie becomes a difference in the fused score and the
// sortHits at the end of fusion has nothing left to settle.
//
// Measured on the boutique corpus before this: "which services ask the shipping
// service for a shipping quote" returned src/frontend/rpc.go and
// src/frontend/handlers.go — identical BM25 scores — at ranks 12 and 13 in
// either order between two indexes built from the same sources, which moved the
// question's span rank and the run's nDCG.
func TestTiedScoresDoNotFollowSearcherOrder(t *testing.T) {
	ctx := context.Background()

	// Equal scores, so only the searcher's arrangement separates them, and
	// paths whose alphabetical order is neither of the two orders below.
	tied := func() []*index.Hit {
		return []*index.Hit{
			{RepoID: "r1", FilePath: "src/frontend/rpc.go", Line: 61, EndLine: 120, Score: 0.5},
			{RepoID: "r1", FilePath: "src/frontend/handlers.go", Line: 241, EndLine: 300, Score: 0.5},
			{RepoID: "r1", FilePath: "src/checkoutservice/main.go", Line: 1, EndLine: 60, Score: 0.5},
		}
	}
	reversed := func() []*index.Hit {
		hits := tied()
		return []*index.Hit{hits[2], hits[1], hits[0]}
	}

	order := func(hits []*index.Hit) []string {
		t.Helper()
		svc := New(map[index.IndexType]index.Searcher{
			index.IndexTypeBM25: &mockSearcher{hits: hits},
		}, nil)
		result, err := svc.Search(ctx, &index.SearchQuery{Query: "quote", Limit: 10}, true)
		if err != nil {
			t.Fatalf("Search() error = %v", err)
		}
		out := make([]string, 0, len(result.Hits))
		for _, h := range result.Hits {
			out = append(out, h.FilePath)
		}
		return out
	}

	want := []string{
		"src/checkoutservice/main.go",
		"src/frontend/handlers.go",
		"src/frontend/rpc.go",
	}
	forward, backward := order(tied()), order(reversed())
	if !reflect.DeepEqual(forward, backward) {
		t.Errorf("tied hits follow the searcher's order:\n as returned: %v\n reversed:    %v", forward, backward)
	}
	if !reflect.DeepEqual(forward, want) {
		t.Errorf("tied hits = %v, want them by path %v", forward, want)
	}
}
