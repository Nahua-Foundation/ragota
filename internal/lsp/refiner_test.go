package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/index"
	"github.com/Nahua-Foundation/ragota/internal/store"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// --- fake language server ---------------------------------------------------

// fakeServer answers the three requests the refiner makes. symbolsPerFile is
// keyed by the URI's base name; a missing entry means "no symbols", which is
// how a server that cannot see the workspace behaves.
type fakeServer struct {
	addr           string
	symbolsPerFile map[string][]map[string]any
	references     map[string][]map[string]any
	// refError, when set, makes textDocument/references answer with a JSON-RPC
	// error instead of a result — a server that is up but cannot answer.
	refError string

	mu       sync.Mutex
	openURIs []string
}

func startFakeServer(t *testing.T, symbols map[string][]map[string]any, refs map[string][]map[string]any) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeServer{addr: ln.Addr().String(), symbolsPerFile: symbols, references: refs}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *fakeServer) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		msg, err := readMessage(r)
		if err != nil {
			return
		}
		if len(msg.ID) == 0 {
			continue // notification
		}
		reply := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(msg.ID)}
		switch msg.Method {
		case "initialize":
			reply["result"] = map[string]any{"capabilities": map[string]any{}}
		case "textDocument/documentSymbol":
			reply["result"] = s.symbolsFor(msg.Params)
		case "textDocument/references":
			if s.refError != "" {
				reply["error"] = map[string]any{"code": -32603, "message": s.refError}
				break
			}
			reply["result"] = s.referencesFor(msg.Params)
		default:
			reply["result"] = nil
		}
		body, err := json.Marshal(reply)
		if err != nil {
			return
		}
		if _, err := fmt.Fprintf(conn, "Content-Length: %d\r\n\r\n%s", len(body), body); err != nil {
			return
		}
	}
}

func (s *fakeServer) uriParam(params json.RawMessage) string {
	var p struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	_ = json.Unmarshal(params, &p)
	return p.TextDocument.URI
}

func (s *fakeServer) symbolsFor(params json.RawMessage) []map[string]any {
	uri := s.uriParam(params)
	s.mu.Lock()
	s.openURIs = append(s.openURIs, uri)
	s.mu.Unlock()
	return s.symbolsPerFile[filepath.Base(uri)]
}

func (s *fakeServer) referencesFor(params json.RawMessage) []map[string]any {
	return s.references[filepath.Base(s.uriParam(params))]
}

func symbol(name string, kind, startLine, endLine int) map[string]any {
	return map[string]any{
		"name": name,
		"kind": kind,
		"range": map[string]any{
			"start": map[string]any{"line": startLine, "character": 5},
			"end":   map[string]any{"line": endLine, "character": 1},
		},
	}
}

// --- fake storage -----------------------------------------------------------

// fakeStorage implements only the methods the refiner touches; the embedded
// nil interface makes any other call panic loudly instead of compiling away.
type fakeStorage struct {
	store.Storage

	units map[string][]*domain.ASTUnit // file path -> units

	mu    sync.Mutex
	edges []*domain.Edge
}

func newFakeStorage() *fakeStorage {
	return &fakeStorage{units: map[string][]*domain.ASTUnit{}}
}

func (f *fakeStorage) GetASTUnits(_ context.Context, opts domain.QueryOpts) ([]*domain.ASTUnit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.units[opts.FilePath], nil
}

func (f *fakeStorage) StoreASTUnit(_ context.Context, u *domain.ASTUnit) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u.ID = fmt.Sprintf("u%d", len(f.units[u.FilePath])+100)
	f.units[u.FilePath] = append(f.units[u.FilePath], u)
	return nil
}

func (f *fakeStorage) StoreEdge(_ context.Context, e *domain.Edge) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edges = append(f.edges, e)
	return nil
}

// --- helpers ----------------------------------------------------------------

func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func goRefiner(t *testing.T, st store.Storage, addr string) *Refiner {
	t.Helper()
	return NewRefiner(st, &config.LSPConfig{
		Enabled:        true,
		TimeoutSeconds: 5,
		Servers:        map[string]config.LSPServerConfig{"go": {Addr: addr}},
	})
}

func indexReq(repoPath string, force bool, paths ...string) *index.IndexRequest {
	req := &index.IndexRequest{RepoID: "r1", RepoPath: repoPath, RepoName: "repo", Force: force}
	for _, p := range paths {
		req.Files = append(req.Files, &index.FileToIndex{Path: p, Language: "go"})
	}
	return req
}

// --- tests ------------------------------------------------------------------

func TestIndex_AddsUnitsTheParsersMissed(t *testing.T) {
	repo := writeRepo(t, map[string]string{
		"a.go": "package main\n\nfunc Alpha() {}\n\nfunc Gamma() {}\n",
	})
	srv := startFakeServer(t, map[string][]map[string]any{
		"a.go": {symbol("Alpha", 12, 2, 2), symbol("Gamma", 12, 4, 4)},
	}, nil)

	st := newFakeStorage()
	st.units["a.go"] = []*domain.ASTUnit{
		{ID: "1", RepoID: "r1", FilePath: "a.go", Kind: "function", Name: "Alpha", StartLine: 3, EndLine: 3},
	}

	res, err := goRefiner(t, st, srv.addr).Index(context.Background(), indexReq(repo, false, "a.go"))
	if err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	if res.FilesIndexed != 1 {
		t.Errorf("FilesIndexed = %d, want 1", res.FilesIndexed)
	}
	var added *domain.ASTUnit
	for _, u := range st.units["a.go"] {
		if u.Name == "Gamma" {
			added = u
		}
	}
	if added == nil {
		t.Fatalf("units = %+v, want the symbol the parser missed to be added", st.units["a.go"])
	}
	if added.Hash != unitHashLSP {
		t.Errorf("added unit hash = %q, want %q so its origin stays visible", added.Hash, unitHashLSP)
	}
	if len(st.edges) != 0 {
		t.Errorf("the file-scoped pass must not write edges, got %+v", st.edges)
	}
}

func TestIndex_LanguageWithoutSymbolsCountsAsFailure(t *testing.T) {
	repo := writeRepo(t, map[string]string{"a.go": "package main\n\nfunc Alpha() {}\n"})
	// A server that sees nothing at the mapped path: the classic wrong
	// host_root/mount_root symptom.
	srv := startFakeServer(t, map[string][]map[string]any{}, nil)

	before := testutil.ToFloat64(lspEmptyLanguages)
	st := newFakeStorage()
	res, err := goRefiner(t, st, srv.addr).Index(context.Background(), indexReq(repo, false, "a.go"))
	if err == nil {
		t.Fatal("expected an error when every language yields no symbols")
	}
	if res.FilesFailed != 1 {
		t.Errorf("FilesFailed = %d, want 1", res.FilesFailed)
	}
	if len(res.Errors) == 0 || !strings.Contains(res.Errors[0], "no symbols") {
		t.Errorf("Errors = %v, want one mentioning the empty result", res.Errors)
	}
	if v := testutil.ToFloat64(lspEmptyLanguages) - before; v != 1 {
		t.Errorf("ragota_lsp_empty_languages delta = %g, want 1", v)
	}
}

func TestIndex_NoServersConfiguredIsAnError(t *testing.T) {
	r := NewRefiner(newFakeStorage(), &config.LSPConfig{Enabled: true})

	res, err := r.Index(context.Background(), indexReq(t.TempDir(), false, "a.go"))
	if err == nil {
		t.Fatal("an enabled pass with no servers must not report success")
	}
	if res.FilesIndexed != 0 {
		t.Errorf("FilesIndexed = %d, want 0", res.FilesIndexed)
	}
}

func TestInit_EmptyServersFails(t *testing.T) {
	r := NewRefiner(newFakeStorage(), &config.LSPConfig{Enabled: true})
	if err := r.Init(context.Background(), nil); err == nil {
		t.Fatal("Init() must reject an empty servers map")
	}
}

func TestInit_ProbesEveryServer(t *testing.T) {
	srv := startFakeServer(t, nil, nil)

	checksBefore := testutil.ToFloat64(lspDialChecks)
	failuresBefore := testutil.ToFloat64(lspDialFailures)
	r := NewRefiner(newFakeStorage(), &config.LSPConfig{
		Enabled:        true,
		TimeoutSeconds: 2,
		Servers: map[string]config.LSPServerConfig{
			"go": {Addr: srv.addr},
			// 127.0.0.1:1 is reserved and refuses connections immediately.
			"java": {Addr: "127.0.0.1:1"},
		},
	})
	if err := r.Init(context.Background(), nil); err != nil {
		t.Fatalf("Init() error = %v (an unreachable server must not fail startup)", err)
	}
	if v := testutil.ToFloat64(lspDialChecks) - checksBefore; v != 2 {
		t.Errorf("ragota_lsp_dial_checks delta = %g, want 2", v)
	}
	if v := testutil.ToFloat64(lspDialFailures) - failuresBefore; v != 1 {
		t.Errorf("ragota_lsp_dial_failures delta = %g, want 1", v)
	}
}
