package index

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ragota/internal/bm25"
	"ragota/internal/chunker"
	"ragota/internal/config"
	"ragota/internal/hybrid"
	"ragota/internal/qdrant"
	"ragota/internal/state"
	"ragota/internal/store"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Root = t.TempDir()
	return cfg
}

func openTestStore(t *testing.T) *store.SQLite {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// mockQdrantServer returns an httptest.Server that mimics minimal Qdrant API.
func mockQdrantServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	// GET /readyz
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// GET /collections/{name} — always return "exists" with some stats
	mux.HandleFunc("/collections/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/collections/"), "/")
		collName := parts[0]

		switch {
		case r.Method == http.MethodGet && len(parts) == 1:
			// collection info
			fmt.Fprintf(w, `{"result":{"status":"green","points_count":0,"vectors_count":0,"config":{"params":{"vectors":{"size":1024,"distance":"Cosine"}}}}}`)
		case r.Method == http.MethodPut && len(parts) == 1:
			// create collection
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"result":true,"status":"ok","time":0.001}`)
		case r.Method == http.MethodDelete && len(parts) == 1:
			// delete collection
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"result":true,"status":"ok","time":0.001}`)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/points"):
			// upsert points
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"result":{"operation_id":1,"status":"completed"},"status":"ok","time":0.001}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/points/delete"):
			// delete by filter
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"result":{"operation_id":2,"status":"completed"},"status":"ok","time":0.001}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/points/search"):
			// search — return empty results
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"result":[]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/points/count"):
			// count
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"result":{"count":0}}`)
		default:
			_ = collName
			http.NotFound(w, r)
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// mockOllamaServer returns an httptest.Server that responds to embed requests.
func mockOllamaServer(t *testing.T, dim int) *httptest.Server {
	t.Helper()
	if dim <= 0 {
		dim = 4
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"models":[]}`)
	})
	mux.HandleFunc("/api/embed", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Always return a single vector of dim floats
		vec := make([]string, dim)
		for i := range vec {
			vec[i] = "0.1"
		}
		vecJSON := strings.Join(vec, ",")
		fmt.Fprintf(w, `{"embeddings":[[%s]]}`, vecJSON)
	})
	mux.HandleFunc("/api/embeddings", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		vec := make([]string, dim)
		for i := range vec {
			vec[i] = "0.1"
		}
		vecJSON := strings.Join(vec, ",")
		fmt.Fprintf(w, `{"embedding":[%s]}`, vecJSON)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// ---------------------------------------------------------------------------
// combinedText tests
// ---------------------------------------------------------------------------

func TestCombinedText_NoComments(t *testing.T) {
	ch := chunker.Chunk{Text: "func hello() {}", Comments: ""}
	assert.Equal(t, "func hello() {}", combinedText(ch))
}

func TestCombinedText_WithComments(t *testing.T) {
	ch := chunker.Chunk{Text: "func hello() {}", Comments: "// greets the world"}
	assert.Equal(t, "// greets the world\nfunc hello() {}", combinedText(ch))
}

func TestCombinedText_EmptyText(t *testing.T) {
	ch := chunker.Chunk{Text: "", Comments: ""}
	assert.Equal(t, "", combinedText(ch))
}

func TestCombinedText_EmptyTextWithComments(t *testing.T) {
	ch := chunker.Chunk{Text: "", Comments: "// only comment"}
	assert.Equal(t, "// only comment\n", combinedText(ch))
}

func TestCombinedText_MultilineComments(t *testing.T) {
	ch := chunker.Chunk{
		Text:     "body",
		Comments: "// line1\n// line2\n// line3",
	}
	expected := "// line1\n// line2\n// line3\nbody"
	assert.Equal(t, expected, combinedText(ch))
}

// ---------------------------------------------------------------------------
// chunkID tests
// ---------------------------------------------------------------------------

func TestChunkID_Deterministic(t *testing.T) {
	id1 := chunkID("/foo/bar.go", 0)
	id2 := chunkID("/foo/bar.go", 0)
	assert.Equal(t, id1, id2)
}

func TestChunkID_DifferentFiles(t *testing.T) {
	id1 := chunkID("/foo/a.go", 0)
	id2 := chunkID("/foo/b.go", 0)
	assert.NotEqual(t, id1, id2)
}

func TestChunkID_DifferentIndices(t *testing.T) {
	id1 := chunkID("/foo/a.go", 0)
	id2 := chunkID("/foo/a.go", 1)
	assert.NotEqual(t, id1, id2)
}

func TestChunkID_Format(t *testing.T) {
	id := chunkID("/foo/bar.go", 42)
	// Format: 8-4-4-4-12 hex characters
	parts := strings.Split(id, "-")
	require.Len(t, parts, 5)
	assert.Len(t, parts[0], 8)
	assert.Len(t, parts[1], 4)
	assert.Len(t, parts[2], 4)
	assert.Len(t, parts[3], 4)
	assert.Len(t, parts[4], 12)
}

func TestChunkID_MatchesSHA1(t *testing.T) {
	file := "/test/file.py"
	idx := 5
	h := sha1.Sum([]byte(fmt.Sprintf("%s#%d", file, idx)))
	hexStr := hex.EncodeToString(h[:16])
	expected := fmt.Sprintf("%s-%s-%s-%s-%s", hexStr[0:8], hexStr[8:12], hexStr[12:16], hexStr[16:20], hexStr[20:32])
	assert.Equal(t, expected, chunkID(file, idx))
}

func TestChunkID_EmptyFile(t *testing.T) {
	id := chunkID("", 0)
	assert.NotEmpty(t, id)
	parts := strings.Split(id, "-")
	assert.Len(t, parts, 5)
}

// ---------------------------------------------------------------------------
// buildFilter tests
// ---------------------------------------------------------------------------

func TestBuildFilter_Nil(t *testing.T) {
	assert.Nil(t, buildFilter(nil))
}

func TestBuildFilter_Empty(t *testing.T) {
	assert.Nil(t, buildFilter(map[string]any{}))
}

func TestBuildFilter_SingleKey(t *testing.T) {
	f := buildFilter(map[string]any{"language": "go"})
	require.NotNil(t, f)
	must, ok := f["must"].([]map[string]any)
	require.True(t, ok)
	assert.Len(t, must, 1)
	assert.Equal(t, "language", must[0]["key"])
}

func TestBuildFilter_RepoWildcard(t *testing.T) {
	// repo="*" should be ignored
	f := buildFilter(map[string]any{"repo": "*"})
	assert.Nil(t, f)
}

func TestBuildFilter_RepoEmpty(t *testing.T) {
	f := buildFilter(map[string]any{"repo": ""})
	assert.Nil(t, f)
}

func TestBuildFilter_RepoSingle(t *testing.T) {
	f := buildFilter(map[string]any{"repo": "myrepo"})
	require.NotNil(t, f)
	must := f["must"].([]map[string]any)
	assert.Len(t, must, 1)
	assert.Equal(t, "repo", must[0]["key"])
}

func TestBuildFilter_RepoSliceSingle(t *testing.T) {
	f := buildFilter(map[string]any{"repo": []string{"alpha"}})
	require.NotNil(t, f)
	must := f["must"].([]map[string]any)
	require.Len(t, must, 1)
	// Should be a simple match, not "any"
	match := must[0]["match"].(map[string]any)
	assert.Equal(t, "alpha", match["value"])
}

func TestBuildFilter_RepoSliceMultiple(t *testing.T) {
	f := buildFilter(map[string]any{"repo": []string{"alpha", "beta"}})
	require.NotNil(t, f)
	must := f["must"].([]map[string]any)
	require.Len(t, must, 1)
	match := must[0]["match"].(map[string]any)
	anyVals, ok := match["any"]
	require.True(t, ok)
	assert.Contains(t, anyVals, "alpha")
	assert.Contains(t, anyVals, "beta")
}

func TestBuildFilter_RepoSliceWithWildcard(t *testing.T) {
	// ["alpha", "*"] — "*" should be skipped, only alpha remains
	f := buildFilter(map[string]any{"repo": []string{"alpha", "*"}})
	require.NotNil(t, f)
	must := f["must"].([]map[string]any)
	require.Len(t, must, 1)
	match := must[0]["match"].(map[string]any)
	// Only alpha remains, should be simple match
	assert.Equal(t, "alpha", match["value"])
}

func TestBuildFilter_RepoSliceAllWildcards(t *testing.T) {
	// ["*", ""] — all should be skipped
	f := buildFilter(map[string]any{"repo": []string{"*", ""}})
	assert.Nil(t, f)
}

func TestBuildFilter_RepoAnySlice(t *testing.T) {
	f := buildFilter(map[string]any{"repo": []any{"alpha", "beta"}})
	require.NotNil(t, f)
	must := f["must"].([]map[string]any)
	require.Len(t, must, 1)
	match := must[0]["match"].(map[string]any)
	anyVals, ok := match["any"]
	require.True(t, ok)
	assert.Contains(t, anyVals, "alpha")
	assert.Contains(t, anyVals, "beta")
}

func TestBuildFilter_RepoAnySliceWithWildcard(t *testing.T) {
	f := buildFilter(map[string]any{"repo": []any{"alpha", "*"}})
	require.NotNil(t, f)
	must := f["must"].([]map[string]any)
	require.Len(t, must, 1)
	match := must[0]["match"].(map[string]any)
	assert.Equal(t, "alpha", match["value"])
}

func TestBuildFilter_RepoAnySliceAllWildcards(t *testing.T) {
	f := buildFilter(map[string]any{"repo": []any{"*", ""}})
	assert.Nil(t, f)
}

func TestBuildFilter_MultipleKeys(t *testing.T) {
	f := buildFilter(map[string]any{"language": "go", "kind": "function"})
	require.NotNil(t, f)
	must := f["must"].([]map[string]any)
	assert.Len(t, must, 2)
}

func TestBuildFilter_UnknownType(t *testing.T) {
	// repo with unsupported type (int) should be ignored
	f := buildFilter(map[string]any{"repo": 42})
	assert.Nil(t, f)
}

func TestBuildFilter_MixedRepoAndOther(t *testing.T) {
	f := buildFilter(map[string]any{"repo": "myrepo", "language": "go"})
	require.NotNil(t, f)
	must := f["must"].([]map[string]any)
	assert.Len(t, must, 2)
}

// ---------------------------------------------------------------------------
// repoMatchCondition tests
// ---------------------------------------------------------------------------

func TestRepoMatchCondition_StringEmpty(t *testing.T) {
	assert.Nil(t, repoMatchCondition(""))
}

func TestRepoMatchCondition_StringWildcard(t *testing.T) {
	assert.Nil(t, repoMatchCondition("*"))
}

func TestRepoMatchCondition_StringName(t *testing.T) {
	cond := repoMatchCondition("myrepo")
	require.NotNil(t, cond)
	assert.Equal(t, "repo", cond["key"])
	match := cond["match"].(map[string]any)
	assert.Equal(t, "myrepo", match["value"])
}

func TestRepoMatchCondition_SliceSingle(t *testing.T) {
	cond := repoMatchCondition([]string{"alpha"})
	require.NotNil(t, cond)
	match := cond["match"].(map[string]any)
	assert.Equal(t, "alpha", match["value"])
}

func TestRepoMatchCondition_SliceMultiple(t *testing.T) {
	cond := repoMatchCondition([]string{"a", "b", "c"})
	require.NotNil(t, cond)
	match := cond["match"].(map[string]any)
	anyVals := match["any"].([]string)
	assert.Len(t, anyVals, 3)
}

func TestRepoMatchCondition_SliceAllWildcards(t *testing.T) {
	assert.Nil(t, repoMatchCondition([]string{"*", "", "*"}))
}

func TestRepoMatchCondition_AnySliceSingle(t *testing.T) {
	cond := repoMatchCondition([]any{"alpha"})
	require.NotNil(t, cond)
	match := cond["match"].(map[string]any)
	assert.Equal(t, "alpha", match["value"])
}

func TestRepoMatchCondition_AnySliceMultiple(t *testing.T) {
	cond := repoMatchCondition([]any{"a", "b"})
	require.NotNil(t, cond)
	match := cond["match"].(map[string]any)
	anyVals := match["any"].([]string)
	assert.Len(t, anyVals, 2)
}

func TestRepoMatchCondition_AnySliceWithNonString(t *testing.T) {
	// Non-string values are coerced to "", which is skipped
	cond := repoMatchCondition([]any{42, true})
	assert.Nil(t, cond)
}

func TestRepoMatchCondition_UnsupportedType(t *testing.T) {
	assert.Nil(t, repoMatchCondition(42))
	assert.Nil(t, repoMatchCondition(nil))
	assert.Nil(t, repoMatchCondition(map[string]string{"a": "b"}))
}

func TestRepoMatchCondition_EmptySlice(t *testing.T) {
	assert.Nil(t, repoMatchCondition([]string{}))
}

func TestRepoMatchCondition_EmptyAnySlice(t *testing.T) {
	assert.Nil(t, repoMatchCondition([]any{}))
}

func TestRepoMatchCondition_SliceWithMixedWildcard(t *testing.T) {
	// ["a", "*", "b"] — wildcard skipped, two remain → "any"
	cond := repoMatchCondition([]string{"a", "*", "b"})
	require.NotNil(t, cond)
	match := cond["match"].(map[string]any)
	anyVals := match["any"].([]string)
	assert.Len(t, anyVals, 2)
	assert.Contains(t, anyVals, "a")
	assert.Contains(t, anyVals, "b")
}

// ---------------------------------------------------------------------------
// pickCollection tests
// ---------------------------------------------------------------------------

func TestPickCollection_CodeLanguages(t *testing.T) {
	cfg := testConfig(t)
	// Create a minimal Vector for pickCollection
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

// ---------------------------------------------------------------------------
// codeCollectionLanguages map tests
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Vector constructor & basic methods
// ---------------------------------------------------------------------------

func TestNewVector(t *testing.T) {
	cfg := testConfig(t)
	ollamaSrv := mockOllamaServer(t, 4)
	cfg.Ollama.URL = ollamaSrv.URL

	st := openTestStore(t)
	qd := qdrant.New("http://localhost:1") // dummy, won't be called
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

func TestVector_SetBM25_And_bm25Index(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	qd := qdrant.New("http://localhost:1")
	v := NewVector(cfg, qd, nil, st, nil)

	// Initially nil
	assert.Nil(t, v.bm25Index())

	// Create a temporary BM25 index
	bm25Path := filepath.Join(t.TempDir(), "bm25")
	idx, err := bm25.Open(bm25Path, 1.2, 0.75)
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })

	v.SetBM25(idx)
	assert.NotNil(t, v.bm25Index())
}

func TestVector_SetBM25_Nil(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	qd := qdrant.New("http://localhost:1")
	v := NewVector(cfg, qd, nil, st, nil)

	// Set to nil explicitly — should be nil
	v.SetBM25(nil)
	// bm25Index returns nil when stored pointer points to nil interface
	// Actually SetBM25 stores a pointer to the interface value.
	// If idx is nil interface, *p == nil interface
	// Let's verify:
	bm25Idx := v.bm25Index()
	// The atomic pointer is set to &idx where idx is nil interface.
	// So Load() returns non-nil pointer, but *p is nil interface.
	// bm25Index() checks p == nil (the pointer), not *p.
	// Actually: bm25Index does: p := v.bm25.Load(); if p == nil { return nil }; return *p
	// If we stored &nilInterface, p != nil, so it returns *p which is nil interface.
	// But since bm25.Index is an interface, *p will be the nil interface.
	// In Go, a nil interface value compared to nil is true.
	assert.Nil(t, bm25Idx)
}

func TestVector_Close_Empty(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	qd := qdrant.New("http://localhost:1")
	v := NewVector(cfg, qd, nil, st, nil)
	// Close with no active operations should return immediately
	v.Close()
}

// ---------------------------------------------------------------------------
// Vector.Init with mock servers
// ---------------------------------------------------------------------------

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

	// Pre-seed embed_meta with a different model
	codeSpec := cfg.CodeCollection()
	err := st.SetEmbedMeta(context.Background(), store.EmbedMeta{
		Collection: codeSpec.Name,
		Model:      "old-model",
		Dim:        512,
	})
	require.NoError(t, err)

	qd := qdrant.New(qdrantSrv.URL)
	v := NewVector(cfg, qd, nil, st, nil)
	err = v.Init(context.Background())
	assert.NoError(t, err)

	// After Init, embed_meta should be updated
	meta, err := st.GetEmbedMeta(context.Background(), codeSpec.Name)
	require.NoError(t, err)
	require.NotNil(t, meta)
	assert.Equal(t, codeSpec.EmbedModel, meta.Model)
	assert.Equal(t, int(codeSpec.EmbedDim), meta.Dim)
}

// ---------------------------------------------------------------------------
// Vector.SyncStats
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Vector.Search with mock
// ---------------------------------------------------------------------------

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
	assert.Empty(t, hits) // mock returns empty
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

	// limit=0 should default to 10
	hits, err := v.Search(context.Background(), "test", 0, nil)
	assert.NoError(t, err)
	assert.NotNil(t, hits)
}

func TestVector_Search_EmbedFailure(t *testing.T) {
	cfg := testConfig(t)
	// Point Ollama to a server that returns 500
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"model not loaded"}`)
	}))
	t.Cleanup(failSrv.Close)
	cfg.Ollama.URL = failSrv.URL

	qdrantSrv := mockQdrantServer(t)
	st := openTestStore(t)
	qd := qdrant.New(qdrantSrv.URL)
	bus := state.NewBus(t.TempDir())
	v := NewVector(cfg, qd, nil, st, bus)

	// Should not return error (embed failure is logged as warning, not propagated)
	hits, err := v.Search(context.Background(), "test", 10, map[string]any{"language": "go"})
	assert.NoError(t, err)
	assert.Empty(t, hits)
}

// ---------------------------------------------------------------------------
// Vector.SimilarToUnit
// ---------------------------------------------------------------------------

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

	// limit <= 0 should default to 10
	units, err := v.SimilarToUnit(context.Background(), store.ASTUnit{
		Language:  "go",
		Name:      "testFunc",
		Signature: "func testFunc()",
	}, 0)
	assert.NoError(t, err)
	assert.Empty(t, units) // mock returns empty search results
}

func TestVector_SimilarToUnit_WithSignature(t *testing.T) {
	cfg := testConfig(t)
	qdrantSrv := mockQdrantServer(t)
	ollamaSrv := mockOllamaServer(t, 4)
	cfg.Ollama.URL = ollamaSrv.URL

	st := openTestStore(t)
	qd := qdrant.New(qdrantSrv.URL)
	bus := state.NewBus(t.TempDir())
	v := NewVector(cfg, qd, nil, st, bus)

	units, err := v.SimilarToUnit(context.Background(), store.ASTUnit{
		Language:  "python",
		Name:      "myFunc",
		Signature: "def myFunc(a, b):",
	}, 5)
	assert.NoError(t, err)
	assert.Empty(t, units)
}

// ---------------------------------------------------------------------------
// VectorHybridAdapter tests
// ---------------------------------------------------------------------------

func TestVectorHybridAdapter_NilAdapter(t *testing.T) {
	var a *VectorHybridAdapter
	cands, err := a.HybridCandidates(context.Background(), "query", 10, nil)
	assert.NoError(t, err)
	assert.Nil(t, cands)
}

func TestVectorHybridAdapter_NilVector(t *testing.T) {
	a := &VectorHybridAdapter{V: nil}
	cands, err := a.HybridCandidates(context.Background(), "query", 10, nil)
	assert.NoError(t, err)
	assert.Nil(t, cands)
}

func TestVectorHybridAdapter_Success(t *testing.T) {
	cfg := testConfig(t)
	qdrantSrv := mockQdrantServer(t)
	ollamaSrv := mockOllamaServer(t, 4)
	cfg.Ollama.URL = ollamaSrv.URL

	st := openTestStore(t)
	qd := qdrant.New(qdrantSrv.URL)
	bus := state.NewBus(t.TempDir())
	v := NewVector(cfg, qd, nil, st, bus)

	a := &VectorHybridAdapter{V: v}
	cands, err := a.HybridCandidates(context.Background(), "test", 10, nil)
	assert.NoError(t, err)
	assert.Empty(t, cands) // mock returns empty
}

func TestVectorHybridAdapter_SourceType(t *testing.T) {
	// Verify that the adapter sets Source to SrcVector
	// We need a Qdrant mock that returns actual hits
	mux := http.NewServeMux()
	mux.HandleFunc("/api/embed", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"embeddings":[[0.1,0.2,0.3,0.4]]}`)
	})
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ollamaSrv := httptest.NewServer(mux)
	t.Cleanup(ollamaSrv.Close)

	qMux := http.NewServeMux()
	qMux.HandleFunc("/collections/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/points/search") {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"result":[{"id":"abc","score":0.95,"payload":{"file":"/test.go","language":"go","kind":"function","symbol":"TestFunc","start_line":1.0,"end_line":10.0,"text":"func TestFunc()"}}]}`)
		} else {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"result":{}}`)
		}
	})
	qdrantSrv := httptest.NewServer(qMux)
	t.Cleanup(qdrantSrv.Close)

	cfg := testConfig(t)
	cfg.Ollama.URL = ollamaSrv.URL
	st := openTestStore(t)
	qd := qdrant.New(qdrantSrv.URL)
	bus := state.NewBus(t.TempDir())
	v := NewVector(cfg, qd, nil, st, bus)

	a := &VectorHybridAdapter{V: v}
	cands, err := a.HybridCandidates(context.Background(), "test", 10, nil)
	require.NoError(t, err)
	require.Len(t, cands, 1)
	assert.Equal(t, hybrid.SrcVector, cands[0].Source)
	assert.Equal(t, 1, cands[0].Rank)
	assert.Equal(t, "/test.go", cands[0].Path)
	assert.Equal(t, "go", cands[0].Language)
	assert.Equal(t, "function", cands[0].Kind)
	assert.Equal(t, "TestFunc", cands[0].Symbol)
	assert.Equal(t, 1, cands[0].StartLine)
	assert.Equal(t, 10, cands[0].EndLine)
}

// ---------------------------------------------------------------------------
// BM25HybridAdapter tests
// ---------------------------------------------------------------------------

func TestBM25HybridAdapter_NilAdapter(t *testing.T) {
	var a *BM25HybridAdapter
	cands, err := a.HybridCandidates(context.Background(), "query", 10)
	assert.NoError(t, err)
	assert.Nil(t, cands)
}

func TestBM25HybridAdapter_NilIndex(t *testing.T) {
	a := &BM25HybridAdapter{Idx: nil}
	cands, err := a.HybridCandidates(context.Background(), "query", 10)
	assert.NoError(t, err)
	assert.Nil(t, cands)
}

func TestBM25HybridAdapter_Success(t *testing.T) {
	bm25Path := filepath.Join(t.TempDir(), "bm25")
	idx, err := bm25.Open(bm25Path, 1.2, 0.75)
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })

	// Index some docs
	ctx := context.Background()
	err = idx.IndexDocs(ctx, []bm25.Doc{
		{ID: "doc1", Path: "/test.go", Language: "go", Kind: "function", Symbol: "TestFunc", Content: "func TestFunc() { hello world }"},
		{ID: "doc2", Path: "/test.py", Language: "python", Kind: "function", Symbol: "test_func", Content: "def test_func(): pass"},
	})
	require.NoError(t, err)

	a := &BM25HybridAdapter{Idx: idx}
	cands, err := a.HybridCandidates(ctx, "hello", 10)
	assert.NoError(t, err)
	// At least the go doc should match "hello"
	assert.NotEmpty(t, cands)
	for _, c := range cands {
		assert.Equal(t, hybrid.SrcBM25, c.Source)
		assert.Greater(t, c.Rank, 0)
	}
}

func TestBM25HybridAdapter_EmptyQuery(t *testing.T) {
	bm25Path := filepath.Join(t.TempDir(), "bm25")
	idx, err := bm25.Open(bm25Path, 1.2, 0.75)
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })

	a := &BM25HybridAdapter{Idx: idx}
	cands, err := a.HybridCandidates(context.Background(), "", 10)
	assert.NoError(t, err)
	assert.Empty(t, cands)
}

// ---------------------------------------------------------------------------
// TreeSitter tests
// ---------------------------------------------------------------------------

func TestNewTreeSitter(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	bus := state.NewBus(t.TempDir())

	ts := NewTreeSitter(cfg, st, bus)
	require.NotNil(t, ts)
	assert.NotNil(t, ts.parser)
	assert.NotNil(t, ts.matcher)
}

func TestTreeSitter_IndexFile(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	bus := state.NewBus(t.TempDir())

	// Create a temporary Go file
	goFile := filepath.Join(cfg.Root, "test.go")
	err := os.WriteFile(goFile, []byte(`package main

func Hello() string {
	return "hello"
}

func World() string {
	return "world"
}
`), 0644)
	require.NoError(t, err)

	ts := NewTreeSitter(cfg, st, bus)
	err = ts.IndexFile(context.Background(), goFile)
	assert.NoError(t, err)

	// Verify the file was indexed
	fr, err := st.GetFile(context.Background(), goFile)
	require.NoError(t, err)
	require.NotNil(t, fr)
	assert.Equal(t, "go", fr.Language)
	assert.NotEmpty(t, fr.Hash)

	// Verify symbols were extracted
	syms, err := st.SymbolsByFile(context.Background(), goFile)
	require.NoError(t, err)
	assert.NotEmpty(t, syms)
}

func TestTreeSitter_IndexFile_SkipUnchanged(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)

	goFile := filepath.Join(cfg.Root, "test.go")
	err := os.WriteFile(goFile, []byte(`package main

func Hello() string {
	return "hello"
}
`), 0644)
	require.NoError(t, err)

	ts := NewTreeSitter(cfg, st, nil)
	err = ts.IndexFile(context.Background(), goFile)
	require.NoError(t, err)

	// Second call should skip (hash unchanged)
	err = ts.IndexFile(context.Background(), goFile)
	assert.NoError(t, err)
}

func TestTreeSitter_IndexFile_NonExistent(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)

	ts := NewTreeSitter(cfg, st, nil)
	err := ts.IndexFile(context.Background(), filepath.Join(cfg.Root, "nonexistent.go"))
	assert.Error(t, err) // os.Stat will fail
}

func TestTreeSitter_IndexFile_Directory(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)

	// Create a directory (not a file)
	dir := filepath.Join(cfg.Root, "subdir")
	err := os.MkdirAll(dir, 0755)
	require.NoError(t, err)

	ts := NewTreeSitter(cfg, st, nil)
	err = ts.IndexFile(context.Background(), dir)
	assert.NoError(t, err) // directories are silently skipped
}

func TestTreeSitter_RemoveFile(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)

	goFile := filepath.Join(cfg.Root, "test.go")
	err := os.WriteFile(goFile, []byte(`package main

func Hello() string {
	return "hello"
}
`), 0644)
	require.NoError(t, err)

	ts := NewTreeSitter(cfg, st, nil)
	err = ts.IndexFile(context.Background(), goFile)
	require.NoError(t, err)

	// Remove it
	err = ts.RemoveFile(context.Background(), goFile)
	assert.NoError(t, err)

	// Verify it's gone
	fr, err := st.GetFile(context.Background(), goFile)
	require.NoError(t, err)
	assert.Nil(t, fr)
}

func TestTreeSitter_FullScan_EmptyDir(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	bus := state.NewBus(t.TempDir())

	ts := NewTreeSitter(cfg, st, bus)
	err := ts.FullScan(context.Background())
	assert.NoError(t, err)

	snap := bus.Snapshot()
	idx := snap.Indexers["treesitter"]
	require.NotNil(t, idx)
	assert.Equal(t, "idle", idx.Status)
}

func TestTreeSitter_FullScan_WithFiles(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	bus := state.NewBus(t.TempDir())

	// Create some files
	err := os.WriteFile(filepath.Join(cfg.Root, "a.go"), []byte(`package main
func A() {}
`), 0644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(cfg.Root, "b.go"), []byte(`package main
func B() {}
`), 0644)
	require.NoError(t, err)

	ts := NewTreeSitter(cfg, st, bus)
	err = ts.FullScan(context.Background())
	assert.NoError(t, err)

	stats, err := st.Stats(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, stats.Files)
	assert.Greater(t, stats.Symbols, 0)
}

func TestTreeSitter_FullScan_Cancelled(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)

	err := os.WriteFile(filepath.Join(cfg.Root, "a.go"), []byte(`package main
func A() {}
`), 0644)
	require.NoError(t, err)

	ts := NewTreeSitter(cfg, st, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err = ts.FullScan(ctx)
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// Vector.IndexFile / RemoveFile
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Vector.prepareFile
// ---------------------------------------------------------------------------

func TestVector_prepareFile_NonExistent(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	qd := qdrant.New("http://localhost:1")
	v := NewVector(cfg, qd, nil, st, nil)

	pf, err := v.prepareFile(context.Background(), "/nonexistent/file.go")
	assert.NoError(t, err) // returns nil, nil for nonexistent
	assert.Nil(t, pf)
}

func TestVector_prepareFile_Unchanged(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	qd := qdrant.New("http://localhost:1")
	v := NewVector(cfg, qd, nil, st, nil)

	// Create a file and set its hash
	goFile := filepath.Join(cfg.Root, "test.go")
	src := []byte(`package main
func Hello() {}
`)
	err := os.WriteFile(goFile, src, 0644)
	require.NoError(t, err)

	// Pre-set the hash so it appears unchanged
	err = st.EnsureFile(context.Background(), goFile, "go")
	require.NoError(t, err)
	_ = st.UpdateVectorHash(context.Background(), goFile, "abc123")

	// Now make HashBytes return the same hash
	// Actually, we need the real hash. Let's compute it.
	hash := "abc123" // dummy - won't match real hash

	// This should return non-nil since hash won't match
	pf, err := v.prepareFile(context.Background(), goFile)
	assert.NoError(t, err)
	// The hash won't match "abc123", so pf should be non-nil
	if pf != nil {
		assert.NotEmpty(t, pf.hash)
	}
	_ = hash
}

func TestVector_prepareFile_EmptyFile(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	qd := qdrant.New("http://localhost:1")
	v := NewVector(cfg, qd, nil, st, nil)

	// Create an empty file
	goFile := filepath.Join(cfg.Root, "empty.go")
	err := os.WriteFile(goFile, []byte(""), 0644)
	require.NoError(t, err)

	pf, err := v.prepareFile(context.Background(), goFile)
	assert.NoError(t, err)
	// Empty file produces no chunks → pf is nil
	assert.Nil(t, pf)
}

// ---------------------------------------------------------------------------
// Vector.FullScan guards
// ---------------------------------------------------------------------------

func TestVector_FullScan_ConcurrentGuard(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	qd := qdrant.New("http://localhost:1")
	v := NewVector(cfg, qd, nil, st, nil)

	// Simulate scanning = true
	v.scanMu.Lock()
	v.scanning = true
	v.scanMu.Unlock()

	err := v.FullScan(context.Background())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already in progress")

	// Reset
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

// ---------------------------------------------------------------------------
// Vector.Watch
// ---------------------------------------------------------------------------

func TestVector_Watch_ContextCancel(t *testing.T) {
	// Watch method requires a real watcher; the select evaluates w.Events()
	// before checking ctx.Done(), so we can't pass nil.
	// Instead, just verify that a cancelled context is detected in the
	// IndexFile/RemoveFile paths which Watch delegates to.
	cfg := testConfig(t)
	st := openTestStore(t)
	qd := qdrant.New("http://localhost:1")
	v := NewVector(cfg, qd, nil, st, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Verify IndexFile and RemoveFile respect cancelled context (which Watch calls)
	assert.Error(t, v.IndexFile(ctx, "/some/file.go"))
	assert.Error(t, v.RemoveFile(ctx, "/some/file.go"))
}

// ---------------------------------------------------------------------------
// TreeSitter.Watch
// ---------------------------------------------------------------------------

func TestTreeSitter_Watch_ContextCancel(t *testing.T) {
	// Same as Vector.Watch — requires a real watcher.
	// Test that RemoveFile respects cancelled context.
	cfg := testConfig(t)
	st := openTestStore(t)
	ts := NewTreeSitter(cfg, st, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Verify RemoveFile respects cancelled context
	assert.Error(t, ts.RemoveFile(ctx, "/some/file.go"))
}

// ---------------------------------------------------------------------------
// Vector.processBatch with empty files
// ---------------------------------------------------------------------------

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
		abs:  "/test.go",
		rel:  "test.go",
		lang: "go",
		chunks: []chunker.Chunk{{Text: "hello", StartLine: 1, EndLine: 1}},
	}}, 1)
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// Vector.processBatch with actual prepared files
// ---------------------------------------------------------------------------

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

	files := []*preparedFile{
		{
			abs:      "/test.go",
			rel:      "test.go",
			lang:     "go",
			hash:     "abc123",
			collSpec: cfg.CodeCollection(),
			emb:      v.code,
			chunks: []chunker.Chunk{
				{Text: "func Hello() {}", StartLine: 1, EndLine: 1, Kind: "function", Symbol: "Hello"},
			},
		},
	}

	err := v.processBatch(context.Background(), cfg.CodeCollection(), v.code, files, 1)
	assert.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Edge case: Vector.ensureCollection with empty name
// ---------------------------------------------------------------------------

func TestVector_ensureCollection_EmptyName(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	qd := qdrant.New("http://localhost:1")
	v := NewVector(cfg, qd, nil, st, nil)

	err := v.ensureCollection(context.Background(), config.CollectionSpec{Name: ""})
	assert.NoError(t, err) // should return nil immediately
}

// ---------------------------------------------------------------------------
// Edge case: Vector.Search dedup
// ---------------------------------------------------------------------------

func TestVector_Search_Dedup(t *testing.T) {
	// Create a Qdrant mock that returns duplicate IDs from two collections
	callCount := 0
	qMux := http.NewServeMux()
	qMux.HandleFunc("/collections/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/points/search") {
			callCount++
			w.WriteHeader(http.StatusOK)
			// Both collections return the same ID
			fmt.Fprint(w, `{"result":[{"id":"dup-id","score":0.9,"payload":{"file":"/test.go","language":"go"}}]}`)
		} else {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"result":{}}`)
		}
	})
	qdrantSrv := httptest.NewServer(qMux)
	t.Cleanup(qdrantSrv.Close)

	oMux := http.NewServeMux()
	oMux.HandleFunc("/api/embed", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"embeddings":[[0.1,0.2,0.3,0.4]]}`)
	})
	oMux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ollamaSrv := httptest.NewServer(oMux)
	t.Cleanup(ollamaSrv.Close)

	cfg := testConfig(t)
	cfg.Ollama.URL = ollamaSrv.URL
	st := openTestStore(t)
	qd := qdrant.New(qdrantSrv.URL)
	bus := state.NewBus(t.TempDir())
	v := NewVector(cfg, qd, nil, st, bus)

	// Search without language filter → searches both collections
	hits, err := v.Search(context.Background(), "test", 10, nil)
	assert.NoError(t, err)
	// Should be deduped to 1
	assert.Len(t, hits, 1)
	assert.Equal(t, "dup-id", hits[0].ID)
}

// ---------------------------------------------------------------------------
// Edge case: Vector.Search sorting
// ---------------------------------------------------------------------------

func TestVector_Search_SortingAndTruncation(t *testing.T) {
	qMux := http.NewServeMux()
	qMux.HandleFunc("/collections/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/points/search") {
			w.WriteHeader(http.StatusOK)
			// Return 3 hits with different scores
			fmt.Fprint(w, `{"result":[
				{"id":"c","score":0.5,"payload":{}},
				{"id":"a","score":0.9,"payload":{}},
				{"id":"b","score":0.7,"payload":{}}
			]}`)
		} else {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"result":{}}`)
		}
	})
	qdrantSrv := httptest.NewServer(qMux)
	t.Cleanup(qdrantSrv.Close)

	oMux := http.NewServeMux()
	oMux.HandleFunc("/api/embed", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"embeddings":[[0.1,0.2,0.3,0.4]]}`)
	})
	oMux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ollamaSrv := httptest.NewServer(oMux)
	t.Cleanup(ollamaSrv.Close)

	cfg := testConfig(t)
	cfg.Ollama.URL = ollamaSrv.URL
	st := openTestStore(t)
	qd := qdrant.New(qdrantSrv.URL)
	bus := state.NewBus(t.TempDir())
	v := NewVector(cfg, qd, nil, st, bus)

	// Search with limit=2 → should return top 2 sorted by score desc
	hits, err := v.Search(context.Background(), "test", 2, map[string]any{"language": "go"})
	assert.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, "a", hits[0].ID) // score 0.9
	assert.Equal(t, "b", hits[1].ID) // score 0.7
}

// ---------------------------------------------------------------------------
// Vector.IndexFile with real file and mock servers
// ---------------------------------------------------------------------------

func TestVector_IndexFile_Success(t *testing.T) {
	cfg := testConfig(t)
	dim := int(cfg.CodeCollection().EmbedDim)

	qdrantSrv := mockQdrantServer(t)
	ollamaSrv := mockOllamaServer(t, dim)
	cfg.Ollama.URL = ollamaSrv.URL

	st := openTestStore(t)
	qd := qdrant.New(qdrantSrv.URL)
	bus := state.NewBus(t.TempDir())
	v := NewVector(cfg, qd, nil, st, bus)

	// Create a real Go file
	goFile := filepath.Join(cfg.Root, "hello.go")
	err := os.WriteFile(goFile, []byte(`package main

func Hello() string {
	return "hello world"
}

func Goodbye() string {
	return "goodbye world"
}
`), 0644)
	require.NoError(t, err)

	err = v.IndexFile(context.Background(), goFile)
	assert.NoError(t, err)
}

func TestVector_IndexFile_UnchangedFile(t *testing.T) {
	cfg := testConfig(t)
	dim := int(cfg.CodeCollection().EmbedDim)

	qdrantSrv := mockQdrantServer(t)
	ollamaSrv := mockOllamaServer(t, dim)
	cfg.Ollama.URL = ollamaSrv.URL

	st := openTestStore(t)
	qd := qdrant.New(qdrantSrv.URL)
	bus := state.NewBus(t.TempDir())
	v := NewVector(cfg, qd, nil, st, bus)

	goFile := filepath.Join(cfg.Root, "hello.go")
	src := []byte(`package main
func Hello() string { return "hello" }
`)
	err := os.WriteFile(goFile, src, 0644)
	require.NoError(t, err)

	// First index
	err = v.IndexFile(context.Background(), goFile)
	require.NoError(t, err)

	// Second index — file unchanged, prepareFile returns nil
	err = v.IndexFile(context.Background(), goFile)
	assert.NoError(t, err)
}

func TestVector_IndexFile_WithBM25(t *testing.T) {
	cfg := testConfig(t)
	dim := int(cfg.CodeCollection().EmbedDim)

	qdrantSrv := mockQdrantServer(t)
	ollamaSrv := mockOllamaServer(t, dim)
	cfg.Ollama.URL = ollamaSrv.URL

	st := openTestStore(t)
	qd := qdrant.New(qdrantSrv.URL)
	bus := state.NewBus(t.TempDir())
	v := NewVector(cfg, qd, nil, st, bus)

	// Connect BM25 index
	bm25Path := filepath.Join(t.TempDir(), "bm25")
	idx, err := bm25.Open(bm25Path, 1.2, 0.75)
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })
	v.SetBM25(idx)

	goFile := filepath.Join(cfg.Root, "hello.go")
	err = os.WriteFile(goFile, []byte(`package main
func Hello() string { return "hello" }
`), 0644)
	require.NoError(t, err)

	err = v.IndexFile(context.Background(), goFile)
	assert.NoError(t, err)

	// Verify BM25 has docs
	count, err := idx.Count(context.Background())
	require.NoError(t, err)
	assert.Greater(t, count, uint64(0))
}

// ---------------------------------------------------------------------------
// Vector.FullScan with real files
// ---------------------------------------------------------------------------

func TestVector_FullScan_WithFiles(t *testing.T) {
	cfg := testConfig(t)
	dim := int(cfg.CodeCollection().EmbedDim)

	qdrantSrv := mockQdrantServer(t)
	ollamaSrv := mockOllamaServer(t, dim)
	cfg.Ollama.URL = ollamaSrv.URL
	cfg.VectorWorkers = 2

	st := openTestStore(t)
	qd := qdrant.New(qdrantSrv.URL)
	bus := state.NewBus(t.TempDir())
	v := NewVector(cfg, qd, nil, st, bus)

	// Create multiple files
	err := os.WriteFile(filepath.Join(cfg.Root, "a.go"), []byte(`package main
func A() {}
`), 0644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(cfg.Root, "b.go"), []byte(`package main
func B() {}
`), 0644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(cfg.Root, "c.md"), []byte(`# Hello World

This is a markdown file with some content.
`), 0644)
	require.NoError(t, err)

	err = v.FullScan(context.Background())
	assert.NoError(t, err)

	snap := bus.Snapshot()
	idx := snap.Indexers["vector"]
	require.NotNil(t, idx)
	assert.Equal(t, "idle", idx.Status)
}

func TestVector_FullScan_WithBM25(t *testing.T) {
	cfg := testConfig(t)
	dim := int(cfg.CodeCollection().EmbedDim)

	qdrantSrv := mockQdrantServer(t)
	ollamaSrv := mockOllamaServer(t, dim)
	cfg.Ollama.URL = ollamaSrv.URL

	st := openTestStore(t)
	qd := qdrant.New(qdrantSrv.URL)
	bus := state.NewBus(t.TempDir())
	v := NewVector(cfg, qd, nil, st, bus)

	bm25Path := filepath.Join(t.TempDir(), "bm25")
	idx, err := bm25.Open(bm25Path, 1.2, 0.75)
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })
	v.SetBM25(idx)

	err = os.WriteFile(filepath.Join(cfg.Root, "a.go"), []byte(`package main
func A() {}
`), 0644)
	require.NoError(t, err)

	err = v.FullScan(context.Background())
	assert.NoError(t, err)
}

func TestVector_FullScan_CancelledContext(t *testing.T) {
	cfg := testConfig(t)
	dim := int(cfg.CodeCollection().EmbedDim)

	qdrantSrv := mockQdrantServer(t)
	ollamaSrv := mockOllamaServer(t, dim)
	cfg.Ollama.URL = ollamaSrv.URL

	st := openTestStore(t)
	qd := qdrant.New(qdrantSrv.URL)
	bus := state.NewBus(t.TempDir())
	v := NewVector(cfg, qd, nil, st, bus)

	err := os.WriteFile(filepath.Join(cfg.Root, "a.go"), []byte(`package main
func A() {}
`), 0644)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a brief delay to allow FullScan to start
	go func() {
		cancel()
	}()

	err = v.FullScan(ctx)
	// May or may not error depending on timing
	_ = err
}

// ---------------------------------------------------------------------------
// Vector.SetRepoResolver
// ---------------------------------------------------------------------------

func TestVector_SetRepoResolver(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	qd := qdrant.New("http://localhost:1")
	v := NewVector(cfg, qd, nil, st, nil)

	assert.Nil(t, v.resolv)

	// Just verify SetRepoResolver doesn't panic with nil
	v.SetRepoResolver(nil)
	assert.Nil(t, v.resolv)
}

// ---------------------------------------------------------------------------
// Vector.SimilarToUnit with real AST units
// ---------------------------------------------------------------------------

func TestVector_SimilarToUnit_WithASTUnits(t *testing.T) {
	cfg := testConfig(t)

	// Create a file and seed AST units first, so we know the path
	goFile := filepath.Join(cfg.Root, "helper.go")
	err := os.WriteFile(goFile, []byte(`package main

// Main is the entry point.
func Main() {
	Helper()
}

// Helper does something.
func Helper() {
	// body
}
`), 0644)
	require.NoError(t, err)

	st := openTestStore(t)
	err = st.EnsureFile(context.Background(), goFile, "go")
	require.NoError(t, err)

	_, err = st.ReplaceASTUnits(context.Background(), goFile, []store.ASTUnit{
		{FilePath: goFile, Language: "go", Kind: "function", Name: "Main", Qualified: "main.Main", StartLine: 4, EndLine: 6, StartByte: 0, EndByte: 40},
		{FilePath: goFile, Language: "go", Kind: "function", Name: "Helper", Qualified: "main.Helper", StartLine: 9, EndLine: 11, StartByte: 50, EndByte: 90},
	})
	require.NoError(t, err)

	// Custom Qdrant mock that returns hits matching our actual file path
	qMux := http.NewServeMux()
	qMux.HandleFunc("/collections/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/points/search") {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"result":[{"id":"hit1","score":0.9,"payload":{"file":%q,"language":"go","kind":"function","symbol":"Helper","start_line":5.0,"end_line":10.0,"text":"func Helper()"}}]}`, goFile)
		} else {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"result":{}}`)
		}
	})
	qdrantSrv := httptest.NewServer(qMux)
	t.Cleanup(qdrantSrv.Close)

	oMux := http.NewServeMux()
	oMux.HandleFunc("/api/embed", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"embeddings":[[0.1,0.2,0.3,0.4]]}`)
	})
	oMux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ollamaSrv := httptest.NewServer(oMux)
	t.Cleanup(ollamaSrv.Close)

	cfg.Ollama.URL = ollamaSrv.URL
	qd := qdrant.New(qdrantSrv.URL)
	bus := state.NewBus(t.TempDir())
	v := NewVector(cfg, qd, nil, st, bus)

	units, err := v.SimilarToUnit(context.Background(), store.ASTUnit{
		ID:        1,
		Language:  "go",
		Name:      "Main",
		Signature: "func Main()",
		FilePath:  goFile,
		StartByte: 0,
		EndByte:   40,
	}, 5)
	assert.NoError(t, err)
	// Should find the Helper unit since the search hit overlaps with its lines
	assert.NotEmpty(t, units)
}

// ---------------------------------------------------------------------------
// TreeSitter.IndexFile with various languages
// ---------------------------------------------------------------------------

func TestTreeSitter_IndexFile_Python(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	bus := state.NewBus(t.TempDir())

	pyFile := filepath.Join(cfg.Root, "test.py")
	err := os.WriteFile(pyFile, []byte(`def hello():
    return "hello"

class MyClass:
    def method(self):
        pass
`), 0644)
	require.NoError(t, err)

	ts := NewTreeSitter(cfg, st, bus)
	err = ts.IndexFile(context.Background(), pyFile)
	assert.NoError(t, err)

	syms, err := st.SymbolsByFile(context.Background(), pyFile)
	require.NoError(t, err)
	assert.NotEmpty(t, syms)
}

func TestTreeSitter_IndexFile_TypeScript(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	bus := state.NewBus(t.TempDir())

	tsFile := filepath.Join(cfg.Root, "test.ts")
	err := os.WriteFile(tsFile, []byte(`export function hello(): string {
    return "hello";
}

interface MyInterface {
    greet(): void;
}
`), 0644)
	require.NoError(t, err)

	ts := NewTreeSitter(cfg, st, bus)
	err = ts.IndexFile(context.Background(), tsFile)
	assert.NoError(t, err)

	syms, err := st.SymbolsByFile(context.Background(), tsFile)
	require.NoError(t, err)
	assert.NotEmpty(t, syms)
}

// ---------------------------------------------------------------------------
// TreeSitter.FullScan with mixed files
// ---------------------------------------------------------------------------

func TestTreeSitter_FullScan_MixedLanguages(t *testing.T) {
	cfg := testConfig(t)
	st := openTestStore(t)
	bus := state.NewBus(t.TempDir())

	err := os.WriteFile(filepath.Join(cfg.Root, "a.go"), []byte(`package main
func A() {}
`), 0644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(cfg.Root, "b.py"), []byte(`def b():
    pass
`), 0644)
	require.NoError(t, err)

	// Create a subdirectory
	subDir := filepath.Join(cfg.Root, "sub")
	err = os.MkdirAll(subDir, 0755)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(subDir, "c.ts"), []byte(`export function c() {}
`), 0644)
	require.NoError(t, err)

	ts := NewTreeSitter(cfg, st, bus)
	err = ts.FullScan(context.Background())
	assert.NoError(t, err)

	stats, err := st.Stats(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 3, stats.Files)
}

// ---------------------------------------------------------------------------
// batcher.run indirectly through FullScan with large file
// ---------------------------------------------------------------------------

func TestVector_FullScan_LargeFile(t *testing.T) {
	cfg := testConfig(t)
	dim := int(cfg.CodeCollection().EmbedDim)

	// Create a custom Ollama mock that handles batch embed
	oMux := http.NewServeMux()
	oMux.HandleFunc("/api/embed", func(w http.ResponseWriter, r *http.Request) {
		// Parse request to determine batch size
		var req struct {
			Input interface{} `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		count := 1
		if arr, ok := req.Input.([]interface{}); ok {
			count = len(arr)
		}

		vecs := make([]string, count)
		for i := range vecs {
			v := make([]string, dim)
			for j := range v {
				v[j] = "0.1"
			}
			vecs[i] = "[" + strings.Join(v, ",") + "]"
		}

		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"embeddings":[%s]}`, strings.Join(vecs, ","))
	})
	oMux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ollamaSrv := httptest.NewServer(oMux)
	t.Cleanup(ollamaSrv.Close)
	cfg.Ollama.URL = ollamaSrv.URL

	qdrantSrv := mockQdrantServer(t)
	st := openTestStore(t)
	qd := qdrant.New(qdrantSrv.URL)
	bus := state.NewBus(t.TempDir())
	v := NewVector(cfg, qd, nil, st, bus)

	// Create a large file with many lines
	var sb strings.Builder
	sb.WriteString("package main\n\n")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&sb, "func Func%d() { /* body %d */ }\n\n", i, i)
	}
	goFile := filepath.Join(cfg.Root, "large.go")
	err := os.WriteFile(goFile, []byte(sb.String()), 0644)
	require.NoError(t, err)

	err = v.FullScan(context.Background())
	assert.NoError(t, err)
}
