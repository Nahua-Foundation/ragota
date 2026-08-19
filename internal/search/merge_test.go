package search

import (
	"context"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/index"
)

// windowHits is what one file looks like coming out of a keyword index: line
// windows cut with an overlap, so consecutive ones share ten lines and carry
// the same evidence twice. b.go is there to be crowded out.
func windowHits() []*index.Hit {
	return []*index.Hit{
		{RepoID: "r1", FilePath: "a.go", Path: "a.go", Line: 1, EndLine: 60, Score: 0.9, Reason: "keyword"},
		{RepoID: "r1", FilePath: "a.go", Path: "a.go", Line: 51, EndLine: 110, Score: 0.8, Reason: "keyword"},
		{RepoID: "r1", FilePath: "a.go", Path: "a.go", Line: 101, EndLine: 160, Score: 0.7, Reason: "keyword"},
		{RepoID: "r1", FilePath: "b.go", Path: "b.go", Line: 1, EndLine: 60, Score: 0.6, Reason: "keyword"},
	}
}

func TestMergeSpansCollapsesOverlappingWindows(t *testing.T) {
	meta := map[string]interface{}{}
	got := mergeSpans(windowHits(), meta)

	if len(got) != 2 {
		t.Fatalf("got %d hits, want 2 (one per file); ranges: %s", len(got), ranges(got))
	}
	if got[0].FilePath != "a.go" || got[1].FilePath != "b.go" {
		t.Fatalf("order changed: %s", ranges(got))
	}
	if got[0].Line != 1 || got[0].EndLine != 160 {
		t.Errorf("merged range = %d-%d, want 1-160 (the union of the three windows)", got[0].Line, got[0].EndLine)
	}
	if got[0].Score != 0.9 {
		t.Errorf("score = %v, want the best-ranked hit's 0.9", got[0].Score)
	}
	if !strings.Contains(got[0].Reason, mergeReason) {
		t.Errorf("reason = %q, want it to say the hit absorbed others", got[0].Reason)
	}
	if strings.Contains(got[1].Reason, mergeReason) {
		t.Errorf("b.go absorbed nothing but says %q", got[1].Reason)
	}
	if meta["merged_spans"] != 2 {
		t.Errorf("meta[merged_spans] = %v, want 2", meta["merged_spans"])
	}
}

// TestMergeSpansKeepsSeparateAnswersSeparate: two symbols that happen to sit in
// one file are two answers. Only shared lines are shared evidence.
func TestMergeSpansKeepsSeparateAnswersSeparate(t *testing.T) {
	hits := []*index.Hit{
		{RepoID: "r1", FilePath: "a.go", Line: 1, EndLine: 40, Symbol: "First", Score: 0.9},
		{RepoID: "r1", FilePath: "a.go", Line: 41, EndLine: 80, Symbol: "Second", Score: 0.8},
		{RepoID: "r2", FilePath: "a.go", Line: 1, EndLine: 40, Symbol: "Other", Score: 0.7},
	}
	got := mergeSpans(hits, nil)
	if len(got) != 3 {
		t.Fatalf("got %d hits, want 3: adjacent symbols and same-named files in other repositories are not one span; %s", len(got), ranges(got))
	}
}

// TestMergeSpansDoesNotEditTheInput pins the copy: the candidate list can be
// shared with a result that was already handed out, and widening a range in
// place would rewrite it under the caller.
func TestMergeSpansDoesNotEditTheInput(t *testing.T) {
	hits := windowHits()
	mergeSpans(hits, nil)
	if hits[0].EndLine != 60 {
		t.Errorf("input hit was widened to %d; mergeSpans must copy", hits[0].EndLine)
	}
	if strings.Contains(hits[0].Reason, mergeReason) {
		t.Errorf("input hit's reason was rewritten to %q", hits[0].Reason)
	}
}

// TestMergeSpansRunsAfterTheReranker is the ordering this stage depends on. A
// cross-encoder's measured strength is picking the chunk that contains the
// answer out of a file's chunks; collapsing first would take that choice away
// and keep whichever chunk fusion happened to rank higher. fakeReranker
// inverts the order, so the last window must be the one that survives.
func TestMergeSpansRunsAfterTheReranker(t *testing.T) {
	svc := New(map[index.IndexType]index.Searcher{
		index.IndexTypeBM25: &mockSearcher{name: index.IndexTypeBM25, hits: windowHits()},
	}, nil)
	svc.SetReranker(&fakeReranker{}, 0)

	result, err := svc.KeywordSearch(context.Background(), &index.SearchQuery{Query: "test", Limit: 10})
	if err != nil {
		t.Fatalf("KeywordSearch: %v", err)
	}
	if len(result.Hits) != 2 {
		t.Fatalf("got %d hits, want 2; %s", len(result.Hits), ranges(result.Hits))
	}
	// b.go scores last before reranking, so inversion puts it first, and a.go's
	// surviving window is the one the reranker ranked highest — its last, which
	// covers 101-160 and is widened back over the two it absorbed.
	if result.Hits[0].FilePath != "b.go" {
		t.Errorf("first hit = %q, want b.go (the reranker's choice)", result.Hits[0].FilePath)
	}
	a := result.Hits[1]
	if a.Line != 1 || a.EndLine != 160 {
		t.Errorf("a.go range = %d-%d, want the union 1-160", a.Line, a.EndLine)
	}
}

// TestNoMergeKeepsEveryChunk: the collapse is on by default, so measuring what
// it is worth needs a way to turn it off — search.no_merge_spans is it.
func TestNoMergeKeepsEveryChunk(t *testing.T) {
	svc := New(map[index.IndexType]index.Searcher{
		index.IndexTypeBM25: &mockSearcher{name: index.IndexTypeBM25, hits: windowHits()},
	}, &Config{RRFK: 60, NoMerge: true})

	result, err := svc.Search(context.Background(), &index.SearchQuery{Query: "test", Limit: 10}, true)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(result.Hits) != 4 {
		t.Errorf("got %d hits, want all 4 kept apart; %s", len(result.Hits), ranges(result.Hits))
	}
	if _, ok := result.Metadata["merged_spans"]; ok {
		t.Error("metadata claims spans were merged")
	}
}

func ranges(hits []*index.Hit) string {
	var b strings.Builder
	for i, h := range hits {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(h.FilePath)
		start, end := h.Range()
		b.WriteString(":")
		b.WriteString(itoa(start))
		b.WriteString("-")
		b.WriteString(itoa(end))
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
