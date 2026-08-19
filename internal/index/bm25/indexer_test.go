package bm25

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/index"
	"github.com/Nahua-Foundation/ragota/internal/index/ast"
	"github.com/Nahua-Foundation/ragota/internal/store"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/blevesearch/bleve/v2"
	bleveSearch "github.com/blevesearch/bleve/v2/search"
	bleveIndexAPI "github.com/blevesearch/bleve_index_api"
)

func TestBM25SearchReturnsLineNumbers(t *testing.T) {
	idx, err := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()

	content := []byte("package main\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n")
	if _, err := idx.Index(context.Background(), &index.IndexRequest{
		RepoID: "r1",
		Files:  []*index.FileToIndex{{Path: "calc.go", Language: "go", Content: content}},
	}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	res, err := idx.Search(context.Background(), &index.SearchQuery{Query: "Add", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("expected at least 1 hit")
	}
	h := res.Hits[0]
	if h.Line < 1 {
		t.Errorf("Line = %d, want >= 1", h.Line)
	}
	if h.EndLine < h.Line {
		t.Errorf("EndLine = %d, want >= Line (%d)", h.EndLine, h.Line)
	}
	if h.FilePath != "calc.go" {
		t.Errorf("FilePath = %q, want calc.go", h.FilePath)
	}
	if h.Kind == "go" {
		t.Errorf("Kind should not be the language; got %q", h.Kind)
	}
}

// TestBM25QueryWithReservedSyntax pins the parser fix: bleve's query-string
// syntax reads "Content-Type: application/json" as a filter on a field named
// Content-Type and matched nothing, with no error to show for it.
func TestBM25QueryWithReservedSyntax(t *testing.T) {
	idx, err := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()
	ctx := context.Background()

	content := []byte("func handler() {\n\tw.Header().Set(\"Content-Type\", \"application/json\")\n}\n")
	if _, err := idx.Index(ctx, &index.IndexRequest{
		RepoID: "r1",
		Files:  []*index.FileToIndex{{Path: "h.go", Language: "go", Content: content}},
	}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	plain, err := idx.Search(ctx, &index.SearchQuery{Query: "Content-Type application/json", Limit: 10})
	if err != nil {
		t.Fatalf("Search plain: %v", err)
	}
	if len(plain.Hits) == 0 {
		t.Fatal("expected hits for the colon-free query")
	}

	for _, query := range []string{
		"Content-Type: application/json",
		"handler: json",
		"handler AND",
		"what does handler() do?",
		`"handler`,
		"handler^json",
		"+handler -json",
	} {
		res, err := idx.Search(ctx, &index.SearchQuery{Query: query, Limit: 10})
		if err != nil {
			t.Fatalf("Search(%q): %v", query, err)
		}
		if len(res.Hits) == 0 {
			t.Errorf("Search(%q) returned 0 hits; reserved syntax must not eat the query", query)
		}
	}
}

func TestBM25EmptyQueryIsAnError(t *testing.T) {
	idx, err := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()

	for _, query := range []string{"", "   ", "\t\n"} {
		if _, err := idx.Search(context.Background(), &index.SearchQuery{Query: query, Limit: 10}); !errors.Is(err, ErrEmptyQuery) {
			t.Errorf("Search(%q) error = %v, want ErrEmptyQuery", query, err)
		}
	}
}

// TestBM25DocumentsCarrySymbolMetadata: keyword hits used to have no symbol or
// kind, which left fused hits half-annotated and reduced the reranker's
// fallback document to a bare path.
func TestBM25DocumentsCarrySymbolMetadata(t *testing.T) {
	idx, err := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()
	ctx := context.Background()

	content := []byte("package demo\n\n// Add adds.\nfunc Add(a, b int) int {\n\treturn a + b\n}\n")
	if _, err := idx.Index(ctx, &index.IndexRequest{
		RepoID: "r1",
		Files:  []*index.FileToIndex{{Path: "calc.go", Language: "go", Content: content}},
	}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	res, err := idx.Search(ctx, &index.SearchQuery{Query: "Add", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("expected a hit")
	}
	h := res.Hits[0]
	if !strings.Contains(h.Symbol, "Add") {
		t.Errorf("Symbol = %q, want it to name Add", h.Symbol)
	}
	if h.Kind == "" || h.Kind == "go" {
		t.Errorf("Kind = %q, want a symbol kind", h.Kind)
	}
	if h.Language != "go" {
		t.Errorf("Language = %q, want go", h.Language)
	}
}

func TestBM25SearchFilters(t *testing.T) {
	idx, err := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()
	ctx := context.Background()

	if _, err := idx.Index(ctx, &index.IndexRequest{
		RepoID: "r1",
		Files: []*index.FileToIndex{
			{Path: "src/calc.go", Language: "go", Content: []byte("package demo\n\nfunc Add(a, b int) int { return a + b }\n")},
			{Path: "lib/calc.py", Language: "python", Content: []byte("def Add(a, b):\n    return a + b\n")},
		},
	}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	tests := []struct {
		name   string
		filter map[string]interface{}
		want   string
	}{
		{name: "language", filter: map[string]interface{}{"language": "python"}, want: "lib/calc.py"},
		{name: "languages list", filter: map[string]interface{}{"languages": []string{"go"}}, want: "src/calc.go"},
		{name: "path prefix", filter: map[string]interface{}{"path_prefix": "lib/"}, want: "lib/calc.py"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := idx.Search(ctx, &index.SearchQuery{Query: "Add", Limit: 10, Filter: tt.filter})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(res.Hits) == 0 {
				t.Fatalf("filter %v removed every hit", tt.filter)
			}
			for _, h := range res.Hits {
				if h.FilePath != tt.want {
					t.Errorf("got %s, want only %s", h.FilePath, tt.want)
				}
			}
		})
	}
}

// TestBM25ConfigParamsApplied checks that the configured k1/b reach bleve.
// They are process-wide multipliers, which is why they are asserted globally.
func TestBM25ConfigParamsApplied(t *testing.T) {
	origK1, origB := bleveSearch.BM25_k1, bleveSearch.BM25_b
	defer func() { bleveSearch.BM25_k1, bleveSearch.BM25_b = origK1, origB }()

	idx, err := New(&Config{Path: t.TempDir(), K1: 1.7, B: 0.4})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()

	if bleveSearch.BM25_k1 != 1.7 {
		t.Errorf("bleve k1 = %v, want the configured 1.7", bleveSearch.BM25_k1)
	}
	if bleveSearch.BM25_b != 0.4 {
		t.Errorf("bleve b = %v, want the configured 0.4", bleveSearch.BM25_b)
	}
	m, err := buildMapping(false)
	if err != nil {
		t.Fatalf("buildMapping: %v", err)
	}
	if got := m.ScoringModel; got != bleveIndexAPI.BM25Scoring {
		t.Errorf("scoring model = %q, want %q", got, bleveIndexAPI.BM25Scoring)
	}
}

func TestBM25SearchRepoFilter(t *testing.T) {
	idx, err := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()
	ctx := context.Background()

	if _, err := idx.Index(ctx, &index.IndexRequest{
		RepoID: "r1",
		Files:  []*index.FileToIndex{{Path: "a.go", Language: "go", Content: []byte("func Add() {}\n")}},
	}); err != nil {
		t.Fatalf("Index r1: %v", err)
	}
	if _, err := idx.Index(ctx, &index.IndexRequest{
		RepoID: "r2",
		Files:  []*index.FileToIndex{{Path: "b.go", Language: "go", Content: []byte("func Add() {}\n")}},
	}); err != nil {
		t.Fatalf("Index r2: %v", err)
	}

	// Scoped to r1 → only r1 hits.
	res, err := idx.Search(ctx, &index.SearchQuery{Query: "Add", Repos: []string{"r1"}, Limit: 10})
	if err != nil {
		t.Fatalf("Search scoped: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("expected hits in r1")
	}
	for _, h := range res.Hits {
		if h.RepoID != "r1" {
			t.Errorf("got hit from repo %q, want only r1", h.RepoID)
		}
	}

	// Unscoped → both repos.
	res2, err := idx.Search(ctx, &index.SearchQuery{Query: "Add", Limit: 10})
	if err != nil {
		t.Fatalf("Search unscoped: %v", err)
	}
	seen := map[string]bool{}
	for _, h := range res2.Hits {
		seen[h.RepoID] = true
	}
	if !seen["r1"] || !seen["r2"] {
		t.Errorf("unscoped search missing repos, saw: %v", seen)
	}
}

func TestBM25RemovePath(t *testing.T) {
	idx, err := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()
	ctx := context.Background()

	files := []*index.FileToIndex{
		{Path: "a.go", Language: "go", Content: []byte("func Add() {}\n")},
		{Path: "b.go", Language: "go", Content: []byte("func Add() {}\n")},
	}
	if _, err := idx.Index(ctx, &index.IndexRequest{RepoID: "r1", Files: files}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	if err := idx.Remove(ctx, "r1", []string{"a.go"}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	res, err := idx.Search(ctx, &index.SearchQuery{Query: "Add", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("expected b.go to remain after removing a.go")
	}
	for _, h := range res.Hits {
		if h.FilePath == "a.go" {
			t.Errorf("a.go chunk still present after Remove")
		}
	}
}

func TestBM25ReindexRemovesStaleChunks(t *testing.T) {
	idx, err := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()
	ctx := context.Background()

	// 70 lines → 2 window chunks; unique token lives in the second chunk.
	var b strings.Builder
	for i := 0; i < 70; i++ {
		if i == 65 {
			b.WriteString("UNIQUETOKEN payload\n")
		} else {
			b.WriteString("filler line here\n")
		}
	}
	if _, err := idx.Index(ctx, &index.IndexRequest{
		RepoID: "r1",
		Files:  []*index.FileToIndex{{Path: "big.go", Language: "go", Content: []byte(b.String())}},
	}); err != nil {
		t.Fatalf("Index big: %v", err)
	}
	// Re-index same path with tiny content — the unique token must disappear.
	if _, err := idx.Index(ctx, &index.IndexRequest{
		RepoID: "r1",
		Files:  []*index.FileToIndex{{Path: "big.go", Language: "go", Content: []byte("small file now\n")}},
	}); err != nil {
		t.Fatalf("Index small: %v", err)
	}
	res, err := idx.Search(ctx, &index.SearchQuery{Query: "UNIQUETOKEN", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) != 0 {
		t.Errorf("stale chunk still found: %d hits", len(res.Hits))
	}
}

// filler builds a file of n lines, with marks[line] written on the lines it
// names so a query can tell which chunk of it survived.
func filler(n int, marks map[int]string) []byte {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if mark, ok := marks[i]; ok {
			b.WriteString(mark + " payload\n")
			continue
		}
		b.WriteString("filler line here\n")
	}
	return []byte(b.String())
}

// chunkCount is what the indexer's own chunker makes of this content, so a
// document-count assertion states the expected total rather than restating the
// window arithmetic.
func chunkCount(t *testing.T, content []byte) int {
	t.Helper()
	chunks, err := index.NewWindowChunker(index.ChunkConfig{}).Chunk(context.Background(),
		&index.ChunkInput{Path: "x", Content: content})
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	return len(chunks)
}

func docCount(t *testing.T, idx *Indexer) int {
	t.Helper()
	stats, err := idx.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	return int(stats.Documents)
}

func hitPaths(t *testing.T, idx *Indexer, q string) []string {
	t.Helper()
	res, err := idx.Search(context.Background(), &index.SearchQuery{Query: q, Limit: 50})
	if err != nil {
		t.Fatalf("Search(%q): %v", q, err)
	}
	paths := make([]string, 0, len(res.Hits))
	for _, h := range res.Hits {
		paths = append(paths, h.FilePath)
	}
	return paths
}

// One scan covers the whole window, so what it removes has to be exactly the
// chunks the window stops writing: not the chunks a rewritten file keeps, and
// not a file the window never mentions.
func TestBM25ReindexWindowDropsOnlyStaleChunks(t *testing.T) {
	idx, err := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()
	ctx := context.Background()

	before := map[string][]byte{
		"shrinks.go":   filler(200, map[int]string{5: "HEADALPHA", 190: "TAILALPHA"}),
		"grows.go":     filler(60, map[int]string{10: "HEADBETA"}),
		"steady.go":    filler(120, map[int]string{100: "MIDGAMMA"}),
		"untouched.go": filler(150, map[int]string{140: "TAILDELTA"}),
	}
	files := func(paths ...string) []*index.FileToIndex {
		out := make([]*index.FileToIndex, 0, len(paths))
		for _, p := range paths {
			out = append(out, &index.FileToIndex{Path: p, Content: before[p]})
		}
		return out
	}
	if _, err := idx.Index(ctx, &index.IndexRequest{
		RepoID: "r1",
		Files:  files("shrinks.go", "grows.go", "steady.go", "untouched.go"),
	}); err != nil {
		t.Fatalf("Index first window: %v", err)
	}

	// Second window: one file shrinks past its own chunks, one grows past
	// them, one is rewritten unchanged, and untouched.go is not in it at all.
	before["shrinks.go"] = filler(30, map[int]string{5: "HEADEPSILON"})
	before["grows.go"] = filler(300, map[int]string{10: "HEADBETA", 290: "TAILZETA"})
	if _, err := idx.Index(ctx, &index.IndexRequest{
		RepoID: "r1",
		Files:  files("shrinks.go", "grows.go", "steady.go"),
	}); err != nil {
		t.Fatalf("Index second window: %v", err)
	}

	for _, tt := range []struct {
		query string
		want  []string
	}{
		{"TAILALPHA", nil},                      // chunk the shrunken file no longer has
		{"HEADEPSILON", []string{"shrinks.go"}}, // its surviving chunk, rewritten in the same batch
		{"TAILZETA", []string{"grows.go"}},      // a chunk only the larger file has
		{"HEADBETA", []string{"grows.go"}},
		{"MIDGAMMA", []string{"steady.go"}},
		{"TAILDELTA", []string{"untouched.go"}}, // absent from the window, so untouched
	} {
		if got := hitPaths(t, idx, tt.query); !slices.Equal(got, tt.want) {
			t.Errorf("Search(%q) hit %v, want %v", tt.query, got, tt.want)
		}
	}

	want := 0
	for _, content := range before {
		want += chunkCount(t, content)
	}
	if got := docCount(t, idx); got != want {
		t.Errorf("document count = %d, want %d", got, want)
	}
}

// The scan is scoped to one repository: two repositories holding the same path
// are independent, and re-indexing one must not empty the other.
func TestBM25ReindexKeepsSamePathInOtherRepo(t *testing.T) {
	idx, err := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()
	ctx := context.Background()

	shared := filler(200, map[int]string{190: "TAILSHARED"})
	for _, repo := range []string{"r1", "r2"} {
		if _, err := idx.Index(ctx, &index.IndexRequest{
			RepoID: repo,
			Files:  []*index.FileToIndex{{Path: "shared.go", Content: shared}},
		}); err != nil {
			t.Fatalf("Index %s: %v", repo, err)
		}
	}

	if _, err := idx.Index(ctx, &index.IndexRequest{
		RepoID: "r1",
		Files:  []*index.FileToIndex{{Path: "shared.go", Content: filler(10, nil)}},
	}); err != nil {
		t.Fatalf("Index r1 again: %v", err)
	}

	res, err := idx.Search(ctx, &index.SearchQuery{Query: "TAILSHARED", Limit: 50})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) != 1 || res.Hits[0].RepoID != "r2" {
		t.Errorf("got %d hits (first repo %q), want r2's chunk alone", len(res.Hits), res.Hits[0].RepoID)
	}
	if got, want := docCount(t, idx), chunkCount(t, filler(10, nil))+chunkCount(t, shared); got != want {
		t.Errorf("document count = %d, want %d", got, want)
	}
}

// A file with more chunks than one scan page is the case paging exists for:
// the ids come back over several requests, and a stale chunk on any page but
// the first would otherwise survive.
func TestBM25ChunkScanPagesThroughEveryChunk(t *testing.T) {
	original := chunkScanPage
	chunkScanPage = 3
	defer func() { chunkScanPage = original }()

	idx, err := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()
	ctx := context.Background()

	big := filler(600, map[int]string{590: "TAILWIDE"})
	if n := chunkCount(t, big); n <= 2*chunkScanPage {
		t.Fatalf("file makes %d chunks, need more than %d for the scan to page", n, 2*chunkScanPage)
	}
	if _, err := idx.Index(ctx, &index.IndexRequest{
		RepoID: "r1",
		Files:  []*index.FileToIndex{{Path: "wide.go", Content: big}},
	}); err != nil {
		t.Fatalf("Index big: %v", err)
	}

	small := filler(10, nil)
	if _, err := idx.Index(ctx, &index.IndexRequest{
		RepoID: "r1",
		Files:  []*index.FileToIndex{{Path: "wide.go", Content: small}},
	}); err != nil {
		t.Fatalf("Index small: %v", err)
	}

	if got := hitPaths(t, idx, "TAILWIDE"); len(got) != 0 {
		t.Errorf("stale chunk survived paging: %v", got)
	}
	if got, want := docCount(t, idx), chunkCount(t, small); got != want {
		t.Errorf("document count = %d, want %d", got, want)
	}
}

// More paths than one query names, so Remove has to group them.
func TestBM25RemoveMorePathsThanOneQueryNames(t *testing.T) {
	idx, err := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()
	ctx := context.Background()

	const nFiles = chunkScanPaths + 3
	files := make([]*index.FileToIndex, 0, nFiles)
	paths := make([]string, 0, nFiles)
	for i := 0; i < nFiles; i++ {
		path := fmt.Sprintf("f%04d.txt", i)
		files = append(files, &index.FileToIndex{Path: path, Content: []byte("KEEPTOKEN body\n")})
		paths = append(paths, path)
	}
	if _, err := idx.Index(ctx, &index.IndexRequest{RepoID: "r1", Files: files}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	if got := docCount(t, idx); got != nFiles {
		t.Fatalf("document count = %d, want %d", got, nFiles)
	}

	if err := idx.Remove(ctx, "r1", paths[:nFiles-1]); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := docCount(t, idx); got != 1 {
		t.Errorf("document count = %d, want the one file that was kept", got)
	}
	if got, want := hitPaths(t, idx, "KEEPTOKEN"), []string{paths[nFiles-1]}; !slices.Equal(got, want) {
		t.Errorf("remaining hits = %v, want %v", got, want)
	}
}

// bleveIndex is embedded under an alias because the interface has a method
// named Index, which a field of that name would shadow.
type bleveIndex = bleve.Index

// countingIndex counts the searches an operation puts through the index.
type countingIndex struct {
	bleveIndex
	searches int
}

func (c *countingIndex) Search(req *bleve.SearchRequest) (*bleve.SearchResult, error) {
	c.searches++
	return c.bleveIndex.Search(req)
}

// What the batched scan is for: a window costs one scan however many files it
// holds. One per file meant 512 searches read from the index that same window
// was about to be written into.
func TestBM25WindowCostsOneChunkScan(t *testing.T) {
	idx, err := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()
	ctx := context.Background()

	files := make([]*index.FileToIndex, 0, 64)
	for i := 0; i < 64; i++ {
		files = append(files, &index.FileToIndex{
			Path:    fmt.Sprintf("pkg%02d/file.txt", i),
			Content: []byte("body of the file\n"),
		})
	}

	counter := &countingIndex{bleveIndex: idx.index}
	idx.index = counter

	for _, pass := range []string{"first", "second"} {
		counter.searches = 0
		if _, err := idx.Index(ctx, &index.IndexRequest{RepoID: "r1", Files: files}); err != nil {
			t.Fatalf("Index %s pass: %v", pass, err)
		}
		if counter.searches != 1 {
			t.Errorf("%s pass over %d files issued %d searches, want 1", pass, len(files), counter.searches)
		}
	}
}

// The AST indexer parses every file of a window anyway, so a file whose
// symbols it published must not be parsed a second time here — that duplicate
// parse was 14.8% of the CPU of a full pass over Elasticsearch.
func TestBM25AnnotatesFromPublishedSymbols(t *testing.T) {
	idx, err := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()
	ctx := context.Background()

	content := []byte("package demo\n\n// Add adds.\nfunc Add(a, b int) int {\n\treturn a + b\n}\n")
	// A name no parser would produce, so a hit carrying it can only have come
	// from the published symbols.
	index.SharedSymbols.Put("r1", "calc.go", "hash-1", []index.Symbol{
		{Name: "Published", Qualified: "demo.Published", Kind: "function", StartLine: 1, EndLine: 6},
	})

	reusedBefore := testutil.ToFloat64(symbolsReused)
	parsedBefore := testutil.ToFloat64(symbolsParsed)
	if _, err := idx.Index(ctx, &index.IndexRequest{
		RepoID: "r1",
		Files:  []*index.FileToIndex{{Path: "calc.go", Language: "go", Hash: "hash-1", Content: content}},
	}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	res, err := idx.Search(ctx, &index.SearchQuery{Query: "Add", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("expected a hit")
	}
	if got := res.Hits[0].Symbol; got != "demo.Published" {
		t.Errorf("Symbol = %q, want the published demo.Published", got)
	}
	if got := testutil.ToFloat64(symbolsReused) - reusedBefore; got != 1 {
		t.Errorf("reused counter delta = %g, want 1", got)
	}
	if got := testutil.ToFloat64(symbolsParsed) - parsedBefore; got != 0 {
		t.Errorf("parsed counter delta = %g, want 0: the file's symbols were already published", got)
	}
}

// Running without the AST indexer (or ahead of it) is the fallback, not a
// failure: the file is parsed here and annotated exactly as before.
func TestBM25ParsesWhenNoSymbolsPublished(t *testing.T) {
	idx, err := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()
	ctx := context.Background()

	content := []byte("package demo\n\n// Add adds.\nfunc Add(a, b int) int {\n\treturn a + b\n}\n")

	parsedBefore := testutil.ToFloat64(symbolsParsed)
	if _, err := idx.Index(ctx, &index.IndexRequest{
		RepoID: "r-unpublished",
		Files:  []*index.FileToIndex{{Path: "calc.go", Language: "go", Hash: "hash-2", Content: content}},
	}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	res, err := idx.Search(ctx, &index.SearchQuery{Query: "Add", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("expected a hit")
	}
	if got := res.Hits[0].Symbol; !strings.Contains(got, "Add") {
		t.Errorf("Symbol = %q, want it to name Add", got)
	}
	if got := testutil.ToFloat64(symbolsParsed) - parsedBefore; got != 1 {
		t.Errorf("parsed counter delta = %g, want 1", got)
	}
}

// Whichever way the symbols arrived, the indexed documents must be identical —
// the handoff is an optimization, not a change of output.
func TestBM25PublishedAndParsedAnnotationsMatch(t *testing.T) {
	ctx := context.Background()
	content := []byte("package demo\n\ntype T struct{ N int }\n\n" +
		"func (t *T) Describe() string { return \"x\" }\n\n" +
		"func Helper() int {\n\treturn 1\n}\n")
	file := func() *index.FileToIndex {
		return &index.FileToIndex{Path: "demo.go", Language: "go", Hash: "h", Content: content}
	}

	annotate := func(t *testing.T, publish bool) map[string]string {
		t.Helper()
		idx, err := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer idx.Close()

		repoID := "parsed"
		if publish {
			repoID = "published"
			units, _, err := ast.GetParserForLanguage("go").Parse("demo.go", string(content))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			index.SharedSymbols.Put(repoID, "demo.go", "h", index.ProjectSymbols(units))
		}
		if _, err := idx.Index(ctx, &index.IndexRequest{RepoID: repoID, Files: []*index.FileToIndex{file()}}); err != nil {
			t.Fatalf("Index: %v", err)
		}
		res, err := idx.Search(ctx, &index.SearchQuery{Query: "Describe Helper T", Limit: 50})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(res.Hits) == 0 {
			t.Fatal("expected hits")
		}
		out := map[string]string{}
		for _, h := range res.Hits {
			out[fmt.Sprintf("%d-%d", h.Line, h.EndLine)] = h.Symbol + "|" + h.Kind
		}
		return out
	}

	fromCache := annotate(t, true)
	fromParse := annotate(t, false)
	if len(fromCache) != len(fromParse) {
		t.Fatalf("annotated %d chunks from the cache and %d from a parse", len(fromCache), len(fromParse))
	}
	for span, want := range fromParse {
		if got := fromCache[span]; got != want {
			t.Errorf("chunk %s: from cache %q, from parse %q", span, got, want)
		}
	}
}

// storageStub swallows the AST indexer's writes: this test runs it only for
// the symbols it publishes on the way. Every other method panics through the
// embedded nil interface.
type storageStub struct{ store.Storage }

func (storageStub) DeleteASTUnitsByFiles(context.Context, string, []string) error { return nil }
func (storageStub) DeleteEdgesByFiles(context.Context, string, []string) error    { return nil }
func (storageStub) BatchStoreASTUnits(context.Context, []*domain.ASTUnit) error   { return nil }
func (storageStub) BatchStoreEdges(context.Context, []*domain.Edge) error         { return nil }

// Both indexers run over the same window at the same time. Neither may wait
// for the other: whichever order they happen to run in, this must terminate
// and every chunk must end up with the symbol that covers it — from the cache
// when the AST indexer got there first, from a local parse when it did not.
func TestBM25RunsConcurrentlyWithASTIndexer(t *testing.T) {
	const nFiles = 24
	ctx := context.Background()

	build := func() []*index.FileToIndex {
		files := make([]*index.FileToIndex, 0, nFiles)
		for i := 0; i < nFiles; i++ {
			content := fmt.Sprintf("package p%d\n\nfunc Fn%02d() int {\n\treturn %d\n}\n", i, i, i)
			files = append(files, &index.FileToIndex{
				Path:     fmt.Sprintf("pkg%02d/file.go", i),
				Language: "go",
				Hash:     fmt.Sprintf("h%02d", i),
				Content:  []byte(content),
			})
		}
		return files
	}

	idx, err := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()

	astIdx := ast.New(&ast.Config{Storage: storageStub{}, Workers: 4})
	astIdx.RegisterParser(ast.NewTreeSitterParser("go"))

	reusedBefore := testutil.ToFloat64(symbolsReused)
	parsedBefore := testutil.ToFloat64(symbolsParsed)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := astIdx.Index(ctx, &index.IndexRequest{RepoID: "shared", Files: build()}); err != nil {
			t.Errorf("ast Index: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := idx.Index(ctx, &index.IndexRequest{RepoID: "shared", Files: build()}); err != nil {
			t.Errorf("bm25 Index: %v", err)
		}
	}()
	wg.Wait()

	// Every file was annotated exactly once, either way round.
	reused := testutil.ToFloat64(symbolsReused) - reusedBefore
	parsed := testutil.ToFloat64(symbolsParsed) - parsedBefore
	if reused+parsed != nFiles {
		t.Errorf("annotated %g files (%g reused, %g parsed), want %d", reused+parsed, reused, parsed, nFiles)
	}

	for i := 0; i < nFiles; i++ {
		name := fmt.Sprintf("Fn%02d", i)
		res, err := idx.Search(ctx, &index.SearchQuery{Query: name, Repos: []string{"shared"}, Limit: 5})
		if err != nil {
			t.Fatalf("Search %s: %v", name, err)
		}
		if len(res.Hits) == 0 {
			t.Fatalf("no hit for %s", name)
		}
		if got := res.Hits[0].Symbol; !strings.Contains(got, name) {
			t.Errorf("%s: Symbol = %q, want it to name the function", name, got)
		}
	}
}

// TestSearchRecoversFromDamagedSegment pins the containment: a corrupt on-disk
// segment makes bleve's postings reader index out of range instead of
// reporting an overflow, and an unrecovered panic there turns every keyword
// query into a 500. It must surface as an error so hybrid search can drop the
// keyword leg and still answer.
func TestSearchRecoversFromDamagedSegment(t *testing.T) {
	idx, nerr := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75})
	if nerr != nil {
		t.Fatalf("New() error = %v", nerr)
	}
	if err := idx.Init(context.Background(), nil); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	// Closing the underlying index makes bleve operate on freed state; whether
	// that panics or errors, Search must return an error and not unwind.
	if err := idx.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	res, err := idx.Search(context.Background(), &index.SearchQuery{Query: "anything", Limit: 5})
	if err == nil {
		t.Fatalf("Search() on a closed index returned no error (res=%+v)", res)
	}
	if res != nil {
		t.Errorf("Search() returned a result alongside the error: %+v", res)
	}
}

// TestNewOnNonEmptyDirWithoutMetadata pins the diagnosis a half-written or
// foreign index gets. bleve.New refuses a non-empty path, so falling through to
// it reported "cannot create new index, path already exists" — the symptom of
// our own fallback rather than the cause. An empty directory, which is what a
// freshly mounted volume looks like, must still create the index.
func TestNewOnNonEmptyDirWithoutMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stray.zap"), []byte("not an index"), 0o600); err != nil {
		t.Fatalf("seed dir: %v", err)
	}

	_, err := New(&Config{Path: dir, K1: 1.2, B: 0.75})
	if err == nil {
		t.Fatal("New() on a non-empty directory without metadata returned no error")
	}
	if !errors.Is(err, ErrIndexDamaged) {
		t.Errorf("error = %v, want it to wrap ErrIndexDamaged", err)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error does not name the path: %v", err)
	}

	empty := t.TempDir()
	idx, err := New(&Config{Path: empty, K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("New() on an empty existing directory = %v, want it to create the index", err)
	}
	_ = idx.Close()
}

// TestDamagedSegmentClassification pins which read failures count as a damaged
// index. A segment whose bytes do not decode reports itself two ways — the
// postings reader panics, or it returns the decode failure as an ordinary
// error. Only the panic was recognised, so the error variant propagated as a
// generic failure and became an anonymous 500 on every query touching the
// segment. Both messages below are verbatim from a search that hit one.
func TestDamagedSegmentClassification(t *testing.T) {
	damaged := []string{
		"error reading frequency: memUvarintReader overflow",
		"error optimized currChunkNext: error reading freqHasLocs: memUvarintReader overflow",
	}
	for _, msg := range damaged {
		if !damagedSegment(errors.New(msg)) {
			t.Errorf("damagedSegment(%q) = false, want true", msg)
		}
	}

	// An ordinary failure must stay ordinary: classifying it as damage would
	// advertise a forced reindex as the repair for something a reindex cannot fix.
	ordinary := []string{
		"context canceled",
		"no such field 'symbol'",
		"syntax error parsing query",
	}
	for _, msg := range ordinary {
		if damagedSegment(errors.New(msg)) {
			t.Errorf("damagedSegment(%q) = true, want false", msg)
		}
	}

	if damagedSegment(nil) {
		t.Error("damagedSegment(nil) = true, want false")
	}
}

// TestErrIndexDamagedIsShared keeps the sentinel recognisable outside this
// package: the search service and the HTTP layer match on it to answer with a
// named, actionable status instead of a generic internal error.
func TestErrIndexDamagedIsShared(t *testing.T) {
	if !errors.Is(ErrIndexDamaged, index.ErrIndexDamaged) {
		t.Error("bm25.ErrIndexDamaged does not match index.ErrIndexDamaged")
	}
	wrapped := fmt.Errorf("%w: %v", ErrIndexDamaged, errors.New("memUvarintReader overflow"))
	if !errors.Is(wrapped, index.ErrIndexDamaged) {
		t.Error("a wrapped damaged-index error does not match index.ErrIndexDamaged")
	}
}

// TestCloseTwiceDoesNotPanic pins the guard on Close. Bleve shuts its scorch
// index down by closing an unguarded channel, so an unguarded delegation
// panicked with "close of closed channel" on the second call — a shutdown that
// can run twice (an explicit close plus a deferred one, a cleanup plus its
// caller) took the process down instead of returning an error.
func TestCloseTwiceDoesNotPanic(t *testing.T) {
	idx, err := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Recovering keeps a regression legible and local: a panic here would
	// otherwise abort the whole test binary, taking every sibling test with it.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Close panicked on the second call: %v", r)
		}
	}()

	if err := idx.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
	if err := idx.Close(); err != nil {
		t.Errorf("third Close = %v, want nil", err)
	}
}

// TestCloseIsSafeConcurrently covers the racing shutdown — a signal handler and
// a deferred close arriving at once — which a plain "already closed" flag read
// outside a lock would not survive.
func TestCloseIsSafeConcurrently(t *testing.T) {
	idx, err := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const closers = 8
	errs := make([]error, closers)
	var wg sync.WaitGroup
	for n := range closers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errs[n] = fmt.Errorf("panic: %v", r)
				}
			}()
			errs[n] = idx.Close()
		}()
	}
	wg.Wait()

	for n, err := range errs {
		if err != nil {
			t.Errorf("concurrent Close %d: %v", n, err)
		}
	}
}
