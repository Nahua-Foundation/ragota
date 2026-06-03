package vector

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ragota/internal/indexing/chunker"
	"ragota/pkg/config"
	"ragota/pkg/qdrant"
	"ragota/pkg/state"
	"ragota/internal/store"
)

func TestPickCollection_CodeLanguages(t *testing.T) {
	cfg := testConfig(t)
	v := &Vector{cfg: cfg}
	for _, lang := range []string{"go", "typescript", "javascript", "python", "java", "proto", "json", "yaml", "toml"} {
		sp, _ := v.pickCollection(lang)
		assert.Equal(t, cfg.CodeCollection().Name, sp.Name, "lang=%s should use code collection", lang)
	}
}

func TestPickCollection_TextLanguages(t *testing.T) {
	cfg := testConfig(t)
	v := &Vector{cfg: cfg}
	for _, lang := range []string{"markdown", "rst", "txt", "unknown", ""} {
		sp, _ := v.pickCollection(lang)
		assert.Equal(t, cfg.TextCollection().Name, sp.Name, "lang=%q should use text collection", lang)
	}
}

func TestCodeCollectionLanguages(t *testing.T) {
	expected := []string{"go", "typescript", "javascript", "python", "java", "proto", "json", "yaml", "toml"}
	for _, lang := range expected {
		assert.True(t, codeCollectionLanguages[lang], "expected %s in codeCollectionLanguages", lang)
	}
	notExpected := []string{"markdown", "rst", "txt", "ruby", "rust", ""}
	for _, lang := range notExpected {
		assert.False(t, codeCollectionLanguages[lang], "did not expect %s in codeCollectionLanguages", lang)
	}
}

func TestNewVector(t *testing.T) {
	cfg := testConfig(t)
	ollamaSrv := mockOllamaServer(t, 4)
	cfg.Ollama.URL = ollamaSrv.URL
	st := openTestStore(t)
	qd := qdrant.New("http://localhost:1")
	bus := state.NewBus(t.TempDir())
	v := NewVector(cfg, qd, nil, st, bus)
	require.NotNil(t, v)
	assert.NotNil(t, v.sem)
	assert.NotNil(t, v.parser)
	assert.NotNil(t, v.chunker)
	assert.NotNil(t, v.matcher)
	assert.NotNil(t, v.store)
	assert.Nil(t, v.resolv)
}

func TestVector_GetSemaphore(t *testing.T) {
	cfg := testConfig(t)
	cfg.EmbedParallelism = 8
	st := openTestStore(t)
	qd := qdrant.New("http://localhost:1")
	v := NewVector(cfg, qd, nil, st, nil)
	sem := v.GetSemaphore()
	assert.Equal(t, 8, cap(sem))
}

// mockWriteSink implements WriteSink for testing.
type mockWriteSink struct{}

func (m mockWriteSink) IndexDocs(context.Context, []Doc) error              { return nil }
func (m mockWriteSink) DeleteByPath(context.Context, string) error          { return nil }
func (m mockWriteSink) Clear(context.Context) error                         { return nil }
func (m mockWriteSink) Count(context.Context) (uint64, error)               { return 42, nil }
func (m mockWriteSink) Search(context.Context, SearchQuery) ([]SearchResult, error) {
	return nil, nil
}

func TestVector_SetBM25_And_writeSink(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	qd := qdrant.New("http://localhost:1")
	v := NewVector(cfg, qd, nil, st, nil)
	assert.Nil(t, v.writeSink())
	v.SetBM25(mockWriteSink{})
	assert.NotNil(t, v.writeSink())
	assert.Equal(t, uint64(42), mustCount(v.writeSink()))
}

func mustCount(s WriteSink) uint64 {
	n, err := s.Count(context.Background())
	if err != nil {
		return 0
	}
	return n
}

func TestVector_Close_Empty(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	qd := qdrant.New("http://localhost:1")
	v := NewVector(cfg, qd, nil, st, nil)
	v.Close()
}

func TestVector_Init_Success(t *testing.T) {
	cfg := testConfig(t)
	qdrantSrv := mockQdrantServer(t)
	ollamaSrv := mockOllamaServer(t, 4)
	cfg.Ollama.URL = ollamaSrv.URL
	st := openTestStore(t)
	qd := qdrant.New(qdrantSrv.URL)
	bus := state.NewBus(t.TempDir())
	v := NewVector(cfg, qd, nil, st, bus)
	err := v.Init(context.Background())
	assert.NoError(t, err)
}

func TestVector_Init_EmbedModelChange(t *testing.T) {
	cfg := testConfig(t)
	qdrantSrv := mockQdrantServer(t)
	ollamaSrv := mockOllamaServer(t, 4)
	cfg.Ollama.URL = ollamaSrv.URL
	st := openTestStore(t)
	codeSpec := cfg.CodeCollection()
	err := st.SetEmbedMeta(context.Background(), store.EmbedMeta{
		Collection: codeSpec.Name, Model: "old-model", Dim: 512,
	})
	require.NoError(t, err)
	qd := qdrant.New(qdrantSrv.URL)
	v := NewVector(cfg, qd, nil, st, nil)
	err = v.Init(context.Background())
	assert.NoError(t, err)
	meta, err := st.GetEmbedMeta(context.Background(), codeSpec.Name)
	require.NoError(t, err)
	require.NotNil(t, meta)
	assert.Equal(t, codeSpec.EmbedModel, meta.Model)
	assert.Equal(t, int(codeSpec.EmbedDim), meta.Dim)
}

func TestVector_SyncStats(t *testing.T) {
	cfg := testConfig(t)
	qdrantSrv := mockQdrantServer(t)
	ollamaSrv := mockOllamaServer(t, 4)
	cfg.Ollama.URL = ollamaSrv.URL
	st := openTestStore(t)
	qd := qdrant.New(qdrantSrv.URL)
	bus := state.NewBus(t.TempDir())
	v := NewVector(cfg, qd, nil, st, bus)
	v.SyncStats(context.Background())
	snap := bus.Snapshot()
	assert.NotNil(t, snap.Indexers["vector"])
}

func TestVector_Search_NoLanguage(t *testing.T) {
	cfg := testConfig(t)
	qdrantSrv := mockQdrantServer(t)
	ollamaSrv := mockOllamaServer(t, 4)
	cfg.Ollama.URL = ollamaSrv.URL
	st := openTestStore(t)
	qd := qdrant.New(qdrantSrv.URL)
	bus := state.NewBus(t.TempDir())
	v := NewVector(cfg, qd, nil, st, bus)
	hits, err := v.Search(context.Background(), "test query", 10, nil)
	assert.NoError(t, err)
	assert.Empty(t, hits)
}

func TestVector_Search_WithLanguage(t *testing.T) {
	cfg := testConfig(t)
	qdrantSrv := mockQdrantServer(t)
	ollamaSrv := mockOllamaServer(t, 4)
	cfg.Ollama.URL = ollamaSrv.URL
	st := openTestStore(t)
	qd := qdrant.New(qdrantSrv.URL)
	bus := state.NewBus(t.TempDir())
	v := NewVector(cfg, qd, nil, st, bus)
	hits, err := v.Search(context.Background(), "test query", 10, map[string]any{"language": "go"})
	assert.NoError(t, err)
	assert.Empty(t, hits)
}

func TestVector_Search_DefaultLimit(t *testing.T) {
	cfg := testConfig(t)
	qdrantSrv := mockQdrantServer(t)
	ollamaSrv := mockOllamaServer(t, 4)
	cfg.Ollama.URL = ollamaSrv.URL
	st := openTestStore(t)
	qd := qdrant.New(qdrantSrv.URL)
	bus := state.NewBus(t.TempDir())
	v := NewVector(cfg, qd, nil, st, bus)
	hits, err := v.Search(context.Background(), "test", 0, nil)
	assert.NoError(t, err)
	assert.NotNil(t, hits)
}

func TestVector_SimilarToUnit_NilStore(t *testing.T) {
	cfg := testConfig(t)
	v := &Vector{cfg: cfg, store: nil}
	units, err := v.SimilarToUnit(context.Background(), store.ASTUnit{}, 10)
	assert.NoError(t, err)
	assert.Nil(t, units)
}

func TestVector_SimilarToUnit_DefaultLimit(t *testing.T) {
	cfg := testConfig(t)
	qdrantSrv := mockQdrantServer(t)
	ollamaSrv := mockOllamaServer(t, 4)
	cfg.Ollama.URL = ollamaSrv.URL
	st := openTestStore(t)
	qd := qdrant.New(qdrantSrv.URL)
	bus := state.NewBus(t.TempDir())
	v := NewVector(cfg, qd, nil, st, bus)
	units, err := v.SimilarToUnit(context.Background(), store.ASTUnit{
		Language: "go", Name: "testFunc", Signature: "func testFunc()",
	}, 0)
	assert.NoError(t, err)
	assert.Empty(t, units)
}

func TestVector_ensureCollection_EmptyName(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	qd := qdrant.New("http://localhost:1")
	v := NewVector(cfg, qd, nil, st, nil)
	err := v.ensureCollection(context.Background(), config.CollectionSpec{Name: ""})
	assert.NoError(t, err)
}

func TestVector_FullScan_ConcurrentGuard(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	qd := qdrant.New("http://localhost:1")
	v := NewVector(cfg, qd, nil, st, nil)
	v.scanMu.Lock()
	v.scanning = true
	v.scanMu.Unlock()
	err := v.FullScan(context.Background())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already in progress")
	v.scanMu.Lock()
	v.scanning = false
	v.scanMu.Unlock()
}

func TestVector_FullScan_EmptyDir(t *testing.T) {
	cfg := testConfig(t)
	qdrantSrv := mockQdrantServer(t)
	ollamaSrv := mockOllamaServer(t, 4)
	cfg.Ollama.URL = ollamaSrv.URL
	st := openTestStore(t)
	qd := qdrant.New(qdrantSrv.URL)
	bus := state.NewBus(t.TempDir())
	v := NewVector(cfg, qd, nil, st, bus)
	err := v.FullScan(context.Background())
	assert.NoError(t, err)
}

func TestVector_IndexFile_CancelledContext(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	qd := qdrant.New("http://localhost:1")
	v := NewVector(cfg, qd, nil, st, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := v.IndexFile(ctx, "/some/file.go")
	assert.Error(t, err)
}

func TestVector_RemoveFile_CancelledContext(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	qd := qdrant.New("http://localhost:1")
	v := NewVector(cfg, qd, nil, st, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := v.RemoveFile(ctx, "/some/file.go")
	assert.Error(t, err)
}

func TestVector_RemoveFile_Success(t *testing.T) {
	cfg := testConfig(t)
	qdrantSrv := mockQdrantServer(t)
	st := openTestStore(t)
	qd := qdrant.New(qdrantSrv.URL)
	v := NewVector(cfg, qd, nil, st, nil)
	err := v.RemoveFile(context.Background(), "/some/file.go")
	assert.NoError(t, err)
}

func TestVector_prepareFile_NonExistent(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	qd := qdrant.New("http://localhost:1")
	v := NewVector(cfg, qd, nil, st, nil)
	pf, err := v.prepareFile(context.Background(), "/nonexistent/file.go")
	assert.NoError(t, err)
	assert.Nil(t, pf)
}

func TestVector_prepareFile_EmptyFile(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	qd := qdrant.New("http://localhost:1")
	v := NewVector(cfg, qd, nil, st, nil)
	goFile := filepath.Join(cfg.Root, "empty.go")
	err := os.WriteFile(goFile, []byte(""), 0644)
	require.NoError(t, err)
	pf, err := v.prepareFile(context.Background(), goFile)
	assert.NoError(t, err)
	assert.Nil(t, pf)
}

func TestVector_processBatch_EmptyFiles(t *testing.T) {
	cfg := testConfig(t)
	qdrantSrv := mockQdrantServer(t)
	ollamaSrv := mockOllamaServer(t, 4)
	cfg.Ollama.URL = ollamaSrv.URL
	st := openTestStore(t)
	qd := qdrant.New(qdrantSrv.URL)
	v := NewVector(cfg, qd, nil, st, nil)
	err := v.processBatch(context.Background(), cfg.CodeCollection(), v.code, nil, 0)
	assert.NoError(t, err)
}

func TestVector_processBatch_CancelledContext(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	qd := qdrant.New("http://localhost:1")
	v := NewVector(cfg, qd, nil, st, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := v.processBatch(ctx, cfg.CodeCollection(), v.code, []*preparedFile{{
		abs: "/test.go", rel: "test.go", lang: "go",
		chunks: []chunker.Chunk{{Text: "hello", StartLine: 1, EndLine: 1}},
	}}, 1)
	assert.Error(t, err)
}

func TestVector_processBatch_WithFiles(t *testing.T) {
	cfg := testConfig(t)
	qdrantSrv := mockQdrantServer(t)
	dim := int(cfg.CodeCollection().EmbedDim)
	ollamaSrv := mockOllamaServer(t, dim)
	cfg.Ollama.URL = ollamaSrv.URL
	st := openTestStore(t)
	qd := qdrant.New(qdrantSrv.URL)
	bus := state.NewBus(t.TempDir())
	v := NewVector(cfg, qd, nil, st, bus)
	files := []*preparedFile{{
		abs: "/test.go", rel: "test.go", lang: "go", hash: "abc123",
		collSpec: cfg.CodeCollection(), emb: v.code,
		chunks: []chunker.Chunk{{Text: "func Hello() {}", StartLine: 1, EndLine: 1, Kind: "function", Symbol: "Hello"}},
	}}
	err := v.processBatch(context.Background(), cfg.CodeCollection(), v.code, files, 1)
	assert.NoError(t, err)
}
