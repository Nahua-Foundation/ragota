package ast

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/indexing"
	"github.com/Nahua-Foundation/ragota/internal/indexing/symbols"
)

func indexFiles(t *testing.T, st *memStorage, files []*indexing.FileToIndex) *indexing.IndexResult {
	t.Helper()
	idx := New(&Config{Storage: st, Workers: 4})
	idx.RegisterParser(NewTreeSitterParser("go"))
	res, err := idx.Index(context.Background(), &indexing.IndexRequest{RepoID: "r", Files: files})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	return res
}

// The store stage costs one transaction per storage call. Deletes as well as
// inserts go out per window, so a window costs four transactions however many
// files it holds — 4 rather than 4×256.
func TestStoreStageBatchesTransactions(t *testing.T) {
	const nFiles = storeWindow + 7

	st := &memStorage{}
	res := indexFiles(t, st, syntheticGoFiles(nFiles))
	if res.FilesIndexed != nFiles {
		t.Fatalf("FilesIndexed = %d, want %d", res.FilesIndexed, nFiles)
	}

	wantWindows := 2 // one full window plus the remainder
	for _, method := range []string{
		"DeleteASTUnitsByFiles", "DeleteEdgesByFiles", "BatchStoreASTUnits", "BatchStoreEdges",
	} {
		if got := st.calls[method]; got != wantWindows {
			t.Errorf("%s called %d times for %d files, want %d", method, got, nFiles, wantWindows)
		}
	}
	for _, method := range []string{"DeleteASTUnitsByFile", "DeleteEdgesByFile"} {
		if got := st.calls[method]; got != 0 {
			t.Errorf("%s called %d times, want none (the window deletes are batched)", method, got)
		}
	}
	if len(st.units) == 0 || len(st.edges) == 0 {
		t.Fatalf("stored %d units and %d edges, want both non-empty", len(st.units), len(st.edges))
	}
}

// A window whose delete is rejected is retried file by file, so the one bad
// file is named and the other files' stale rows are still cleared.
func TestStoreStageRetriesRejectedDeletePerFile(t *testing.T) {
	files := syntheticGoFiles(5)
	bad := files[2].Path

	st := &memStorage{}
	indexFiles(t, st, files)
	first := len(st.units)

	st.failDeleteFor = bad
	res := indexFiles(t, st, files)

	if res.FilesIndexed != len(files)-1 {
		t.Errorf("FilesIndexed = %d, want %d", res.FilesIndexed, len(files)-1)
	}
	if res.FilesFailed != 1 {
		t.Errorf("FilesFailed = %d, want 1", res.FilesFailed)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], bad) {
		t.Errorf("Errors = %v, want one naming %s", res.Errors, bad)
	}
	// The four good files were deleted and re-stored; the bad one kept the
	// rows of the first pass and gained none.
	if len(st.units) != first {
		t.Errorf("units after the retried pass = %d, want %d", len(st.units), first)
	}
	// One batch attempt, then one retry per file in the window.
	if got, want := st.calls["DeleteASTUnitsByFiles"], 1+(1+len(files)); got != want {
		t.Errorf("DeleteASTUnitsByFiles called %d times, want %d", got, want)
	}
}

// Every stored edge must point at a real unit id: the positional markers the
// parser emits are resolved per file, so batching them together must not let a
// file's edge resolve against another file's units.
func TestStoreStageResolvesEdgeSourcesPerFile(t *testing.T) {
	st := &memStorage{}
	indexFiles(t, st, syntheticGoFiles(12))

	byID := map[string]string{}
	for _, u := range st.units {
		byID[u.ID] = u.FilePath
	}
	for _, e := range st.edges {
		if strings.HasPrefix(e.SrcID, srcMarkPrefix) {
			t.Fatalf("edge %s->%s kept its positional marker %q", e.Kind, e.DstName, e.SrcID)
		}
		file, ok := byID[e.SrcID]
		if !ok {
			t.Fatalf("edge %s->%s points at unknown unit %q", e.Kind, e.DstName, e.SrcID)
		}
		if file != e.FilePath {
			t.Errorf("edge in %s resolved to a unit of %s", e.FilePath, file)
		}
	}
}

// A window whose batch is rejected is retried file by file, so the one bad
// file is named and the rest are still stored.
func TestStoreStageRetriesRejectedWindowPerFile(t *testing.T) {
	files := syntheticGoFiles(5)
	bad := files[2].Path

	st := &memStorage{failUnitsFor: bad}
	res := indexFiles(t, st, files)

	if res.FilesIndexed != len(files)-1 {
		t.Errorf("FilesIndexed = %d, want %d", res.FilesIndexed, len(files)-1)
	}
	if res.FilesFailed != 1 {
		t.Errorf("FilesFailed = %d, want 1", res.FilesFailed)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], bad) {
		t.Errorf("Errors = %v, want one naming %s", res.Errors, bad)
	}
	for _, u := range st.units {
		if u.FilePath == bad {
			t.Fatalf("units of the rejected file %s were stored", bad)
		}
	}
	for _, e := range st.edges {
		if e.FilePath == bad {
			t.Fatalf("edges of the rejected file %s were stored", bad)
		}
	}
	// One window batch, then one retry per file in it.
	if got, want := st.calls["BatchStoreASTUnits"], 1+len(files); got != want {
		t.Errorf("BatchStoreASTUnits called %d times, want %d", got, want)
	}
}

// Re-indexing must not accumulate: the whole window is deleted before any of
// it is stored, so a file's previous rows are gone whichever window it lands
// in.
func TestStoreStageReindexIsIdempotent(t *testing.T) {
	files := syntheticGoFiles(9)

	st := &memStorage{}
	indexFiles(t, st, files)
	first := len(st.units)

	for _, f := range files {
		f.Content = []byte(fmt.Sprintf("%s\n\nfunc Extra() {}\n", string(f.Content)))
	}
	indexFiles(t, st, files)

	if len(st.units) != first+len(files) {
		t.Errorf("units after re-index = %d, want %d (one added function per file)", len(st.units), first+len(files))
	}
}

// The parse stage publishes each file's symbols so another indexer over the
// same window does not have to parse the bytes again. What it publishes must
// be the units it parsed — and a copy of them, since the store stage rewrites
// the units in place while the consumer is reading.
func TestParseStagePublishesSymbols(t *testing.T) {
	files := syntheticGoFiles(3)
	for i, f := range files {
		f.Hash = fmt.Sprintf("hash-%d", i)
	}

	st := &memStorage{}
	idx := New(&Config{Storage: st, Workers: 2})
	idx.RegisterParser(NewTreeSitterParser("go"))
	if _, err := idx.Index(context.Background(), &indexing.IndexRequest{RepoID: "r-publish", Files: files}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	for _, f := range files {
		syms, ok := symbols.Shared.Take("r-publish", f.Path, f.Hash)
		if !ok {
			t.Fatalf("no symbols published for %s", f.Path)
		}
		var stored []string
		for _, u := range st.units {
			if u.FilePath == f.Path && u.Name != "" {
				stored = append(stored, u.Name+"|"+u.Kind)
			}
		}
		var published []string
		for _, s := range syms {
			published = append(published, s.Name+"|"+s.Kind)
		}
		if len(published) != len(stored) {
			t.Fatalf("%s: published %v, stored %v", f.Path, published, stored)
		}
		for i := range stored {
			if published[i] != stored[i] {
				t.Errorf("%s symbol %d: published %q, stored %q", f.Path, i, published[i], stored[i])
			}
		}
	}
}

// Languages whose symbols no consumer annotates with are not published: the
// cache is bounded, and filling it with entries nobody takes evicts the ones
// somebody would.
func TestParseStageDoesNotPublishUnannotatedLanguages(t *testing.T) {
	file := &indexing.FileToIndex{
		Path:     "conf/app.yaml",
		Language: "yaml",
		Hash:     "yaml-hash",
		Content:  []byte("server:\n  port: 8080\n"),
	}

	idx := New(&Config{Storage: &memStorage{}, Workers: 1})
	idx.RegisterParser(GetParserForLanguage("yaml"))
	if _, err := idx.Index(context.Background(), &indexing.IndexRequest{
		RepoID: "r-yaml", Files: []*indexing.FileToIndex{file},
	}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	if _, ok := symbols.Shared.Take("r-yaml", file.Path, file.Hash); ok {
		t.Error("yaml symbols were published; no consumer annotates chunks with them")
	}
}
