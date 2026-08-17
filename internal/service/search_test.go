package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/indexing"
	"github.com/Nahua-Foundation/ragota/internal/search"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// stubSearcher returns a fixed set of hits and records the query it was given.
// The hybrid search runs its searchers concurrently, and one stub can be
// registered under several index types, so the recorded query is guarded.
type stubSearcher struct {
	mu      sync.Mutex
	hits    []*indexing.Hit
	got     *indexing.SearchQuery
	byQuery map[string][]*indexing.Hit
}

func (s *stubSearcher) Search(ctx context.Context, q *indexing.SearchQuery) (*indexing.SearchResult, error) {
	s.mu.Lock()
	s.got = q
	s.mu.Unlock()
	hits := s.hits
	if s.byQuery != nil {
		hits = s.byQuery[q.Query]
	}
	if q.Limit > 0 && len(hits) > q.Limit {
		hits = hits[:q.Limit]
	}
	return &indexing.SearchResult{Hits: hits, Total: len(hits), Query: q.Query}, nil
}

// serviceWithSearcher builds a Service whose only searcher is srch.
func serviceWithSearcher(srch indexing.Searcher, st storage.Storage) *Service {
	if st == nil {
		st = &mockStorage{}
	}
	return New(nil, st, nil, nil, search.New(map[indexing.IndexType]indexing.Searcher{
		indexing.IndexTypeBM25:   srch,
		indexing.IndexTypeVector: srch,
	}, nil))
}

func hitsForFiles(paths ...string) []*indexing.Hit {
	hits := make([]*indexing.Hit, len(paths))
	for i, p := range paths {
		hits[i] = &indexing.Hit{RepoID: "r1", FilePath: p, Path: p, Line: 1, EndLine: 10, Reason: "keyword"}
	}
	return hits
}

// TestSearchValidatesLimit: limit=-1 used to report total=10 with an empty
// list, and limit=100000000 was honoured verbatim.
func TestSearchValidatesLimit(t *testing.T) {
	tests := []struct {
		name  string
		limit int
		want  int
	}{
		{name: "zero uses the default", limit: 0, want: defaultSearchLimit},
		{name: "negative uses the default", limit: -1, want: defaultSearchLimit},
		{name: "huge is clamped", limit: 100000000, want: maxSearchLimit},
		{name: "in range is kept", limit: 7, want: 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srch := &stubSearcher{}
			svc := serviceWithSearcher(srch, nil)
			defer svc.Close(context.Background())

			q := &indexing.SearchQuery{Query: "add", Limit: tt.limit}
			if _, err := svc.Search(context.Background(), q, "keyword"); err != nil {
				t.Fatalf("Search() error = %v", err)
			}
			if q.Limit != tt.want {
				t.Errorf("limit = %d, want %d", q.Limit, tt.want)
			}
		})
	}
}

func TestSearchRejectsUnknownMode(t *testing.T) {
	svc := serviceWithSearcher(&stubSearcher{}, nil)
	defer svc.Close(context.Background())

	for _, mode := range []string{"sematic", "SEMANTIC", "fuzzy", "bm25"} {
		_, err := svc.Search(context.Background(), &indexing.SearchQuery{Query: "add"}, mode)
		if !errors.Is(err, ErrBadRequest) {
			t.Errorf("Search(mode=%q) error = %v, want ErrBadRequest", mode, err)
		}
	}

	for _, mode := range []string{"", SearchModeSemantic, SearchModeKeyword, SearchModeHybrid} {
		if _, err := svc.Search(context.Background(), &indexing.SearchQuery{Query: "add"}, mode); err != nil {
			t.Errorf("Search(mode=%q) error = %v, want success", mode, err)
		}
	}
}

func TestSearchRejectsEmptyQuery(t *testing.T) {
	svc := serviceWithSearcher(&stubSearcher{}, nil)
	defer svc.Close(context.Background())

	for _, query := range []string{"", "   "} {
		_, err := svc.Search(context.Background(), &indexing.SearchQuery{Query: query}, "keyword")
		if !errors.Is(err, ErrBadRequest) {
			t.Errorf("Search(%q) error = %v, want ErrBadRequest", query, err)
		}
	}

	// A pre-embedded vector search carries no text and must still be allowed.
	if _, err := svc.Search(context.Background(), &indexing.SearchQuery{Vector: []float32{0.1}}, "semantic"); err != nil {
		t.Errorf("Search() with a vector and no text error = %v, want success", err)
	}
}

// unitStorage serves fixed AST units so BuildContext can resolve hits to units.
type unitStorage struct {
	*mockStorage
	units []*storage.ASTUnit
}

func (u *unitStorage) GetASTUnits(context.Context, storage.QueryOpts) ([]*storage.ASTUnit, error) {
	return u.units, nil
}

// TestBuildContextDedupesUnits: several chunks of one function used to become
// several identical context items, eating the caller's limit.
func TestBuildContextDedupesUnits(t *testing.T) {
	st := &unitStorage{
		mockStorage: &mockStorage{},
		units: []*storage.ASTUnit{
			{ID: "u-add", RepoID: "r1", FilePath: "calc.go", Name: "Add", Kind: "function", StartLine: 1, EndLine: 40},
		},
	}

	// Three overlapping chunks of the same function, then a different file.
	hits := []*indexing.Hit{
		{RepoID: "r1", FilePath: "calc.go", Line: 1, EndLine: 20, Reason: "keyword"},
		{RepoID: "r1", FilePath: "calc.go", Line: 10, EndLine: 30, Reason: "keyword"},
		{RepoID: "r1", FilePath: "calc.go", Line: 20, EndLine: 40, Reason: "keyword"},
	}
	svc := serviceWithSearcher(&stubSearcher{hits: hits}, st)
	defer svc.Close(context.Background())

	res, err := svc.BuildContext(context.Background(), "add", nil, "keyword", 5, 1, "")
	if err != nil {
		t.Fatalf("BuildContext() error = %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("got %d items, want 1 distinct unit", len(res.Items))
	}
	if res.Items[0].Unit == nil || res.Items[0].Unit.ID != "u-add" {
		t.Errorf("item unit = %+v, want u-add", res.Items[0].Unit)
	}
}

func TestBuildContextDedupesByPathWithoutUnits(t *testing.T) {
	hits := []*indexing.Hit{
		{RepoID: "r1", FilePath: "a.go", Line: 1, EndLine: 20},
		{RepoID: "r1", FilePath: "a.go", Line: 1, EndLine: 20},
		{RepoID: "r1", FilePath: "b.go", Line: 1, EndLine: 20},
	}
	svc := serviceWithSearcher(&stubSearcher{hits: hits}, nil)
	defer svc.Close(context.Background())

	res, err := svc.BuildContext(context.Background(), "add", nil, "keyword", 5, 1, "")
	if err != nil {
		t.Fatalf("BuildContext() error = %v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("got %d items, want 2 distinct files", len(res.Items))
	}
}

// TestBuildContextFillsLimitWithDistinctUnits checks the retrieval window is
// wide enough to still return `limit` items after deduplication.
func TestBuildContextFillsLimitWithDistinctUnits(t *testing.T) {
	var hits []*indexing.Hit
	for i := 0; i < 3; i++ {
		// Two chunks per file, so half of every page is a duplicate.
		path := fmt.Sprintf("f%d.go", i)
		hits = append(hits,
			&indexing.Hit{RepoID: "r1", FilePath: path, Line: 1, EndLine: 20},
			&indexing.Hit{RepoID: "r1", FilePath: path, Line: 21, EndLine: 40},
		)
	}
	svc := serviceWithSearcher(&stubSearcher{hits: hits}, nil)
	defer svc.Close(context.Background())

	res, err := svc.BuildContext(context.Background(), "add", nil, "keyword", 3, 1, "")
	if err != nil {
		t.Fatalf("BuildContext() error = %v", err)
	}
	if len(res.Items) != 3 {
		t.Errorf("got %d items, want 3", len(res.Items))
	}
}

// TestBuildContextItemsSerializeAsArray: an empty result used to marshal as
// null instead of [].
func TestBuildContextItemsNeverNil(t *testing.T) {
	svc := serviceWithSearcher(&stubSearcher{}, nil)
	defer svc.Close(context.Background())

	res, err := svc.BuildContext(context.Background(), "nothing matches", nil, "keyword", 5, 1, "")
	if err != nil {
		t.Fatalf("BuildContext() error = %v", err)
	}
	if res.Items == nil {
		t.Fatal("Items is nil; an empty result must serialize as []")
	}
	if len(res.Items) != 0 {
		t.Errorf("got %d items, want 0", len(res.Items))
	}
}

// fakeGenerator returns a canned rewrite.
type fakeGenerator struct {
	out string
	err error
}

func (f *fakeGenerator) Name() string { return "fake" }
func (f *fakeGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	return f.out, f.err
}

// TestBuildContextFallsBackWhenRewriteFindsNothing: a live model returned the
// rewrite wrapped in quotes, which became a phrase query and returned nothing.
// Even after sanitizing, a rewrite that retrieves nothing must not win.
func TestBuildContextFallsBackWhenRewriteFindsNothing(t *testing.T) {
	srch := &stubSearcher{byQuery: map[string][]*indexing.Hit{
		"how does login work": hitsForFiles("auth.go"),
		"unrelated tokens":    nil,
	}}
	svc := serviceWithSearcher(srch, nil)
	defer svc.Close(context.Background())
	svc.SetAssistant(&fakeGenerator{out: "unrelated tokens"}, true)

	res, err := svc.BuildContext(context.Background(), "how does login work", nil, "keyword", 5, 1, "")
	if err != nil {
		t.Fatalf("BuildContext() error = %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("got %d items, want the original query's 1", len(res.Items))
	}
	if res.RewrittenQuery != "" {
		t.Errorf("RewrittenQuery = %q, want it cleared after the fallback", res.RewrittenQuery)
	}
}

func TestBuildContextKeepsUsefulRewrite(t *testing.T) {
	srch := &stubSearcher{byQuery: map[string][]*indexing.Hit{
		"how does login work": nil,
		"login auth session":  hitsForFiles("auth.go"),
	}}
	svc := serviceWithSearcher(srch, nil)
	defer svc.Close(context.Background())
	svc.SetAssistant(&fakeGenerator{out: "\"login auth session\""}, true)

	res, err := svc.BuildContext(context.Background(), "how does login work", nil, "keyword", 5, 1, "")
	if err != nil {
		t.Fatalf("BuildContext() error = %v", err)
	}
	if res.RewrittenQuery != "login auth session" {
		t.Errorf("RewrittenQuery = %q, want the sanitized rewrite", res.RewrittenQuery)
	}
	if len(res.Items) != 1 {
		t.Errorf("got %d items, want 1", len(res.Items))
	}
}

func TestRewriteQueryIgnoresUnusableOutput(t *testing.T) {
	svc := serviceWithSearcher(&stubSearcher{}, nil)
	defer svc.Close(context.Background())

	for _, out := range []string{"", "   ", "<think>only reasoning"} {
		svc.SetAssistant(&fakeGenerator{out: out}, true)
		got, rewritten := svc.enrich.RewriteQuery(context.Background(), "original query")
		if rewritten || got != "original query" {
			t.Errorf("rewriteQuery with output %q = (%q, %v), want the original", out, got, rewritten)
		}
	}

	svc.SetAssistant(&fakeGenerator{err: fmt.Errorf("llm down")}, true)
	if got, rewritten := svc.enrich.RewriteQuery(context.Background(), "original query"); rewritten || got != "original query" {
		t.Errorf("rewriteQuery on error = (%q, %v), want the original", got, rewritten)
	}
}

// TestBuildContextKeepsIntentQueryUnrewritten pins the rewrite interaction the
// eval caught: the assistant rephrases a callers question into keywords, the
// intent detector no longer recognises it, and a rank-1 graph answer becomes a
// miss (measured: callers recall@10 0.400 -> 0.200 with rewriting on).
func TestBuildContextKeepsIntentQueryUnrewritten(t *testing.T) {
	st, _ := callersFixture(t)
	srch := &stubSearcher{byQuery: map[string][]*indexing.Hit{}}
	svc := serviceWithSearcher(srch, st)
	defer svc.Close(context.Background())
	// A rewrite in the shape the live model produced: keywords, no question.
	svc.SetAssistant(&fakeGenerator{out: "ShipOrder shipping rpc usage"}, true)

	res, err := svc.BuildContext(context.Background(),
		"what calls the shipping service ShipOrder rpc", []string{"r1"}, "keyword", 5, 1, "")
	if err != nil {
		t.Fatalf("BuildContext() error = %v", err)
	}
	if res.RewrittenQuery != "" {
		t.Errorf("RewrittenQuery = %q, want the rewrite skipped for an intent question", res.RewrittenQuery)
	}
	if len(res.Items) == 0 {
		t.Fatal("no items: the graph answer was lost")
	}
	hit := res.Items[0].Hit
	if hit.FilePath != "checkout/main.go" || hit.Line != 25 {
		t.Errorf("first item = %s:%d, want the call site checkout/main.go:25", hit.FilePath, hit.Line)
	}
}

// A question the detector does not recognise still goes through the rewrite.
func TestBuildContextStillRewritesPlainQueries(t *testing.T) {
	srch := &stubSearcher{byQuery: map[string][]*indexing.Hit{
		"how does login work": nil,
		"login auth session":  hitsForFiles("auth.go"),
	}}
	svc := serviceWithSearcher(srch, nil)
	defer svc.Close(context.Background())
	svc.SetAssistant(&fakeGenerator{out: "login auth session"}, true)

	res, err := svc.BuildContext(context.Background(), "how does login work", nil, "keyword", 5, 1, "")
	if err != nil {
		t.Fatalf("BuildContext() error = %v", err)
	}
	if res.RewrittenQuery != "login auth session" {
		t.Errorf("RewrittenQuery = %q, want the rewrite applied", res.RewrittenQuery)
	}
}

// TestSearchRejectsUnknownIntent pins the boundary between the two packages:
// promote reports an unknown intent, and the service turns it into the
// ErrBadRequest the HTTP layer maps to 400. A silent fallback would answer a
// different question from the one the client asked.
func TestSearchRejectsUnknownIntent(t *testing.T) {
	svc := serviceWithSearcher(&stubSearcher{}, nil)
	defer svc.Close(context.Background())

	_, err := svc.Search(context.Background(),
		&indexing.SearchQuery{Query: "anything", Intent: "sideways"}, SearchModeKeyword)
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("unknown intent error = %v, want ErrBadRequest", err)
	}
}
