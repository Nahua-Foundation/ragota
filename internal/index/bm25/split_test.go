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
