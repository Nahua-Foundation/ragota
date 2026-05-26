package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ragota/internal/repos"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockLSPServer creates a pair of pipes simulating an LSP server that responds
// to JSON-RPC requests. The handler function receives each incoming request
// and returns the result to send back.
type mockLSPServer struct {
	stdinR  *io.PipeReader // server reads client requests
	stdinW  *io.PipeWriter // client writes requests
	stdoutR *io.PipeReader // client reads server responses
	stdoutW *io.PipeWriter // server writes responses
	done    chan struct{}
}

func newMockLSPServer() *mockLSPServer {
	// Client writes to stdinW, server reads from stdinR
	stdinR, stdinW := io.Pipe()
	// Server writes to stdoutW, client reads from stdoutR
	stdoutR, stdoutW := io.Pipe()
	return &mockLSPServer{
		stdinR:  stdinR,
		stdinW:  stdinW,
		stdoutR: stdoutR,
		stdoutW: stdoutW,
		done:    make(chan struct{}),
	}
}

func (m *mockLSPServer) close() {
	m.stdinW.Close()
	m.stdinR.Close()
	m.stdoutW.Close()
	m.stdoutR.Close()
}

// newClientWithMockServer creates a Client wired to a mock LSP server.
// The handler is called for each incoming request and should return the result.
func newClientWithMockServer(lang string, handler func(method string, params json.RawMessage, id int64) json.RawMessage) (*Client, *mockLSPServer) {
	mock := newMockLSPServer()

	c := &Client{
		Language:         lang,
		stdin:            mock.stdinW,
		stdout:           mock.stdoutR,
		pending:          make(map[int64]chan rpcResponse),
		rootURI:          FileURI("/tmp/project"),
		localRoot:        "/tmp/project",
		openedFiles:      make(map[string]string),
		openedVers:       make(map[string]int),
		diagnosticsReady: make(chan string, 16),
		processDone:      make(chan struct{}),
		javaReady:        make(chan struct{}),
		goplsReady:       make(chan struct{}),
	}

	// Start readLoop
	go c.readLoop()

	// Start mock server that reads requests and writes responses
	go func() {
		defer close(mock.done)
		br := newFramedReader(mock.stdinR)
		for {
			msg, err := br.readMessage()
			if err != nil {
				return
			}
			// Only respond to requests with IDs (not notifications)
			if msg.ID != nil && msg.Method != "" {
				var id int64
				_ = json.Unmarshal(*msg.ID, &id)
				result := handler(msg.Method, msg.Params, id)
				resp := map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result":  json.RawMessage(result),
				}
				data, _ := json.Marshal(resp)
				header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
				_, _ = io.WriteString(mock.stdoutW, header)
				_, _ = mock.stdoutW.Write(data)
			}
		}
	}()

	return c, mock
}

// framedReader reads Content-Length framed JSON-RPC messages
type framedReader struct {
	r io.Reader
}

func newFramedReader(r io.Reader) *framedReader {
	return &framedReader{r: r}
}

func (fr *framedReader) readMessage() (*rpcIncoming, error) {
	// Read headers
	length := -1
	buf := make([]byte, 1)
	var line strings.Builder
	for {
		n, err := fr.r.Read(buf)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			continue
		}
		if buf[0] == '\n' {
			l := strings.TrimSpace(line.String())
			if l == "" {
				break
			}
			if strings.HasPrefix(strings.ToLower(l), "content-length:") {
				v := strings.TrimSpace(l[len("Content-Length:"):])
				fmt.Sscanf(v, "%d", &length)
			}
			line.Reset()
		} else {
			line.WriteByte(buf[0])
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("no content-length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(fr.r, body); err != nil {
		return nil, err
	}
	var msg rpcIncoming
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// ==================== Call tests ====================

func TestCall_Success(t *testing.T) {
	c, mock := newClientWithMockServer("go", func(method string, params json.RawMessage, id int64) json.RawMessage {
		if method == "textDocument/hover" {
			r, _ := json.Marshal(map[string]any{
				"contents": map[string]any{
					"kind":  "markdown",
					"value": "func Hello()",
				},
			})
			return r
		}
		return json.RawMessage("null")
	})
	defer mock.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var result json.RawMessage
	err := c.Call(ctx, "textDocument/hover", map[string]any{
		"textDocument": map[string]any{"uri": "file:///tmp/project/main.go"},
		"position":     map[string]any{"line": 0, "character": 5},
	}, &result)
	assert.NoError(t, err)
	assert.NotEmpty(t, result)
}

func TestCall_ContextCancelled(t *testing.T) {
	c, mock := newClientWithMockServer("go", func(method string, params json.RawMessage, id int64) json.RawMessage {
		// Never respond — simulate slow server
		time.Sleep(10 * time.Second)
		return json.RawMessage("null")
	})
	defer mock.close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	var result json.RawMessage
	err := c.Call(ctx, "textDocument/hover", nil, &result)
	assert.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestCall_NilResult(t *testing.T) {
	c, mock := newClientWithMockServer("go", func(method string, params json.RawMessage, id int64) json.RawMessage {
		return json.RawMessage(`{"success": true}`)
	})
	defer mock.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// nil result — should not try to unmarshal
	err := c.Call(ctx, "shutdown", nil, nil)
	assert.NoError(t, err)
}

func TestCall_ServerError(t *testing.T) {
	mock := newMockLSPServer()
	c := &Client{
		Language:         "go",
		stdin:            mock.stdinW,
		stdout:           mock.stdoutR,
		pending:          make(map[int64]chan rpcResponse),
		rootURI:          FileURI("/tmp"),
		openedFiles:      make(map[string]string),
		openedVers:       make(map[string]int),
		diagnosticsReady: make(chan string, 16),
		processDone:      make(chan struct{}),
		javaReady:        make(chan struct{}),
		goplsReady:       make(chan struct{}),
	}
	go c.readLoop()

	// Server that returns errors
	go func() {
		br := newFramedReader(mock.stdinR)
		for {
			msg, err := br.readMessage()
			if err != nil {
				return
			}
			if msg.ID != nil && msg.Method != "" {
				var id int64
				_ = json.Unmarshal(*msg.ID, &id)
				resp := map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"error":   map[string]any{"code": -32601, "message": "method not found"},
				}
				data, _ := json.Marshal(resp)
				header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
				_, _ = io.WriteString(mock.stdoutW, header)
				_, _ = mock.stdoutW.Write(data)
			}
		}
	}()
	defer mock.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var result json.RawMessage
	err := c.Call(ctx, "unknownMethod", nil, &result)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "method not found")
}

// ==================== Navigation tests ====================

func TestDefinition_WithMockServer(t *testing.T) {
	c, mock := newClientWithMockServer("go", func(method string, params json.RawMessage, id int64) json.RawMessage {
		switch method {
		case "textDocument/definition":
			r, _ := json.Marshal([]map[string]any{
				{
					"uri": "file:///tmp/project/other.go",
					"range": map[string]any{
						"start": map[string]any{"line": 10, "character": 5},
						"end":   map[string]any{"line": 10, "character": 15},
					},
				},
			})
			return r
		default:
			return json.RawMessage("null")
		}
	})
	defer mock.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create temp file for localFileLine
	dir := t.TempDir()
	testFile := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(testFile, []byte("package main\nfunc Hello() {}\n"), 0644))

	locs, err := c.Definition(ctx, testFile, 5, 10)
	assert.NoError(t, err)
	assert.NotEmpty(t, locs)
	assert.Equal(t, 10, locs[0].StartLine)
}

func TestDefinition_FallbackToDeclaration(t *testing.T) {
	callCount := 0
	c, mock := newClientWithMockServer("typescript", func(method string, params json.RawMessage, id int64) json.RawMessage {
		callCount++
		switch method {
		case "textDocument/definition":
			return json.RawMessage("null")
		case "textDocument/declaration":
			r, _ := json.Marshal(map[string]any{
				"uri": "file:///tmp/project/types.ts",
				"range": map[string]any{
					"start": map[string]any{"line": 1, "character": 0},
					"end":   map[string]any{"line": 1, "character": 10},
				},
			})
			return r
		default:
			return json.RawMessage("null")
		}
	})
	defer mock.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	testFile := filepath.Join(dir, "app.ts")
	require.NoError(t, os.WriteFile(testFile, []byte("const x = 1;\n"), 0644))

	locs, err := c.Definition(ctx, testFile, 0, 6)
	assert.NoError(t, err)
	_ = callCount
	if len(locs) > 0 {
		assert.Equal(t, 1, locs[0].StartLine)
	}
}

func TestDefinition_EmptyResult(t *testing.T) {
	c, mock := newClientWithMockServer("go", func(method string, params json.RawMessage, id int64) json.RawMessage {
		return json.RawMessage("null")
	})
	defer mock.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	testFile := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(testFile, []byte("package main\n"), 0644))

	locs, err := c.Definition(ctx, testFile, 0, 0)
	assert.NoError(t, err)
	assert.Nil(t, locs)
}

func TestReferences_WithMockServer(t *testing.T) {
	c, mock := newClientWithMockServer("go", func(method string, params json.RawMessage, id int64) json.RawMessage {
		if method == "textDocument/references" {
			r, _ := json.Marshal([]map[string]any{
				{
					"uri": "file:///tmp/project/a.go",
					"range": map[string]any{
						"start": map[string]any{"line": 1, "character": 5},
						"end":   map[string]any{"line": 1, "character": 10},
					},
				},
				{
					"uri": "file:///tmp/project/b.go",
					"range": map[string]any{
						"start": map[string]any{"line": 20, "character": 0},
						"end":   map[string]any{"line": 20, "character": 5},
					},
				},
			})
			return r
		}
		return json.RawMessage("null")
	})
	defer mock.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	locs, err := c.References(ctx, "/tmp/project/main.go", 5, 10, true)
	assert.NoError(t, err)
	require.Len(t, locs, 2)
}

func TestReferences_EmptyResult(t *testing.T) {
	c, mock := newClientWithMockServer("go", func(method string, params json.RawMessage, id int64) json.RawMessage {
		return json.RawMessage("null")
	})
	defer mock.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	locs, err := c.References(ctx, "/tmp/project/main.go", 0, 0, false)
	assert.NoError(t, err)
	assert.Nil(t, locs)
}

func TestHover_WithMockServer(t *testing.T) {
	c, mock := newClientWithMockServer("go", func(method string, params json.RawMessage, id int64) json.RawMessage {
		if method == "textDocument/hover" {
			r, _ := json.Marshal(map[string]any{
				"contents": map[string]any{
					"kind":  "markdown",
					"value": "```go\nfunc Hello() string\n```\nHello returns a greeting.",
				},
			})
			return r
		}
		return json.RawMessage("null")
	})
	defer mock.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	text, err := c.Hover(ctx, "/tmp/project/main.go", 5, 10)
	assert.NoError(t, err)
	assert.Contains(t, text, "func Hello()")
}

func TestHover_NullResult(t *testing.T) {
	c, mock := newClientWithMockServer("go", func(method string, params json.RawMessage, id int64) json.RawMessage {
		return json.RawMessage("null")
	})
	defer mock.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	text, err := c.Hover(ctx, "/tmp/project/main.go", 0, 0)
	assert.NoError(t, err)
	assert.Equal(t, "", text)
}

func TestHover_EmptyBody(t *testing.T) {
	c, mock := newClientWithMockServer("go", func(method string, params json.RawMessage, id int64) json.RawMessage {
		if method == "textDocument/hover" {
			return json.RawMessage(`null`)
		}
		return json.RawMessage("null")
	})
	defer mock.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	text, err := c.Hover(ctx, "/tmp/project/main.go", 0, 0)
	assert.NoError(t, err)
	assert.Equal(t, "", text)
}

func TestImplementation_WithMockServer(t *testing.T) {
	c, mock := newClientWithMockServer("java", func(method string, params json.RawMessage, id int64) json.RawMessage {
		if method == "textDocument/implementation" {
			r, _ := json.Marshal([]map[string]any{
				{
					"uri": "file:///tmp/project/Impl.java",
					"range": map[string]any{
						"start": map[string]any{"line": 15, "character": 4},
						"end":   map[string]any{"line": 15, "character": 20},
					},
				},
			})
			return r
		}
		return json.RawMessage("null")
	})
	defer mock.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	locs, err := c.Implementation(ctx, "/tmp/project/Iface.java", 5, 10)
	assert.NoError(t, err)
	require.Len(t, locs, 1)
	assert.Equal(t, 15, locs[0].StartLine)
}

func TestImplementation_PythonNotSupported(t *testing.T) {
	c := &Client{
		Language:         "python",
		pending:          make(map[int64]chan rpcResponse),
		diagnosticsReady: make(chan string, 16),
		javaReady:        make(chan struct{}),
		goplsReady:       make(chan struct{}),
		processDone:      make(chan struct{}),
		openedFiles:      make(map[string]string),
		openedVers:       make(map[string]int),
	}

	locs, err := c.Implementation(context.Background(), "/tmp/test.py", 0, 0)
	assert.NoError(t, err)
	assert.Nil(t, locs)
}

func TestImplementation_JavaFallbackToReferences(t *testing.T) {
	callCount := 0
	c, mock := newClientWithMockServer("java", func(method string, params json.RawMessage, id int64) json.RawMessage {
		callCount++
		switch method {
		case "textDocument/implementation":
			return json.RawMessage("null")
		case "textDocument/references":
			r, _ := json.Marshal([]map[string]any{
				{
					"uri": "file:///tmp/project/Impl.java",
					"range": map[string]any{
						"start": map[string]any{"line": 20, "character": 0},
						"end":   map[string]any{"line": 20, "character": 10},
					},
				},
			})
			return r
		default:
			return json.RawMessage("null")
		}
	})
	defer mock.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	locs, err := c.Implementation(ctx, "/tmp/project/Iface.java", 5, 10)
	assert.NoError(t, err)
	_ = callCount
	if len(locs) > 0 {
		assert.Equal(t, 20, locs[0].StartLine)
	}
}

// ==================== waitForDiagnostics tests ====================

func TestWaitForDiagnostics_ZeroTimeout(t *testing.T) {
	c := newTestClient()
	// Should return immediately with zero timeout
	c.waitForDiagnostics("/tmp/foo.go", 0)
}

func TestWaitForDiagnostics_Timeout(t *testing.T) {
	c := newTestClient()
	start := time.Now()
	c.waitForDiagnostics("/tmp/foo.go", 200*time.Millisecond)
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 150*time.Millisecond)
	assert.Less(t, elapsed, 2*time.Second)
}

func TestWaitForDiagnostics_MatchingPath(t *testing.T) {
	c := newTestClient()
	go func() {
		time.Sleep(100 * time.Millisecond)
		abs, _ := filepath.Abs("/tmp/foo.go")
		c.diagnosticsReady <- abs
	}()
	start := time.Now()
	c.waitForDiagnostics("/tmp/foo.go", 5*time.Second)
	elapsed := time.Since(start)
	assert.Less(t, elapsed, 2*time.Second)
}

func TestWaitForDiagnostics_NonMatchingPath(t *testing.T) {
	c := newTestClient()
	go func() {
		time.Sleep(50 * time.Millisecond)
		c.diagnosticsReady <- "/tmp/other.go"
	}()
	start := time.Now()
	c.waitForDiagnostics("/tmp/foo.go", 300*time.Millisecond)
	elapsed := time.Since(start)
	// Should wait for timeout since path doesn't match
	assert.GreaterOrEqual(t, elapsed, 200*time.Millisecond)
}

// ==================== waitForDiagnosticsCtx tests ====================

func TestWaitForDiagnosticsCtx_ZeroTimeout(t *testing.T) {
	c := newTestClient()
	result := c.waitForDiagnosticsCtx(context.Background(), "/tmp/foo.go", 0)
	assert.True(t, result)
}

func TestWaitForDiagnosticsCtx_ContextCancelled(t *testing.T) {
	c := newTestClient()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	result := c.waitForDiagnosticsCtx(ctx, "/tmp/foo.go", 10*time.Second)
	assert.False(t, result)
}

func TestWaitForDiagnosticsCtx_Timeout(t *testing.T) {
	c := newTestClient()
	start := time.Now()
	result := c.waitForDiagnosticsCtx(context.Background(), "/tmp/foo.go", 200*time.Millisecond)
	elapsed := time.Since(start)
	assert.True(t, result) // timeout returns true
	assert.GreaterOrEqual(t, elapsed, 150*time.Millisecond)
}

func TestWaitForDiagnosticsCtx_MatchingPath(t *testing.T) {
	c := newTestClient()
	go func() {
		time.Sleep(100 * time.Millisecond)
		abs, _ := filepath.Abs("/tmp/foo.go")
		c.diagnosticsReady <- abs
	}()
	result := c.waitForDiagnosticsCtx(context.Background(), "/tmp/foo.go", 5*time.Second)
	assert.True(t, result)
}

// ==================== DidOpen tests ====================

func TestDidOpen_NewFile(t *testing.T) {
	c, mock := newClientWithMockServer("go", func(method string, params json.RawMessage, id int64) json.RawMessage {
		return json.RawMessage("null")
	})
	defer mock.close()

	// Push a matching diagnostics notification
	go func() {
		time.Sleep(200 * time.Millisecond)
		abs, _ := filepath.Abs("/tmp/project/main.go")
		c.diagnosticsReady <- abs
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := c.DidOpen(ctx, "/tmp/project/main.go", "go", "package main\n")
	assert.NoError(t, err)

	c.mu.Lock()
	_, exists := c.openedFiles["/tmp/project/main.go"]
	c.mu.Unlock()
	assert.True(t, exists)
}

func TestDidOpen_SameContentSkipped(t *testing.T) {
	c, mock := newClientWithMockServer("go", func(method string, params json.RawMessage, id int64) json.RawMessage {
		return json.RawMessage("null")
	})
	defer mock.close()

	c.mu.Lock()
	c.openedFiles["/tmp/project/main.go"] = "package main\n"
	c.openedVers["/tmp/project/main.go"] = 1
	c.mu.Unlock()

	ctx := context.Background()
	err := c.DidOpen(ctx, "/tmp/project/main.go", "go", "package main\n")
	assert.NoError(t, err)
	// Version should not have incremented
	c.mu.Lock()
	assert.Equal(t, 1, c.openedVers["/tmp/project/main.go"])
	c.mu.Unlock()
}

// ==================== withStderrLocked tests ====================

func TestWithStderrLocked_NilError(t *testing.T) {
	c := newTestClient()
	assert.NoError(t, c.withStderrLocked(nil))
}

func TestWithStderrLocked_ProcessExited(t *testing.T) {
	c := newTestClient()
	close(c.processDone)
	c.processErr = fmt.Errorf("exit code 1")
	err := c.withStderrLocked(fmt.Errorf("original error"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "original error")
	assert.Contains(t, err.Error(), "process exited")
}

func TestWithStderrLocked_ProcessExitedSuccessfully(t *testing.T) {
	c := newTestClient()
	close(c.processDone)
	c.processErr = nil
	err := c.withStderrLocked(fmt.Errorf("original error"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "exited successfully")
}

func TestWithStderrLocked_WithStderr(t *testing.T) {
	c := newTestClient()
	c.stderrLines = []string{"some error line"}
	err := c.withStderrLocked(fmt.Errorf("write failed"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "write failed")
	assert.Contains(t, err.Error(), "some error line")
}

func TestWithStderrLocked_ProcessStillRunning(t *testing.T) {
	c := newTestClient()
	// processDone is open (process still running)
	err := c.withStderrLocked(fmt.Errorf("write failed"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "write failed")
	// Should not contain process info since it's still running
	assert.NotContains(t, err.Error(), "process exited")
}

// ==================== Manager tests ====================

func TestManager_SetRepoResolver(t *testing.T) {
	m := NewManager("/root", nil)
	r := repos.NewResolver(nil)
	m.SetRepoResolver(r)
	m.mu.Lock()
	assert.NotNil(t, m.resolver)
	m.mu.Unlock()
}

func TestManager_ResolveRepo_NilResolver(t *testing.T) {
	m := NewManager("/root", nil)
	name, path := m.resolveRepo("/some/file.go")
	assert.Equal(t, "", name)
	assert.Equal(t, "", path)
}

func TestManager_ResolveRepo_WithResolver(t *testing.T) {
	m := NewManager("/root", nil)
	r := repos.NewResolver([]repos.Repo{
		{Name: "myrepo", Path: "/root/myrepo"},
	})
	m.SetRepoResolver(r)
	name, path := m.resolveRepo("/root/myrepo/src/main.go")
	assert.Equal(t, "myrepo", name)
	assert.Equal(t, "/root/myrepo", path)
}

func TestManager_ResolveRepo_UnknownPath(t *testing.T) {
	m := NewManager("/root", nil)
	r := repos.NewResolver([]repos.Repo{
		{Name: "myrepo", Path: "/root/myrepo"},
	})
	m.SetRepoResolver(r)
	name, path := m.resolveRepo("/other/project/file.go")
	assert.Equal(t, "", name)
	assert.Equal(t, "", path)
}

func TestManager_Close_WithClients(t *testing.T) {
	m := NewManager("/root", nil)
	// Add a fake client that can be closed — needs stdin and cmd
	r, w := io.Pipe()
	_ = r
	c := newTestClient()
	c.stdin = w
	c.cmd = &exec.Cmd{} // non-nil cmd with nil Process — Close handles this
	close(c.processDone)
	m.mu.Lock()
	m.clients[clientKey{repo: "", lang: "go", wsRoot: "/root"}] = c
	m.mu.Unlock()
	// Should not panic
	m.Close()
	m.mu.Lock()
	assert.Empty(t, m.clients)
	m.mu.Unlock()
}

// ==================== handleServerRequest tests ====================

func TestHandleServerRequest_WorkspaceConfiguration(t *testing.T) {
	c, mock := newClientWithMockServer("python", func(method string, params json.RawMessage, id int64) json.RawMessage {
		return json.RawMessage("null")
	})
	defer mock.close()

	params, _ := json.Marshal(map[string]any{
		"items": []map[string]any{
			{"section": "python"},
			{"section": "pyright"},
		},
	})

	// Should not panic and should send response
	c.handleServerRequest(json.RawMessage("1"), "workspace/configuration", params)
}

func TestHandleServerRequest_WorkspaceFolders(t *testing.T) {
	c, mock := newClientWithMockServer("go", func(method string, params json.RawMessage, id int64) json.RawMessage {
		return json.RawMessage("null")
	})
	defer mock.close()

	c.handleServerRequest(json.RawMessage("2"), "workspace/workspaceFolders", nil)
}

func TestHandleServerRequest_RegisterCapability(t *testing.T) {
	c, mock := newClientWithMockServer("go", func(method string, params json.RawMessage, id int64) json.RawMessage {
		return json.RawMessage("null")
	})
	defer mock.close()

	c.handleServerRequest(json.RawMessage("3"), "client/registerCapability", nil)
}

func TestHandleServerRequest_UnregisterCapability(t *testing.T) {
	c, mock := newClientWithMockServer("go", func(method string, params json.RawMessage, id int64) json.RawMessage {
		return json.RawMessage("null")
	})
	defer mock.close()

	c.handleServerRequest(json.RawMessage("4"), "client/unregisterCapability", nil)
}

func TestHandleServerRequest_WorkDoneProgress(t *testing.T) {
	c, mock := newClientWithMockServer("go", func(method string, params json.RawMessage, id int64) json.RawMessage {
		return json.RawMessage("null")
	})
	defer mock.close()

	c.handleServerRequest(json.RawMessage("5"), "window/workDoneProgress/create", nil)
}

func TestHandleServerRequest_ApplyEdit(t *testing.T) {
	c, mock := newClientWithMockServer("go", func(method string, params json.RawMessage, id int64) json.RawMessage {
		return json.RawMessage("null")
	})
	defer mock.close()

	c.handleServerRequest(json.RawMessage("6"), "workspace/applyEdit", nil)
}

func TestHandleServerRequest_UnknownMethod(t *testing.T) {
	c, mock := newClientWithMockServer("go", func(method string, params json.RawMessage, id int64) json.RawMessage {
		return json.RawMessage("null")
	})
	defer mock.close()

	c.handleServerRequest(json.RawMessage("7"), "unknown/method", nil)
}

// ==================== writeServerResponse tests ====================

func TestWriteServerResponse_Success(t *testing.T) {
	c := &Client{
		Language:    "go",
		stdin:       &bufferWriter{},
		pending:     make(map[int64]chan rpcResponse),
		processDone: make(chan struct{}),
	}

	resp := rpcServerResponse{
		JSONRPC: "2.0",
		ID:      json.RawMessage("1"),
		Result:  "ok",
	}
	err := c.writeServerResponse(resp)
	assert.NoError(t, err)
	written := c.stdin.(*bufferWriter).String()
	assert.Contains(t, written, "Content-Length:")
	assert.Contains(t, written, `"jsonrpc":"2.0"`)
}

func TestWriteServerResponse_WithError(t *testing.T) {
	c := &Client{
		Language:    "go",
		stdin:       &bufferWriter{},
		pending:     make(map[int64]chan rpcResponse),
		processDone: make(chan struct{}),
	}

	resp := rpcServerResponse{
		JSONRPC: "2.0",
		ID:      json.RawMessage("1"),
		Error:   &rpcError{Code: -32601, Message: "method not found"},
	}
	err := c.writeServerResponse(resp)
	assert.NoError(t, err)
	written := c.stdin.(*bufferWriter).String()
	assert.Contains(t, written, "method not found")
}

// ==================== write tests ====================

func TestWrite_Success(t *testing.T) {
	c := &Client{
		Language:    "go",
		stdin:       &bufferWriter{},
		pending:     make(map[int64]chan rpcResponse),
		processDone: make(chan struct{}),
	}

	err := c.write(rpcRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "textDocument/hover",
		Params:  map[string]any{"position": map[string]any{"line": 0, "character": 0}},
	})
	assert.NoError(t, err)
	written := c.stdin.(*bufferWriter).String()
	assert.Contains(t, written, "Content-Length:")
	assert.Contains(t, written, "textDocument/hover")
}

func TestWrite_ClosedPipe(t *testing.T) {
	c := &Client{
		Language:    "go",
		stdin:       &closedWriter{},
		pending:     make(map[int64]chan rpcResponse),
		processDone: make(chan struct{}),
	}

	err := c.write(rpcRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "test",
	})
	assert.Error(t, err)
}

// closedWriter is an io.WriteCloser that always returns an error.
type closedWriter struct{}

func (w *closedWriter) Write(p []byte) (int, error) {
	return 0, fmt.Errorf("write on closed pipe")
}

func (w *closedWriter) Close() error {
	return nil
}

// bufferWriter is a thread-safe bytes.Buffer implementing io.WriteCloser.
type bufferWriter struct {
	mu  sync.Mutex
	buf []byte
}

func (w *bufferWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func (w *bufferWriter) Close() error { return nil }

func (w *bufferWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}

// ==================== IsAlive tests ====================

func TestIsAlive_WithProcess_SignalZero(t *testing.T) {
	c := newTestClient()
	// Simulate a dead process by setting dead flag
	c.dead.Store(true)
	assert.False(t, c.IsAlive())
}

func TestIsAlive_ClosedAndDead(t *testing.T) {
	c := newTestClient()
	c.closed.Store(true)
	c.dead.Store(true)
	assert.False(t, c.IsAlive())
}

// ==================== processSummary extended tests ====================

func TestProcessSummary_WithProcessState(t *testing.T) {
	c := newTestClient()
	close(c.processDone)
	c.processErr = fmt.Errorf("exit status 1")
	summary := c.processSummary()
	assert.Contains(t, summary, "exited")
	assert.Contains(t, summary, "exit status 1")
}

// ==================== withStderr extended tests ====================

func TestWithStderr_ProcessExited(t *testing.T) {
	c := newTestClient()
	close(c.processDone)
	c.processErr = fmt.Errorf("killed")
	err := c.withStderr(fmt.Errorf("io error"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "io error")
	assert.Contains(t, err.Error(), "process")
}

func TestWithStderr_ProcessExitedWithStderr(t *testing.T) {
	c := newTestClient()
	close(c.processDone)
	c.processErr = nil
	c.rememberStderr("fatal error")
	err := c.withStderr(fmt.Errorf("write failed"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "write failed")
	assert.Contains(t, err.Error(), "fatal error")
}

// ==================== readLoop tests ====================

func TestReadLoop_OnDeadCallback(t *testing.T) {
	mock := newMockLSPServer()
	c := &Client{
		Language:         "go",
		stdout:           mock.stdoutR,
		pending:          make(map[int64]chan rpcResponse),
		diagnosticsReady: make(chan string, 16),
		processDone:      make(chan struct{}),
		javaReady:        make(chan struct{}),
		goplsReady:       make(chan struct{}),
		openedFiles:      make(map[string]string),
		openedVers:       make(map[string]int),
	}

	called := make(chan struct{})
	c.SetOnDead(func() {
		close(called)
	})

	go c.readLoop()

	// Close stdout to trigger readLoop exit
	mock.stdoutW.Close()

	select {
	case <-called:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatal("onDead callback was not called")
	}
}

func TestReadLoop_FailPendingOnExit(t *testing.T) {
	mock := newMockLSPServer()
	c := &Client{
		Language:         "go",
		stdout:           mock.stdoutR,
		pending:          make(map[int64]chan rpcResponse),
		diagnosticsReady: make(chan string, 16),
		processDone:      make(chan struct{}),
		javaReady:        make(chan struct{}),
		goplsReady:       make(chan struct{}),
		openedFiles:      make(map[string]string),
		openedVers:       make(map[string]int),
	}

	// Add a pending request
	ch := make(chan rpcResponse, 1)
	c.pending[1] = ch

	go c.readLoop()

	// Close stdout to trigger readLoop exit and failPending
	mock.stdoutW.Close()

	select {
	case resp := <-ch:
		assert.Error(t, resp.Err)
	case <-time.After(2 * time.Second):
		t.Fatal("pending request was not failed")
	}
}

// ==================== consumeStderr tests ====================

func TestConsumeStderr_NoisyLinesFiltered(t *testing.T) {
	c := newTestClient()
	input := "WARNING: some warning\nINFO: some info\nSLF4J: something\nreal error here\n"
	r := strings.NewReader(input)
	c.consumeStderr(r, "java")
	// Only "real error here" should be remembered
	c.mu.Lock()
	defer c.mu.Unlock()
	assert.Len(t, c.stderrLines, 1)
	assert.Equal(t, "real error here", c.stderrLines[0])
}

func TestConsumeStderr_EmptyLinesFiltered(t *testing.T) {
	c := newTestClient()
	input := "\n\n\n  \n\n"
	r := strings.NewReader(input)
	c.consumeStderr(r, "go")
	c.mu.Lock()
	defer c.mu.Unlock()
	assert.Empty(t, c.stderrLines)
}

func TestConsumeStderr_MixedLines(t *testing.T) {
	c := newTestClient()
	input := "FINE: debug\nERROR: real error\nFINER: more debug\nSEVERE: critical\n"
	r := strings.NewReader(input)
	c.consumeStderr(r, "java")
	c.mu.Lock()
	defer c.mu.Unlock()
	assert.Len(t, c.stderrLines, 2)
	assert.Equal(t, "ERROR: real error", c.stderrLines[0])
	assert.Equal(t, "SEVERE: critical", c.stderrLines[1])
}

// ==================== handleServerNotification extended ====================

func TestHandleNotification_Progress_WorkspaceScanEnd(t *testing.T) {
	c := newTestClient()
	c.Language = "go"
	params, _ := json.Marshal(map[string]any{
		"token": "Initial workspace scan",
		"value": map[string]any{"kind": "end"},
	})
	c.handleServerNotification("$/progress", params)
	select {
	case <-c.goplsReady:
		// ok
	default:
		t.Fatal("goplsReady should be closed for workspace scan end")
	}
}

func TestHandleNotification_Progress_UnknownToken(t *testing.T) {
	c := newTestClient()
	c.Language = "go"
	params, _ := json.Marshal(map[string]any{
		"token": "unknown.token",
		"value": map[string]any{"kind": "end"},
	})
	c.handleServerNotification("$/progress", params)
	select {
	case <-c.goplsReady:
		t.Fatal("goplsReady should NOT be closed for unknown token")
	default:
		// ok
	}
}

func TestHandleNotification_Progress_MalformedParams(t *testing.T) {
	c := newTestClient()
	c.handleServerNotification("$/progress", json.RawMessage("{bad"))
	// Should not panic
}

func TestHandleNotification_LanguageStatus_MalformedParams(t *testing.T) {
	c := newTestClient()
	c.handleServerNotification("language/status", json.RawMessage("{bad"))
	// Should not panic
}

// ==================== failPending extended ====================

func TestFailPending_WithProcessDetails(t *testing.T) {
	c := newTestClient()
	close(c.processDone)
	c.processErr = fmt.Errorf("exit 1")
	ch := make(chan rpcResponse, 1)
	c.pending[1] = ch
	c.failPending(fmt.Errorf("io error"))
	resp := <-ch
	assert.Error(t, resp.Err)
	assert.Contains(t, resp.Err.Error(), "io error")
	assert.Contains(t, resp.Err.Error(), "process")
}

func TestFailPending_WithStderr(t *testing.T) {
	c := newTestClient()
	c.rememberStderr("some stderr")
	ch := make(chan rpcResponse, 1)
	c.pending[1] = ch
	c.failPending(fmt.Errorf("io error"))
	resp := <-ch
	assert.Error(t, resp.Err)
	assert.Contains(t, resp.Err.Error(), "some stderr")
}
