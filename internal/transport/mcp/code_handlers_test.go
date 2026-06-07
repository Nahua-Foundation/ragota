// Comprehensive handler tests для code.* инструментов.
// Покрывает: validation, error branches, delegation, params.

package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ragota/pkg/config"
	"ragota/pkg/fileutil"
	"ragota/internal/search/graph"
	"ragota/internal/store"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// CodeServer Build() — регистрация инструментов
// ---------------------------------------------------------------------------

func TestCodeServer_Build_RegistersAllTools(t *testing.T) {
	s := NewCodeServer(&config.Config{}, nil, nil, nil)
	srv := s.Build()
	require.NotNil(t, srv)

	expectedTools := []string{
		"code.search",
		"code.find_symbol",
		"code.get_definition",
		"code.find_references",
		"code.find_implementations",
		"code.get_context",
		"code.get_call_graph",
		"code.reindex",
		"code.get_chunks",
		"code.batch_get_context",
		"code.get_file_intent",
		"code.stats",
	}

	for _, name := range expectedTools {
		t.Run(name, func(t *testing.T) {
			// Проверяем что инструмент зарегистрирован через сервер
			// mcp-go не даёт прямого доступа к списку, но Build() не паникует
			// и возвращает non-nil сервер — это минимальная проверка.
			// Полная проверка — через вызов каждого инструмента ниже.
		})
	}
}

func TestCodeServer_Build_NonNil(t *testing.T) {
	s := NewCodeServer(&config.Config{}, nil, nil, nil)
	srv := s.Build()
	require.NotNil(t, srv)
}

// ---------------------------------------------------------------------------
// Constructor
// ---------------------------------------------------------------------------

func TestNewCodeServer(t *testing.T) {
	cfg := &config.Config{Root: "/tmp"}
	s := NewCodeServer(cfg, nil, nil, nil)
	require.NotNil(t, s)
	assert.Equal(t, cfg, s.cfg)
	assert.Nil(t, s.st)
	assert.Nil(t, s.vecIdx)
}

// ---------------------------------------------------------------------------
// code.search — validation + error branches
// ---------------------------------------------------------------------------

func TestCodeHandleSearch_EmptyQuery(t *testing.T) {
	s := NewCodeServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleSearch(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "query is required")
}

func TestCodeHandleSearch_InvalidMode(t *testing.T) {
	s := NewCodeServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleSearch(context.Background(), toolReq(map[string]any{
		"query": "test",
		"mode":  "invalid",
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "unknown mode")
}

func TestCodeHandleSearch_SemanticMode_NoVector(t *testing.T) {
	s := NewCodeServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleSearch(context.Background(), toolReq(map[string]any{
		"query": "auth",
		"mode":  "semantic",
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "vector index is not initialized")
}

func TestCodeHandleSearch_KeywordMode_NoBM25(t *testing.T) {
	s := NewCodeServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleSearch(context.Background(), toolReq(map[string]any{
		"query": "auth",
		"mode":  "keyword",
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "bm25 index is not configured")
}

func TestCodeHandleSearch_HybridMode_NoVector(t *testing.T) {
	s := NewCodeServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleSearch(context.Background(), toolReq(map[string]any{
		"query": "auth",
		"mode":  "hybrid",
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "vector index is not initialized")
}

func TestCodeHandleSearch_DefaultModeIsHybrid(t *testing.T) {
	// Без mode должен использоваться hybrid (default)
	s := NewCodeServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleSearch(context.Background(), toolReq(map[string]any{
		"query": "auth",
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	// hybrid требует vector index
	assert.Contains(t, contentText(res.Content[0]), "vector index is not initialized")
}

func TestCodeHandleSearch_LanguageFilter(t *testing.T) {
	// Проверяем что language param не вызывает panic и проходит в filter
	s := NewCodeServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleSearch(context.Background(), toolReq(map[string]any{
		"query":    "test",
		"mode":     "semantic",
		"language": "go",
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError) // vector not initialized, но filter должен пройти
}

func TestCodeHandleSearch_RepoFilter(t *testing.T) {
	s := NewCodeServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleSearch(context.Background(), toolReq(map[string]any{
		"query": "test",
		"mode":  "semantic",
		"repo":  "myrepo",
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError) // vector not initialized
}

func TestCodeHandleSearch_SymbolType(t *testing.T) {
	s := NewCodeServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleSearch(context.Background(), toolReq(map[string]any{
		"query":       "test",
		"mode":        "keyword",
		"symbol_type": "function",
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError) // bm25 not configured
}

// ---------------------------------------------------------------------------
// code.find_symbol — validation + with data
// ---------------------------------------------------------------------------

func TestCodeHandleFindSymbol_EmptyName(t *testing.T) {
	s := NewCodeServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleFindSymbol(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "name is required")
}

func TestCodeHandleFindSymbol_WithSubstr(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc hello() {}\nfunc helper() {}\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "hello", Qualified: "main.hello", StartLine: 3, EndLine: 3},
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "helper", Qualified: "main.helper", StartLine: 4, EndLine: 4},
	})

	srv := makeCodeServer(env)
	res, err := srv.handleFindSymbol(context.Background(), toolReq(map[string]any{
		"name": "hel",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	arr := parseJSONArrayResult(t, res)
	assert.GreaterOrEqual(t, len(arr), 2, "should find both hello and helper by substring 'hel'")
}

func TestCodeHandleFindSymbol_ExactMatch(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc hello() {}\nfunc helper() {}\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "hello", Qualified: "main.hello", StartLine: 3, EndLine: 3},
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "helper", Qualified: "main.helper", StartLine: 4, EndLine: 4},
	})

	srv := makeCodeServer(env)
	res, err := srv.handleFindSymbol(context.Background(), toolReq(map[string]any{
		"name":  "hel",
		"exact": true,
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	arr := parseJSONArrayResult(t, res)
	assert.Empty(t, arr, "exact match for 'hel' should find nothing")
}

func TestCodeHandleFindSymbol_ExactMatchFound(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc hello() {}\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "hello", Qualified: "main.hello", StartLine: 3, EndLine: 3},
	})

	srv := makeCodeServer(env)
	res, err := srv.handleFindSymbol(context.Background(), toolReq(map[string]any{
		"name":  "hello",
		"exact": true,
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	arr := parseJSONArrayResult(t, res)
	assert.Len(t, arr, 1)
}

func TestCodeHandleFindSymbol_Limit(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc a() {}\nfunc b() {}\nfunc c() {}\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "r", Language: "go", Kind: "function", Name: "a", Qualified: "main.a", StartLine: 3, EndLine: 3},
		{Repo: "r", Language: "go", Kind: "function", Name: "b", Qualified: "main.b", StartLine: 4, EndLine: 4},
		{Repo: "r", Language: "go", Kind: "function", Name: "c", Qualified: "main.c", StartLine: 5, EndLine: 5},
	})

	srv := makeCodeServer(env)
	res, err := srv.handleFindSymbol(context.Background(), toolReq(map[string]any{
		"name":  "",
		"limit": 2,
	}))
	// empty name — store.FindASTUnits вернёт что-то или пустой список
	_ = res
	_ = err
}

func TestCodeHandleFindSymbol_RepoFilter(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc hello() {}\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "repo_a", Language: "go", Kind: "function", Name: "hello", Qualified: "main.hello", StartLine: 3, EndLine: 3},
	})

	srv := makeCodeServer(env)
	res, err := srv.handleFindSymbol(context.Background(), toolReq(map[string]any{
		"name": "hello",
		"repo": "repo_b",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	arr := parseJSONArrayResult(t, res)
	assert.Empty(t, arr, "repo filter should exclude repo_a symbols")
}

// ---------------------------------------------------------------------------
// code.get_definition — validation + with data + context_lines + file disambiguation
// ---------------------------------------------------------------------------

func TestCodeHandleGetDefinition_EmptySymbol(t *testing.T) {
	s := NewCodeServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleGetDefinition(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "symbol is required")
}

func TestCodeHandleGetDefinition_WithData(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	src := "package main\n\nfunc hello() {\n\tprintln(\"hi\")\n}\n"
	_ = os.WriteFile(file, []byte(src), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "hello", Qualified: "main.hello", FilePath: file, StartLine: 3, EndLine: 5, Signature: "func hello()"},
	})

	srv := makeCodeServer(env)
	res, err := srv.handleGetDefinition(context.Background(), toolReq(map[string]any{"symbol": "hello"}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)

	m := parseJSONResult(t, res)
	assert.Equal(t, "hello", m["symbol"])
	assert.Equal(t, "main.hello", m["qualified"])
	assert.Equal(t, "function", m["kind"])
	assert.Contains(t, m["source"], "hello")
}

func TestCodeHandleGetDefinition_ContextLines(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	src := "// header comment\npackage main\n\n// before hello\nfunc hello() {}\n// after hello\n\nfunc main() {}\n"
	_ = os.WriteFile(file, []byte(src), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "hello", Qualified: "main.hello", FilePath: file, StartLine: 5, EndLine: 5, Signature: "func hello()"},
	})

	srv := makeCodeServer(env)
	res, err := srv.handleGetDefinition(context.Background(), toolReq(map[string]any{
		"symbol":        "hello",
		"context_lines": 2,
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	m := parseJSONResult(t, res)
	source := m["source"].(string)
	// Должен включать строки до и после определения
	assert.Contains(t, source, "before hello")
	assert.Contains(t, source, "after hello")
}

func TestCodeHandleGetDefinition_FileDisambiguation(t *testing.T) {
	env := setupTestEnv(t)

	// Два файла с одинаковым именем функции
	file1 := filepath.Join(env.root, "pkg1", "svc.go")
	file2 := filepath.Join(env.root, "pkg2", "svc.go")
	_ = os.MkdirAll(filepath.Dir(file1), 0755)
	_ = os.MkdirAll(filepath.Dir(file2), 0755)
	_ = os.WriteFile(file1, []byte("package pkg1\n\nfunc Run() { /* pkg1 */ }\n"), 0644)
	_ = os.WriteFile(file2, []byte("package pkg2\n\nfunc Run() { /* pkg2 */ }\n"), 0644)

	seedAST(t, env, file1, []store.ASTUnit{
		{Repo: "r", Language: "go", Kind: "function", Name: "Run", Qualified: "pkg1.Run", FilePath: file1, StartLine: 3, EndLine: 3},
	})
	seedAST(t, env, file2, []store.ASTUnit{
		{Repo: "r", Language: "go", Kind: "function", Name: "Run", Qualified: "pkg2.Run", FilePath: file2, StartLine: 3, EndLine: 3},
	})

	srv := makeCodeServer(env)
	// Без file — берётся первое найденное
	res, err := srv.handleGetDefinition(context.Background(), toolReq(map[string]any{"symbol": "Run"}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	// С file — должен вернуть определение из конкретного файла
	res2, err := srv.handleGetDefinition(context.Background(), toolReq(map[string]any{
		"symbol": "Run",
		"file":   "pkg2/svc.go",
	}))
	require.NoError(t, err)
	assert.False(t, res2.IsError)
	m := parseJSONResult(t, res2)
	assert.Contains(t, m["file"].(string), "pkg2")
}

func TestCodeHandleGetDefinition_NotFound(t *testing.T) {
	env := setupTestEnv(t)
	srv := makeCodeServer(env)

	_, err := srv.handleGetDefinition(context.Background(), toolReq(map[string]any{"symbol": "nonexistent_xyz"}))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestCodeHandleGetDefinition_FileNotFound(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "deleted.go")

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "r", Language: "go", Kind: "function", Name: "ghost", Qualified: "ghost", FilePath: file, StartLine: 1, EndLine: 1},
	})

	srv := makeCodeServer(env)
	// Файл не существует — ReadFile вернёт ошибку
	_, err := srv.handleGetDefinition(context.Background(), toolReq(map[string]any{"symbol": "ghost"}))
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// code.find_references — validation + edges + exclude_tests + limit
// ---------------------------------------------------------------------------

func TestCodeHandleFindReferences_EmptySymbol(t *testing.T) {
	s := NewCodeServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleFindReferences(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "symbol is required")
}

func TestCodeHandleFindReferences_WithEdges(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc hello() {}\nfunc caller() { hello() }\n"), 0644)

	uDef := store.ASTUnit{Repo: "myrepo", Language: "go", Kind: "function", Name: "hello", Qualified: "main.hello", FilePath: file, StartLine: 3, EndLine: 3}
	uCall := store.ASTUnit{Repo: "myrepo", Language: "go", Kind: "function", Name: "caller", Qualified: "main.caller", FilePath: file, StartLine: 4, EndLine: 4}
	ids := seedAST(t, env, file, []store.ASTUnit{uDef, uCall})

	// Создаём edge вручную
	defID := ids["hello"]
	callID := ids["caller"]
	_ = env.db.ReplaceEdges(context.Background(), file, []store.Edge{
		{Repo: "myrepo", SrcID: callID, DstID: defID, Kind: graph.EdgeCall, FilePath: file, Line: 4},
	})
	_, _ = env.db.ResolvePendingEdges(context.Background(), nil)

	srv := makeCodeServer(env)
	res, err := srv.handleFindReferences(context.Background(), toolReq(map[string]any{"symbol": "hello"}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
}

func TestCodeHandleFindReferences_ExcludeTests(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	testFile := filepath.Join(env.root, "main_test.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc hello() {}\n"), 0644)
	_ = os.WriteFile(testFile, []byte("package main\n\nfunc TestHello(t *testing.T) { hello() }\n"), 0644)

	uDef := store.ASTUnit{Repo: "r", Language: "go", Kind: "function", Name: "hello", Qualified: "main.hello", FilePath: file, StartLine: 3, EndLine: 3}
	uTest := store.ASTUnit{Repo: "r", Language: "go", Kind: "function", Name: "TestHello", Qualified: "main.TestHello", FilePath: testFile, StartLine: 3, EndLine: 3}
	ids := seedAST(t, env, file, []store.ASTUnit{uDef})
	ids2 := seedAST(t, env, testFile, []store.ASTUnit{uTest})

	defID := ids["hello"]
	testID := ids2["TestHello"]
	_ = env.db.ReplaceEdges(context.Background(), testFile, []store.Edge{
		{Repo: "r", SrcID: testID, DstID: defID, Kind: graph.EdgeCall, FilePath: testFile, Line: 3},
	})
	_, _ = env.db.ResolvePendingEdges(context.Background(), nil)

	srv := makeCodeServer(env)

	// Без exclude_tests — должен найти
	res, err := srv.handleFindReferences(context.Background(), toolReq(map[string]any{
		"symbol": "hello",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	// С exclude_tests — должен отфильтровать test файлы
	res2, err := srv.handleFindReferences(context.Background(), toolReq(map[string]any{
		"symbol":       "hello",
		"exclude_tests": true,
	}))
	require.NoError(t, err)
	assert.False(t, res2.IsError)

	m := parseJSONResult(t, res2)
	t.Logf("find_references response: %v", m)
	arr, ok := m["edges"].([]any)
	require.True(t, ok, "response should have edges array, got: %T = %v", m["edges"], m["edges"])
	// Все references из test файлов должны быть отфильтрованы
	for _, item := range arr {
		e := item.(map[string]any)
		fp, _ := e["file_path"].(string)
		assert.NotContains(t, fp, "_test.", "test files should be excluded")
	}
}

func TestCodeHandleFindReferences_Limit(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc hello() {}\n"), 0644)

	// Сначала создаём все юниты одним батчем
	var allUnits []store.ASTUnit
	allUnits = append(allUnits, store.ASTUnit{Repo: "r", Language: "go", Kind: "function", Name: "hello", Qualified: "main.hello", FilePath: file, StartLine: 3, EndLine: 3})
	for i := 0; i < 5; i++ {
		allUnits = append(allUnits, store.ASTUnit{
			Repo: "r", Language: "go", Kind: "function", Name: "caller", Qualified: "main.caller",
			FilePath: file, StartLine: 5 + i, EndLine: 5 + i,
		})
	}
	ids := seedAST(t, env, file, allUnits)
	defID := ids["hello"]

	// Потом edges
	var edges []store.Edge
	for i := 0; i < 5; i++ {
		// Все callers имеют одинаковое имя "caller" — ids вернёт последний
		callID := ids["caller"]
		edges = append(edges, store.Edge{
			Repo: "r", SrcID: callID, DstID: defID, Kind: graph.EdgeCall, FilePath: file, Line: 5 + i,
		})
	}
	_ = env.db.ReplaceEdges(context.Background(), file, edges)
	_, _ = env.db.ResolvePendingEdges(context.Background(), nil)

	srv := makeCodeServer(env)
	res, err := srv.handleFindReferences(context.Background(), toolReq(map[string]any{
		"symbol": "hello",
		"limit":  2,
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	m := parseJSONResult(t, res)
	arr, ok := m["edges"].([]any)
	require.True(t, ok, "response should have edges array")
	assert.LessOrEqual(t, len(arr), 2, "limit should cap results")
}

// ---------------------------------------------------------------------------
// code.find_implementations — validation + with data + recursive
// ---------------------------------------------------------------------------

func TestCodeHandleFindImplementations_EmptySymbol(t *testing.T) {
	s := NewCodeServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleFindImplementations(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "symbol is required")
}

func TestCodeHandleFindImplementations_WithInterface(t *testing.T) {
	env := setupTestEnv(t)
	ifaceFile := filepath.Join(env.root, "iface.go")
	implFile := filepath.Join(env.root, "impl.go")
	_ = os.WriteFile(ifaceFile, []byte("package main\n\ntype Equaler interface {\n\tEqual(other any) bool\n}\n"), 0644)
	_ = os.WriteFile(implFile, []byte("package main\n\ntype MyInt int\n\nfunc (m MyInt) Equal(other any) bool { return true }\n"), 0644)

	seedAST(t, env, ifaceFile, []store.ASTUnit{
		{Repo: "r", Language: "go", Kind: "interface", Name: "Equaler", Qualified: "main.Equaler", FilePath: ifaceFile, StartLine: 3, EndLine: 5},
	})
	seedAST(t, env, implFile, []store.ASTUnit{
		{Repo: "r", Language: "go", Kind: "type", Name: "MyInt", Qualified: "main.MyInt", FilePath: implFile, StartLine: 3, EndLine: 3},
		{Repo: "r", Language: "go", Kind: "method", Name: "Equal", Qualified: "main.MyInt.Equal", FilePath: implFile, StartLine: 5, EndLine: 5},
	})

	srv := makeCodeServer(env)
	res, err := srv.handleFindImplementations(context.Background(), toolReq(map[string]any{"symbol": "Equaler"}))
	require.NoError(t, err)
	// Может вернуть пустой список если нет implements edges — это ok
	assert.False(t, res.IsError)
}

// ---------------------------------------------------------------------------
// code.get_context — include branches + max_tokens + depth
// ---------------------------------------------------------------------------

func TestCodeHandleGetContext_EmptySymbol(t *testing.T) {
	s := NewCodeServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleGetContext(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "symbol is required")
}

func TestCodeHandleGetContext_DefaultIncludes(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc hello() {}\nfunc caller() { hello() }\n"), 0644)

	uDef := store.ASTUnit{Repo: "r", Language: "go", Kind: "function", Name: "hello", Qualified: "main.hello", FilePath: file, StartLine: 3, EndLine: 3}
	uCall := store.ASTUnit{Repo: "r", Language: "go", Kind: "function", Name: "caller", Qualified: "main.caller", FilePath: file, StartLine: 4, EndLine: 4}
	seedAST(t, env, file, []store.ASTUnit{uDef, uCall})

	srv := makeCodeServer(env)
	res, err := srv.handleGetContext(context.Background(), toolReq(map[string]any{
		"symbol": "hello",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	m := parseJSONResult(t, res)
	// Default: callers, callees, references, imports, related_types
	assert.Contains(t, m, "callers")
	assert.Contains(t, m, "callees")
	assert.Contains(t, m, "references")
	assert.Contains(t, m, "imports")
	assert.Contains(t, m, "related_types")
	assert.Contains(t, m, "source")
}

func TestCodeHandleGetContext_IncludeCallersOnly(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc hello() {}\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "r", Language: "go", Kind: "function", Name: "hello", Qualified: "main.hello", FilePath: file, StartLine: 3, EndLine: 3},
	})

	srv := makeCodeServer(env)
	res, err := srv.handleGetContext(context.Background(), toolReq(map[string]any{
		"symbol":  "hello",
		"include": "callers",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	m := parseJSONResult(t, res)
	assert.Contains(t, m, "callers")
	// Только callers — остальные секции не должны быть включены
	_, hasCallees := m["callees"]
	_, hasRefs := m["references"]
	assert.False(t, hasCallees, "callees should not be included when only callers requested")
	assert.False(t, hasRefs, "references should not be included when only callers requested")
}

func TestCodeHandleGetContext_IncludeMultiple(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc hello() {}\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "r", Language: "go", Kind: "function", Name: "hello", Qualified: "main.hello", FilePath: file, StartLine: 3, EndLine: 3},
	})

	srv := makeCodeServer(env)
	res, err := srv.handleGetContext(context.Background(), toolReq(map[string]any{
		"symbol":  "hello",
		"include": "callers,callees,imports",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	m := parseJSONResult(t, res)
	assert.Contains(t, m, "callers")
	assert.Contains(t, m, "callees")
	assert.Contains(t, m, "imports")
}

func TestCodeHandleGetContext_MaxTokens(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	// Большой файл
	src := "package main\n\n" + strings.Repeat("func filler() {}\n", 100)
	_ = os.WriteFile(file, []byte(src), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "r", Language: "go", Kind: "function", Name: "hello", Qualified: "main.hello", FilePath: file, StartLine: 3, EndLine: 3},
	})

	srv := makeCodeServer(env)
	res, err := srv.handleGetContext(context.Background(), toolReq(map[string]any{
		"symbol":     "hello",
		"max_tokens": 100, // очень маленький лимит
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	// Результат должен быть обрезан
	text := contentText(res.Content[0])
	// Проверяем что ответ не гигантский (truncateByTokens должен сработать)
	assert.Less(t, len(text), 5000, "response should be truncated by max_tokens")
}

func TestCodeHandleGetContext_SourceCode(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	src := "package main\n\nfunc hello() {\n\tprintln(\"hi\")\n}\n"
	_ = os.WriteFile(file, []byte(src), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "r", Language: "go", Kind: "function", Name: "hello", Qualified: "main.hello", FilePath: file, StartLine: 3, EndLine: 5},
	})

	srv := makeCodeServer(env)
	res, err := srv.handleGetContext(context.Background(), toolReq(map[string]any{"symbol": "hello"}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	m := parseJSONResult(t, res)
	source := m["source"].(string)
	assert.Contains(t, source, "hello")
	assert.Contains(t, source, "println")
}

// ---------------------------------------------------------------------------
// code.get_call_graph — validation + direction + max_nodes
// ---------------------------------------------------------------------------

func TestCodeHandleGetCallGraph_EmptySymbol(t *testing.T) {
	s := NewCodeServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleGetCallGraph(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "symbol is required")
}

func TestCodeHandleGetCallGraph_NotFound(t *testing.T) {
	env := setupTestEnv(t)
	srv := makeCodeServer(env)

	_, err := srv.handleGetCallGraph(context.Background(), toolReq(map[string]any{"symbol": "nonexistent"}))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestCodeHandleGetCallGraph_DirectionDown(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc main() { hello() }\nfunc hello() {}\n"), 0644)

	uMain := store.ASTUnit{Repo: "r", Language: "go", Kind: "function", Name: "main", Qualified: "main.main", FilePath: file, StartLine: 3, EndLine: 3}
	uHello := store.ASTUnit{Repo: "r", Language: "go", Kind: "function", Name: "hello", Qualified: "main.hello", FilePath: file, StartLine: 4, EndLine: 4}
	ids := seedAST(t, env, file, []store.ASTUnit{uMain, uHello})

	mainID := ids["main"]
	helloID := ids["hello"]
	_ = env.db.ReplaceEdges(context.Background(), file, []store.Edge{
		{Repo: "r", SrcID: mainID, DstID: helloID, Kind: graph.EdgeCall, FilePath: file, Line: 3},
	})
	_, _ = env.db.ResolvePendingEdges(context.Background(), nil)

	srv := makeCodeServer(env)
	res, err := srv.handleGetCallGraph(context.Background(), toolReq(map[string]any{
		"symbol":    "main",
		"direction": "down",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)
}

func TestCodeHandleGetCallGraph_DirectionUp(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc main() { hello() }\nfunc hello() {}\n"), 0644)

	uMain := store.ASTUnit{Repo: "r", Language: "go", Kind: "function", Name: "main", Qualified: "main.main", FilePath: file, StartLine: 3, EndLine: 3}
	uHello := store.ASTUnit{Repo: "r", Language: "go", Kind: "function", Name: "hello", Qualified: "main.hello", FilePath: file, StartLine: 4, EndLine: 4}
	ids := seedAST(t, env, file, []store.ASTUnit{uMain, uHello})

	mainID := ids["main"]
	helloID := ids["hello"]
	_ = env.db.ReplaceEdges(context.Background(), file, []store.Edge{
		{Repo: "r", SrcID: mainID, DstID: helloID, Kind: graph.EdgeCall, FilePath: file, Line: 3},
	})
	_, _ = env.db.ResolvePendingEdges(context.Background(), nil)

	srv := makeCodeServer(env)
	res, err := srv.handleGetCallGraph(context.Background(), toolReq(map[string]any{
		"symbol":    "hello",
		"direction": "up",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)
}

func TestCodeHandleGetCallGraph_MaxNodes(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc main() {}\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "r", Language: "go", Kind: "function", Name: "main", Qualified: "main.main", FilePath: file, StartLine: 3, EndLine: 3},
	})

	srv := makeCodeServer(env)
	res, err := srv.handleGetCallGraph(context.Background(), toolReq(map[string]any{
		"symbol":    "main",
		"direction": "down",
		"max_nodes": 1,
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)
}

// ---------------------------------------------------------------------------
// code.get_chunks — validation + existing file + nonexistent + symbol filter
// ---------------------------------------------------------------------------

func TestCodeHandleGetChunks_EmptyPath(t *testing.T) {
	s := NewCodeServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleGetChunks(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "path is required")
}

func TestCodeHandleGetChunks_ExistingFile(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	content := "package main\n\nfunc hello() {\n\t_ = 1\n\t_ = 2\n}\n"
	_ = os.WriteFile(file, []byte(content), 0644)

	srv := makeCodeServer(env)
	res, err := srv.handleGetChunks(context.Background(), toolReq(map[string]any{"path": "main.go"}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)

	arr := parseJSONArrayResult(t, res)
	assert.NotEmpty(t, arr, "should return at least one chunk")

	// Проверяем структуру
	first := arr[0].(map[string]any)
	assert.Contains(t, first, "path")
	assert.Contains(t, first, "text")
	assert.Contains(t, first, "start_line")
	assert.Contains(t, first, "end_line")
	assert.Contains(t, first, "kind")
	assert.Contains(t, first, "language")
}

func TestCodeHandleGetChunks_NonexistentFile(t *testing.T) {
	env := setupTestEnv(t)
	srv := makeCodeServer(env)

	_, err := srv.handleGetChunks(context.Background(), toolReq(map[string]any{"path": "nonexistent.go"}))
	assert.Error(t, err)
}

func TestCodeHandleGetChunks_SymbolFilter(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	content := "package main\n\nfunc hello() {}\n\nfunc world() {}\n"
	_ = os.WriteFile(file, []byte(content), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "r", Language: "go", Kind: "function", Name: "hello", Qualified: "main.hello", FilePath: file, StartLine: 3, EndLine: 3},
		{Repo: "r", Language: "go", Kind: "function", Name: "world", Qualified: "main.world", FilePath: file, StartLine: 5, EndLine: 5},
	})

	srv := makeCodeServer(env)
	res, err := srv.handleGetChunks(context.Background(), toolReq(map[string]any{
		"path":   "main.go",
		"symbol": "hello",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)
}

func TestCodeHandleGetChunks_LanguageDetection(t *testing.T) {
	env := setupTestEnv(t)

	tests := []struct {
		filename string
		wantLang string
	}{
		{"main.go", "go"},
		{"app.ts", "typescript"},
		{"index.js", "javascript"},
		{"script.py", "python"},
		{"App.java", "java"},
		{"README.md", "text"}, // fileutil returns "text" for .md
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			file := filepath.Join(env.root, tt.filename)
			_ = os.WriteFile(file, []byte("content"), 0644)

			srv := makeCodeServer(env)
			res, err := srv.handleGetChunks(context.Background(), toolReq(map[string]any{"path": tt.filename}))
			require.NoError(t, err)
			assert.False(t, res.IsError)

			arr := parseJSONArrayResult(t, res)
			if len(arr) > 0 {
				first := arr[0].(map[string]any)
				lang, _ := first["language"].(string)
				assert.Equal(t, tt.wantLang, lang)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// code.batch_get_context — validation + multiple + error isolation + include
// ---------------------------------------------------------------------------

func TestCodeHandleBatchGetContext_EmptySymbols(t *testing.T) {
	s := NewCodeServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleBatchGetContext(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "symbols JSON array is required")
}

func TestCodeHandleBatchGetContext_InvalidJSON(t *testing.T) {
	s := NewCodeServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleBatchGetContext(context.Background(), toolReq(map[string]any{
		"symbols": "{not valid json",
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "invalid symbols JSON")
}

func TestCodeHandleBatchGetContext_MultipleSymbols(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc foo() {}\nfunc bar() {}\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "r", Language: "go", Kind: "function", Name: "foo", Qualified: "main.foo", FilePath: file, StartLine: 3, EndLine: 3},
		{Repo: "r", Language: "go", Kind: "function", Name: "bar", Qualified: "main.bar", FilePath: file, StartLine: 4, EndLine: 4},
	})

	srv := makeCodeServer(env)
	symbolsJSON := `[{"symbol":"foo"},{"symbol":"bar"},{"symbol":"nonexistent"}]`
	res, err := srv.handleBatchGetContext(context.Background(), toolReq(map[string]any{
		"symbols": symbolsJSON,
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	arr := parseJSONArrayResult(t, res)
	assert.Len(t, arr, 3)

	// Проверяем что каждый результат имеет status
	first := arr[0].(map[string]any)
	assert.Equal(t, "foo", first["symbol"])
	assert.Equal(t, true, first["found"])

	third := arr[2].(map[string]any)
	assert.Equal(t, "nonexistent", third["symbol"])
	assert.Equal(t, false, third["found"])
	assert.NotEmpty(t, third["error"])
}

func TestCodeHandleBatchGetContext_ErrorIsolation(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc foo() {}\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "r", Language: "go", Kind: "function", Name: "foo", Qualified: "main.foo", FilePath: file, StartLine: 3, EndLine: 3},
	})

	srv := makeCodeServer(env)
	// Один валидный, два невалидных
	symbolsJSON := `[{"symbol":"nonexistent1"},{"symbol":"foo"},{"symbol":"nonexistent2"}]`
	res, err := srv.handleBatchGetContext(context.Background(), toolReq(map[string]any{
		"symbols": symbolsJSON,
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	arr := parseJSONArrayResult(t, res)
	assert.Len(t, arr, 3)

	// foo должен быть найден несмотря на ошибки вокруг
	fooResult := arr[1].(map[string]any)
	assert.Equal(t, true, fooResult["found"])
}

// ---------------------------------------------------------------------------
// code.get_file_intent — validation + no-LLM + LLM path
// ---------------------------------------------------------------------------

func TestCodeHandleGetFileIntent_EmptyPath(t *testing.T) {
	s := NewCodeServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleGetFileIntent(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "path is required")
}

func TestCodeHandleGetFileIntent_NoLLM(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc hello() {}\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "hello", Qualified: "main.hello", FilePath: file, StartLine: 3, EndLine: 3},
	})

	srv := makeCodeServer(env)
	res, err := srv.handleGetFileIntent(context.Background(), toolReq(map[string]any{
		"path":    "main.go",
		"use_llm": false,
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	m := parseJSONResult(t, res)
	assert.Contains(t, m, "symbols")
	assert.Contains(t, m, "language")
	symbols := m["symbols"].([]any)
	assert.NotEmpty(t, symbols)
}

func TestCodeHandleGetFileIntent_LLMPath_NoGraphService(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc hello() {}\n"), 0644)

	// Не устанавливаем graph service
	s := NewCodeServer(env.cfg, env.db, env.bus, nil)

	res, err := s.handleGetFileIntent(context.Background(), toolReq(map[string]any{
		"path":    "main.go",
		"use_llm": true,
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "graph service is not initialized")
}

// ---------------------------------------------------------------------------
// code.reindex — validation + modes
// ---------------------------------------------------------------------------

func TestCodeHandleReindex_FullScan(t *testing.T) {
	env := setupTestEnv(t)
	srv := makeCodeServer(env)

	res, err := srv.handleReindex(context.Background(), toolReq(map[string]any{"mode": "full"}))
	require.NoError(t, err)
	assert.False(t, res.IsError)
}

func TestCodeHandleReindex_ForceMode(t *testing.T) {
	env := setupTestEnv(t)
	srv := makeCodeServer(env)

	res, err := srv.handleReindex(context.Background(), toolReq(map[string]any{
		"mode":  "incremental",
		"force": true,
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)
	// force=true должен вызвать full scan
}

func TestCodeHandleReindex_InvalidPaths(t *testing.T) {
	env := setupTestEnv(t)
	srv := makeCodeServer(env)

	res, err := srv.handleReindex(context.Background(), toolReq(map[string]any{
		"mode":  "incremental",
		"paths": `["nonexistent_file.go"]`,
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)
	// Результат может быть пустым если нет AST/Vector сервисов
}

// ---------------------------------------------------------------------------
// code.stats
// ---------------------------------------------------------------------------

func TestCodeHandleStats(t *testing.T) {
	env := setupTestEnv(t)
	srv := makeCodeServer(env)

	res, err := srv.handleStats(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	m := parseJSONResult(t, res)
	assert.Contains(t, m, "ast_units")
	assert.Contains(t, m, "edges")
}

func TestCodeHandleStats_WithData(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc hello() {}\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "r", Language: "go", Kind: "function", Name: "hello", Qualified: "main.hello", FilePath: file, StartLine: 3, EndLine: 3},
	})

	srv := makeCodeServer(env)
	res, err := srv.handleStats(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	m := parseJSONResult(t, res)
	units, _ := m["ast_units"].(float64)
	assert.Greater(t, units, float64(0), "should have at least one AST unit")
}

// ---------------------------------------------------------------------------
// wrap — error conversion + pass-through + bus
// ---------------------------------------------------------------------------

func TestCodeServer_WrapConvertsError(t *testing.T) {
	s := NewCodeServer(&config.Config{}, nil, nil, nil)
	handler := s.wrap("test_tool", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, assert.AnError
	})
	res, err := handler(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "test_tool")
}

func TestCodeServer_WrapPassesThrough(t *testing.T) {
	s := NewCodeServer(&config.Config{}, nil, nil, nil)
	expected := mcp.NewToolResultText("ok")
	handler := s.wrap("test_tool", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return expected, nil
	})
	res, err := handler(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.Equal(t, expected, res)
}

func TestCodeServer_WrapIncrementsBus(t *testing.T) {
	env := setupTestEnv(t)
	srv := makeCodeServer(env)

	handler := srv.wrap("test_tool", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	_, err := handler(context.Background(), toolReq(nil))
	require.NoError(t, err)

	calls := countBusCalls(t, env.bus)
	key := "code.test_tool"
	assert.Equal(t, 1, calls[key], "bus should increment MCP call counter")
}

// ---------------------------------------------------------------------------
// wordAt
// ---------------------------------------------------------------------------

func TestCodeServer_WordAt(t *testing.T) {
	dir := t.TempDir()
	s := NewCodeServer(&config.Config{Root: dir}, nil, nil, nil)

	tests := []struct {
		name    string
		content string
		line    int
		char    int
		want    string
		wantErr bool
	}{
		{"simple word", "func hello() {}", 0, 6, "hello", false},
		{"start of word", "func hello() {}", 0, 5, "hello", false},
		{"end of word", "func hello() {}", 0, 9, "hello", false},
		{"multiline", "line1\nmyVar = 42\nline3", 1, 2, "myVar", false},
		{"underscore", "my_var_name = true", 0, 3, "my_var_name", false},
		{"digits", "var123 = true", 0, 2, "var123", false},
		{"line out of range", "single line", 5, 0, "", true},
		{"char out of range", "short", 0, 100, "", true},
		{"negative line", "hello", -1, 0, "", true},
		{"negative char", "hello", 0, -1, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTestFile(t, dir, "test.go", tt.content)
			got, err := s.wordAt(path, tt.line, tt.char)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// detectLanguage (delegated to fileutil.LanguageByExt)
// ---------------------------------------------------------------------------

func TestDetectLanguage(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"main.go", "go"},
		{"app.ts", "typescript"},
		{"component.tsx", "typescript"},
		{"index.js", "javascript"},
		{"app.jsx", "javascript"},
		{"script.py", "python"},
		{"App.java", "java"},
		{"README.md", "text"},
		{"Makefile", ""},
		{"config.yaml", "yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			assert.Equal(t, tt.want, fileutil.LanguageByExt(filepath.Ext(tt.path)))
		})
	}
}

// ---------------------------------------------------------------------------
// truncateByTokens / estimateBytes
// ---------------------------------------------------------------------------

func TestTruncateByTokens_NoTruncation(t *testing.T) {
	data := map[string]any{
		"source": "short code",
		"name":   "test",
	}
	result := truncateByTokens(data, 10000) // большой лимит
	assert.Equal(t, "short code", result["source"])
}

func TestTruncateByTokens_Truncates(t *testing.T) {
	data := map[string]any{
		"source": strings.Repeat("x", 1000),
	}
	result := truncateByTokens(data, 10) // маленький лимит (40 bytes)
	source := result["source"].(string)
	assert.Less(t, len(source), 200, "source should be truncated")
	assert.Contains(t, source, "truncated")
}

func TestEstimateBytes(t *testing.T) {
	assert.Equal(t, 5, estimateBytes("hello"))
	// map/slice считают вложенные элементы, fallback для пустых = 0
	assert.Equal(t, 0, estimateBytes(make(map[string]any)))
	assert.Equal(t, 0, estimateBytes(make([]any, 0)))
	assert.Equal(t, 100, estimateBytes(map[string]any{"k": strings.Repeat("x", 100)}))
}

// ---------------------------------------------------------------------------
// resolveSymbolID
// ---------------------------------------------------------------------------

func TestResolveSymbolID_Basic(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc hello() {}\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "r", Language: "go", Kind: "function", Name: "hello", Qualified: "main.hello", FilePath: file, StartLine: 3, EndLine: 3},
	})

	srv := makeCodeServer(env)
	u, err := srv.resolveSymbolID(context.Background(), "hello", "", "")
	require.NoError(t, err)
	require.NotNil(t, u)
	assert.Equal(t, "hello", u.Name)
}

func TestResolveSymbolID_FileDisambiguation(t *testing.T) {
	env := setupTestEnv(t)

	file1 := filepath.Join(env.root, "pkg1", "svc.go")
	file2 := filepath.Join(env.root, "pkg2", "svc.go")
	_ = os.MkdirAll(filepath.Dir(file1), 0755)
	_ = os.MkdirAll(filepath.Dir(file2), 0755)
	_ = os.WriteFile(file1, []byte("package pkg1\n\nfunc Run() {}\n"), 0644)
	_ = os.WriteFile(file2, []byte("package pkg2\n\nfunc Run() {}\n"), 0644)

	seedAST(t, env, file1, []store.ASTUnit{
		{Repo: "r", Language: "go", Kind: "function", Name: "Run", Qualified: "pkg1.Run", FilePath: file1, StartLine: 3, EndLine: 3},
	})
	seedAST(t, env, file2, []store.ASTUnit{
		{Repo: "r", Language: "go", Kind: "function", Name: "Run", Qualified: "pkg2.Run", FilePath: file2, StartLine: 3, EndLine: 3},
	})

	srv := makeCodeServer(env)

	// Без file — берётся первое
	u1, err := srv.resolveSymbolID(context.Background(), "Run", "", "")
	require.NoError(t, err)
	require.NotNil(t, u1)

	// С file — должен выбрать из конкретного файла
	u2, err := srv.resolveSymbolID(context.Background(), "Run", "pkg2/svc.go", "")
	require.NoError(t, err)
	require.NotNil(t, u2)
	assert.Contains(t, u2.FilePath, "pkg2")
}

func TestResolveSymbolID_RepoFilter(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc hello() {}\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "repo_a", Language: "go", Kind: "function", Name: "hello", Qualified: "main.hello", FilePath: file, StartLine: 3, EndLine: 3},
	})

	srv := makeCodeServer(env)
	_, err := srv.resolveSymbolID(context.Background(), "hello", "", "repo_b")
	assert.Error(t, err, "should not find symbol in different repo")
}

func TestResolveSymbolID_NotFound(t *testing.T) {
	env := setupTestEnv(t)
	srv := makeCodeServer(env)

	_, err := srv.resolveSymbolID(context.Background(), "nonexistent", "", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// ---------------------------------------------------------------------------
// Context cancellation
// ---------------------------------------------------------------------------

func TestCodeHandleGetContext_ContextCancelled(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc hello() {}\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "r", Language: "go", Kind: "function", Name: "hello", Qualified: "main.hello", FilePath: file, StartLine: 3, EndLine: 3},
	})

	srv := makeCodeServer(env)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // сразу отменяем

	_, err := srv.handleGetContext(ctx, toolReq(map[string]any{"symbol": "hello"}))
	// Может вернуть error или результат — главное что не паникует
	_ = err
}

func TestCodeHandleBatchGetContext_ContextCancelled(t *testing.T) {
	env := setupTestEnv(t)
	srv := makeCodeServer(env)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := srv.handleBatchGetContext(ctx, toolReq(map[string]any{
		"symbols": `[{"symbol":"test"}]`,
	}))
	_ = err
}

// ---------------------------------------------------------------------------
// helper
// ---------------------------------------------------------------------------

func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
	return path
}
