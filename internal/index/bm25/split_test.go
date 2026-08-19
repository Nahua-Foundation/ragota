package bm25

import (
	"context"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/index"
)

// splitSource is written entirely in camelCase and snake_case, so that no
// plain word in a question appears in it literally. Anything a word-shaped
// query finds here, it found by taking an identifier apart.
var splitSource = []byte("package svc\n\n" +
	"func getUserByID(loginAttempt *LoginAttempt) error {\n" +
	"\treturn nil\n" +
	"}\n")

func indexSplitSource(t *testing.T, split bool) *Indexer {
	t.Helper()
	idx, err := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75, SplitIdentifiers: split})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { idx.Close() })

	if _, err := idx.Index(context.Background(), &index.IndexRequest{
		RepoID: "r1",
		Files:  []*index.FileToIndex{{Path: "svc.go", Language: "go", Content: splitSource}},
	}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	return idx
}

func hitCount(t *testing.T, idx *Indexer, q string) int {
	t.Helper()
	res, err := idx.Search(context.Background(), &index.SearchQuery{Query: q, Limit: 10})
	if err != nil {
		t.Fatalf("Search(%q): %v", q, err)
	}
	return len(res.Hits)
}

// TestSplitIdentifiersReachesWordsInsideIdentifiers is the whole point of the
// field: the default analyser leaves getUserByID and login_attempt as single
// tokens (UAX#29 joins on "_" and splits nothing on case), so a question
// phrased in words never reached the keyword leg at all.
func TestSplitIdentifiersReachesWordsInsideIdentifiers(t *testing.T) {
	questions := []string{"get user by id", "login attempt"}

	plain := indexSplitSource(t, false)
	for _, q := range questions {
		if n := hitCount(t, plain, q); n != 0 {
			t.Errorf("without split_identifiers %q found %d hits; the baseline this change is measured against is zero", q, n)
		}
	}

	split := indexSplitSource(t, true)
	for _, q := range questions {
		if n := hitCount(t, split, q); n == 0 {
			t.Errorf("with split_identifiers %q found nothing", q)
		}
	}
}

// TestSplitIdentifiersKeepsLiteralMatches guards the other direction: the
// code-aware view is added beside the literal one, not in place of it, so an
// identifier pasted into the query still matches as itself.
func TestSplitIdentifiersKeepsLiteralMatches(t *testing.T) {
	for _, split := range []bool{false, true} {
		idx := indexSplitSource(t, split)
		if n := hitCount(t, idx, "getUserByID"); n == 0 {
			t.Errorf("split_identifiers=%v: the literal identifier found nothing", split)
		}
	}
}

// TestSplitIdentifiersRespectsFilters pins the query assembly: the text half
// of a query became a disjunction of two clauses, and it still has to sit
// inside the conjunction that carries the repository and language filters.
func TestSplitIdentifiersRespectsFilters(t *testing.T) {
	idx := indexSplitSource(t, true)

	res, err := idx.Search(context.Background(), &index.SearchQuery{
		Query: "get user by id",
		Repos: []string{"other"},
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) != 0 {
		t.Errorf("a query scoped to another repository returned %d hits", len(res.Hits))
	}

	res, err = idx.Search(context.Background(), &index.SearchQuery{
		Query: "get user by id",
		Repos: []string{"r1"},
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Error("a query scoped to the indexed repository returned nothing")
	}
}

// pathSource says nothing about where it lives: every file below is the same
// entry point, so the only thing that can tell them apart is the path.
var pathSource = []byte("package main\n\nfunc main() {\n\tserve()\n}\n")

// The three ways a service names its directory. The first two are the ones a
// tokenizer can take apart; the third is the documented limit.
var pathFiles = []*index.FileToIndex{
	{Path: "src/checkout_service/main.go", Language: "go", Content: pathSource},
	{Path: "src/PaymentService/Program.cs", Language: "csharp", Content: pathSource},
	{Path: "src/shippingservice/main.go", Language: "go", Content: pathSource},
}

func indexPathSource(t *testing.T, paths bool) *Indexer {
	t.Helper()
	idx, err := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75, IndexPaths: paths})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { idx.Close() })

	if _, err := idx.Index(context.Background(), &index.IndexRequest{
		RepoID: "r1", Files: pathFiles,
	}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	return idx
}

func topHit(t *testing.T, idx *Indexer, q string) string {
	t.Helper()
	res, err := idx.Search(context.Background(), &index.SearchQuery{Query: q, Limit: 5})
	if err != nil {
		t.Fatalf("Search(%q): %v", q, err)
	}
	if len(res.Hits) == 0 {
		return ""
	}
	return res.Hits[0].FilePath
}

// TestIndexPathsReachesTheDirectoryName is the keyword half of a result the
// vector side already has: a Go service's entry point is `func main`, and the
// only place the word "checkout" appears is the path the index never searched.
func TestIndexPathsReachesTheDirectoryName(t *testing.T) {
	off := indexPathSource(t, false)
	for _, q := range []string{"checkout service", "payment service"} {
		if n := hitCount(t, off, q); n != 0 {
			t.Errorf("without index_paths %q found %d hits; the baseline is zero", q, n)
		}
	}

	on := indexPathSource(t, true)
	for q, want := range map[string]string{
		"checkout service": "src/checkout_service/main.go",
		"payment service":  "src/PaymentService/Program.cs",
	} {
		if got := topHit(t, on, q); got != want {
			t.Errorf("%q ranked %q first, want %q", q, got, want)
		}
	}
}

// TestIndexPathsCannotSplitARunOfWords records what this field does not do. A
// tokenizer needs a boundary — a case change or a delimiter — and
// "shippingservice" offers neither, so BM25 cannot reach it from the word
// "shipping" the way an embedding model's subwords can. The path is a fact
// worth indexing, not a decompounder; the vector channel keeps that case.
func TestIndexPathsCannotSplitARunOfWords(t *testing.T) {
	idx := indexPathSource(t, true)
	res, err := idx.Search(context.Background(), &index.SearchQuery{Query: "shipping service", Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, h := range res.Hits {
		if h.FilePath == "src/shippingservice/main.go" {
			t.Fatal("shippingservice split after all — this test is the boundary of the method and should be revisited, not deleted")
		}
	}
}

// TestIndexPathsIsIndependentOfSplitIdentifiers keeps the two settings
// separately measurable: each brings the shared analyser with it, so either
// can be turned on alone.
func TestIndexPathsIsIndependentOfSplitIdentifiers(t *testing.T) {
	paths := indexPathSource(t, true)
	if paths.splitIdentifiers {
		t.Error("index_paths alone should not add the code-aware content field")
	}
	if n := hitCount(t, paths, "checkout service"); n == 0 {
		t.Error("index_paths alone did not answer a path question")
	}

	split := indexSplitSource(t, true)
	if split.indexPaths {
		t.Error("split_identifiers alone should not add the path field")
	}
}

// TestSplitIdentifiersFollowsTheIndexNotTheConfig covers the trap this setting
// invites: a mapping is written once, when the index is created, so turning
// the flag on over an index built without it changes nothing until a forced
// reindex. The indexer takes its answer from the index on disk, and a query
// therefore never asks for a field no document has.
func TestSplitIdentifiersFollowsTheIndexNotTheConfig(t *testing.T) {
	dir := t.TempDir()

	first, err := New(&Config{Path: dir, K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := first.Index(context.Background(), &index.IndexRequest{
		RepoID: "r1",
		Files:  []*index.FileToIndex{{Path: "svc.go", Language: "go", Content: splitSource}},
	}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := New(&Config{Path: dir, K1: 1.2, B: 0.75, SplitIdentifiers: true})
	if err != nil {
		t.Fatalf("New (reopen): %v", err)
	}
	defer reopened.Close()

	if reopened.splitIdentifiers {
		t.Error("reopening an index built without the field claimed to have it")
	}
	if n := hitCount(t, reopened, "getUserByID"); n == 0 {
		t.Error("reopened index lost its literal matches")
	}
}
