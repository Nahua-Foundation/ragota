//go:build e2e

// Package e2e proves the two shipped binaries against each other: cmd/server
// built and started on a real corpus, cmd/mcp launched over stdio the way an
// MCP client launches it, and the answers read back through both doors.
//
// The three phases follow the working-set lifecycle, because that is where an
// end-to-end seam actually broke once: a wide --source, then a narrow one that
// must leave the other repository answering nothing by default, then the wide
// one again bringing it back.
package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Nahua-Foundation/ragota/pkg/client"
)

// The question phase 1 asks. Its terms live in the payments repository's doc
// comments and nowhere in the gateway, so the right answer names payments.
const chargeQuestion = "where is the charge captured and withdrawn from the card"

// Every tool cmd/mcp serves. This is the public contract an agent's config
// relies on, so the test spells the set out rather than importing it from the
// package under test. tools/list reports them sorted by name, which is what
// this list is — the order tools are registered in is not part of the wire.
var wantTools = []string{
	"ragota_context",
	"ragota_neighbors",
	"ragota_path",
	"ragota_references",
	"ragota_search",
	"ragota_services",
	"ragota_status",
	"ragota_symbol",
	"ragota_topics",
	"ragota_trace",
}

func TestEndToEnd(t *testing.T) {
	bins := buildBinaries(t)
	fx := writeFixture(t)
	work := t.TempDir()
	port := freePort(t)
	cfg := writeConfig(t, work, port)

	api := client.New(fmt.Sprintf("http://127.0.0.1:%d", port))
	ctx := context.Background()

	// --- Phase 1: the whole estate ---
	srv := startServer(t, bins.server, cfg, fx.root, filepath.Join(work, "server-1.log"))
	waitHealthy(t, api)
	repos := waitRepos(t, api, func(rs []*client.Repo) error {
		return indexed(rs, 2, 2)
	})
	payments, gateway := repoNamed(t, repos, "payments"), repoNamed(t, repos, "gateway")

	t.Run("search answers the question with the payments repository", func(t *testing.T) {
		res, err := api.Search(ctx, &client.SearchRequest{Query: chargeQuestion, Limit: 5, Snippet: client.SnippetNone})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if !hitsRepo(res.Hits, payments.ID) {
			t.Fatalf("no hit from payments in %s", describeHits(res.Hits))
		}
	})

	t.Run("symbol lookup finds the definition", func(t *testing.T) {
		res, err := api.Symbol(ctx, &client.SymbolRequest{Symbol: "CaptureCharge"})
		if err != nil {
			t.Fatalf("symbol: %v", err)
		}
		if len(res.Symbols) == 0 {
			t.Fatal("no symbols for CaptureCharge")
		}
		s := res.Symbols[0]
		if s.RepoID != payments.ID || !strings.HasSuffix(s.FilePath, "main.go") || s.StartLine == 0 {
			t.Fatalf("unexpected definition: %+v", s)
		}
	})

	t.Run("the service graph joins the two repositories", func(t *testing.T) {
		res, err := api.Services(ctx, &client.ServicesRequest{})
		if err != nil {
			t.Fatalf("services: %v", err)
		}
		if len(res.Services) < 2 {
			t.Fatalf("want at least 2 services, got %d", len(res.Services))
		}
		for _, l := range res.Links {
			if l.Kind == "http_call" && strings.Contains(l.Via, "POST /charges") {
				return
			}
		}
		t.Fatalf("no http_call link over POST /charges in %d links", len(res.Links))
	})

	t.Run("mcp serves the tool set and answers through it", func(t *testing.T) {
		sess := connectMCP(t, bins.mcp, port)

		tools, err := sess.ListTools(ctx, nil)
		if err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		var names []string
		for _, tool := range tools.Tools {
			names = append(names, tool.Name)
		}
		if got, want := fmt.Sprint(names), fmt.Sprint(wantTools); got != want {
			t.Fatalf("tool set drifted:\n got %s\nwant %s", got, want)
		}

		status := callText(t, sess, "ragota_status", nil)
		if !strings.Contains(status, "payments") || !strings.Contains(status, "gateway") {
			t.Fatalf("ragota_status names neither repository:\n%s", status)
		}
		if !strings.Contains(status, "2 in the working set") {
			t.Fatalf("ragota_status does not report the working set:\n%s", status)
		}

		search := callText(t, sess, "ragota_search", map[string]any{"query": chargeQuestion})
		if !strings.Contains(search, "CaptureCharge") && !strings.Contains(search, "payments") {
			t.Fatalf("ragota_search does not reach the payments code:\n%s", search)
		}

		symbol := callText(t, sess, "ragota_symbol", map[string]any{"symbol": "CaptureCharge"})
		if !strings.Contains(symbol, "main.go") {
			t.Fatalf("ragota_symbol finds no definition:\n%s", symbol)
		}

		routes := callText(t, sess, "ragota_symbol", map[string]any{"kind": "http_route", "repo": "payments"})
		if !strings.Contains(routes, "/charges") {
			t.Fatalf("enumerating http_route misses /charges:\n%s", routes)
		}
	})

	stopServer(t, srv)

	// --- Phase 2: a narrow --source leaves payments dormant ---
	srv = startServer(t, bins.server, cfg, fx.gateway, filepath.Join(work, "server-2.log"))
	waitHealthy(t, api)
	waitRepos(t, api, func(rs []*client.Repo) error {
		return indexed(rs, 2, 1)
	})

	t.Run("a dormant repository is out of the default answer", func(t *testing.T) {
		res, err := api.Search(ctx, &client.SearchRequest{Query: chargeQuestion, Limit: 5, Snippet: client.SnippetNone})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if hitsRepo(res.Hits, payments.ID) {
			t.Fatalf("dormant payments leaked into the default scope: %s", describeHits(res.Hits))
		}

		named, err := api.Search(ctx, &client.SearchRequest{Query: chargeQuestion, Repos: []string{payments.ID}, Limit: 5, Snippet: client.SnippetNone})
		if err != nil {
			t.Fatalf("search with repos: %v", err)
		}
		if !hitsRepo(named.Hits, payments.ID) {
			t.Fatalf("naming the dormant repository must reach it: %s", describeHits(named.Hits))
		}
	})

	t.Run("mcp reports the narrowed working set", func(t *testing.T) {
		sess := connectMCP(t, bins.mcp, port)

		status := callText(t, sess, "ragota_status", nil)
		if !strings.Contains(status, "1 in the working set") {
			t.Fatalf("ragota_status does not report the narrowed set:\n%s", status)
		}

		search := callText(t, sess, "ragota_search", map[string]any{"query": chargeQuestion})
		if strings.Contains(search, "CaptureCharge") {
			t.Fatalf("dormant payments leaked through mcp:\n%s", search)
		}
	})

	stopServer(t, srv)

	// --- Phase 3: the wide --source brings payments back ---
	srv = startServer(t, bins.server, cfg, fx.root, filepath.Join(work, "server-3.log"))
	waitHealthy(t, api)
	waitRepos(t, api, func(rs []*client.Repo) error {
		return indexed(rs, 2, 2)
	})

	t.Run("returning to the wide source revives the answer", func(t *testing.T) {
		res, err := api.Search(ctx, &client.SearchRequest{Query: chargeQuestion, Limit: 5, Snippet: client.SnippetNone})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if !hitsRepo(res.Hits, payments.ID) {
			t.Fatalf("payments did not come back: %s", describeHits(res.Hits))
		}
	})

	stopServer(t, srv)
	_ = gateway
}

// --- binaries ---

type binaries struct {
	server string
	mcp    string
}

// buildBinaries compiles both shipped commands the way a release would, so
// the test exercises the artifacts and not just the packages.
func buildBinaries(t *testing.T) binaries {
	t.Helper()
	dir := t.TempDir()
	root := repoRoot(t)
	b := binaries{
		server: filepath.Join(dir, "ragota"),
		mcp:    filepath.Join(dir, "ragota-mcp"),
	}
	for out, pkg := range map[string]string{b.server: "./cmd/server", b.mcp: "./cmd/mcp"} {
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Dir = root
		if msg, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", pkg, err, msg)
		}
	}
	return b
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller information")
	}
	return filepath.Dir(filepath.Dir(file))
}

// --- server lifecycle ---

func writeConfig(t *testing.T, work string, port int) string {
	t.Helper()
	cfg := fmt.Sprintf(`server:
  host: 127.0.0.1
  port: %d
  auth: {type: none}
  read_timeout_seconds: 60
  write_timeout_seconds: 300
log: {level: warn, format: text}
storage:
  sqlite: {path: %s, pool_size: 10}
indexes:
  workers: 4
  ast: {enabled: true}
  bm25: {enabled: true, path: %s, no_compact: true}
`, port, filepath.Join(work, "ragota.db"), filepath.Join(work, "bm25"))
	path := filepath.Join(work, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatalf("config: %v", err)
	}
	return path
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func startServer(t *testing.T, bin, cfg, source, logPath string) *exec.Cmd {
	t.Helper()
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("server log: %v", err)
	}
	cmd := exec.Command(bin, "--config", cfg, "--source", source, "run")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
		logFile.Close()
		if t.Failed() {
			if body, err := os.ReadFile(logPath); err == nil && len(body) > 0 {
				t.Logf("server log %s:\n%s", filepath.Base(logPath), tail(string(body), 40))
			}
		}
	})
	return cmd
}

// stopServer asks politely first: SIGTERM is the path the server's own
// shutdown handling owns, and a test that only ever kills would never notice
// that path wedging.
func stopServer(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal server: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("server did not exit within 20s of SIGTERM")
	}
}

// --- polling ---

func waitHealthy(t *testing.T, api *client.Client) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := api.Health(ctx)
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never became healthy: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitRepos polls the repository list until want stops objecting. Indexing the
// fixture takes moments; the generous deadline is for the slowest CI disk, not
// for the common case.
func waitRepos(t *testing.T, api *client.Client, want func([]*client.Repo) error) []*client.Repo {
	t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	var lastErr error
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		repos, err := api.ListRepos(ctx)
		cancel()
		if err == nil {
			if lastErr = want(repos); lastErr == nil {
				return repos
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			t.Fatalf("repositories never reached the wanted state: %v", lastErr)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// indexed objects until the estate has total repositories, active of them in
// the working set, and every active one idle with a finished index pass.
func indexed(rs []*client.Repo, total, active int) error {
	if len(rs) != total {
		return fmt.Errorf("have %d repositories, want %d", len(rs), total)
	}
	got := 0
	for _, r := range rs {
		if !r.Active {
			continue
		}
		got++
		if r.Status != "idle" || r.IndexedAt == 0 {
			return fmt.Errorf("%s is %s (indexed_at %d)", r.Name, r.Status, r.IndexedAt)
		}
	}
	if got != active {
		return fmt.Errorf("have %d active repositories, want %d", got, active)
	}
	return nil
}

func repoNamed(t *testing.T, rs []*client.Repo, name string) *client.Repo {
	t.Helper()
	for _, r := range rs {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no repository named %s", name)
	return nil
}

// --- assertions ---

func hitsRepo(hits []*client.SearchHit, repoID string) bool {
	for _, h := range hits {
		if h.RepoID == repoID {
			return true
		}
	}
	return false
}

func describeHits(hits []*client.SearchHit) string {
	if len(hits) == 0 {
		return "0 hits"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d hits:", len(hits))
	for _, h := range hits {
		fmt.Fprintf(&b, " [%s %s:%d]", h.RepoID, h.FilePath, h.Line)
	}
	return b.String()
}

// --- mcp ---

// connectMCP launches cmd/mcp exactly as an MCP client does: a subprocess
// speaking the protocol on stdio, configured through the environment.
func connectMCP(t *testing.T, bin string, port int) *mcp.ClientSession {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), fmt.Sprintf("RAGOTA_URL=http://127.0.0.1:%d", port))
	cmd.Stderr = os.Stderr

	cl := mcp.NewClient(&mcp.Implementation{Name: "ragota-e2e", Version: "test"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, err := cl.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect mcp: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func callText(t *testing.T, sess *mcp.ClientSession, tool string, args map[string]any) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	if res.IsError {
		t.Fatalf("%s answered an error: %s", tool, b.String())
	}
	return b.String()
}

func tail(s string, lines int) string {
	all := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}
