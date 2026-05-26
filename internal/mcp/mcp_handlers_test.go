package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"ragota/internal/config"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helper to construct a CallToolRequest with given arguments.
func toolReq(args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Arguments: args,
		},
	}
}

// ---------------------------------------------------------------------------
// Server constructors
// ---------------------------------------------------------------------------

func TestNewVectorServer(t *testing.T) {
	cfg := &config.Config{}
	s := NewVectorServer(cfg, nil, nil, nil)
	require.NotNil(t, s)
	assert.Equal(t, cfg, s.cfg)
}

func TestNewTreeSitterServer(t *testing.T) {
	cfg := &config.Config{}
	s := NewTreeSitterServer(cfg, nil, nil, nil)
	require.NotNil(t, s)
	assert.Equal(t, cfg, s.cfg)
}

func TestNewSymbolServer(t *testing.T) {
	cfg := &config.Config{}
	s := NewSymbolServer(cfg, nil, nil, nil, nil)
	require.NotNil(t, s)
	assert.Equal(t, cfg, s.cfg)
}

func TestNewLSPServer(t *testing.T) {
	cfg := &config.Config{}
	s := NewLSPServer(cfg, nil, nil, nil)
	require.NotNil(t, s)
	assert.Equal(t, cfg, s.cfg)
}

// ---------------------------------------------------------------------------
// VectorServer.SetBM25 / SetReranker
// ---------------------------------------------------------------------------

func TestVectorServer_SetBM25(t *testing.T) {
	s := NewVectorServer(&config.Config{}, nil, nil, nil)
	assert.Nil(t, s.bm25)
	// We can't set a real bm25.Index without a Bleve backend, but
	// we can verify the setter doesn't panic on nil.
	s.SetBM25(nil)
}

func TestVectorServer_SetReranker(t *testing.T) {
	s := NewVectorServer(&config.Config{}, nil, nil, nil)
	assert.Nil(t, s.rer)
	s.SetReranker(nil)
}

// ---------------------------------------------------------------------------
// Build() smoke tests — verify non-nil servers are returned.
// ---------------------------------------------------------------------------

func TestVectorServer_Build(t *testing.T) {
	s := NewVectorServer(&config.Config{}, nil, nil, nil)
	srv := s.Build()
	require.NotNil(t, srv)
}

func TestTreeSitterServer_Build(t *testing.T) {
	s := NewTreeSitterServer(&config.Config{}, nil, nil, nil)
	srv := s.Build()
	require.NotNil(t, srv)
}

func TestSymbolServer_Build(t *testing.T) {
	s := NewSymbolServer(&config.Config{}, nil, nil, nil, nil)
	srv := s.Build()
	require.NotNil(t, srv)
}

func TestLSPServer_Build(t *testing.T) {
	s := NewLSPServer(&config.Config{}, nil, nil, nil)
	srv := s.Build()
	require.NotNil(t, srv)
}

// ---------------------------------------------------------------------------
// VectorServer handler validation paths (no external services needed)
// ---------------------------------------------------------------------------

func TestVectorHandleSearch_EmptyQuery(t *testing.T) {
	s := NewVectorServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleSearch(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	text := contentText(res.Content[0])
	assert.Contains(t, text, "query is required")
}

func TestVectorHandleSearchKeyword_EmptyQuery(t *testing.T) {
	s := NewVectorServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleSearchKeyword(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	text := contentText(res.Content[0])
	assert.Contains(t, text, "query is required")
}

func TestVectorHandleSearchKeyword_NoBM25(t *testing.T) {
	s := NewVectorServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleSearchKeyword(context.Background(), toolReq(map[string]any{"query": "test"}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	text := contentText(res.Content[0])
	assert.Contains(t, text, "bm25 index is not configured")
}

func TestVectorHandleSearchHybrid_EmptyQuery(t *testing.T) {
	s := NewVectorServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleSearchHybrid(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	text := contentText(res.Content[0])
	assert.Contains(t, text, "query is required")
}

func TestVectorHandleRerank_EmptyQuery(t *testing.T) {
	s := NewVectorServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleRerank(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	text := contentText(res.Content[0])
	assert.Contains(t, text, "query is required")
}

func TestVectorHandleRerank_EmptyCandidates(t *testing.T) {
	s := NewVectorServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleRerank(context.Background(), toolReq(map[string]any{"query": "test"}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	text := contentText(res.Content[0])
	assert.Contains(t, text, "candidates JSON is required")
}

func TestVectorHandleRerank_InvalidCandidatesJSON(t *testing.T) {
	s := NewVectorServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleRerank(context.Background(), toolReq(map[string]any{
		"query":      "test",
		"candidates": "{not valid json",
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	text := contentText(res.Content[0])
	assert.Contains(t, text, "invalid candidates JSON")
}

func TestVectorHandleRerank_NoRerankerIdentityFallback(t *testing.T) {
	cfg := &config.Config{}
	s := NewVectorServer(cfg, nil, nil, nil)
	// No reranker set — should do identity pass-through
	candidates := `[{"id":"1","content":"hello","score":0.9},{"id":"2","content":"world","score":0.5}]`
	res, err := s.handleRerank(context.Background(), toolReq(map[string]any{
		"query":      "test",
		"candidates": candidates,
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
	text := contentText(res.Content[0])
	assert.Contains(t, text, "hello")
	assert.Contains(t, text, "world")
}

func TestVectorHandleRerank_TopNLimitsOutput(t *testing.T) {
	cfg := &config.Config{}
	s := NewVectorServer(cfg, nil, nil, nil)
	candidates := `[{"id":"1","content":"a","score":0.9},{"id":"2","content":"b","score":0.5},{"id":"3","content":"c","score":0.1}]`
	res, err := s.handleRerank(context.Background(), toolReq(map[string]any{
		"query":      "test",
		"candidates": candidates,
		"top_n":      1,
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	text := contentText(res.Content[0])
	// Should only contain first candidate
	assert.Contains(t, text, "\"a\"")
	assert.NotContains(t, text, "\"b\"")
}

func TestVectorHandleRerank_EmptyCandidatesArray(t *testing.T) {
	cfg := &config.Config{}
	s := NewVectorServer(cfg, nil, nil, nil)
	res, err := s.handleRerank(context.Background(), toolReq(map[string]any{
		"query":      "test",
		"candidates": "[]",
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	text := contentText(res.Content[0])
	assert.Equal(t, "[]", text)
}

// ---------------------------------------------------------------------------
// TreeSitterServer handler validation paths
// ---------------------------------------------------------------------------

func TestTreeSitterHandleSearch_EmptyQuery(t *testing.T) {
	s := NewTreeSitterServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleSearch(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	text := contentText(res.Content[0])
	assert.Contains(t, text, "query is required")
}

func TestTreeSitterHandleListSymbols_EmptyFile(t *testing.T) {
	s := NewTreeSitterServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleListSymbols(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	text := contentText(res.Content[0])
	assert.Contains(t, text, "file is required")
}

// ---------------------------------------------------------------------------
// SymbolServer handler validation paths
// ---------------------------------------------------------------------------

func TestSymbolHandleFindDefinition_EmptySymbol(t *testing.T) {
	s := NewSymbolServer(&config.Config{}, nil, nil, nil, nil)
	res, err := s.handleFindDefinition(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "symbol is required")
}

func TestSymbolHandleFindReferences_EmptySymbol(t *testing.T) {
	s := NewSymbolServer(&config.Config{}, nil, nil, nil, nil)
	res, err := s.handleFindReferences(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "symbol is required")
}

func TestSymbolHandleFindImplementations_EmptyInterface(t *testing.T) {
	s := NewSymbolServer(&config.Config{}, nil, nil, nil, nil)
	res, err := s.handleFindImplementations(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "interface is required")
}

func TestSymbolHandleFindCallers_EmptyFunction(t *testing.T) {
	s := NewSymbolServer(&config.Config{}, nil, nil, nil, nil)
	res, err := s.handleFindCallers(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "function is required")
}

func TestSymbolHandleFindCallees_EmptyFunction(t *testing.T) {
	s := NewSymbolServer(&config.Config{}, nil, nil, nil, nil)
	res, err := s.handleFindCallees(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "function is required")
}

func TestSymbolHandleGetFileSymbols_EmptyPath(t *testing.T) {
	s := NewSymbolServer(&config.Config{}, nil, nil, nil, nil)
	res, err := s.handleGetFileSymbols(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "path is required")
}

func TestSymbolHandleGetSymbol_ZeroID(t *testing.T) {
	s := NewSymbolServer(&config.Config{}, nil, nil, nil, nil)
	res, err := s.handleGetSymbol(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "symbol_id is required")
}

func TestSymbolHandleGetSymbol_NegativeID(t *testing.T) {
	s := NewSymbolServer(&config.Config{}, nil, nil, nil, nil)
	res, err := s.handleGetSymbol(context.Background(), toolReq(map[string]any{"symbol_id": -1}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "symbol_id is required")
}

func TestSymbolHandleGetParent_ZeroID(t *testing.T) {
	s := NewSymbolServer(&config.Config{}, nil, nil, nil, nil)
	res, err := s.handleGetParent(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "symbol_id is required")
}

func TestSymbolHandleGetChildren_ZeroID(t *testing.T) {
	s := NewSymbolServer(&config.Config{}, nil, nil, nil, nil)
	res, err := s.handleGetChildren(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "symbol_id is required")
}

func TestSymbolHandleExpandNeighbors_ZeroID(t *testing.T) {
	s := NewSymbolServer(&config.Config{}, nil, nil, nil, nil)
	res, err := s.handleExpandNeighbors(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "node_id is required")
}

func TestSymbolHandleDependencyGraph_EmptyModule(t *testing.T) {
	s := NewSymbolServer(&config.Config{}, nil, nil, nil, nil)
	res, err := s.handleDependencyGraph(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "module is required")
}

func TestSymbolHandleCallGraph_NoFunctionOrID(t *testing.T) {
	s := NewSymbolServer(&config.Config{}, nil, nil, nil, nil)
	res, err := s.handleCallGraph(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "either `function` (name) or `symbol_id` is required")
}

func TestSymbolHandleTraverseGraph_ZeroID(t *testing.T) {
	s := NewSymbolServer(&config.Config{}, nil, nil, nil, nil)
	res, err := s.handleTraverseGraph(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "symbol_id is required")
}

func TestSymbolHandleSurroundingContext_ZeroID(t *testing.T) {
	s := NewSymbolServer(&config.Config{}, nil, nil, nil, nil)
	res, err := s.handleSurroundingContext(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "symbol_id is required")
}

func TestSymbolHandleRelatedFiles_ZeroID(t *testing.T) {
	s := NewSymbolServer(&config.Config{}, nil, nil, nil, nil)
	res, err := s.handleRelatedFiles(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "symbol_id is required")
}

func TestSymbolHandleSimilarCode_ZeroID(t *testing.T) {
	s := NewSymbolServer(&config.Config{}, nil, nil, nil, nil)
	res, err := s.handleSimilarCode(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "symbol_id is required")
}

func TestSymbolHandleGetExecutionContext_ZeroID(t *testing.T) {
	s := NewSymbolServer(&config.Config{}, nil, nil, nil, nil)
	res, err := s.handleGetExecutionContext(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "symbol_id is required")
}

func TestSymbolHandleGetSymbolSummary_ZeroID(t *testing.T) {
	s := NewSymbolServer(&config.Config{}, nil, nil, nil, nil)
	res, err := s.handleGetSymbolSummary(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "symbol_id is required")
}

func TestSymbolHandleGetFileIntent_EmptyPath(t *testing.T) {
	s := NewSymbolServer(&config.Config{}, nil, nil, nil, nil)
	res, err := s.handleGetFileIntent(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "path is required")
}

func TestSymbolHandleGetSemanticNeighborhood_ZeroID(t *testing.T) {
	s := NewSymbolServer(&config.Config{}, nil, nil, nil, nil)
	res, err := s.handleGetSemanticNeighborhood(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "symbol_id is required")
}

// ---------------------------------------------------------------------------
// LSPServer handler validation paths
// ---------------------------------------------------------------------------

func TestLSPHandleDefinition_EmptyFile(t *testing.T) {
	s := NewLSPServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleDefinition(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "file is required")
}

func TestLSPHandleReferences_EmptyFile(t *testing.T) {
	s := NewLSPServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleReferences(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "file is required")
}

func TestLSPHandleHover_EmptyFile(t *testing.T) {
	s := NewLSPServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleHover(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "file is required")
}

func TestLSPHandleImplementation_EmptyFile(t *testing.T) {
	s := NewLSPServer(&config.Config{}, nil, nil, nil)
	res, err := s.handleImplementation(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "file is required")
}

// ---------------------------------------------------------------------------
// LSPServer.wordAt
// ---------------------------------------------------------------------------

func TestLSPServer_WordAt(t *testing.T) {
	// Create a temp file with known content
	dir := t.TempDir()
	s := NewLSPServer(&config.Config{Root: dir}, nil, nil, nil)

	tests := []struct {
		name    string
		content string
		line    int
		char    int
		want    string
		wantErr bool
	}{
		{
			name:    "simple word",
			content: "func hello() {}",
			line:    0,
			char:    6,
			want:    "hello",
		},
		{
			name:    "start of word",
			content: "func hello() {}",
			line:    0,
			char:    5,
			want:    "hello",
		},
		{
			name:    "end of word",
			content: "func hello() {}",
			line:    0,
			char:    9,
			want:    "hello",
		},
		{
			name:    "multiline",
			content: "line1\nmyVar = 42\nline3",
			line:    1,
			char:    2,
			want:    "myVar",
		},
		{
			name:    "underscore in word",
			content: "my_var_name = true",
			line:    0,
			char:    3,
			want:    "my_var_name",
		},
		{
			name:    "digits in word",
			content: "var123 = true",
			line:    0,
			char:    2,
			want:    "var123",
		},
		{
			name:    "line out of range",
			content: "single line",
			line:    5,
			char:    0,
			wantErr: true,
		},
		{
			name:    "char out of range",
			content: "short",
			line:    0,
			char:    100,
			wantErr: true,
		},
		{
			name:    "negative line",
			content: "hello",
			line:    -1,
			char:    0,
			wantErr: true,
		},
		{
			name:    "negative char",
			content: "hello",
			line:    0,
			char:    -1,
			wantErr: true,
		},
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

func TestLSPServer_WordAt_FileNotFound(t *testing.T) {
	s := NewLSPServer(&config.Config{}, nil, nil, nil)
	_, err := s.wordAt("/nonexistent/file.go", 0, 0)
	assert.Error(t, err)
}

func TestLSPServer_WordAt_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	s := NewLSPServer(&config.Config{Root: dir}, nil, nil, nil)
	path := writeTestFile(t, dir, "empty.go", "")
	_, err := s.wordAt(path, 0, 0)
	assert.Error(t, err)
}

func TestLSPServer_WordAt_SpaceChar(t *testing.T) {
	dir := t.TempDir()
	s := NewLSPServer(&config.Config{Root: dir}, nil, nil, nil)
	path := writeTestFile(t, dir, "space.go", "a b c")
	// At space position (char=1), wordAt scans left and finds "a"
	got, err := s.wordAt(path, 0, 1)
	require.NoError(t, err)
	assert.Equal(t, "a", got)
}

// ---------------------------------------------------------------------------
// LSPServer.resolveAbs
// ---------------------------------------------------------------------------

func TestLSPServer_ResolveAbs(t *testing.T) {
	dir := t.TempDir()
	s := NewLSPServer(&config.Config{Root: dir}, nil, nil, nil)
	abs, err := s.resolveAbs("subdir/file.go")
	require.NoError(t, err)
	assert.Contains(t, abs, "subdir")
	assert.Contains(t, abs, "file.go")
}

func TestLSPServer_ResolveAbs_PathTraversal(t *testing.T) {
	dir := t.TempDir()
	s := NewLSPServer(&config.Config{Root: dir}, nil, nil, nil)
	_, err := s.resolveAbs("../../etc/passwd")
	assert.Error(t, err, "path traversal should be rejected")
}

// ---------------------------------------------------------------------------
// wrap error-to-result conversion
// ---------------------------------------------------------------------------

func TestVectorServer_WrapConvertsError(t *testing.T) {
	s := NewVectorServer(&config.Config{}, nil, nil, nil)
	handler := s.wrap("test_tool", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, assert.AnError
	})
	res, err := handler(context.Background(), toolReq(nil))
	require.NoError(t, err) // wrap converts errors to results
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "test_tool")
}

func TestTreeSitterServer_WrapConvertsError(t *testing.T) {
	s := NewTreeSitterServer(&config.Config{}, nil, nil, nil)
	handler := s.toolWrap("test_tool", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, assert.AnError
	})
	res, err := handler(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "test_tool")
}

func TestSymbolServer_WrapConvertsError(t *testing.T) {
	s := NewSymbolServer(&config.Config{}, nil, nil, nil, nil)
	handler := s.wrap("test_tool", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, assert.AnError
	})
	res, err := handler(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "test_tool")
}

func TestLSPServer_WrapConvertsError(t *testing.T) {
	s := NewLSPServer(&config.Config{}, nil, nil, nil)
	handler := s.wrap("test_tool", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, assert.AnError
	})
	res, err := handler(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res.Content[0]), "test_tool")
}

// ---------------------------------------------------------------------------
// wrap passes through successful results
// ---------------------------------------------------------------------------

func TestVectorServer_WrapPassesThrough(t *testing.T) {
	s := NewVectorServer(&config.Config{}, nil, nil, nil)
	expected := mcp.NewToolResultText("ok")
	handler := s.wrap("test_tool", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return expected, nil
	})
	res, err := handler(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.Equal(t, expected, res)
}

// helper to write a file
func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
	return path
}
