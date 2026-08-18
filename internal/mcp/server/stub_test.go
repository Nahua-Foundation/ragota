package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/Nahua-Foundation/ragota/client"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Nahua-Foundation/ragota/internal/mcp/config"
)

// Repository ids as ragota derives them: the name, then a hash of name and
// path. A model cannot produce one, which is why the tools accept names too.
const (
	ordersID  = "orders-aaaaaaaaaaaa"
	billingID = "billing-bbbbbbbbbbbb"
)

// stub stands in for ragota.
//
// The suite deliberately does not need a real server. Every behaviour these tests
// are about — a degraded search, a damaged index, a version this build cannot
// speak, a repository that does not exist — is a state a running ragota
// would have to be broken into, and a stub can simply be asked for it.
type stub struct {
	srv *httptest.Server

	mu       sync.Mutex
	handlers map[string]http.HandlerFunc
	requests map[string][]recorded
}

type recorded struct {
	body   []byte
	query  url.Values
	header http.Header
}

func newStub(t *testing.T) *stub {
	t.Helper()
	s := &stub{
		handlers: map[string]http.HandlerFunc{},
		requests: map[string][]recorded{},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)

	// The two routes the startup check needs, so that each test declares only
	// what it is actually about.
	s.reply("/health", client.HealthResponse{
		Status: "ok", Version: "v1.2.3-test", APIVersion: client.SchemaVersion,
	})
	s.reply("/api/v1/repos", []client.Repo{
		{ID: ordersID, Name: "orders", Status: "idle", IndexedAt: 1_700_000_000},
		{ID: billingID, Name: "billing", Status: "idle", IndexedAt: 1_700_000_000},
	})
	return s
}

func (s *stub) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	s.mu.Lock()
	h := s.handlers[r.URL.Path]
	s.requests[r.URL.Path] = append(s.requests[r.URL.Path], recorded{
		body: body, query: r.URL.Query(), header: r.Header.Clone(),
	})
	s.mu.Unlock()

	if h == nil {
		writeAPIError(w, http.StatusNotFound, client.CodeNotFound, "no stub registered for "+r.URL.Path)
		return
	}
	h(w, r)
}

// on registers a raw handler, for a test that needs a particular status or header.
func (s *stub) on(path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[path] = h
}

// reply answers path with v, JSON-encoded.
func (s *stub) reply(path string, v any) {
	s.on(path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	})
}

// text answers path with a body that is not JSON, as /services/export does.
func (s *stub) text(path, body string) {
	s.on(path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body))
	})
}

// fail answers path with the API's error shape.
func (s *stub) fail(path string, status int, code, message string) {
	s.on(path, func(w http.ResponseWriter, _ *http.Request) {
		writeAPIError(w, status, code, message)
	})
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(client.ErrorResponse{Error: message, Code: code})
}

func (s *stub) last(t *testing.T, path string) recorded {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	reqs := s.requests[path]
	if len(reqs) == 0 {
		t.Fatalf("no request reached %s", path)
	}
	return reqs[len(reqs)-1]
}

// lastBody decodes the most recent request body sent to path.
func (s *stub) lastBody(t *testing.T, path string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(s.last(t, path).body, &out); err != nil {
		t.Fatalf("decode request body for %s: %v", path, err)
	}
	return out
}

func (s *stub) calls(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests[path])
}

// --- harness ---

func testConfig(s *stub, mutate ...func(*config.Config)) *config.Config {
	cfg := &config.Config{
		BaseURL:   s.srv.URL,
		APIKey:    "test-key",
		AuthStyle: config.AuthAPIKey,
		Timeout:   5 * time.Second,
		MaxBytes:  config.DefaultMaxBytes,
	}
	for _, m := range mutate {
		m(cfg)
	}
	return cfg
}

// newServer builds a Server against the stub without connecting a session, for
// the tests that are about StartupCheck rather than about a tool.
func newServer(s *stub, mutate ...func(*config.Config)) *Server {
	cfg := testConfig(s, mutate...)
	c := client.New(cfg.BaseURL,
		client.WithAPIKey(cfg.APIKey),
		// Retries off. A test that stubs a 429 or a 500 asserts on the message it
		// produces, and the client's default two retries would only make the
		// suite wait for the same answer twice more.
		client.WithRetries(-1),
	)
	return New(cfg, c)
}

// connect runs the startup check and returns a live MCP session over the
// in-memory transport, so that every tool assertion below goes through real
// protocol framing, schema validation and result encoding rather than calling a
// handler directly.
func connect(t *testing.T, s *stub, mutate ...func(*config.Config)) *mcp.ClientSession {
	t.Helper()
	srv := newServer(s, mutate...)
	if _, err := srv.StartupCheck(t.Context()); err != nil {
		t.Fatalf("startup check: %v", err)
	}

	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := srv.MCP("test").Connect(t.Context(), serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).
		Connect(t.Context(), clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// call invokes a tool and returns its text, failing the test on an error result.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res := rawCall(t, cs, name, args)
	if res.IsError {
		t.Fatalf("%s returned an error result: %s", name, contentText(t, res))
	}
	return contentText(t, res)
}

// callError invokes a tool that is expected to fail, and returns the message the
// model would be shown.
func callError(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res := rawCall(t, cs, name, args)
	if !res.IsError {
		t.Fatalf("%s succeeded, wanted an error result:\n%s", name, contentText(t, res))
	}
	return contentText(t, res)
}

func rawCall(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	return res
}

func contentText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("result carried no content")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("result content is %T, wanted text", res.Content[0])
	}
	// A structured result is echoed as JSON in the content block too, so a tool
	// that declares an output schema makes the model pay twice for one answer.
	// Nothing here declares one, and this is what keeps that true.
	if res.StructuredContent != nil {
		t.Fatalf("result carried structured content beside its text: %v", res.StructuredContent)
	}
	return tc.Text
}

func unit(id, repo, path, name, kind string, start, end int) *client.Unit {
	return &client.Unit{
		ID: id, RepoID: repo, FilePath: path, Name: name, Kind: kind,
		StartLine: start, EndLine: end, Language: "go",
	}
}
