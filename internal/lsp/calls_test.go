package lsp

import (
	"context"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// --- fake storage -----------------------------------------------------------

// callStore is the slice of storage the call pass uses: two reads (units and
// edges of a repository) and three writes.
type callStore struct {
	store.Storage

	mu    sync.Mutex
	units []*domain.ASTUnit
	edges []*domain.Edge
	next  int
}

func (s *callStore) GetASTUnits(_ context.Context, opts domain.QueryOpts) ([]*domain.ASTUnit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*domain.ASTUnit
	for _, u := range s.units {
		if opts.RepoID != "" && u.RepoID != opts.RepoID {
			continue
		}
		if len(opts.Kinds) > 0 && !containsStr(opts.Kinds, u.Kind) {
			continue
		}
		out = append(out, u)
	}
	return out, nil
}

func (s *callStore) GetEdges(_ context.Context, opts domain.QueryOpts) ([]*domain.Edge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*domain.Edge
	for _, e := range s.edges {
		if opts.RepoID != "" && e.RepoID != opts.RepoID {
			continue
		}
		if opts.Kind != "" && e.Kind != opts.Kind {
			continue
		}
		if len(opts.Kinds) > 0 && !containsStr(opts.Kinds, e.Kind) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *callStore) UpdateEdgeResolution(_ context.Context, edgeID, dstID, dstRepoID string, conf float32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.edges {
		if e.ID == edgeID {
			e.DstID, e.DstRepoID, e.Confidence = dstID, dstRepoID, conf
		}
	}
	return nil
}

func (s *callStore) UpdateEdgeMeta(_ context.Context, edgeID, meta string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.edges {
		if e.ID == edgeID {
			e.Meta = meta
		}
	}
	return nil
}

func (s *callStore) BatchStoreEdges(_ context.Context, edges []*domain.Edge) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range edges {
		s.next++
		e.ID = "new" + strconv.Itoa(s.next)
		s.edges = append(s.edges, e)
	}
	return nil
}

func (s *callStore) edge(id string) *domain.Edge {
	for _, e := range s.edges {
		if e.ID == id {
			return e
		}
	}
	return nil
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// --- helpers ----------------------------------------------------------------

func callRefinerFor(st store.Storage, addr string, calls *config.LSPCallsConfig) *CallRefiner {
	return NewCallRefiner(st, &config.LSPConfig{
		Enabled:        true,
		TimeoutSeconds: 5,
		Servers:        map[string]config.LSPServerConfig{"go": {Addr: addr}},
		Calls:          calls,
	})
}

// refAt builds one textDocument/references location.
func refAt(repo, file string, line0 int) map[string]any {
	return map[string]any{
		"uri": "file://" + filepath.ToSlash(filepath.Join(repo, file)),
		"range": map[string]any{
			"start": map[string]any{"line": line0, "character": 1},
			"end":   map[string]any{"line": line0, "character": 6},
		},
	}
}

func fn(id, file, name, qualified string, start, end int) *domain.ASTUnit {
	return &domain.ASTUnit{
		ID: id, RepoID: "r1", FilePath: file, Language: "go", Kind: "function",
		Name: name, Qualified: qualified, StartLine: start, EndLine: end,
	}
}

// --- tests ------------------------------------------------------------------

// The defect this pass exists for: two definitions share a name, the linker
// had to pick one, and it picked the wrong one. The server knows which.
func TestRefineRepo_MovesAnEdgeToTheDefinitionTheServerNames(t *testing.T) {
	repo := writeRepo(t, map[string]string{
		"a/alpha.go": "package a\n\nfunc Save() {}\n",
		"b/beta.go":  "package b\n\nfunc Save() {}\n",
		"caller.go":  "package main\n\nfunc Run() {\n\tb.Save()\n}\n",
	})
	// The server answers for b/svc.go: its Save is called from caller.go:4.
	srv := startFakeServer(t, nil, map[string][]map[string]any{
		"beta.go": {refAt(repo, "caller.go", 3)},
	})

	st := &callStore{
		units: []*domain.ASTUnit{
			fn("1", "a/alpha.go", "Save", "a.Save", 3, 3),
			fn("2", "b/beta.go", "Save", "b.Save", 3, 3),
			fn("3", "caller.go", "Run", "main.Run", 3, 5),
		},
		edges: []*domain.Edge{{
			ID: "e1", RepoID: "r1", SrcID: "3", DstID: "1", DstRepoID: "r1",
			Kind: store.EdgeCall, DstName: "Save", FilePath: "caller.go", Line: 4,
			Confidence: contract.ConfHeuristic,
		}},
	}

	stats, err := callRefinerFor(st, srv.addr, &config.LSPCallsConfig{Enabled: true, Scope: "ambiguous"}).
		RefineRepo(context.Background(), "r1", repo)
	if err != nil {
		t.Fatalf("RefineRepo() error = %v", err)
	}
	e := st.edge("e1")
	if e.DstID != "2" {
		t.Errorf("edge points at %q, want the definition the server referenced (2)", e.DstID)
	}
	if e.Confidence != contract.ConfExact {
		t.Errorf("confidence = %v, want ConfExact for a server-verified edge", e.Confidence)
	}
	if metaField(e.Meta, metaKeySource) != callEdgeSource {
		t.Errorf("meta = %q, want the resolution marked %q", e.Meta, callEdgeSource)
	}
	if stats.Repointed != 1 {
		t.Errorf("Repointed = %d, want 1 (stats: %v)", stats.Repointed, stats.Log())
	}
}

// A call the extractor never recorded — inside a chained expression, a lambda,
// a generated wrapper — is the other half: there is no edge to correct.
func TestRefineRepo_AddsACallSiteTheParserMissed(t *testing.T) {
	repo := writeRepo(t, map[string]string{
		"a/alpha.go": "package a\n\nfunc Save() {}\n",
		"b/beta.go":  "package b\n\nfunc Save() {}\n",
		"caller.go":  "package main\n\nfunc Run() {\n\tdo().Save()\n}\n",
	})
	srv := startFakeServer(t, nil, map[string][]map[string]any{
		"beta.go": {refAt(repo, "caller.go", 3)},
	})

	st := &callStore{
		units: []*domain.ASTUnit{
			fn("1", "a/alpha.go", "Save", "a.Save", 3, 3),
			fn("2", "b/beta.go", "Save", "b.Save", 3, 3),
			fn("3", "caller.go", "Run", "main.Run", 3, 5),
		},
		// Something has to name Save for the ambiguous tier to select it; this
		// edge sits in another file, so it is not the call site under test.
		edges: []*domain.Edge{{
			ID: "e1", RepoID: "r1", SrcID: "1", DstID: "1", DstRepoID: "r1",
			Kind: store.EdgeCall, DstName: "Save", FilePath: "a/alpha.go", Line: 3,
			Confidence: contract.ConfHeuristic,
		}},
	}

	stats, err := callRefinerFor(st, srv.addr, &config.LSPCallsConfig{Enabled: true, Scope: "ambiguous"}).
		RefineRepo(context.Background(), "r1", repo)
	if err != nil {
		t.Fatalf("RefineRepo() error = %v", err)
	}
	if stats.Added == 0 {
		t.Fatalf("Added = 0, want the missing call site to become an edge (stats: %v)", stats.Log())
	}
	var got *domain.Edge
	for _, e := range st.edges {
		if e.FilePath == "caller.go" {
			got = e
		}
	}
	if got == nil {
		t.Fatalf("edges = %+v, want one at caller.go", st.edges)
	}
	if got.Kind != store.EdgeCall || got.SrcID != "3" || got.Line != 4 {
		t.Errorf("edge = %+v, want a call from Run at caller.go:4", got)
	}
	if got.Confidence != contract.ConfExact {
		t.Errorf("confidence = %v, want ConfExact", got.Confidence)
	}
}

// A resolution the server's complete reference list does not contain is a name
// match the evidence denies; it must stop claiming to be a call of that target.
func TestRefineRepo_DropsAResolutionTheServerDenies(t *testing.T) {
	repo := writeRepo(t, map[string]string{
		"a/alpha.go": "package a\n\nfunc Save() {}\n",
		"b/beta.go":  "package b\n\nfunc Save() {}\n",
		"caller.go":  "package main\n\nfunc Run() {\n\tb.Save()\n\tb.Save()\n}\n",
	})
	// a.Save is referenced from caller.go:5 — so the server has demonstrably
	// analysed caller.go — but not from :4, where an edge claims it is.
	srv := startFakeServer(t, nil, map[string][]map[string]any{
		"alpha.go": {refAt(repo, "caller.go", 4)},
	})

	st := &callStore{
		units: []*domain.ASTUnit{
			fn("1", "a/alpha.go", "Save", "a.Save", 3, 3),
			fn("2", "b/beta.go", "Save", "b.Save", 3, 3),
			fn("3", "caller.go", "Run", "main.Run", 3, 6),
		},
		edges: []*domain.Edge{{
			ID: "e1", RepoID: "r1", SrcID: "3", DstID: "1", DstRepoID: "r1",
			Kind: store.EdgeCall, DstName: "Save", FilePath: "caller.go", Line: 1,
			Confidence: contract.ConfHeuristic,
		}},
	}

	stats, err := callRefinerFor(st, srv.addr, &config.LSPCallsConfig{Enabled: true, Scope: "ambiguous"}).
		RefineRepo(context.Background(), "r1", repo)
	if err != nil {
		t.Fatalf("RefineRepo() error = %v", err)
	}
	if stats.Contradicted != 1 {
		t.Errorf("Contradicted = %d, want 1 (stats: %v)", stats.Contradicted, stats.Log())
	}
	e := st.edge("e1")
	if e.DstID != "" {
		t.Errorf("edge still resolves to %q; a denied resolution must be dropped", e.DstID)
	}
	if e.Confidence != contract.ConfWeak {
		t.Errorf("confidence = %v, want ConfWeak", e.Confidence)
	}
}

// The defect this guard exists for: a server that cannot resolve a symbol
// answers exactly like one that found no references. Reading the empty answer
// as "every recorded caller is wrong" unresolved 44 of petclinic's 144 resolved
// call edges, two of them correct.
func TestRefineRepo_AnEmptyAnswerDeniesNothing(t *testing.T) {
	repo := writeRepo(t, map[string]string{
		"a/alpha.go": "package a\n\nfunc Save() {}\n",
		"b/beta.go":  "package b\n\nfunc Save() {}\n",
		"caller.go":  "package main\n\nfunc Run() {\n\tb.Save()\n}\n",
	})
	srv := startFakeServer(t, nil, map[string][]map[string]any{}) // resolves nothing

	st := &callStore{
		units: []*domain.ASTUnit{
			fn("1", "a/alpha.go", "Save", "a.Save", 3, 3),
			fn("2", "b/beta.go", "Save", "b.Save", 3, 3),
			fn("3", "caller.go", "Run", "main.Run", 3, 5),
		},
		edges: []*domain.Edge{{
			ID: "e1", RepoID: "r1", SrcID: "3", DstID: "1", DstRepoID: "r1",
			Kind: store.EdgeCall, DstName: "Save", FilePath: "caller.go", Line: 4,
			Confidence: contract.ConfHeuristic,
		}},
	}

	stats, err := callRefinerFor(st, srv.addr, &config.LSPCallsConfig{Enabled: true, Scope: "ambiguous"}).
		RefineRepo(context.Background(), "r1", repo)
	if err != nil {
		t.Fatalf("RefineRepo() error = %v", err)
	}
	if stats.Contradicted != 0 {
		t.Errorf("Contradicted = %d, want 0 (stats: %v)", stats.Contradicted, stats.Log())
	}
	if e := st.edge("e1"); e.DstID != "1" || e.Confidence != contract.ConfHeuristic {
		t.Errorf("edge = %+v, want the name-matched resolution left exactly as it was", e)
	}
}

// The pass must be a no-op wherever the language server cannot answer. This is
// the failure mode that would make it dangerous to enable: a server that is
// slow, half-loaded, unconfigured for a language or simply absent must leave
// the name-based graph exactly as the linker built it — never a downgraded or
// deleted version of it.
func TestRefineRepo_ServerTroubleNeverDamagesTheGraph(t *testing.T) {
	repo := writeRepo(t, map[string]string{
		"a/alpha.go": "package a\n\nfunc Save() {}\n",
		"b/beta.go":  "package b\n\nfunc Save() {}\n",
		"caller.go":  "package main\n\nfunc Run() {\n\tb.Save()\n}\n",
	})

	// The state the linker left behind, which every case below must preserve.
	seed := func() *callStore {
		return &callStore{
			units: []*domain.ASTUnit{
				fn("1", "a/alpha.go", "Save", "a.Save", 3, 3),
				fn("2", "b/beta.go", "Save", "b.Save", 3, 3),
				fn("3", "caller.go", "Run", "main.Run", 3, 5),
			},
			edges: []*domain.Edge{{
				ID: "e1", RepoID: "r1", SrcID: "3", DstID: "1", DstRepoID: "r1",
				Kind: store.EdgeCall, DstName: "Save", FilePath: "caller.go", Line: 4,
				Confidence: contract.ConfHeuristic, Meta: `{"receiver":"b"}`,
			}},
		}
	}

	cases := []struct {
		name    string
		addr    func(t *testing.T) string
		wantErr bool
	}{
		{
			// Up, but answering every references request with an error.
			name: "references errors",
			addr: func(t *testing.T) string {
				srv := startFakeServer(t, nil, nil)
				srv.refError = "Internal Error - project not loaded"
				return srv.addr
			},
		},
		{
			// Up, answering, but resolving nothing (an unloaded workspace).
			name: "references empty",
			addr: func(t *testing.T) string {
				return startFakeServer(t, nil, map[string][]map[string]any{}).addr
			},
		},
		{
			// Not up at all: 127.0.0.1:1 refuses connections immediately.
			name:    "server unreachable",
			addr:    func(*testing.T) string { return "127.0.0.1:1" },
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := seed()
			before := *st.edge("e1")
			stats, err := callRefinerFor(st, tc.addr(t), &config.LSPCallsConfig{Enabled: true, Scope: "ambiguous"}).
				RefineRepo(context.Background(), "r1", repo)
			if tc.wantErr && err == nil {
				t.Error("an unusable server must be reported, not passed off as a clean pass")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("RefineRepo() error = %v", err)
			}
			if got := *st.edge("e1"); got != before {
				t.Errorf("edge = %+v, want it untouched at %+v", got, before)
			}
			if len(st.edges) != 1 {
				t.Errorf("edges = %d, want no edge invented from an answer that never came", len(st.edges))
			}
			if stats.Contradicted != 0 || stats.Repointed != 0 || stats.Added != 0 || stats.Confirmed != 0 {
				t.Errorf("stats claim work that no answer supports: %v", stats.Log())
			}
		})
	}
}

// A language with no configured server is not a language this pass has an
// opinion about: its definitions are never selected, so nothing about them can
// be confirmed, moved or denied.
func TestSelectCandidates_OnlyLanguagesWithAServer(t *testing.T) {
	g := &repoGraph{
		nameCount: map[string]int{"Shared": 2},
		called:    map[string]bool{"Shared": true},
		boundary:  map[string]bool{"p1": true},
		byFile:    map[string][]*domain.ASTUnit{},
	}
	php := fn("p1", "svc.php", "Shared", "Svc.Shared", 1, 2)
	php.Language = "php" // no language server exists for it here
	g.units = []*domain.ASTUnit{
		fn("g1", "a.go", "Shared", "a.Shared", 1, 2),
		php,
	}
	got := names(callRefinerFor(nil, "", &config.LSPCallsConfig{Enabled: true}).selectCandidates(g))
	if strings.Join(got, ",") != "a.Shared" {
		t.Errorf("selectCandidates() = %v, want only the language with a configured server", got)
	}
}

// A file the server never resolved anything in may not be contradicted either:
// one module failing to load must not unresolve the graph it appears in.
func TestRefineRepo_AnUnanalysedFileIsNotContradicted(t *testing.T) {
	repo := writeRepo(t, map[string]string{
		"a/alpha.go": "package a\n\nfunc Save() {}\n",
		"b/beta.go":  "package b\n\nfunc Save() {}\n",
		"seen.go":    "package main\n\nfunc Seen() {\n\ta.Save()\n}\n",
		"unseen.go":  "package other\n\nfunc Unseen() {\n\ta.Save()\n}\n",
	})
	srv := startFakeServer(t, nil, map[string][]map[string]any{
		"alpha.go": {refAt(repo, "seen.go", 3)},
	})

	st := &callStore{
		units: []*domain.ASTUnit{
			fn("1", "a/alpha.go", "Save", "a.Save", 3, 3),
			fn("2", "b/beta.go", "Save", "b.Save", 3, 3),
			fn("3", "seen.go", "Seen", "main.Seen", 3, 5),
			fn("4", "unseen.go", "Unseen", "other.Unseen", 3, 5),
		},
		edges: []*domain.Edge{
			{ID: "e1", RepoID: "r1", SrcID: "3", DstID: "1", DstRepoID: "r1",
				Kind: store.EdgeCall, DstName: "Save", FilePath: "seen.go", Line: 4,
				Confidence: contract.ConfHeuristic},
			{ID: "e2", RepoID: "r1", SrcID: "4", DstID: "1", DstRepoID: "r1",
				Kind: store.EdgeCall, DstName: "Save", FilePath: "unseen.go", Line: 4,
				Confidence: contract.ConfHeuristic},
		},
	}

	stats, err := callRefinerFor(st, srv.addr, &config.LSPCallsConfig{Enabled: true, Scope: "ambiguous"}).
		RefineRepo(context.Background(), "r1", repo)
	if err != nil {
		t.Fatalf("RefineRepo() error = %v", err)
	}
	if stats.Contradicted != 0 {
		t.Errorf("Contradicted = %d, want 0: unseen.go was never resolved in (stats: %v)", stats.Contradicted, stats.Log())
	}
	if e := st.edge("e2"); e.DstID != "1" {
		t.Errorf("edge in an unanalysed file = %+v, want its resolution untouched", e)
	}
	if e := st.edge("e1"); e.Confidence != contract.ConfExact {
		t.Errorf("edge in an analysed file = %+v, want it confirmed", e)
	}
}

// The bound is the whole reason the pass is affordable, so what it selects is
// worth asserting on its own.
func TestSelectCandidates_Bound(t *testing.T) {
	g := &repoGraph{
		nameCount: map[string]int{"Shared": 2, "Lonely": 1, "Handled": 1, "Untouched": 3},
		called:    map[string]bool{"Shared": true, "Untouched": false},
		boundary:  map[string]bool{"h1": true},
		byFile:    map[string][]*domain.ASTUnit{},
	}
	g.units = []*domain.ASTUnit{
		fn("s1", "a.go", "Shared", "a.Shared", 1, 2),      // ambiguous and called
		fn("s2", "b.go", "Shared", "b.Shared", 1, 2),      // ambiguous and called
		fn("l1", "c.go", "Lonely", "c.Lonely", 1, 2),      // unique name: nothing to resolve
		fn("h1", "d.go", "Handled", "d.Handled", 1, 2),    // contract boundary
		fn("u1", "e.go", "Untouched", "e.U", 1, 2),        // ambiguous but never called
		fn("t1", "a_test.go", "Shared", "t.Shared", 1, 2), // test scaffolding
	}

	got := names(callRefinerFor(nil, "", &config.LSPCallsConfig{Enabled: true}).selectCandidates(g))
	want := []string{"d.Handled", "a.Shared", "b.Shared"} // boundary first, then by qualified name
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("selectCandidates() = %v, want %v", got, want)
	}

	only := callRefinerFor(nil, "", &config.LSPCallsConfig{Enabled: true, Scope: "boundary"})
	if got := names(only.selectCandidates(g)); strings.Join(got, ",") != "d.Handled" {
		t.Errorf("boundary scope = %v, want only the boundary symbol", got)
	}

	capped := callRefinerFor(nil, "", &config.LSPCallsConfig{Enabled: true, MaxSymbols: 2})
	if n := len(capped.selectCandidates(g)); n != 3 {
		t.Errorf("selectCandidates() = %d, want the full list (the budget is applied by RefineRepo)", n)
	}
}

func names(units []*domain.ASTUnit) []string {
	out := make([]string, len(units))
	for i, u := range units {
		out[i] = u.Qualified
	}
	return out
}

func TestMatchCallEdge_ClosestLineWithinTolerance(t *testing.T) {
	edges := []*domain.Edge{
		{ID: "far", FilePath: "a.go", Line: 10},
		{ID: "near", FilePath: "a.go", Line: 6},
		{ID: "other", FilePath: "b.go", Line: 5},
	}
	if got := matchCallEdge(edges, "a.go", 5); got == nil || got.ID != "near" {
		t.Errorf("matchCallEdge() = %v, want the edge one line away", got)
	}
	if got := matchCallEdge(edges, "a.go", 20); got != nil {
		t.Errorf("matchCallEdge() = %v, want nil beyond the tolerance", got)
	}
	if got := matchCallEdge(edges, "c.go", 5); got != nil {
		t.Errorf("matchCallEdge() = %v, want nil in another file", got)
	}
}

func TestNewCallRefiner_OffUnlessAskedFor(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.LSPConfig
	}{
		{"no lsp", nil},
		{"lsp disabled", &config.LSPConfig{Calls: &config.LSPCallsConfig{Enabled: true}}},
		{"calls absent", &config.LSPConfig{Enabled: true}},
		{"calls disabled", &config.LSPConfig{Enabled: true, Calls: &config.LSPCallsConfig{}}},
	}
	for _, tc := range cases {
		if r := NewCallRefiner(&callStore{}, tc.cfg); r != nil {
			t.Errorf("NewCallRefiner(%s) = %v, want nil", tc.name, r)
		}
	}
}

func TestRepoRelative_RejectsWhatIsNotOurs(t *testing.T) {
	m := NewMapper("/host/corpus", "/workspace")
	if got, ok := repoRelative(m, "/host/corpus/argo", "file:///workspace/argo/util/argo.go"); !ok || got != "util/argo.go" {
		t.Errorf("repoRelative() = %q, %v; want util/argo.go, true", got, ok)
	}
	if _, ok := repoRelative(m, "/host/corpus/argo", "file:///workspace/consul/agent/agent.go"); ok {
		t.Error("a file of another repository under the same mount must be rejected")
	}
	if _, ok := repoRelative(m, "/host/corpus/argo", "file:///go/pkg/mod/x/y.go"); ok {
		t.Error("a dependency outside the mount must be rejected")
	}
}

func TestSortedKeys(t *testing.T) {
	got := sortedKeys(map[string]int{"b": 1, "a": 2, "c": 3})
	if !sort.StringsAreSorted(got) || len(got) != 3 {
		t.Errorf("sortedKeys() = %v, want a sorted 3-element list", got)
	}
}
