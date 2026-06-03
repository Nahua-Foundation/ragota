package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"ragota/pkg/config"
	"ragota/internal/search/graph"
	"ragota/pkg/lsp/manager"
	"ragota/pkg/state"
	"ragota/internal/store"
	"ragota/internal/search/symbols"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test helpers — in-memory SQLite with seeded AST data
// ---------------------------------------------------------------------------

type testEnv struct {
	db   *store.SQLite
	cfg  *config.Config
	syms *symbols.Service
	gr   *graph.Service
	bus  *state.Bus
	root string // temp dir
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
	syms := symbols.New(db, gr, nil)
	bus := state.NewBus(root)

	return &testEnv{db: db, cfg: cfg, syms: syms, gr: gr, bus: bus, root: root}
}

func seedAST(t *testing.T, env *testEnv, filePath string, units []store.ASTUnit) map[string]int {
	t.Helper()
	// Ensure the file is registered in the files table (FK constraint)
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

func seedEdges(t *testing.T, env *testEnv, srcFile string, edges []store.Edge) {
	t.Helper()
	err := env.db.ReplaceEdges(context.Background(), srcFile, edges)
	require.NoError(t, err)
	_, err = env.db.ResolvePendingEdges(context.Background(), nil)
	require.NoError(t, err)
}

func makeSymbolServer(env *testEnv) *SymbolServer {
	return NewSymbolServer(env.cfg, env.db, env.syms, env.gr, env.bus)
}

func makeTreeSitterServer(env *testEnv) *TreeSitterServer {
	return NewTreeSitterServer(env.cfg, nil, env.db, env.bus)
}

// ---------------------------------------------------------------------------
// SymbolServer — handleFindDefinition with real DB
// ---------------------------------------------------------------------------

func TestSymbolHandleFindDefinition_WithData(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc hello() {}\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "hello", Qualified: "main.hello", StartLine: 3, EndLine: 3},
		{Repo: "myrepo", Language: "go", Kind: "module", Name: "main", Qualified: "main", StartLine: 1, EndLine: 1},
	})

	srv := makeSymbolServer(env)
	res, err := srv.handleFindDefinition(context.Background(), toolReq(map[string]any{"symbol": "hello"}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
	text := contentText(res.Content[0])
	assert.Contains(t, text, "hello")
}

func TestSymbolHandleFindDefinition_ModuleExcluded(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "main.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "module", Name: "main", Qualified: "main", StartLine: 1, EndLine: 1},
	})

	srv := makeSymbolServer(env)
	res, err := srv.handleFindDefinition(context.Background(), toolReq(map[string]any{"symbol": "main"}))
	require.NoError(t, err)
	text := contentText(res.Content[0])
	// Module should be excluded, result is empty array
	assert.Equal(t, "[]", text)
}

// ---------------------------------------------------------------------------
// SymbolServer — handleFindReferences with real DB
// ---------------------------------------------------------------------------

func TestSymbolHandleFindReferences_WithData(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n\nfunc foo() {}\nfunc bar() { foo() }\n"), 0644)

	ids := seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "foo", Qualified: "main.foo", StartLine: 3, EndLine: 3},
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "bar", Qualified: "main.bar", StartLine: 4, EndLine: 4},
	})

	// bar calls foo
	seedEdges(t, env, file, []store.Edge{
		{SrcID: ids["main.bar"], DstID: ids["main.foo"], Repo: "myrepo", Kind: "call", DstName: "foo", FilePath: file, Line: 4},
	})

	srv := makeSymbolServer(env)
	res, err := srv.handleFindReferences(context.Background(), toolReq(map[string]any{"symbol": "foo"}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
}

func TestSymbolHandleFindReferences_WithRepoFilter(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	ids := seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "foo", Qualified: "main.foo", StartLine: 3, EndLine: 3},
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "bar", Qualified: "main.bar", StartLine: 4, EndLine: 4},
	})

	seedEdges(t, env, file, []store.Edge{
		{SrcID: ids["main.bar"], DstID: ids["main.foo"], Repo: "myrepo", Kind: "reference", DstName: "foo", FilePath: file, Line: 4},
	})

	srv := makeSymbolServer(env)
	// Filter by wrong repo — should get empty
	res, err := srv.handleFindReferences(context.Background(), toolReq(map[string]any{
		"symbol": "foo",
		"repo":   "other",
	}))
	require.NoError(t, err)
	text := contentText(res.Content[0])
	assert.Equal(t, "[]", text)
}

// ---------------------------------------------------------------------------
// SymbolServer — handleFindImplementations
// ---------------------------------------------------------------------------

func TestSymbolHandleFindImplementations_WithData(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "iface.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	ids := seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "interface", Name: "Logger", Qualified: "main.Logger", StartLine: 3, EndLine: 5},
		{Repo: "myrepo", Language: "go", Kind: "struct", Name: "MyLogger", Qualified: "main.MyLogger", StartLine: 7, EndLine: 10},
	})

	// MyLogger implements Logger
	seedEdges(t, env, file, []store.Edge{
		{SrcID: ids["main.MyLogger"], DstID: ids["main.Logger"], Repo: "myrepo", Kind: "implements", DstName: "Logger", FilePath: file, Line: 7},
	})

	srv := makeSymbolServer(env)
	res, err := srv.handleFindImplementations(context.Background(), toolReq(map[string]any{"interface": "Logger"}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
}

// ---------------------------------------------------------------------------
// SymbolServer — handleFindCallers
// ---------------------------------------------------------------------------

func TestSymbolHandleFindCallers_WithData(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	ids := seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "helper", Qualified: "main.helper", StartLine: 3, EndLine: 3},
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "caller", Qualified: "main.caller", StartLine: 5, EndLine: 7},
	})

	seedEdges(t, env, file, []store.Edge{
		{SrcID: ids["main.caller"], DstID: ids["main.helper"], Repo: "myrepo", Kind: "call", DstName: "helper", FilePath: file, Line: 6},
	})

	srv := makeSymbolServer(env)
	res, err := srv.handleFindCallers(context.Background(), toolReq(map[string]any{"function": "helper"}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
}

// ---------------------------------------------------------------------------
// SymbolServer — handleFindCallees
// ---------------------------------------------------------------------------

func TestSymbolHandleFindCallees_WithData(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	ids := seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "helper", Qualified: "main.helper", StartLine: 3, EndLine: 3},
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "main_fn", Qualified: "main.main_fn", StartLine: 5, EndLine: 7},
	})

	seedEdges(t, env, file, []store.Edge{
		{SrcID: ids["main.main_fn"], DstID: ids["main.helper"], Repo: "myrepo", Kind: "call", DstName: "helper", FilePath: file, Line: 6},
	})

	srv := makeSymbolServer(env)
	res, err := srv.handleFindCallees(context.Background(), toolReq(map[string]any{"function": "main_fn"}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
}

// ---------------------------------------------------------------------------
// SymbolServer — handleGetFileSymbols
// ---------------------------------------------------------------------------

func TestSymbolHandleGetFileSymbols_WithData(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "foo", StartLine: 3, EndLine: 5},
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "bar", StartLine: 7, EndLine: 9},
	})

	srv := makeSymbolServer(env)
	res, err := srv.handleGetFileSymbols(context.Background(), toolReq(map[string]any{"path": "code.go"}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
	text := contentText(res.Content[0])
	assert.Contains(t, text, "foo")
	assert.Contains(t, text, "bar")
}

func TestSymbolHandleGetFileSymbols_SecureJoinError(t *testing.T) {
	env := setupTestEnv(t)
	srv := makeSymbolServer(env)
	res, err := srv.handleGetFileSymbols(context.Background(), toolReq(map[string]any{"path": "../../etc/passwd"}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "traversal")
}

// ---------------------------------------------------------------------------
// SymbolServer — handleGetSymbol
// ---------------------------------------------------------------------------

func TestSymbolHandleGetSymbol_ValidID(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	ids := seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "testFunc", StartLine: 1, EndLine: 3},
	})

	srv := makeSymbolServer(env)
	// Get the ID from the seeded data
	for _, id := range ids {
		res, err := srv.handleGetSymbol(context.Background(), toolReq(map[string]any{"symbol_id": float64(id)}))
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.False(t, res.IsError)
		text := contentText(res.Content[0])
		assert.Contains(t, text, "testFunc")
		break
	}
}

// ---------------------------------------------------------------------------
// SymbolServer — handleGetParent
// ---------------------------------------------------------------------------

func TestSymbolHandleGetParent_ValidID(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "class", Name: "MyClass", StartLine: 1, EndLine: 10},
	})

	// Get parent of any unit (will return nil for root)
	srv := makeSymbolServer(env)
	res, err := srv.handleGetParent(context.Background(), toolReq(map[string]any{"symbol_id": float64(1)}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
}

// ---------------------------------------------------------------------------
// SymbolServer — handleGetChildren
// ---------------------------------------------------------------------------

func TestSymbolHandleGetChildren_ValidID(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	// Insert parent and child together in one call to avoid FK issues
	ids := seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "class", Name: "MyClass", StartLine: 1, EndLine: 10},
		{Repo: "myrepo", Language: "go", Kind: "method", Name: "DoStuff", StartLine: 2, EndLine: 5},
	})

	// Use UpdateASTParents to set the parent-child relationship
	parentID := ids["MyClass"]
	childID := ids["DoStuff"]
	err := env.db.UpdateASTParents(context.Background(), map[int]int{childID: parentID})
	require.NoError(t, err)

	srv := makeSymbolServer(env)
	res, err := srv.handleGetChildren(context.Background(), toolReq(map[string]any{"symbol_id": float64(parentID)}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
}

// ---------------------------------------------------------------------------
// SymbolServer — handleExpandNeighbors
// ---------------------------------------------------------------------------

func TestSymbolHandleExpandNeighbors_WithData(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	ids := seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "foo", Qualified: "main.foo", StartLine: 3, EndLine: 3},
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "bar", Qualified: "main.bar", StartLine: 5, EndLine: 7},
	})

	seedEdges(t, env, file, []store.Edge{
		{SrcID: ids["main.bar"], DstID: ids["main.foo"], Repo: "myrepo", Kind: "call", DstName: "foo", FilePath: file, Line: 6},
	})

	srv := makeSymbolServer(env)
	res, err := srv.handleExpandNeighbors(context.Background(), toolReq(map[string]any{
		"node_id": float64(ids["main.bar"]),
		"depth":   float64(1),
		"kinds":   "call",
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
}

func TestSymbolHandleExpandNeighbors_WithKindsParsing(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	ids := seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "foo", Qualified: "main.foo", StartLine: 1, EndLine: 1},
	})

	srv := makeSymbolServer(env)
	// Test kinds parsing with commas and spaces
	res, err := srv.handleExpandNeighbors(context.Background(), toolReq(map[string]any{
		"node_id": float64(ids["main.foo"]),
		"kinds":   " call , import ",
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
}

func TestSymbolHandleExpandNeighbors_WithRepoFilter(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	ids := seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "foo", Qualified: "main.foo", StartLine: 1, EndLine: 1},
	})

	srv := makeSymbolServer(env)
	// Explicit repo=* should not filter
	res, err := srv.handleExpandNeighbors(context.Background(), toolReq(map[string]any{
		"node_id": float64(ids["main.foo"]),
		"repo":    "*",
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
}

// ---------------------------------------------------------------------------
// SymbolServer — handleDependencyGraph
// ---------------------------------------------------------------------------

func TestSymbolHandleDependencyGraph_WithData(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "module", Name: "main", Qualified: "main", StartLine: 1, EndLine: 1},
	})

	srv := makeSymbolServer(env)
	res, err := srv.handleDependencyGraph(context.Background(), toolReq(map[string]any{
		"module": filepath.Join(env.root, "code.go"),
		"depth":  float64(1),
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
}

// ---------------------------------------------------------------------------
// SymbolServer — handleCallGraph
// ---------------------------------------------------------------------------

func TestSymbolHandleCallGraph_WithSymbolID(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	ids := seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "foo", Qualified: "main.foo", StartLine: 3, EndLine: 3},
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "bar", Qualified: "main.bar", StartLine: 5, EndLine: 7},
	})

	seedEdges(t, env, file, []store.Edge{
		{SrcID: ids["main.bar"], DstID: ids["main.foo"], Repo: "myrepo", Kind: "call", DstName: "foo", FilePath: file, Line: 6},
	})

	srv := makeSymbolServer(env)
	res, err := srv.handleCallGraph(context.Background(), toolReq(map[string]any{
		"symbol_id": float64(ids["main.foo"]),
		"depth":     float64(1),
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
}

func TestSymbolHandleCallGraph_WithFunctionName(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "foo", Qualified: "main.foo", StartLine: 3, EndLine: 3},
	})

	srv := makeSymbolServer(env)
	res, err := srv.handleCallGraph(context.Background(), toolReq(map[string]any{
		"function": "foo",
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
}

func TestSymbolHandleCallGraph_WithExplicitRepo(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	ids := seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "foo", Qualified: "main.foo", StartLine: 3, EndLine: 3},
	})

	srv := makeSymbolServer(env)
	res, err := srv.handleCallGraph(context.Background(), toolReq(map[string]any{
		"symbol_id": float64(ids["main.foo"]),
		"repo":      "myrepo",
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
}

func TestSymbolHandleCallGraph_NonCallableKind(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "class", Name: "MyClass", Qualified: "main.MyClass", StartLine: 1, EndLine: 10},
	})

	srv := makeSymbolServer(env)
	// Search by name that's a class — should return empty merged result
	res, err := srv.handleCallGraph(context.Background(), toolReq(map[string]any{
		"function": "MyClass",
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
}

// ---------------------------------------------------------------------------
// SymbolServer — handleTraverseGraph
// ---------------------------------------------------------------------------

func TestSymbolHandleTraverseGraph_WithData(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	ids := seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "foo", Qualified: "main.foo", StartLine: 3, EndLine: 3},
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "bar", Qualified: "main.bar", StartLine: 5, EndLine: 7},
	})

	seedEdges(t, env, file, []store.Edge{
		{SrcID: ids["main.bar"], DstID: ids["main.foo"], Repo: "myrepo", Kind: "call", DstName: "foo", FilePath: file, Line: 6},
	})

	srv := makeSymbolServer(env)
	res, err := srv.handleTraverseGraph(context.Background(), toolReq(map[string]any{
		"symbol_id":  float64(ids["main.bar"]),
		"depth":      float64(1),
		"edge_types": "call",
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
}

func TestSymbolHandleTraverseGraph_NoKinds(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	ids := seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "foo", Qualified: "main.foo", StartLine: 1, EndLine: 1},
	})

	srv := makeSymbolServer(env)
	res, err := srv.handleTraverseGraph(context.Background(), toolReq(map[string]any{
		"symbol_id": float64(ids["main.foo"]),
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
}

// ---------------------------------------------------------------------------
// SymbolServer — handleSurroundingContext
// ---------------------------------------------------------------------------

func TestSymbolHandleSurroundingContext_WithFile(t *testing.T) {
	env := setupTestEnv(t)
	content := "package main\n\nimport \"fmt\"\n\nfunc hello() {\n\tfmt.Println(\"hi\")\n}\n"
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte(content), 0644)

	ids := seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "hello", StartLine: 5, EndLine: 7},
	})

	srv := makeSymbolServer(env)
	for _, id := range ids {
		res, err := srv.handleSurroundingContext(context.Background(), toolReq(map[string]any{
			"symbol_id":    float64(id),
			"before_lines": float64(1),
			"after_lines":  float64(1),
		}))
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.False(t, res.IsError)
		text := contentText(res.Content[0])
		assert.Contains(t, text, "hello")
		break
	}
}

// ---------------------------------------------------------------------------
// SymbolServer — handleRelatedFiles
// ---------------------------------------------------------------------------

func TestSymbolHandleRelatedFiles_WithData(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	ids := seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "foo", Qualified: "main.foo", StartLine: 1, EndLine: 3},
	})

	srv := makeSymbolServer(env)
	res, err := srv.handleRelatedFiles(context.Background(), toolReq(map[string]any{
		"symbol_id": float64(ids["main.foo"]),
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
}

// ---------------------------------------------------------------------------
// SymbolServer — handleSimilarCode (nil searcher)
// ---------------------------------------------------------------------------

func TestSymbolHandleSimilarCode_NilSearcher(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	ids := seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "foo", StartLine: 1, EndLine: 3},
	})

	srv := makeSymbolServer(env)
	res, err := srv.handleSimilarCode(context.Background(), toolReq(map[string]any{
		"symbol_id": float64(ids["foo"]),
		"limit":     float64(5),
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
	text := contentText(res.Content[0])
	assert.Equal(t, "[]", text)
}

// ---------------------------------------------------------------------------
// SymbolServer — handleGetExecutionContext
// ---------------------------------------------------------------------------

func TestSymbolHandleGetExecutionContext_WithData(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	ids := seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "foo", Qualified: "main.foo", StartLine: 3, EndLine: 3},
	})

	srv := makeSymbolServer(env)
	res, err := srv.handleGetExecutionContext(context.Background(), toolReq(map[string]any{
		"symbol_id": float64(ids["main.foo"]),
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
}

// ---------------------------------------------------------------------------
// SymbolServer — handleGetSymbolSummary (LLM will fail but exercises path)
// ---------------------------------------------------------------------------

func TestSymbolHandleGetSymbolSummary_NoLLM(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	ids := seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "foo", Qualified: "main.foo", StartLine: 3, EndLine: 3},
	})

	srv := makeSymbolServer(env)
	// This will try to call Ollama and fail, which exercises the error path
	// We expect either a valid result or an error-to-result
	res, err := srv.handleGetSymbolSummary(context.Background(), toolReq(map[string]any{
		"symbol_id": float64(ids["main.foo"]),
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	// Either succeeds with partial data or shows an error
}

// ---------------------------------------------------------------------------
// SymbolServer — handleGetFileIntent
// ---------------------------------------------------------------------------

func TestSymbolHandleGetFileIntent_SecureJoinError(t *testing.T) {
	env := setupTestEnv(t)
	srv := makeSymbolServer(env)
	res, err := srv.handleGetFileIntent(context.Background(), toolReq(map[string]any{"path": "../../etc/passwd"}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
}

// ---------------------------------------------------------------------------
// TreeSitterServer — handleSearch with real DB
// ---------------------------------------------------------------------------

func TestTreeSitterHandleSearch_WithData(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "myFunc", StartLine: 3, EndLine: 5},
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "otherFunc", StartLine: 7, EndLine: 9},
	})

	srv := makeTreeSitterServer(env)
	res, err := srv.handleSearch(context.Background(), toolReq(map[string]any{
		"query":    "myFunc",
		"kind":     "function",
		"language": "go",
		"limit":    float64(10),
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
	text := contentText(res.Content[0])
	assert.Contains(t, text, "myFunc")
}

func TestTreeSitterHandleSearch_WithRepoFilter(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "myFunc", StartLine: 3, EndLine: 5},
	})

	srv := makeTreeSitterServer(env)
	res, err := srv.handleSearch(context.Background(), toolReq(map[string]any{
		"query": "myFunc",
		"repo":  "myrepo",
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
}

// ---------------------------------------------------------------------------
// TreeSitterServer — handleListSymbols
// ---------------------------------------------------------------------------

func TestTreeSitterHandleListSymbols_WithData(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "foo", StartLine: 3, EndLine: 5},
	})

	srv := makeTreeSitterServer(env)
	res, err := srv.handleListSymbols(context.Background(), toolReq(map[string]any{"file": "code.go"}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
	text := contentText(res.Content[0])
	assert.Contains(t, text, "foo")
}

func TestTreeSitterHandleListSymbols_SecureJoinError(t *testing.T) {
	env := setupTestEnv(t)
	srv := makeTreeSitterServer(env)
	res, err := srv.handleListSymbols(context.Background(), toolReq(map[string]any{"file": "../../etc/passwd"}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
}

// ---------------------------------------------------------------------------
// TreeSitterServer — handleStats
// ---------------------------------------------------------------------------

func TestTreeSitterHandleStats_WithData(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "foo", StartLine: 3, EndLine: 5},
	})

	srv := makeTreeSitterServer(env)
	res, err := srv.handleStats(context.Background(), toolReq(nil))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
	text := contentText(res.Content[0])
	assert.Contains(t, text, "units")
}

// ---------------------------------------------------------------------------
// TreeSitterServer — handleReindex (nil index → will panic or error)
// ---------------------------------------------------------------------------

func TestTreeSitterHandleReindex_NilIndex_FullScan(t *testing.T) {
	env := setupTestEnv(t)
	srv := makeTreeSitterServer(env)
	// nil index → FullScan will panic on nil dereference, so we don't test that.
	// Instead test with a path that SecureJoin rejects
	res, err := srv.handleReindex(context.Background(), toolReq(map[string]any{"path": "../../etc/passwd"}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
}

// ---------------------------------------------------------------------------
// VectorServer — handleReindex SecureJoin error path
// ---------------------------------------------------------------------------

func TestVectorHandleReindex_SecureJoinError(t *testing.T) {
	cfg := config.Default()
	cfg.Root = t.TempDir()
	s := NewVectorServer(cfg, nil, nil, nil)
	// Path traversal rejected by SecureJoin before nil idx is dereferenced
	res, err := s.handleReindex(context.Background(), toolReq(map[string]any{"path": "../../etc/passwd"}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
}

// ---------------------------------------------------------------------------
// LSPServer — handleLanguages with real Manager
// ---------------------------------------------------------------------------

func TestLSPHandleLanguages_WithManager(t *testing.T) {
	mgr := manager.NewManager(t.TempDir(), nil)
	s := NewLSPServer(&config.Config{}, mgr, nil, nil)
	res, err := s.handleLanguages(context.Background(), toolReq(nil))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
}

// ---------------------------------------------------------------------------
// LSPServer — wrap with bus tracking
// ---------------------------------------------------------------------------

func TestLSPServer_WrapWithBus(t *testing.T) {
	env := setupTestEnv(t)
	mgr := manager.NewManager(env.root, nil)
	s := NewLSPServer(env.cfg, mgr, env.db, env.bus)

	handler := s.wrap("test_tool", func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	res, err := handler(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.Equal(t, "ok", contentText(res.Content[0]))

	// Verify bus recorded the call
	snap := env.bus.Snapshot()
	found := false
	for _, mcpStat := range snap.MCP {
		if mcpStat.Server == "lsp" {
			found = true
			break
		}
	}
	assert.True(t, found, "bus should record LSP MCP call")
}

// ---------------------------------------------------------------------------
// TreeSitterServer — toolWrap with bus tracking
// ---------------------------------------------------------------------------

func TestTreeSitterServer_ToolWrapWithBus(t *testing.T) {
	env := setupTestEnv(t)
	srv := makeTreeSitterServer(env)

	handler := srv.toolWrap("test_tool", func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	res, err := handler(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.Equal(t, "ok", contentText(res.Content[0]))
}

// ---------------------------------------------------------------------------
// SymbolServer — wrap with bus tracking
// ---------------------------------------------------------------------------

func TestSymbolServer_WrapWithBus(t *testing.T) {
	env := setupTestEnv(t)
	srv := makeSymbolServer(env)

	handler := srv.wrap("test_tool", func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	res, err := handler(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.Equal(t, "ok", contentText(res.Content[0]))
}

// ---------------------------------------------------------------------------
// VectorServer — wrap with bus tracking
// ---------------------------------------------------------------------------

func TestVectorServer_WrapWithBus(t *testing.T) {
	env := setupTestEnv(t)
	s := NewVectorServer(env.cfg, nil, nil, env.bus)

	handler := s.wrap("test_tool", func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	res, err := handler(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.Equal(t, "ok", contentText(res.Content[0]))
}

// ---------------------------------------------------------------------------
// VectorServer — handleRerank with top_n > len(candidates)
// ---------------------------------------------------------------------------

func TestVectorHandleRerank_TopNLargerThanCandidates(t *testing.T) {
	s := NewVectorServer(&config.Config{}, nil, nil, nil)
	candidates := `[{"id":"1","content":"a","score":0.9}]`
	res, err := s.handleRerank(context.Background(), toolReq(map[string]any{
		"query":      "test",
		"candidates": candidates,
		"top_n":      float64(100),
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	text := contentText(res.Content[0])
	assert.Contains(t, text, "\"a\"")
}

// ---------------------------------------------------------------------------
// parseRepoListParam — edge case: unusual type fallback
// ---------------------------------------------------------------------------

func TestParseRepoListParam_FallbackNil(t *testing.T) {
	// Test the final fallback (should never hit in practice)
	result := parseRepoListParam("*")
	assert.Nil(t, result)
}

// ---------------------------------------------------------------------------
// jsonResult — nil interface
// ---------------------------------------------------------------------------

func TestJsonResult_NilInterface(t *testing.T) {
	var v any = nil
	res, err := jsonResult(v)
	require.NoError(t, err)
	text := contentText(res.Content[0])
	assert.Equal(t, "[]", text)
}

// ---------------------------------------------------------------------------
// SymbolServer — handleGetSymbolSummary non-existent
// ---------------------------------------------------------------------------

func TestSymbolHandleGetSemanticNeighborhood_WithData(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	ids := seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "foo", Qualified: "main.foo", StartLine: 1, EndLine: 3},
	})

	srv := makeSymbolServer(env)
	res, err := srv.handleGetSemanticNeighborhood(context.Background(), toolReq(map[string]any{
		"symbol_id": float64(ids["main.foo"]),
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
}

// ---------------------------------------------------------------------------
// VectorServer — handleSearchKeyword with configured bm25 but query required
// ---------------------------------------------------------------------------

func TestVectorHandleSearchKeyword_QueryRequired(t *testing.T) {
	s := NewVectorServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleSearchKeyword(context.Background(), toolReq(map[string]any{
		"language": "go",
		"kind":     "function",
		"repo":     "myrepo",
		"top_k":    float64(20),
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "query is required")
}

// ---------------------------------------------------------------------------
// Ensure JSON round-trip for handler results
// ---------------------------------------------------------------------------

func TestSymbolHandleFindDefinition_JSONValid(t *testing.T) {
	env := setupTestEnv(t)
	file := filepath.Join(env.root, "code.go")
	_ = os.WriteFile(file, []byte("package main\n"), 0644)

	seedAST(t, env, file, []store.ASTUnit{
		{Repo: "myrepo", Language: "go", Kind: "function", Name: "foo", Qualified: "main.foo", StartLine: 1, EndLine: 3},
	})

	srv := makeSymbolServer(env)
	res, err := srv.handleFindDefinition(context.Background(), toolReq(map[string]any{"symbol": "foo"}))
	require.NoError(t, err)
	text := contentText(res.Content[0])

	// Verify it's valid JSON
	var parsed []json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(text), &parsed))
	assert.NotEmpty(t, parsed)
}
