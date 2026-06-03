// Package mcp — общие test helpers и mock-объекты для MCP-тестов.
// Вынесены в отдельный файл чтобы использовать и в handlers, и в integration тестах.

package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"ragota/internal/indexing/ast"
	"ragota/pkg/config"
	"ragota/internal/search/graph"
	"ragota/internal/search/hybrid"
	"ragota/pkg/repos"
	"ragota/pkg/state"
	"ragota/internal/store"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func toolReq(args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Arguments: args,
		},
	}
}

// contentText извлекает текст из MCP TextContent.
func contentText(c any) string {
	if tc, ok := c.(mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

// parseJSONResult парсит JSON из MCP-ответа в map.
func parseJSONResult(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	require.NotNil(t, res)
	require.Len(t, res.Content, 1)
	text := contentText(res.Content[0])
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &m), "invalid JSON: %s", text)
	return m
}

// parseJSONArrayResult парсит JSON-массив из MCP-ответа.
func parseJSONArrayResult(t *testing.T, res *mcp.CallToolResult) []any {
	t.Helper()
	require.NotNil(t, res)
	require.Len(t, res.Content, 1)
	text := contentText(res.Content[0])
	var arr []any
	require.NoError(t, json.Unmarshal([]byte(text), &arr), "invalid JSON array: %s", text)
	return arr
}

// ---------------------------------------------------------------------------
// Test environment with in-memory SQLite
// ---------------------------------------------------------------------------

type testEnv struct {
	db       *store.SQLite
	cfg      *config.Config
	gr       *graph.Service
	bus      *state.Bus
	root     string
	astIdx   *astindex.Indexer
	resolver *repos.Resolver
}

func setupTestEnv(t *testing.T) *testEnv {
	t.Helper()
	db, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	root := t.TempDir()
	cfg := config.Default()
	cfg.Root = root

	gr := graph.New(cfg, db)
	bus := state.NewBus(root)

	return &testEnv{db: db, cfg: cfg, gr: gr, bus: bus, root: root}
}

func setupTestEnvWithAST(t *testing.T) *testEnv {
	t.Helper()
	env := setupTestEnv(t)

	// Создаём resolver для тестового root
	rs, err := repos.Discover(env.root)
	require.NoError(t, err)
	env.resolver = repos.NewResolver(rs)

	env.astIdx = astindex.New(env.cfg, env.db)
	env.astIdx.SetRepoResolver(env.resolver)

	return env
}

func makeCodeServer(env *testEnv) *CodeServer {
	s := NewCodeServer(env.cfg, env.db, env.bus, env.resolver)
	s.SetGraphService(env.gr)
	if env.astIdx != nil {
		s.SetASTIndex(env.astIdx)
	}
	return s
}

// ---------------------------------------------------------------------------
// AST seeding helpers
// ---------------------------------------------------------------------------

func seedAST(t *testing.T, env *testEnv, filePath string, units []store.ASTUnit) map[string]int {
	t.Helper()
	lang := ""
	if len(units) > 0 {
		lang = units[0].Language
	}
	err := env.db.EnsureFile(context.Background(), filePath, lang)
	require.NoError(t, err)
	ids, err := env.db.ReplaceASTUnits(context.Background(), filePath, units)
	require.NoError(t, err)
	return ids
}

func seedASTFromDir(t *testing.T, env *testEnv, dir string) {
	t.Helper()
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		ext := filepath.Ext(path)
		if ext != ".go" && ext != ".ts" && ext != ".js" && ext != ".py" && ext != ".java" {
			return nil
		}
		// Индексируем через AST indexer если он есть
		if env.astIdx != nil {
			_ = env.astIdx.IndexFile(context.Background(), path)
		}
		return nil
	})
	if err != nil {
		t.Logf("seedASTFromDir walk error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Mock vector index для тестов search без Qdrant/Ollama
// ---------------------------------------------------------------------------

// mockVectorIndex — stubbed vector index для тестов delegation.
type mockVectorIndex struct {
	searchCalls int
	lastQuery   string
	lastLimit   int
	lastFilter  map[string]any
	results     []any
	searchErr   error
}

func (m *mockVectorIndex) Search(ctx context.Context, query string, limit int, filter map[string]any) ([]any, error) {
	m.searchCalls++
	m.lastQuery = query
	m.lastLimit = limit
	m.lastFilter = filter
	return m.results, m.searchErr
}

func (m *mockVectorIndex) HybridCandidates(ctx context.Context, query string, limit int, filter map[string]any) ([]hybrid.Candidate, error) {
	m.searchCalls++
	m.lastQuery = query
	m.lastLimit = limit
	m.lastFilter = filter
	cands := make([]hybrid.Candidate, 0, len(m.results))
	for _, r := range m.results {
		if c, ok := r.(hybrid.Candidate); ok {
			cands = append(cands, c)
		}
	}
	return cands, m.searchErr
}

func (m *mockVectorIndex) SetResults(results []any) {
	m.results = results
}

func (m *mockVectorIndex) SetError(err error) {
	m.searchErr = err
}

// ---------------------------------------------------------------------------
// Test project paths
// ---------------------------------------------------------------------------

// testProjectDir возвращает абсолютный путь к тестовому проекту.
func testProjectDir(t *testing.T, lang string) string {
	t.Helper()
	// Находим путь относительно текущей working directory
	wd, err := os.Getwd()
	require.NoError(t, err)
	// Ищем tests/testprojects/<lang>
	candidates := []string{
		filepath.Join(wd, "tests", "testprojects", lang),
		filepath.Join(wd, "..", "..", "tests", "testprojects", lang),
		filepath.Join(wd, "..", "..", "..", "tests", "testprojects", lang),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Fatalf("test project %q not found", lang)
	return ""
}

// ---------------------------------------------------------------------------
// Bus verification helper
// ---------------------------------------------------------------------------

// countBusCalls возвращает snapshot bus-счётчиков для MCP вызовов.
func countBusCalls(t *testing.T, bus *state.Bus) map[string]int {
	t.Helper()
	if bus == nil {
		return nil
	}
	snap := bus.Snapshot()
	calls := make(map[string]int)
	for server, stat := range snap.MCP {
		for tool, count := range stat.Calls {
			key := server + "." + tool
			calls[key] = count
		}
	}
	return calls
}
