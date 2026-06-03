package graph

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"ragota/pkg/config"
	"ragota/internal/store"
)

// mockOllamaServer creates a test server that responds with the given JSON.
func mockOllamaServer(t *testing.T, response string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"response": response})
	}))
}

// mockOllamaServerError creates a test server that returns an error.
func mockOllamaServerError(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
}

// mockOllamaServerInvalidJSON creates a test server that returns invalid JSON.
func mockOllamaServerInvalidJSON(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"response": "not valid json {{{"`))
	}))
}

// mockOllamaServerMarkdownJSON creates a test server that wraps JSON in markdown code blocks.
func mockOllamaServerMarkdownJSON(t *testing.T, jsonContent string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		response := "```json\n" + jsonContent + "\n```"
		json.NewEncoder(w).Encode(map[string]string{"response": response})
	}))
}

// TestGetSymbolSummary_WithLLM tests GetSymbolSummary with mock Ollama.
func TestGetSymbolSummary_WithLLM(t *testing.T) {
	server := mockOllamaServer(t, `{"purpose": "Handles user authentication", "role": "auth service", "importance": "high - security"}`)
	defer server.Close()

	st := openTestDB(t)
	cfg := config.Default()
	cfg.Ollama.URL = server.URL
	cfg.Ollama.SymbolModel = "test-model"

	svc := New(cfg, st)
	ctx := context.Background()

	path := "/tmp/auth.go"
	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "Authenticate", Qualified: "pkg.Authenticate", StartLine: 1, EndLine: 20, Signature: "func Authenticate(user, pass string) bool"},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	summary, err := svc.GetSymbolSummary(ctx, ids["pkg.Authenticate"])
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, "Authenticate", summary.Name)
	assert.Equal(t, "Handles user authentication", summary.Purpose)
	assert.Equal(t, "auth service", summary.Role)
	assert.Equal(t, "high - security", summary.Importance)
	assert.Empty(t, summary.LLMError)
}

// TestGetSymbolSummary_LLMError tests GetSymbolSummary when LLM is unavailable.
func TestGetSymbolSummary_LLMError(t *testing.T) {
	server := mockOllamaServerError(t)
	defer server.Close()

	st := openTestDB(t)
	cfg := config.Default()
	cfg.Ollama.URL = server.URL

	svc := New(cfg, st)
	ctx := context.Background()

	path := "/tmp/test.go"
	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "foo", Qualified: "pkg.foo", StartLine: 1, EndLine: 5},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	summary, err := svc.GetSymbolSummary(ctx, ids["pkg.foo"])
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.NotEmpty(t, summary.LLMError)
	assert.Contains(t, summary.LLMError, "LLM model unavailable")
}

// TestGetSymbolSummary_LLMInvalidJSON tests GetSymbolSummary with malformed LLM response.
func TestGetSymbolSummary_LLMInvalidJSON(t *testing.T) {
	server := mockOllamaServer(t, "this is not json at all")
	defer server.Close()

	st := openTestDB(t)
	cfg := config.Default()
	cfg.Ollama.URL = server.URL

	svc := New(cfg, st)
	ctx := context.Background()

	path := "/tmp/test.go"
	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "bar", Qualified: "pkg.bar", StartLine: 1, EndLine: 5},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	summary, err := svc.GetSymbolSummary(ctx, ids["pkg.bar"])
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.NotEmpty(t, summary.LLMError)
	assert.Contains(t, summary.LLMError, "LLM response parse failed")
}

// TestGetSymbolSummary_LLMMarkdownWrapped tests GetSymbolSummary with markdown-wrapped JSON.
func TestGetSymbolSummary_LLMMarkdownWrapped(t *testing.T) {
	server := mockOllamaServerMarkdownJSON(t, `{"purpose": "Test function", "role": "testing", "importance": "low - trivial"}`)
	defer server.Close()

	st := openTestDB(t)
	cfg := config.Default()
	cfg.Ollama.URL = server.URL

	svc := New(cfg, st)
	ctx := context.Background()

	path := "/tmp/test.go"
	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "baz", Qualified: "pkg.baz", StartLine: 1, EndLine: 5},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	summary, err := svc.GetSymbolSummary(ctx, ids["pkg.baz"])
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, "Test function", summary.Purpose)
	assert.Empty(t, summary.LLMError)
}

// TestGetSymbolSummary_WithCallersAndCallees tests GetSymbolSummary includes callers and callees.
func TestGetSymbolSummary_WithCallersAndCallees(t *testing.T) {
	server := mockOllamaServer(t, `{"purpose": "Test", "role": "test", "importance": "medium"}`)
	defer server.Close()

	st := openTestDB(t)
	cfg := config.Default()
	cfg.Ollama.URL = server.URL

	svc := New(cfg, st)
	ctx := context.Background()

	path := "/tmp/calls.go"
	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "caller", Qualified: "pkg.caller", StartLine: 1, EndLine: 10},
		{FilePath: path, Language: "go", Kind: "function", Name: "target", Qualified: "pkg.target", StartLine: 20, EndLine: 30},
		{FilePath: path, Language: "go", Kind: "function", Name: "callee", Qualified: "pkg.callee", StartLine: 40, EndLine: 50},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	edges := []store.Edge{
		{SrcID: ids["pkg.caller"], DstID: ids["pkg.target"], Kind: EdgeCall, FilePath: path, Line: 5, DstName: "target"},
		{SrcID: ids["pkg.target"], DstID: ids["pkg.callee"], Kind: EdgeCall, FilePath: path, Line: 25, DstName: "callee"},
	}
	require.NoError(t, st.ReplaceEdges(ctx, path, edges))

	summary, err := svc.GetSymbolSummary(ctx, ids["pkg.target"])
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Contains(t, summary.Callers, "pkg.caller")
	assert.Contains(t, summary.Calls, "pkg.callee")
}

// TestGetFileIntent_WithLLM tests GetFileIntent with mock Ollama.
func TestGetFileIntent_WithLLM(t *testing.T) {
	server := mockOllamaServer(t, `{"purpose": "Provides database access", "layer": "implementation", "responsibilities": ["query db", "cache results"]}`)
	defer server.Close()

	st := openTestDB(t)
	cfg := config.Default()
	cfg.Ollama.URL = server.URL

	svc := New(cfg, st)
	ctx := context.Background()

	// Create a real temp file for sourceContentFile
	tmpDir := t.TempDir()
	path := tmpDir + "/db.go"
	require.NoError(t, writeTestFile(path, "package db\n\nfunc Query() {}\n"))

	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "Query", Qualified: "db.Query", StartLine: 3, EndLine: 5},
	}
	_, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	intent, err := svc.GetFileIntent(ctx, path)
	require.NoError(t, err)
	require.NotNil(t, intent)
	assert.Equal(t, "Provides database access", intent.Purpose)
	assert.Equal(t, "implementation", intent.Layer)
	assert.Contains(t, intent.Responsibilities, "query db")
	assert.Empty(t, intent.LLMError)
}

// TestGetFileIntent_LLMError tests GetFileIntent when LLM is unavailable.
func TestGetFileIntent_LLMError(t *testing.T) {
	server := mockOllamaServerError(t)
	defer server.Close()

	st := openTestDB(t)
	cfg := config.Default()
	cfg.Ollama.URL = server.URL

	svc := New(cfg, st)
	ctx := context.Background()

	tmpDir := t.TempDir()
	path := tmpDir + "/test.go"
	require.NoError(t, writeTestFile(path, "package main\n"))

	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	intent, err := svc.GetFileIntent(ctx, path)
	require.NoError(t, err)
	require.NotNil(t, intent)
	assert.NotEmpty(t, intent.LLMError)
}

// TestGetFileIntent_WithImports tests GetFileIntent includes import edges.
func TestGetFileIntent_WithImports(t *testing.T) {
	server := mockOllamaServer(t, `{"purpose": "Test", "layer": "test", "responsibilities": ["testing"]}`)
	defer server.Close()

	st := openTestDB(t)
	cfg := config.Default()
	cfg.Ollama.URL = server.URL

	svc := New(cfg, st)
	ctx := context.Background()

	tmpDir := t.TempDir()
	path := tmpDir + "/imports.go"
	require.NoError(t, writeTestFile(path, "package main\nimport \"fmt\"\n"))

	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "main", Qualified: "main", StartLine: 3, EndLine: 5},
		{FilePath: path, Language: "go", Kind: "module", Name: "fmt", Qualified: "fmt", StartLine: 2, EndLine: 2},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	edges := []store.Edge{
		{SrcID: ids["main"], DstID: ids["fmt"], Kind: EdgeImport, FilePath: path, Line: 2, DstName: "fmt"},
	}
	require.NoError(t, st.ReplaceEdges(ctx, path, edges))

	intent, err := svc.GetFileIntent(ctx, path)
	require.NoError(t, err)
	require.NotNil(t, intent)
	assert.Contains(t, intent.Imports, "fmt")
}

// TestGetSemanticNeighborhood_WithLLM tests GetSemanticNeighborhood with mock Ollama.
func TestGetSemanticNeighborhood_WithLLM(t *testing.T) {
	server := mockOllamaServer(t, `{"cluster": "auth", "core": ["login", "validate"], "dependencies": ["database"], "boundary": ["api"]}`)
	defer server.Close()

	st := openTestDB(t)
	cfg := config.Default()
	cfg.Ollama.URL = server.URL

	svc := New(cfg, st)
	ctx := context.Background()

	path := "/tmp/auth.go"
	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "login", Qualified: "pkg.login", StartLine: 1, EndLine: 10},
		{FilePath: path, Language: "go", Kind: "function", Name: "validate", Qualified: "pkg.validate", StartLine: 20, EndLine: 30},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	edges := []store.Edge{
		{SrcID: ids["pkg.login"], DstID: ids["pkg.validate"], Kind: EdgeCall, FilePath: path, Line: 5, DstName: "validate"},
	}
	require.NoError(t, st.ReplaceEdges(ctx, path, edges))

	nh, err := svc.GetSemanticNeighborhood(ctx, ids["pkg.login"])
	require.NoError(t, err)
	require.NotNil(t, nh)
	assert.Equal(t, "login", nh.Center)
	assert.Equal(t, "auth", nh.Cluster)
	assert.Contains(t, nh.Core, "login")
	assert.Contains(t, nh.Dependencies, "database")
	assert.Empty(t, nh.LLMError)
}

// TestGetSemanticNeighborhood_LLMError tests GetSemanticNeighborhood when LLM is unavailable.
func TestGetSemanticNeighborhood_LLMError(t *testing.T) {
	server := mockOllamaServerError(t)
	defer server.Close()

	st := openTestDB(t)
	cfg := config.Default()
	cfg.Ollama.URL = server.URL

	svc := New(cfg, st)
	ctx := context.Background()

	path := "/tmp/test.go"
	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "foo", Qualified: "pkg.foo", StartLine: 1, EndLine: 5},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	nh, err := svc.GetSemanticNeighborhood(ctx, ids["pkg.foo"])
	require.NoError(t, err)
	require.NotNil(t, nh)
	assert.NotEmpty(t, nh.LLMError)
}

// TestGetSemanticNeighborhood_WithReferences tests GetSemanticNeighborhood includes reference edges.
func TestGetSemanticNeighborhood_WithReferences(t *testing.T) {
	server := mockOllamaServer(t, `{"cluster": "test", "core": [], "dependencies": [], "boundary": []}`)
	defer server.Close()

	st := openTestDB(t)
	cfg := config.Default()
	cfg.Ollama.URL = server.URL

	svc := New(cfg, st)
	ctx := context.Background()

	path := "/tmp/refs.go"
	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "user", Qualified: "pkg.user", StartLine: 1, EndLine: 10},
		{FilePath: path, Language: "go", Kind: "struct", Name: "User", Qualified: "pkg.User", StartLine: 20, EndLine: 25},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	edges := []store.Edge{
		{SrcID: ids["pkg.user"], DstID: ids["pkg.User"], Kind: EdgeReference, FilePath: path, Line: 5, DstName: "User"},
	}
	require.NoError(t, st.ReplaceEdges(ctx, path, edges))

	nh, err := svc.GetSemanticNeighborhood(ctx, ids["pkg.user"])
	require.NoError(t, err)
	require.NotNil(t, nh)
	assert.Contains(t, nh.Neighbors.Types, "User")
}

// TestCallOllama_Success tests callOllama with successful response.
func TestCallOllama_Success(t *testing.T) {
	server := mockOllamaServer(t, "test response")
	defer server.Close()

	st := openTestDB(t)
	cfg := config.Default()
	cfg.Ollama.URL = server.URL

	svc := New(cfg, st)
	ctx := context.Background()

	response, err := svc.callOllama(ctx, "test prompt", "test-model")
	require.NoError(t, err)
	assert.Equal(t, "test response", response)
}

// TestCallOllama_ServerError tests callOllama with server error.
func TestCallOllama_ServerError(t *testing.T) {
	server := mockOllamaServerError(t)
	defer server.Close()

	st := openTestDB(t)
	cfg := config.Default()
	cfg.Ollama.URL = server.URL

	svc := New(cfg, st)
	ctx := context.Background()

	response, err := svc.callOllama(ctx, "test prompt", "test-model")
	assert.Error(t, err)
	assert.Empty(t, response)
	assert.Contains(t, err.Error(), "ollama error")
}

// TestCallOllama_InvalidJSON tests callOllama with invalid JSON response.
func TestCallOllama_InvalidJSON(t *testing.T) {
	server := mockOllamaServerInvalidJSON(t)
	defer server.Close()

	st := openTestDB(t)
	cfg := config.Default()
	cfg.Ollama.URL = server.URL

	svc := New(cfg, st)
	ctx := context.Background()

	response, err := svc.callOllama(ctx, "test prompt", "test-model")
	assert.Error(t, err)
	assert.Empty(t, response)
}

// TestCallOllama_WithURLTrailingSlash tests callOllama handles URL with trailing slash.
func TestCallOllama_WithURLTrailingSlash(t *testing.T) {
	server := mockOllamaServer(t, "response")
	defer server.Close()

	st := openTestDB(t)
	cfg := config.Default()
	cfg.Ollama.URL = server.URL + "/"

	svc := New(cfg, st)
	ctx := context.Background()

	response, err := svc.callOllama(ctx, "prompt", "model")
	require.NoError(t, err)
	assert.Equal(t, "response", response)
}

// TestGetSymbolSummary_WithParentInfo tests GetSymbolSummary includes parent information.
func TestGetSymbolSummary_WithParentInfo(t *testing.T) {
	server := mockOllamaServer(t, `{"purpose": "Test", "role": "test", "importance": "low"}`)
	defer server.Close()

	st := openTestDB(t)
	cfg := config.Default()
	cfg.Ollama.URL = server.URL

	svc := New(cfg, st)
	ctx := context.Background()

	path := "/tmp/parent.go"
	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "class", Name: "MyClass", Qualified: "pkg.MyClass", StartLine: 1, EndLine: 50},
		{FilePath: path, Language: "go", Kind: "method", Name: "MyMethod", Qualified: "pkg.MyClass.MyMethod", StartLine: 10, EndLine: 20},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	summary, err := svc.GetSymbolSummary(ctx, ids["pkg.MyClass.MyMethod"])
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, "MyMethod", summary.Name)
}

// TestGetSymbolSummary_WithDoc tests GetSymbolSummary includes doc comment.
func TestGetSymbolSummary_WithDoc(t *testing.T) {
	server := mockOllamaServer(t, `{"purpose": "Test", "role": "test", "importance": "low"}`)
	defer server.Close()

	st := openTestDB(t)
	cfg := config.Default()
	cfg.Ollama.URL = server.URL

	svc := New(cfg, st)
	ctx := context.Background()

	path := "/tmp/doc.go"
	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "Documented", Qualified: "pkg.Documented", StartLine: 1, EndLine: 10, Doc: "This is a documented function that does something important."},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	summary, err := svc.GetSymbolSummary(ctx, ids["pkg.Documented"])
	require.NoError(t, err)
	require.NotNil(t, summary)
}

// TestGetFileIntent_LLMInvalidJSON tests GetFileIntent with malformed LLM response.
func TestGetFileIntent_LLMInvalidJSON(t *testing.T) {
	server := mockOllamaServer(t, "not json")
	defer server.Close()

	st := openTestDB(t)
	cfg := config.Default()
	cfg.Ollama.URL = server.URL

	svc := New(cfg, st)
	ctx := context.Background()

	tmpDir := t.TempDir()
	path := tmpDir + "/test.go"
	require.NoError(t, writeTestFile(path, "package main\n"))

	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	intent, err := svc.GetFileIntent(ctx, path)
	require.NoError(t, err)
	require.NotNil(t, intent)
	assert.NotEmpty(t, intent.LLMError)
	assert.Contains(t, intent.LLMError, "LLM response parse failed")
}

// TestGetFileIntent_LLMMarkdownWrapped tests GetFileIntent with markdown-wrapped JSON.
func TestGetFileIntent_LLMMarkdownWrapped(t *testing.T) {
	server := mockOllamaServerMarkdownJSON(t, `{"purpose": "Test", "layer": "test", "responsibilities": ["testing"]}`)
	defer server.Close()

	st := openTestDB(t)
	cfg := config.Default()
	cfg.Ollama.URL = server.URL

	svc := New(cfg, st)
	ctx := context.Background()

	tmpDir := t.TempDir()
	path := tmpDir + "/test.go"
	require.NoError(t, writeTestFile(path, "package main\n"))

	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	intent, err := svc.GetFileIntent(ctx, path)
	require.NoError(t, err)
	require.NotNil(t, intent)
	assert.Equal(t, "Test", intent.Purpose)
	assert.Empty(t, intent.LLMError)
}
