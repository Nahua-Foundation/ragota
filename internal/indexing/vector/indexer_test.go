package vector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Nahua-Foundation/ragota/internal/indexing"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// fakeEmbedder returns a tiny deterministic vector per text.
type fakeEmbedder struct{}

func (fakeEmbedder) Name() string { return "fake" }
func (fakeEmbedder) Dim() int     { return 2 }

func (fakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{float32(len(texts[i])), 1}
	}
	return out, nil
}

// memVecStore is a minimal in-memory storage.VectorStorage. The indexer
// stores from concurrent embed workers, so it is guarded like a real backend.
type memVecStore struct {
	mu     sync.Mutex
	points []*storage.VectorPoint
}

func (m *memVecStore) Init(ctx context.Context) error { return nil }
func (m *memVecStore) Close() error                   { return nil }

func (m *memVecStore) Upsert(ctx context.Context, points []*storage.VectorPoint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.points = append(m.points, points...)
	return nil
}

func (m *memVecStore) Search(ctx context.Context, opts storage.VectorSearchOpts) ([]*storage.VectorResult, error) {
	results := make([]*storage.VectorResult, 0, len(m.points))
	for _, p := range m.points {
		results = append(results, &storage.VectorResult{
			ID:       p.ID,
			Score:    1,
			RepoID:   p.RepoID,
			FilePath: p.FilePath,
			Line:     p.StartLine,
			EndLine:  p.EndLine,
			Text:     p.Text,
			Metadata: p.Metadata,
		})
	}
	if opts.Limit > 0 && len(results) > opts.Limit {
		results = results[:opts.Limit]
	}
	return results, nil
}

func (m *memVecStore) Delete(ctx context.Context, repoID, filePath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.points[:0]
	for _, p := range m.points {
		if p.RepoID == repoID && (filePath == "" || p.FilePath == filePath) {
			continue
		}
		kept = append(kept, p)
	}
	m.points = kept
	return nil
}

func (m *memVecStore) Stats(ctx context.Context) (*storage.VectorStats, error) {
	return &storage.VectorStats{Documents: int64(len(m.points))}, nil
}

const goSource = `package demo

// Add adds two numbers.
func Add(a, b int) int {
	return a + b
}

// Sub subtracts b from a.
func Sub(a, b int) int {
	return a - b
}
`

func indexOne(t *testing.T, idx *Indexer, path, language, content string) *indexing.IndexResult {
	t.Helper()
	res, err := idx.Index(context.Background(), &indexing.IndexRequest{
		RepoID:   "repo1",
		RepoPath: t.TempDir(),
		Files: []*indexing.FileToIndex{
			{Path: path, Language: language, Content: []byte(content)},
		},
	})
	if err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	return res
}

func TestCardsModeBuildsSymbolCards(t *testing.T) {
	store := &memVecStore{}
	idx := New(&Config{
		Embedder: fakeEmbedder{},
		Storage:  store,
		Chunking: indexing.ChunkConfig{Method: "window", WindowLines: 60, Overlap: 10},
		Cards:    true,
	})

	res := indexOne(t, idx, "demo/math.go", "go", goSource)
	if res.FilesIndexed != 1 {
		t.Fatalf("FilesIndexed = %d, want 1 (errors: %v)", res.FilesIndexed, res.Errors)
	}

	bySymbol := map[string]*storage.VectorPoint{}
	for _, p := range store.points {
		bySymbol[p.Symbol] = p
	}
	for _, sym := range []string{"Add", "Sub"} {
		p, ok := bySymbol[sym]
		if !ok {
			t.Fatalf("no card point for symbol %s; points: %d", sym, len(store.points))
		}
		if p.Kind == "" {
			t.Errorf("card %s has empty kind", sym)
		}
		if p.StartLine <= 0 || p.EndLine < p.StartLine {
			t.Errorf("card %s has bad line range %d-%d", sym, p.StartLine, p.EndLine)
		}
		if !strings.Contains(p.Text, sym) {
			t.Errorf("card %s text does not mention the symbol:\n%s", sym, p.Text)
		}
		// The card header is "<path>\n<kind> <qualified>".
		header := strings.SplitN(p.Text, "\n", 3)
		if len(header) < 2 {
			t.Fatalf("card %s has no header:\n%s", sym, p.Text)
		}
		if header[0] != p.FilePath {
			t.Errorf("card %s first line %q is not its path %q", sym, header[0], p.FilePath)
		}
		if !strings.HasPrefix(header[1], p.Kind+" ") {
			t.Errorf("card %s second line %q does not start with kind %q", sym, header[1], p.Kind)
		}
	}
}

func TestCardsModeFallsBackToWindows(t *testing.T) {
	store := &memVecStore{}
	idx := New(&Config{
		Embedder: fakeEmbedder{},
		Storage:  store,
		Chunking: indexing.ChunkConfig{Method: "window", WindowLines: 5, Overlap: 1},
		Cards:    true,
	})

	// Markdown is not a code language: cards mode must fall back to windows.
	content := strings.Repeat("some documentation line\n", 12)
	res := indexOne(t, idx, "README.md", "markdown", content)
	if res.FilesIndexed != 1 {
		t.Fatalf("FilesIndexed = %d, want 1 (errors: %v)", res.FilesIndexed, res.Errors)
	}
	if len(store.points) < 2 {
		t.Fatalf("expected multiple window chunks, got %d", len(store.points))
	}
	for _, p := range store.points {
		if p.Symbol != "" {
			t.Errorf("window chunk unexpectedly has symbol %q", p.Symbol)
		}
	}
}

func TestWindowModeUnaffectedByCardsFlag(t *testing.T) {
	store := &memVecStore{}
	idx := New(&Config{
		Embedder: fakeEmbedder{},
		Storage:  store,
		Chunking: indexing.ChunkConfig{Method: "window", WindowLines: 60, Overlap: 10},
		Cards:    false,
	})

	res := indexOne(t, idx, "demo/math.go", "go", goSource)
	if res.FilesIndexed != 1 {
		t.Fatalf("FilesIndexed = %d, want 1 (errors: %v)", res.FilesIndexed, res.Errors)
	}
	for _, p := range store.points {
		if p.Symbol == "Add" || p.Symbol == "Sub" {
			t.Errorf("cards disabled but got a symbol card for %q", p.Symbol)
		}
	}
	if len(store.points) == 0 {
		t.Fatalf("no window chunks indexed")
	}
}

// TestReindexReplacesFilePoints pins the stale-point fix: point IDs embed the
// chunk's line range, so an edit that shifts lines produced a second point for
// the same function and the old one was never deleted.
func TestReindexReplacesFilePoints(t *testing.T) {
	store := &memVecStore{}
	idx := New(&Config{
		Embedder: fakeEmbedder{},
		Storage:  store,
		Chunking: indexing.ChunkConfig{Method: "window", WindowLines: 60, Overlap: 10},
		Cards:    true,
	})

	indexOne(t, idx, "demo/math.go", "go", goSource)
	first := len(store.points)
	if first == 0 {
		t.Fatal("nothing indexed")
	}

	// Same file, every unit shifted down by 12 lines.
	shifted := strings.Repeat("// padding\n", 12) + goSource
	indexOne(t, idx, "demo/math.go", "go", shifted)

	if len(store.points) != first {
		t.Errorf("point count = %d after re-index, want %d; old points were not deleted",
			len(store.points), first)
	}
	for _, p := range store.points {
		if p.Symbol == "Add" && p.StartLine < 12 {
			t.Errorf("stale point for Add at line %d survived the re-index", p.StartLine)
		}
	}
}

func TestReindexDoesNotDeleteOtherFiles(t *testing.T) {
	store := &memVecStore{}
	idx := New(&Config{
		Embedder: fakeEmbedder{},
		Storage:  store,
		Chunking: indexing.ChunkConfig{Method: "window", WindowLines: 60, Overlap: 10},
		Cards:    true,
	})

	indexOne(t, idx, "demo/math.go", "go", goSource)
	indexOne(t, idx, "demo/other.go", "go", goSource)
	indexOne(t, idx, "demo/math.go", "go", goSource)

	files := map[string]bool{}
	for _, p := range store.points {
		files[p.FilePath] = true
	}
	if !files["demo/other.go"] {
		t.Error("re-indexing one file removed another file's points")
	}
}

func TestVectorSearchFilters(t *testing.T) {
	store := &memVecStore{
		points: []*storage.VectorPoint{
			{ID: "1", RepoID: "r1", FilePath: "src/a.go", Language: "go", StartLine: 1, EndLine: 4,
				Metadata: map[string]string{"language": "go", "kind": "function", "symbol": "Add"}},
			{ID: "2", RepoID: "r1", FilePath: "lib/b.py", Language: "python", StartLine: 1, EndLine: 4,
				Metadata: map[string]string{"language": "python", "kind": "function", "symbol": "Add"}},
			{ID: "3", RepoID: "r1", FilePath: "src/c.go", Language: "go", StartLine: 1, EndLine: 4,
				Metadata: map[string]string{"language": "go", "kind": "struct", "symbol": "Calc"}},
		},
	}
	idx := New(&Config{Embedder: fakeEmbedder{}, Storage: store})

	tests := []struct {
		name   string
		filter map[string]interface{}
		want   []string
	}{
		{name: "no filter", want: []string{"src/a.go", "lib/b.py", "src/c.go"}},
		{name: "language", filter: map[string]interface{}{"language": "go"}, want: []string{"src/a.go", "src/c.go"}},
		{name: "languages list", filter: map[string]interface{}{"languages": []interface{}{"python", "go"}},
			want: []string{"src/a.go", "lib/b.py", "src/c.go"}},
		{name: "kind", filter: map[string]interface{}{"kind": "struct"}, want: []string{"src/c.go"}},
		{name: "path prefix", filter: map[string]interface{}{"path_prefix": "lib/"}, want: []string{"lib/b.py"}},
		{name: "language and kind", filter: map[string]interface{}{"language": "go", "kind": "function"},
			want: []string{"src/a.go"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := idx.Search(context.Background(), &indexing.SearchQuery{
				Query: "add", Limit: 10, Filter: tt.filter,
			})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			got := make([]string, len(res.Hits))
			for i, h := range res.Hits {
				got[i] = h.FilePath
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("hits = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestVectorSearchOverFetchesWhenFiltering: post-filtering a page of exactly
// Limit results would silently return fewer hits than asked for.
func TestVectorSearchOverFetchesWhenFiltering(t *testing.T) {
	store := &memVecStore{}
	for i := 0; i < 20; i++ {
		// Matching points sit past a Limit-sized page but inside the over-fetch.
		language := "python"
		if i >= 10 {
			language = "go"
		}
		store.points = append(store.points, &storage.VectorPoint{
			ID:       string(rune('a' + i)),
			RepoID:   "r1",
			FilePath: "f" + string(rune('a'+i)) + ".src",
			Metadata: map[string]string{"language": language},
		})
	}
	idx := New(&Config{Embedder: fakeEmbedder{}, Storage: store})

	res, err := idx.Search(context.Background(), &indexing.SearchQuery{
		Query: "q", Limit: 3, Filter: map[string]interface{}{"language": "go"},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) != 3 {
		t.Errorf("got %d hits, want 3; the matching points sit past the first page", len(res.Hits))
	}
}

func TestBuildCardsBodyCap(t *testing.T) {
	// A unit body longer than cardBodyLines must be truncated.
	var b strings.Builder
	b.WriteString("package demo\n\nfunc Big() {\n")
	for i := 0; i < 100; i++ {
		b.WriteString("\t_ = " + strings.Repeat("x", 3) + "\n")
	}
	b.WriteString("}\n")

	chunks := buildCards("big.go", "go", []byte(b.String()), nil)
	if len(chunks) == 0 {
		t.Fatalf("no cards built")
	}
	for _, c := range chunks {
		if got := len(strings.Split(c.Text, "\n")); got > cardBodyLines+5 {
			t.Errorf("card %s has %d lines, body cap %d not applied", c.SymbolName, got, cardBodyLines)
		}
	}
}

func TestTruncateForEmbed(t *testing.T) {
	if got := truncateForEmbed("short", 100); got != "short" {
		t.Errorf("short text changed: %q", got)
	}
	long := strings.Repeat("x", 100)
	if got := truncateForEmbed(long, 10); len(got) != 10 {
		t.Errorf("len = %d, want 10", len(got))
	}
	// Cutting inside a multi-byte rune must back off to the rune boundary.
	ru := strings.Repeat("я", 10) // 2 bytes each
	got := truncateForEmbed(ru, 5)
	if len(got) != 4 || !utf8.ValidString(got) {
		t.Errorf("multibyte cut = %q (len %d), want 4 valid bytes", got, len(got))
	}
}

// recordingEmbedder counts request sizes and can reject inputs containing a
// poison marker, imitating a strict serving endpoint.
type recordingEmbedder struct {
	mu       sync.Mutex
	requests []int
	poison   string
}

func (r *recordingEmbedder) Name() string { return "recording" }
func (r *recordingEmbedder) Dim() int     { return 2 }

func (r *recordingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	r.mu.Lock()
	r.requests = append(r.requests, len(texts))
	r.mu.Unlock()
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if r.poison != "" && strings.Contains(t, r.poison) {
			return nil, fmt.Errorf("rejected input")
		}
		out[i] = []float32{float32(len(t)), 1}
	}
	return out, nil
}

func manyFiles(n int) []*indexing.FileToIndex {
	files := make([]*indexing.FileToIndex, n)
	for i := range files {
		files[i] = &indexing.FileToIndex{
			Path:     fmt.Sprintf("pkg/f%03d.go", i),
			Language: "go",
			Content:  []byte(fmt.Sprintf("package p\n\n// F%d does one thing.\nfunc F%d() {}\n", i, i)),
		}
	}
	return files
}

// TestIndexPacksSmallFiles: one-chunk files must not become one-text embed
// requests — the packing is the whole point of the pipeline.
func TestIndexPacksSmallFiles(t *testing.T) {
	store := &memVecStore{}
	emb := &recordingEmbedder{}
	idx := New(&Config{
		Embedder: emb,
		Storage:  store,
		Chunking: indexing.ChunkConfig{Method: "window", WindowLines: 60, Overlap: 10},
	})

	res, err := idx.Index(context.Background(), &indexing.IndexRequest{
		RepoID: "repo1", RepoPath: t.TempDir(), Files: manyFiles(30),
	})
	if err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	if res.FilesIndexed != 30 || res.FilesFailed != 0 {
		t.Fatalf("indexed/failed = %d/%d, want 30/0 (errors: %v)", res.FilesIndexed, res.FilesFailed, res.Errors)
	}
	total, biggest := 0, 0
	for _, n := range emb.requests {
		total += n
		if n > biggest {
			biggest = n
		}
	}
	if total != 30 {
		t.Fatalf("embedded %d texts, want 30 (requests: %v)", total, emb.requests)
	}
	if len(emb.requests) >= 30 || biggest < 2 {
		t.Errorf("no packing happened: %d requests, biggest %d (want fewer requests than files)", len(emb.requests), biggest)
	}
	if got := len(store.points); got != 30 {
		t.Errorf("stored %d points, want 30", got)
	}
}

// TestIndexGroupFailureFallsBackPerFile: a rejected input fails its own file
// only; the neighbours packed into the same request still index.
func TestIndexGroupFailureFallsBackPerFile(t *testing.T) {
	store := &memVecStore{}
	emb := &recordingEmbedder{poison: "POISON"}
	idx := New(&Config{
		Embedder:    emb,
		Storage:     store,
		Concurrency: 1,
		Chunking:    indexing.ChunkConfig{Method: "window", WindowLines: 60, Overlap: 10},
	})

	files := manyFiles(3)
	files[1].Content = []byte("package p\n\n// POISON\nfunc Bad() {}\n")
	res, err := idx.Index(context.Background(), &indexing.IndexRequest{
		RepoID: "repo1", RepoPath: t.TempDir(), Files: files,
	})
	if err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	if res.FilesIndexed != 2 || res.FilesFailed != 1 {
		t.Fatalf("indexed/failed = %d/%d, want 2/1 (errors: %v)", res.FilesIndexed, res.FilesFailed, res.Errors)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], files[1].Path) {
		t.Errorf("failure not attributed to the poisoned file: %v", res.Errors)
	}
	for _, p := range store.points {
		if p.FilePath == files[1].Path {
			t.Errorf("poisoned file stored points: %+v", p)
		}
	}
}

// gatedStore is a vector store whose writes block until they are released.
type gatedStore struct {
	memVecStore
	gate chan struct{}
}

func (g *gatedStore) Upsert(ctx context.Context, points []*storage.VectorPoint) error {
	<-g.gate
	return g.memVecStore.Upsert(ctx, points)
}

// signallingEmbedder reports every request on a channel, so a test can wait
// for embedding to have gone further than the store did.
type signallingEmbedder struct {
	calls chan struct{}
}

func (signallingEmbedder) Name() string { return "signalling" }
func (signallingEmbedder) Dim() int     { return 2 }

func (s *signallingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	s.calls <- struct{}{}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{float32(len(texts[i])), 1}
	}
	return out, nil
}

// TestStoreDoesNotBlockEmbedding pins the shape of the pipeline: writing a
// file's points is a stage of its own, so an embed worker hands the vectors on
// and goes straight back to the embedder.
//
// It matters because the endpoint serializes requests behind one accelerator.
// An embed worker that writes is an embed worker that is not embedding, and
// with the write inline the endpoint went idle whenever the workers were all
// talking to the vector store — which, measured over an Elasticsearch pass,
// was a quarter of their time.
//
// The store here never completes until released, so under the old shape the
// single embed worker would stop after its first file and the test would time
// out rather than fail.
func TestStoreDoesNotBlockEmbedding(t *testing.T) {
	const files = 8

	store := &gatedStore{gate: make(chan struct{})}
	emb := &signallingEmbedder{calls: make(chan struct{}, 64)}
	idx := New(&Config{
		Embedder:    emb,
		Storage:     store,
		Concurrency: 1,
		// Two-line windows so each file alone fills a packed request; with one
		// text per file the packer would put every file into one request and
		// the test would prove nothing.
		Chunking: indexing.ChunkConfig{Method: "window", WindowLines: 2, Overlap: 0},
	})

	big := make([]*indexing.FileToIndex, files)
	for i := range big {
		var b strings.Builder
		for line := 0; line < 2*packTargetTexts; line++ {
			fmt.Fprintf(&b, "// file %d line %d\n", i, line)
		}
		big[i] = &indexing.FileToIndex{
			Path: fmt.Sprintf("pkg/f%d.go", i), Language: "go", Content: []byte(b.String()),
		}
	}

	done := make(chan *indexing.IndexResult, 1)
	go func() {
		res, err := idx.Index(context.Background(), &indexing.IndexRequest{
			RepoID: "repo1", RepoPath: t.TempDir(), Files: big,
		})
		if err != nil {
			t.Errorf("Index() error = %v", err)
		}
		done <- res
	}()

	// Every file must reach the embedder while every write is still blocked.
	for i := 0; i < files; i++ {
		select {
		case <-emb.calls:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d files were embedded while the store was blocked: "+
				"the embed worker is waiting on the vector store", i, files)
		}
	}

	close(store.gate)
	res := <-done
	if res.FilesIndexed != files || res.FilesFailed != 0 {
		t.Fatalf("indexed/failed = %d/%d, want %d/0 (errors: %v)",
			res.FilesIndexed, res.FilesFailed, files, res.Errors)
	}
	if len(store.points) == 0 {
		t.Error("no points reached the store")
	}
}

// TestIndexExclude: an excluded path is skipped by the vector channel and
// counted as skipped, not failed.
func TestIndexExclude(t *testing.T) {
	store := &memVecStore{}
	idx := New(&Config{
		Embedder: fakeEmbedder{},
		Storage:  store,
		Exclude:  []string{"/vendor/", "_test."},
		Chunking: indexing.ChunkConfig{Method: "window", WindowLines: 60, Overlap: 10},
	})

	res, err := idx.Index(context.Background(), &indexing.IndexRequest{
		RepoID: "repo1", RepoPath: t.TempDir(), Files: []*indexing.FileToIndex{
			{Path: "vendor/lib/a.go", Language: "go", Content: []byte("package a\nfunc A() {}\n")},
			{Path: "pkg/b_test.go", Language: "go", Content: []byte("package b\nfunc TestB() {}\n")},
			{Path: "pkg/b.go", Language: "go", Content: []byte("package b\nfunc B() {}\n")},
		},
	})
	if err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	if res.FilesIndexed != 1 || res.FilesSkipped != 2 || res.FilesFailed != 0 {
		t.Fatalf("indexed/skipped/failed = %d/%d/%d, want 1/2/0 (errors: %v)",
			res.FilesIndexed, res.FilesSkipped, res.FilesFailed, res.Errors)
	}
	for _, p := range store.points {
		if p.FilePath != "pkg/b.go" {
			t.Errorf("excluded file stored points: %s", p.FilePath)
		}
	}
}

// TestBuildCardsLeadWithPath: every card starts with the file it came from.
// A Go qualified name carries the package, and for a service's entry point the
// package is "main" — the directory is the only place the service is named, so
// a card without it cannot be retrieved by a question that names the service.
func TestBuildCardsLeadWithPath(t *testing.T) {
	const path = "src/checkoutservice/main.go"
	src := "package main\n\nfunc main() {\n\tsvc := new(checkoutService)\n\t_ = svc\n}\n"

	cards := buildCards(path, "go", []byte(src), nil)
	if len(cards) == 0 {
		t.Fatal("no cards built")
	}
	for _, c := range cards {
		if !strings.HasPrefix(c.Text, path+"\n") {
			t.Errorf("card %s does not lead with its path:\n%s", c.SymbolName, c.Text)
		}
	}
}

// TestBuildCardsIncludesAnnotation: a symbol summary written at index time has
// to reach the card, or the whole pass is invisible to retrieval.
func TestBuildCardsIncludesAnnotation(t *testing.T) {
	src := "package main\n\nfunc canAllocate(shard int) bool {\n\treturn shard > 0\n}\n"
	note := "decides whether a shard may be placed on a node given its free disk space"
	ann := map[string]string{indexing.AnnotationKey("alloc.go", 3): note}

	cards := buildCards("alloc.go", "go", []byte(src), ann)
	if len(cards) == 0 {
		t.Fatal("no cards built")
	}
	var found bool
	for _, c := range cards {
		if strings.Contains(c.Text, note) {
			found = true
		}
	}
	if !found {
		t.Errorf("annotation missing from every card; first card:\n%s", cards[0].Text)
	}
	// A card whose symbol has no annotation must be unchanged.
	plain := buildCards("alloc.go", "go", []byte(src), nil)
	if len(plain) != len(cards) {
		t.Errorf("annotation changed the card count: %d vs %d", len(plain), len(cards))
	}
}
