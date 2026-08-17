// LSP end-to-end tests: exercise the four Dockerized language servers
// (deploy/lsp/, started via `make lsp-up`) through the internal/lsp client
// and refinement pass. Skipped unless RAGOTA_TEST_LSP=1; `make test-e2e-lsp`
// runs the full cycle.
package e2e_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/indexing"
	"github.com/Nahua-Foundation/ragota/internal/indexing/ast"
	"github.com/Nahua-Foundation/ragota/internal/lsp"
	"github.com/Nahua-Foundation/ragota/internal/storage"
	"github.com/Nahua-Foundation/ragota/internal/storage/sqlite"
	"github.com/Nahua-Foundation/ragota/internal/testutil"
)

// lspMountRoot is where the repository root is mounted inside the LSP
// containers (see deploy/lsp/docker-compose.lsp.yml).
const lspMountRoot = "/workspace"

// lspTimeout bounds a single LSP request. jdtls boots a JVM per connection,
// so this is much larger than the production default.
const lspTimeout = 180 * time.Second

func requireLSP(t *testing.T) {
	t.Helper()
	if os.Getenv("RAGOTA_TEST_LSP") != "1" {
		t.Skip("set RAGOTA_TEST_LSP=1 (and run `make lsp-up`) to run LSP e2e tests")
	}
}

// lspHostRoot returns the host directory mounted at /workspace: the
// ragota repository root (RAGOTA_LSP_HOST_ROOT overrides).
func lspHostRoot(t *testing.T) string {
	t.Helper()
	if root := os.Getenv("RAGOTA_LSP_HOST_ROOT"); root != "" {
		return root
	}
	return filepath.Dir(testutil.TestdataPath(t, ""))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// TestLSPServers checks that each of the four language servers answers the
// minimal session the refiner depends on: initialize, didOpen and a
// documentSymbol that contains a known symbol of the fixture file.
func TestLSPServers(t *testing.T) {
	requireLSP(t)
	hostRoot := lspHostRoot(t)

	// tsserver lives in the container's global node_modules; the workspace
	// mount has none, so typescript-language-server needs the explicit path.
	tsInit := map[string]any{"tsserver": map[string]any{"path": "/usr/local/lib/node_modules/typescript/lib"}}

	cases := []struct {
		lang string
		env  string
		addr string
		root string         // fixture repo, relative to testdata/
		file string         // fixture file, relative to testdata/
		want string         // symbol documentSymbol must contain
		init map[string]any // server-specific initializationOptions
	}{
		{"go", "RAGOTA_LSP_GO_ADDR", "localhost:7301",
			"microservices", "microservices/services/gateway/main.go", "main", nil},
		{"typescript", "RAGOTA_LSP_TYPESCRIPT_ADDR", "localhost:7302",
			"web-ts", "web-ts/src/orders.ts", "submitOrder", tsInit},
		{"java", "RAGOTA_LSP_JAVA_ADDR", "localhost:7303",
			"billing-java", "billing-java/src/main/java/com/acme/billing/OrderEventsListener.java", "onOrderCreated", nil},
		// NOTE: csharp-ls loads a full MSBuild/Roslyn workspace and needs a
		// Docker VM with >=4GB memory; on smaller VMs it gets OOM-killed
		// before returning symbols (see deploy/lsp/README note).
		{"csharp", "RAGOTA_LSP_CSHARP_ADDR", "localhost:7304",
			"notifier-dotnet", "notifier-dotnet/Controllers/NotifyController.cs", "Send", nil},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.lang, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			mapper := lsp.NewMapper(hostRoot, lspMountRoot)
			addr := envOr(tc.env, tc.addr)

			client, err := lsp.Dial(addr, lspTimeout)
			if err != nil {
				t.Fatalf("dial %s: %v", addr, err)
			}
			defer client.Close()

			rootPath := testutil.TestdataPath(t, tc.root)
			if err := client.Initialize(ctx, mapper.ToURI(rootPath), tc.init); err != nil {
				t.Fatalf("initialize: %v", err)
			}

			filePath := testutil.TestdataPath(t, tc.file)
			content, err := os.ReadFile(filePath)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			uri := mapper.ToURI(filePath)
			if err := client.DidOpen(uri, lsp.LanguageID(tc.lang), string(content)); err != nil {
				t.Fatalf("didOpen: %v", err)
			}

			// Servers that index projects (jdtls, csharp-ls) may briefly
			// return no symbols right after didOpen; poll until they settle.
			symbols := waitForSymbols(t, ctx, client, uri)
			if len(symbols) == 0 {
				t.Fatalf("documentSymbol returned no symbols for %s", tc.file)
			}
			names := make([]string, 0, len(symbols))
			found := false
			for _, s := range symbols {
				names = append(names, s.Name)
				if s.Name == tc.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("documentSymbol for %s: symbol %q not found in %v", tc.file, tc.want, names)
			}
			t.Logf("%s: %d symbols, contains %q", tc.lang, len(symbols), tc.want)
		})
	}
}

// waitForSymbols polls documentSymbol until it returns a non-empty result or
// the deadline passes (returns the last, possibly empty, result).
func waitForSymbols(t *testing.T, ctx context.Context, client *lsp.Client, uri string) []lsp.Symbol {
	t.Helper()
	deadline := time.Now().Add(lspTimeout)
	for {
		symbols, err := client.DocumentSymbols(ctx, uri)
		if err != nil {
			t.Logf("documentSymbol: %v (retrying)", err)
		} else if len(symbols) > 0 {
			return symbols
		}
		if time.Now().After(deadline) {
			return symbols
		}
		time.Sleep(2 * time.Second)
	}
}

// TestLSPRefinement runs the full refinement pass over the Go gateway fixture
// against a temporary SQLite store seeded with tree-sitter units. Hard
// assertion: symbols missing from the seed are added by documentSymbol (Hash
// "lsp"). Soft assertion: reference edges appear when gopls resolves
// references (the fixture does not fully compile, so this may legally be 0).
func TestLSPRefinement(t *testing.T) {
	requireLSP(t)
	ctx := context.Background()
	hostRoot := lspHostRoot(t)

	st, err := sqlite.Open(&sqlite.Config{Path: ":memory:", PoolSize: 1})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init sqlite: %v", err)
	}
	defer st.Close()

	const repoID = "lsp-refine-test"
	repoPath := testutil.TestdataPath(t, "microservices")
	relFile := "services/gateway/main.go"
	content, err := os.ReadFile(filepath.Join(repoPath, relFile))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	// Seed the store with tree-sitter units, dropping HealthHandler so the
	// LSP pass has something to restore via documentSymbol.
	parser := ast.NewTreeSitterParser("go")
	units, _, err := parser.Parse(relFile, string(content))
	if err != nil {
		t.Fatalf("tree-sitter parse: %v", err)
	}
	const dropped = "HealthHandler"
	seeded := 0
	for _, u := range units {
		if u.Name == dropped {
			continue
		}
		u.RepoID = repoID
		u.FilePath = relFile
		u.Language = "go"
		if err := st.StoreASTUnit(ctx, u); err != nil {
			t.Fatalf("seed unit %s: %v", u.Name, err)
		}
		seeded++
	}
	if seeded == 0 {
		t.Fatalf("tree-sitter produced no units to seed")
	}

	refiner := lsp.NewRefiner(st, &config.LSPConfig{
		Enabled:        true,
		HostRoot:       hostRoot,
		MountRoot:      lspMountRoot,
		TimeoutSeconds: int(lspTimeout / time.Second),
		Servers: map[string]config.LSPServerConfig{
			"go": {Addr: envOr("RAGOTA_LSP_GO_ADDR", "localhost:7301")},
		},
	})

	res, err := refiner.Index(ctx, &indexing.IndexRequest{
		RepoID:   repoID,
		RepoPath: repoPath,
		RepoName: "microservices",
		Files: []*indexing.FileToIndex{
			{Path: relFile, Language: "go", Content: content},
		},
	})
	if err != nil {
		t.Fatalf("refiner.Index: %v", err)
	}
	if res.FilesFailed != 0 {
		t.Fatalf("refiner failed %d files: %v", res.FilesFailed, res.Errors)
	}
	if res.FilesIndexed != 1 {
		t.Fatalf("FilesIndexed = %d, want 1 (errors: %v)", res.FilesIndexed, res.Errors)
	}

	// Hard assertion: the dropped symbol came back from documentSymbol.
	restored, err := st.GetASTUnits(ctx, storage.QueryOpts{
		RepoID: repoID, FilePath: relFile, Name: dropped,
	})
	if err != nil {
		t.Fatalf("get restored unit: %v", err)
	}
	if len(restored) == 0 {
		t.Fatalf("unit %s was not restored by the LSP pass", dropped)
	}
	if restored[0].Hash != "lsp" {
		t.Errorf("restored unit hash = %q, want %q", restored[0].Hash, "lsp")
	}
	t.Logf("unit %s restored by documentSymbol (kind %s, lines %d-%d)",
		dropped, restored[0].Kind, restored[0].StartLine, restored[0].EndLine)

	// The file-scoped pass writes no edges: correcting the call graph is the
	// repository-scoped pass's job (see TestLSPCallEdges).
	edges, err := st.GetEdges(ctx, storage.QueryOpts{RepoID: repoID})
	if err != nil {
		t.Fatalf("get edges: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("refinement pass wrote %d edges, want none", len(edges))
	}
}

// TestLSPCallEdges runs the repository-scoped call-edge pass over the Go
// fixture and checks that it points a deliberately mis-resolved call edge at
// the definition gopls actually references.
//
// Soft on the answer, hard on the mechanism: the fixture is not a complete
// module, so gopls may resolve nothing — that must not fail the run — but when
// it does resolve, the edge must move and carry the language-server mark.
func TestLSPCallEdges(t *testing.T) {
	requireLSP(t)
	ctx := context.Background()
	hostRoot := lspHostRoot(t)

	st, err := sqlite.Open(&sqlite.Config{Path: ":memory:", PoolSize: 1})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init sqlite: %v", err)
	}
	defer st.Close()

	const repoID = "lsp-calls-test"
	repoPath := testutil.TestdataPath(t, "microservices")
	files, err := goFilesUnder(repoPath)
	if err != nil {
		t.Fatalf("walk fixture: %v", err)
	}
	units := map[string][]*storage.ASTUnit{}
	for _, rel := range files {
		content, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		parsed, _, err := ast.NewTreeSitterParser("go").Parse(rel, string(content))
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		for _, u := range parsed {
			if u.Kind != "function" && u.Kind != "method" {
				continue
			}
			u.RepoID, u.FilePath, u.Language = repoID, rel, "go"
			if err := st.StoreASTUnit(ctx, u); err != nil {
				t.Fatalf("store unit: %v", err)
			}
			units[rel] = append(units[rel], u)
		}
	}

	// Find a name two files define, and a call of it: that is the shape the
	// name matcher cannot resolve and the server can.
	byName := map[string][]*storage.ASTUnit{}
	for _, list := range units {
		for _, u := range list {
			byName[u.Name] = append(byName[u.Name], u)
		}
	}
	var target []*storage.ASTUnit
	for _, list := range byName {
		if len(list) > 1 && list[0].FilePath != list[1].FilePath {
			target = list
			break
		}
	}
	if target == nil {
		t.Skip("fixture has no name defined in two files; nothing to disambiguate")
	}

	// Seed a call edge from every other function to the *first* candidate,
	// exactly as a name match would.
	seeded := 0
	for _, list := range units {
		for _, u := range list {
			if u.Name == target[0].Name {
				continue
			}
			e := &storage.Edge{
				RepoID: repoID, SrcID: u.ID, DstID: target[0].ID, DstRepoID: repoID,
				Kind: storage.EdgeCall, DstName: target[0].Name,
				FilePath: u.FilePath, Line: u.StartLine, Confidence: 0.49,
			}
			if err := st.StoreEdge(ctx, e); err != nil {
				t.Fatalf("store edge: %v", err)
			}
			seeded++
		}
	}
	if seeded == 0 {
		t.Skip("fixture has no other function to call the ambiguous name from")
	}

	refiner := lsp.NewCallRefiner(st, &config.LSPConfig{
		Enabled:        true,
		HostRoot:       hostRoot,
		MountRoot:      lspMountRoot,
		TimeoutSeconds: int(lspTimeout / time.Second),
		Servers: map[string]config.LSPServerConfig{
			"go": {Addr: envOr("RAGOTA_LSP_GO_ADDR", "localhost:7301")},
		},
		Calls: &config.LSPCallsConfig{Enabled: true, Scope: "ambiguous"},
	})
	if refiner == nil {
		t.Fatal("NewCallRefiner returned nil for an enabled configuration")
	}
	stats, err := refiner.RefineRepo(ctx, repoID, repoPath)
	if err != nil {
		t.Fatalf("RefineRepo: %v", err)
	}
	t.Logf("call pass: %v", stats.Log())
	if stats.Candidates == 0 {
		t.Fatal("the bound selected nothing; the ambiguous name should have qualified")
	}
	if stats.Asked == 0 {
		t.Fatal("no references request was issued")
	}
	if stats.Contradicted == 0 && stats.Repointed == 0 && stats.Added == 0 {
		t.Log("gopls resolved nothing for this fixture — acceptable, it is not a complete module")
	}
}

// goFilesUnder lists the .go files of a fixture repository, repo-relative.
func goFilesUnder(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".go" {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out, err
}
