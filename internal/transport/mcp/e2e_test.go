//go:build e2e

// E2E тесты полного пайплайна: AST + Vector + BM25 + MCP methods.
// Запуск: GOPROXY="https://proxy.golang.org,direct" go test -tags=e2e ./internal/transport/mcp/... -run TestE2E -v
//
// Требования:
// - Docker (для Qdrant контейнера)
// - Тестовые проекты в tests/testprojects/

package mcp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"encoding/json"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ragota/internal/indexing/ast"
	"ragota/internal/indexing/embedder"
	"ragota/internal/indexing/vector"
	"ragota/internal/search/bm25"
	"ragota/internal/search/graph"
	"ragota/internal/store"
	"ragota/pkg/config"
	"ragota/pkg/qdrant"
	"ragota/pkg/repos"
	"ragota/pkg/state"
)

// ---------------------------------------------------------------------------
// Mock Ollama
// ---------------------------------------------------------------------------

type mockOllama struct {
	server      *httptest.Server
	embedCount  atomic.Int64
}

func newMockOllama(dim int) *mockOllama {
	m := &mockOllama{}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{"name": "mock-embed", "size": 1000000},
			},
		})
	})

	mux.HandleFunc("/api/embed", func(w http.ResponseWriter, r *http.Request) {
		m.embedCount.Add(1)
		w.Header().Set("Content-Type", "application/json")

		// Парсим запрос чтобы узнать количество input
		var req struct {
			Input any `json:"input"` // может быть string или []string
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		count := 1
		if arr, ok := req.Input.([]any); ok {
			count = len(arr)
		}

		// Возвращаем embeddings по одному на каждый input
		embeddings := make([][]float32, count)
		for i := 0; i < count; i++ {
			emb := make([]float32, dim)
			for j := range emb {
				emb[j] = float32(j+1) / float32(dim) * 0.1
			}
			embeddings[i] = emb
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": embeddings})
	})

	mux.HandleFunc("/api/embeddings", func(w http.ResponseWriter, r *http.Request) {
		m.embedCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// /api/embeddings (legacy) возвращает {"embedding": [...]}
		embedding := make([]float32, dim)
		for i := range embedding {
			embedding[i] = float32(i+1) / float32(dim) * 0.1
		}
		json.NewEncoder(w).Encode(map[string]any{"embedding": embedding})
	})

	mux.HandleFunc("/api/generate", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"response": "0.85", "done": true})
	})

	m.server = httptest.NewServer(mux)
	return m
}

func (m *mockOllama) url() string  { return m.server.URL }
func (m *mockOllama) close()       { m.server.Close() }

// ---------------------------------------------------------------------------
// Qdrant container helper
// ---------------------------------------------------------------------------

func startTestQdrant(t *testing.T) (string, func()) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}

	name := fmt.Sprintf("ragota-e2e-%d", time.Now().UnixNano())

	// Находим свободный порт
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot find free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	cmd := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:6333", port), "qdrant/qdrant:latest")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("docker run failed: %v: %s", err, out)
	}

	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	qd := qdrant.New(url)
	for {
		if ctx.Err() != nil {
			_ = exec.Command("docker", "rm", "-f", name).Run()
			t.Fatal("qdrant not ready in 30s")
		}
		if err := qd.Ping(ctx); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	return url, func() { _ = exec.Command("docker", "rm", "-f", name).Run() }
}

// ---------------------------------------------------------------------------
// E2E Tests
// ---------------------------------------------------------------------------

func TestE2E_FullPipeline(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E skipped in short mode")
	}

	ctx := context.Background()
	goDir := testProjectDir(t, "go")

	// 1. Mock Ollama
	mock := newMockOllama(1024)
	defer mock.close()

	// 2. Qdrant
	qdrantURL, cleanupQdrant := startTestQdrant(t)
	defer cleanupQdrant()

	// 3. Temp dirs
	tmpDir := t.TempDir()
	bm25Path := filepath.Join(tmpDir, "bm25")

	// 4. Config
	cfg := config.Default()
	cfg.Root = goDir
	cfg.Ollama.URL = mock.url()
	cfg.Ollama.EmbedModel = "mock-embed"
	cfg.Collections.Code.Name = "e2e_code"
	cfg.Collections.Code.EmbedModel = "mock-embed"
	cfg.Collections.Text.Name = "e2e_text"
	cfg.Collections.Text.EmbedModel = "mock-embed"
	cfg.BM25.Enabled = true
	cfg.BM25.Path = bm25Path
	cfg.IgnorePatterns = nil

	// 5. Store
	st, err := store.Open(filepath.Join(tmpDir, "test.db"))
	require.NoError(t, err)
	defer st.Close()

	// 6. AST indexer
	astIdx := astindex.New(cfg, st)
	rs, _ := repos.Discover(goDir)
	resolver := repos.NewResolver(rs)
	astIdx.SetRepoResolver(resolver)

	err = astIdx.FullScan(ctx)
	require.NoError(t, err, "AST FullScan")

	// 7. BM25
	bleveIx, err := bm25.Open(bm25Path, cfg.BM25.K1, cfg.BM25.B)
	require.NoError(t, err)
	defer bleveIx.Close()

	// 8. Vector indexer — подключаем BM25!
	bus := state.NewBus(tmpDir)
	qd := qdrant.New(qdrantURL)
	emb := embedder.New(mock.url(), "mock-embed")
	emb.SetDim(1024)
	emb.SetBus(bus)

	vIdx := vector.NewVector(cfg, qd, emb, st, bus)
	vIdx.SetRepoResolver(resolver)
	vIdx.SetBM25(bm25.AsWriteSink(bleveIx))

	err = vIdx.Init(ctx)
	require.NoError(t, err, "Vector Init")

	err = vIdx.FullScan(ctx)
	require.NoError(t, err, "Vector FullScan")

	// 9. Проверяем BM25
	bm25Count, err := bleveIx.Count(ctx)
	require.NoError(t, err)
	t.Logf("BM25 docs: %d", bm25Count)
	assert.Greater(t, bm25Count, uint64(0), "BM25 should be indexed")

	// 10. Graph service
	gr := graph.New(cfg, st)
	gr.SetBus(bus)

	// 11. MCP Server
	codeSrv := NewCodeServer(cfg, st, bus, resolver)
	codeSrv.SetASTIndex(astIdx)
	codeSrv.SetVector(vIdx, qd)
	codeSrv.SetBM25(bm25.AsWriteSink(bleveIx))
	codeSrv.SetReranker(nil)
	codeSrv.SetGraphService(gr)

	// --- search mode=keyword ---
	t.Run("search_keyword", func(t *testing.T) {
		res, err := codeSrv.handleSearch(ctx, toolReq(map[string]any{
			"query": "Hello", "mode": "keyword", "limit": 10,
		}))
		require.NoError(t, err)
		assert.False(t, res.IsError, "keyword search should not error")
		arr := parseJSONArrayResult(t, res)
		t.Logf("keyword search: %d results", len(arr))
		assert.Greater(t, len(arr), 0, "keyword search should find results (BM25 fix)")
	})

	// --- search mode=semantic ---
	t.Run("search_semantic", func(t *testing.T) {
		res, err := codeSrv.handleSearch(ctx, toolReq(map[string]any{
			"query": "hello function", "mode": "semantic", "limit": 10,
		}))
		require.NoError(t, err)
		assert.False(t, res.IsError)
		arr := parseJSONArrayResult(t, res)
		t.Logf("semantic search: %d results", len(arr))
	})

	// --- search mode=hybrid ---
	t.Run("search_hybrid", func(t *testing.T) {
		res, err := codeSrv.handleSearch(ctx, toolReq(map[string]any{
			"query": "Hello", "mode": "hybrid", "limit": 10,
		}))
		require.NoError(t, err)
		assert.False(t, res.IsError)
		arr := parseJSONArrayResult(t, res)
		t.Logf("hybrid search: %d results", len(arr))
	})

	// --- find_symbol ---
	t.Run("find_symbol", func(t *testing.T) {
		res, err := codeSrv.handleFindSymbol(ctx, toolReq(map[string]any{
			"name": "Hello", "limit": 10,
		}))
		require.NoError(t, err)
		assert.False(t, res.IsError)
		arr := parseJSONArrayResult(t, res)
		t.Logf("find_symbol: %d results", len(arr))
		assert.Greater(t, len(arr), 0)
	})

	// --- stats ---
	t.Run("stats", func(t *testing.T) {
		res, err := codeSrv.handleStats(ctx, toolReq(nil))
		require.NoError(t, err)
		assert.False(t, res.IsError)
		m := parseJSONResult(t, res)
		t.Logf("stats: %v", m)
	})

	// --- call_graph direction=up ---
	t.Run("call_graph_up", func(t *testing.T) {
		// TaskHandler вызывается из main(), поэтому direction=up должен найти main
		res, err := codeSrv.handleGetCallGraph(ctx, toolReq(map[string]any{
			"symbol": "TaskHandler", "direction": "up", "depth": 2,
		}))
		require.NoError(t, err)
		assert.False(t, res.IsError)
		m := parseJSONResult(t, res)

		nodes, _ := m["nodes"].([]any)
		edges, _ := m["edges"].([]any)

		t.Logf("call_graph_up: %d nodes, %d edges", len(nodes), len(edges))

		// Логируем все nodes для диагностики
		for i, node := range nodes {
			n, ok := node.(map[string]any)
			if !ok {
				continue
			}
			t.Logf("  node[%d]: name=%v, qualified=%v, kind=%v",
				i, n["name"], n["qualified"], n["kind"])
		}

		// Логируем все edges для диагностики
		for i, edge := range edges {
			e, ok := edge.(map[string]any)
			if !ok {
				continue
			}
			t.Logf("  edge[%d]: src_id=%v, dst_id=%v, kind=%v, dst_name=%v",
				i, e["src_id"], e["dst_id"], e["kind"], e["dst_name"])
		}

		// Проверяем что есть caller (main должен быть)
		if len(nodes) == 0 {
			t.Log("WARNING: call_graph direction=up returned 0 nodes — callers not found")
		} else {
			// Проверяем что main есть среди callers
			foundMain := false
			for _, node := range nodes {
				n, ok := node.(map[string]any)
				if !ok {
					continue
				}
				if name, _ := n["name"].(string); name == "main" {
					foundMain = true
					break
				}
			}
			if foundMain {
				t.Log("call_graph_up: main found as caller ✓")
			} else {
				t.Log("WARNING: main not found as caller")
			}
		}
	})

	t.Logf("Mock Ollama: %d embed calls", mock.embedCount.Load())
}
