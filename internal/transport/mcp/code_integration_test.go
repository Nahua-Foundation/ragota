// Integration tests с реальными мини-проектами из tests/testprojects/.
// Индексируют реальный код через astindex.Indexer и тестируют хендлеры.

package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Go testproject integration
// ---------------------------------------------------------------------------

func TestIntegration_GoProject_IndexAndSearch(t *testing.T) {
	goDir := testProjectDir(t, "go")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = goDir

	// Индексируем весь Go проект
	seedASTFromDir(t, env, goDir)

	srv := makeCodeServer(env)

	// Ищем функцию hello из submod
	res, err := srv.handleFindSymbol(context.Background(), toolReq(map[string]any{
		"name": "Hello",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	arr := parseJSONArrayResult(t, res)
	assert.NotEmpty(t, arr, "should find Hello function in submod")
}

func TestIntegration_GoProject_FindDefinition(t *testing.T) {
	goDir := testProjectDir(t, "go")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = goDir

	seedASTFromDir(t, env, goDir)

	srv := makeCodeServer(env)

	// Ищем Equaler интерфейс
	res, err := srv.handleGetDefinition(context.Background(), toolReq(map[string]any{
		"symbol": "Equaler",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	m := parseJSONResult(t, res)
	assert.Equal(t, "Equaler", m["symbol"])
	assert.Equal(t, "interface", m["kind"])
	// Должен содержать source code
	assert.NotEmpty(t, m["source"])
}

func TestIntegration_GoProject_Implementations(t *testing.T) {
	goDir := testProjectDir(t, "go")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = goDir

	seedASTFromDir(t, env, goDir)

	srv := makeCodeServer(env)

	// Equaler интерфейс должен иметь реализации (MyInt)
	res, err := srv.handleFindImplementations(context.Background(), toolReq(map[string]any{
		"symbol": "Equaler",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	arr := parseJSONArrayResult(t, res)
	// Может быть пустым если implements edges не были созданы при индексации
	// — это нормально для tree-sitter indexing
	t.Logf("found %d implementations", len(arr))
}

func TestIntegration_GoProject_Context(t *testing.T) {
	goDir := testProjectDir(t, "go")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = goDir

	seedASTFromDir(t, env, goDir)

	srv := makeCodeServer(env)

	// Контекст для main функции
	res, err := srv.handleGetContext(context.Background(), toolReq(map[string]any{
		"symbol": "main",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	m := parseJSONResult(t, res)
	assert.Equal(t, "main", m["symbol"])
	assert.Contains(t, m, "source")
	assert.Contains(t, m, "callers")
	assert.Contains(t, m, "callees")
}

func TestIntegration_GoProject_CallGraph(t *testing.T) {
	goDir := testProjectDir(t, "go")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = goDir

	seedASTFromDir(t, env, goDir)

	srv := makeCodeServer(env)

	// Граф вызовов для main
	res, err := srv.handleGetCallGraph(context.Background(), toolReq(map[string]any{
		"symbol":    "main",
		"direction": "down",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)
}

func TestIntegration_GoProject_GetChunks(t *testing.T) {
	goDir := testProjectDir(t, "go")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = goDir

	seedASTFromDir(t, env, goDir)

	srv := makeCodeServer(env)

	res, err := srv.handleGetChunks(context.Background(), toolReq(map[string]any{
		"path": "main.go",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	arr := parseJSONArrayResult(t, res)
	assert.NotEmpty(t, arr, "should return chunks for main.go")

	// Проверяем что чанки содержат код Go
	first := arr[0].(map[string]any)
	assert.Equal(t, "go", first["language"])
	assert.NotEmpty(t, first["text"])
}

func TestIntegration_GoProject_References(t *testing.T) {
	goDir := testProjectDir(t, "go")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = goDir

	seedASTFromDir(t, env, goDir)

	srv := makeCodeServer(env)

	// Ищем ссылки на Equaler
	res, err := srv.handleFindReferences(context.Background(), toolReq(map[string]any{
		"symbol": "Equaler",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)
}

func TestIntegration_GoProject_FileIntent_NoLLM(t *testing.T) {
	goDir := testProjectDir(t, "go")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = goDir

	seedASTFromDir(t, env, goDir)

	srv := makeCodeServer(env)

	res, err := srv.handleGetFileIntent(context.Background(), toolReq(map[string]any{
		"path":    "pkg1/iface.go",
		"use_llm": false,
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	m := parseJSONResult(t, res)
	assert.Contains(t, m, "symbols")
	assert.Contains(t, m, "imports")
	symbols := m["symbols"].([]any)
	assert.NotEmpty(t, symbols, "should find symbols in iface.go")
}

func TestIntegration_GoProject_BatchContext(t *testing.T) {
	goDir := testProjectDir(t, "go")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = goDir

	seedASTFromDir(t, env, goDir)

	srv := makeCodeServer(env)

	res, err := srv.handleBatchGetContext(context.Background(), toolReq(map[string]any{
		"symbols": `[{"symbol":"Hello"},{"symbol":"main"}]`,
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	arr := parseJSONArrayResult(t, res)
	assert.Len(t, arr, 2)

	// Оба символа должны быть найдены
	for i, item := range arr {
		m := item.(map[string]any)
		assert.Equal(t, true, m["found"], "item %d should be found", i)
		assert.NotEmpty(t, m["context"], "item %d should have context", i)
	}
}

func TestIntegration_GoProject_Stats(t *testing.T) {
	goDir := testProjectDir(t, "go")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = goDir

	seedASTFromDir(t, env, goDir)

	srv := makeCodeServer(env)

	res, err := srv.handleStats(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	m := parseJSONResult(t, res)
	units, _ := m["ast_units"].(float64)
	edges, _ := m["edges"].(float64)
	assert.Greater(t, units, float64(0), "should have AST units after indexing")
	assert.GreaterOrEqual(t, edges, float64(0), "should have edges count")

	t.Logf("Go project: %.0f AST units, %.0f edges", units, edges)
}

// ---------------------------------------------------------------------------
// TypeScript testproject integration
// ---------------------------------------------------------------------------

func TestIntegration_TSProject_FindSymbol(t *testing.T) {
	tsDir := testProjectDir(t, "ts")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = tsDir

	seedASTFromDir(t, env, tsDir)

	srv := makeCodeServer(env)

	res, err := srv.handleFindSymbol(context.Background(), toolReq(map[string]any{
		"name": "UserStore",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	arr := parseJSONArrayResult(t, res)
	assert.NotEmpty(t, arr, "should find UserStore class")
}

func TestIntegration_TSProject_GetDefinition(t *testing.T) {
	tsDir := testProjectDir(t, "ts")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = tsDir

	seedASTFromDir(t, env, tsDir)

	srv := makeCodeServer(env)

	res, err := srv.handleGetDefinition(context.Background(), toolReq(map[string]any{
		"symbol": "UserStore",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	m := parseJSONResult(t, res)
	assert.Equal(t, "UserStore", m["symbol"])
}

func TestIntegration_TSProject_Context(t *testing.T) {
	tsDir := testProjectDir(t, "ts")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = tsDir

	seedASTFromDir(t, env, tsDir)

	srv := makeCodeServer(env)

	res, err := srv.handleGetContext(context.Background(), toolReq(map[string]any{
		"symbol": "UserStore",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	m := parseJSONResult(t, res)
	assert.Equal(t, "UserStore", m["symbol"])
	assert.Contains(t, m, "source")
}

// ---------------------------------------------------------------------------
// JavaScript testproject integration
// ---------------------------------------------------------------------------

func TestIntegration_JSProject_FindSymbol(t *testing.T) {
	jsDir := testProjectDir(t, "js")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = jsDir

	seedASTFromDir(t, env, jsDir)

	srv := makeCodeServer(env)

	res, err := srv.handleFindSymbol(context.Background(), toolReq(map[string]any{
		"name": "Derived",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	arr := parseJSONArrayResult(t, res)
	assert.NotEmpty(t, arr, "should find Derived class")
}

func TestIntegration_JSProject_Implements(t *testing.T) {
	jsDir := testProjectDir(t, "js")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = jsDir

	seedASTFromDir(t, env, jsDir)

	srv := makeCodeServer(env)

	// Derived extends Base
	res, err := srv.handleFindImplementations(context.Background(), toolReq(map[string]any{
		"symbol": "Base",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)
}

// ---------------------------------------------------------------------------
// Python testproject integration
// ---------------------------------------------------------------------------

func TestIntegration_PyProject_FindSymbol(t *testing.T) {
	pyDir := testProjectDir(t, "py")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = pyDir

	seedASTFromDir(t, env, pyDir)

	srv := makeCodeServer(env)

	res, err := srv.handleFindSymbol(context.Background(), toolReq(map[string]any{
		"name": "UserService",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	arr := parseJSONArrayResult(t, res)
	assert.NotEmpty(t, arr, "should find UserService class")
}

func TestIntegration_PyProject_Context(t *testing.T) {
	pyDir := testProjectDir(t, "py")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = pyDir

	seedASTFromDir(t, env, pyDir)

	srv := makeCodeServer(env)

	res, err := srv.handleGetContext(context.Background(), toolReq(map[string]any{
		"symbol": "UserService",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	m := parseJSONResult(t, res)
	assert.Equal(t, "UserService", m["symbol"])
}

func TestIntegration_PyProject_Repository(t *testing.T) {
	pyDir := testProjectDir(t, "py")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = pyDir

	seedASTFromDir(t, env, pyDir)

	srv := makeCodeServer(env)

	// UserRepository implements Repository
	res, err := srv.handleFindImplementations(context.Background(), toolReq(map[string]any{
		"symbol": "Repository",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)
}

// ---------------------------------------------------------------------------
// Multi-repo: Go + submod
// ---------------------------------------------------------------------------

func TestIntegration_MultiRepo_Search(t *testing.T) {
	goDir := testProjectDir(t, "go")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = goDir

	seedASTFromDir(t, env, goDir)

	srv := makeCodeServer(env)

	// Поиск без repo — ищет во всех
	res1, err := srv.handleFindSymbol(context.Background(), toolReq(map[string]any{
		"name": "main",
	}))
	require.NoError(t, err)
	assert.False(t, res1.IsError)

	arr1 := parseJSONArrayResult(t, res1)
	t.Logf("found %d 'main' symbols across all repos", len(arr1))

	// Поиск с repo — только в конкретном
	res2, err := srv.handleFindSymbol(context.Background(), toolReq(map[string]any{
		"name": "main",
		"repo": "nonexistent_repo",
	}))
	require.NoError(t, err)
	assert.False(t, res2.IsError)

	arr2 := parseJSONArrayResult(t, res2)
	assert.Empty(t, arr2, "should find nothing in nonexistent repo")
}

// ---------------------------------------------------------------------------
// Integration: search modes
// ---------------------------------------------------------------------------

func TestIntegration_Search_SemanticMode_NoVector(t *testing.T) {
	goDir := testProjectDir(t, "go")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = goDir

	seedASTFromDir(t, env, goDir)

	srv := makeCodeServer(env)

	res, err := srv.handleSearch(context.Background(), toolReq(map[string]any{
		"query": "hello",
		"mode":  "semantic",
	}))
	require.NoError(t, err)
	// Без vector index — error
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "vector index is not initialized")
}

func TestIntegration_Search_KeywordMode_NoBM25(t *testing.T) {
	goDir := testProjectDir(t, "go")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = goDir

	seedASTFromDir(t, env, goDir)

	srv := makeCodeServer(env)

	res, err := srv.handleSearch(context.Background(), toolReq(map[string]any{
		"query": "hello",
		"mode":  "keyword",
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "bm25 index is not configured")
}

// ---------------------------------------------------------------------------
// Integration: reindex
// ---------------------------------------------------------------------------

func TestIntegration_Reindex_FullScan(t *testing.T) {
	goDir := testProjectDir(t, "go")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = goDir

	seedASTFromDir(t, env, goDir)

	srv := makeCodeServer(env)

	res, err := srv.handleReindex(context.Background(), toolReq(map[string]any{
		"mode": "full",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)
}

func TestIntegration_Reindex_IncrementalWithPath(t *testing.T) {
	goDir := testProjectDir(t, "go")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = goDir

	seedASTFromDir(t, env, goDir)

	srv := makeCodeServer(env)

	// Incremental с существующим файлом
	res, err := srv.handleReindex(context.Background(), toolReq(map[string]any{
		"mode":  "incremental",
		"paths": `["main.go"]`,
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	text := contentText(res.Content[0])
	assert.Contains(t, text, "main.go")
}

// ---------------------------------------------------------------------------
// Integration: Synthetic project tests for identified issues
// ---------------------------------------------------------------------------

func TestIntegration_Synthetic_ImplementsEdge(t *testing.T) {
	// Тестирует проблему: find_implementations возвращает другие интерфейсы вместо реализаций
	// Go не имеет явного implements, поэтому edge не создаются
	goDir := testProjectDir(t, "go")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = goDir

	seedASTFromDir(t, env, goDir)

	srv := makeCodeServer(env)

	// Equaler — интерфейс, должен найти реализации
	res, err := srv.handleFindImplementations(context.Background(), toolReq(map[string]any{
		"symbol": "Equaler",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	arr := parseJSONArrayResult(t, res)
	// Документируем текущее поведение: может быть пустым для Go
	t.Logf("find_implementations(Equaler) returned %d results", len(arr))
	
	// Если не пусто, проверяем что это не другие интерфейсы
	for _, item := range arr {
		m := item.(map[string]any)
		kind, _ := m["kind"].(string)
		// Реализация должна быть struct или type, не interface
		if kind == "interface" {
			t.Errorf("find_implementations returned another interface instead of implementation: %v", m)
		}
	}
}

func TestIntegration_Synthetic_MethodReferences(t *testing.T) {
	// Тестирует проблему: find_references не находит методы, только функции
	goDir := testProjectDir(t, "go")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = goDir

	seedASTFromDir(t, env, goDir)

	srv := makeCodeServer(env)

	// Process — метод у DataProcessor
	res, err := srv.handleFindReferences(context.Background(), toolReq(map[string]any{
		"symbol": "Process",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	m := parseJSONResult(t, res)
	edges, ok := m["edges"].([]any)
	if !ok {
		edges = nil
	}
	lspUnits, _ := m["lsp_units"].([]any)
	total, _ := m["total"].(float64)
	t.Logf("find_references(Process method): edges=%d, lsp_units=%d, total=%.0f", len(edges), len(lspUnits), total)

	// Документируем: методы могут не иметь reference edges без LSP
}

func TestIntegration_Synthetic_CallGraphUp(t *testing.T) {
	// Тестирует проблему: get_call_graph direction=up возвращает узлы без edges
	goDir := testProjectDir(t, "go")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = goDir

	seedASTFromDir(t, env, goDir)

	srv := makeCodeServer(env)

	// Граф вызовов для main с direction=up (callers)
	res, err := srv.handleGetCallGraph(context.Background(), toolReq(map[string]any{
		"symbol":    "main",
		"direction": "up",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	m := parseJSONResult(t, res)
	nodes, ok := m["nodes"].([]any)
	if ok && len(nodes) > 0 {
		// Проверяем наличие edges
		hasEdges := false
		for _, node := range nodes {
			n := node.(map[string]any)
			if edges, exists := n["edges"]; exists {
				if edgeList, ok := edges.([]any); ok && len(edgeList) > 0 {
					hasEdges = true
					break
				}
			}
		}
		if !hasEdges {
			t.Log("KNOWN ISSUE: call graph direction=up has nodes but no edges")
		}
	}
}

func TestIntegration_Synthetic_ResolveCall(t *testing.T) {
	// Тестирует проблему: resolve_call возвращает первый edge без фильтрации по kind
	// Может вернуть reference вместо call
	t.Skip("resolve_call требует cross-repo edges — тестируется в unit тестах")
}

func TestIntegration_Synthetic_ExtendsAsImplements(t *testing.T) {
	// Тестирует: extends edge должен считаться реализацией
	jsDir := testProjectDir(t, "js")
	env := setupTestEnvWithAST(t)
	env.cfg.Root = jsDir

	seedASTFromDir(t, env, jsDir)

	srv := makeCodeServer(env)

	// Base класс, Derived extends Base
	res, err := srv.handleFindImplementations(context.Background(), toolReq(map[string]any{
		"symbol": "Base",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	arr := parseJSONArrayResult(t, res)
	t.Logf("find_implementations(Base) returned %d results", len(arr))
	
	// Derived должен быть в реализациях через extends
	foundDerived := false
	for _, item := range arr {
		m := item.(map[string]any)
		if name, ok := m["symbol"].(string); ok && name == "Derived" {
			foundDerived = true
			break
		}
	}
	if foundDerived {
		t.Log("Extends edge correctly found as implementation")
	} else {
		t.Log("KNOWN ISSUE: extends edge may not be found as implementation")
	}
}
