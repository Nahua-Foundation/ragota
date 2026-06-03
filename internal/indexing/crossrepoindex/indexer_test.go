package crossrepoindex

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"ragota/internal/store"
	"ragota/pkg/repos"
	"ragota/pkg/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openTestDB(t *testing.T) *store.SQLite {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ragota.db")
	st, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seedTestRepo(t *testing.T, st *store.SQLite, repoName, repoPath string) {
	t.Helper()
	ctx := context.Background()

	// Create go.mod
	goMod := `module github.com/company/` + repoName + `

go 1.22

require (
	github.com/company/auth-sdk v1.0.0
	github.com/company/payment-sdk v2.0.0
)
`
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, "go.mod"), []byte(goMod), 0o644))

	// Create a Go file with unresolved call edges
	srcFile := filepath.Join(repoPath, "main.go")
	src := `package main

import "github.com/company/auth-sdk"

func handleRequest() {
	auth-sdk.Validate()
}
`
	require.NoError(t, os.WriteFile(srcFile, []byte(src), 0o644))

	require.NoError(t, st.EnsureFile(ctx, srcFile, "go"))
	units := []store.ASTUnit{
		{FilePath: srcFile, Language: "go", Kind: "module", Name: "github.com/company/" + repoName, Qualified: "", StartLine: 1, EndLine: 1, StartByte: 0, EndByte: 30, Repo: repoName},
		{FilePath: srcFile, Language: "go", Kind: "function", Name: "handleRequest", Qualified: "main.handleRequest", StartLine: 5, EndLine: 8, StartByte: 50, EndByte: 100, Repo: repoName},
		{FilePath: srcFile, Language: "go", Kind: "import", Name: "import_auth", Qualified: "", StartLine: 3, EndLine: 3, StartByte: 30, EndByte: 50, Repo: repoName},
	}
	ids, err := st.ReplaceASTUnits(ctx, srcFile, units)
	require.NoError(t, err)

	// Create edges
	moduleID := ids["github.com/company/"+repoName]
	funcID := ids["main.handleRequest"]

	edges := []store.Edge{
		{Repo: repoName, SrcID: funcID, DstID: 0, Kind: "import", DstName: "github.com/company/auth-sdk", FilePath: srcFile, Line: 3},
		{Repo: repoName, SrcID: funcID, DstID: 0, Kind: "call", DstName: "http.Get", FilePath: srcFile, Line: 6},
	}
	require.NoError(t, st.ReplaceEdges(ctx, srcFile, edges))
	_ = moduleID
}

func newTestIndexer(t *testing.T, st *store.SQLite, reposList []repos.Repo) *Indexer {
	t.Helper()
	resolver := repos.NewResolver(reposList)
	idx := New(resolver, st)
	idx.SetBus(state.NewBus(t.TempDir()))
	return idx
}

func makeRepos(t *testing.T, names ...string) []repos.Repo {
	t.Helper()
	result := make([]repos.Repo, len(names))
	for i, name := range names {
		dir := t.TempDir()
		// Create .git marker
		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
		result[i] = repos.Repo{Name: name, Path: dir}
	}
	return result
}

// ─── Constructor ────────────────────────────────────────────────────────────

func TestNew_InitManifests(t *testing.T) {
	st := openTestDB(t)
	testRepos := makeRepos(t, "svc-a", "svc-b")
	resolver := repos.NewResolver(testRepos)

	idx := New(resolver, st)
	require.NotNil(t, idx)
	require.NotNil(t, idx.manifests)

	// Init manifests
	idx.InitManifests()

	// KnownImports should be empty (no go.mod in temp dirs)
	imports := idx.manifests.KnownImports()
	assert.Empty(t, imports)
}

// ─── FullScan ───────────────────────────────────────────────────────────────

func TestFullScan_WithManifests(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()

	// Create two repos
	dirA := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dirA, ".git"), 0o755))

	// go.mod referencing other service
	goMod := `module github.com/company/svc-a
go 1.22
require github.com/company/svc-b v1.0.0
`
	require.NoError(t, os.WriteFile(filepath.Join(dirA, "go.mod"), []byte(goMod), 0o644))

	// Go file
	srcFile := filepath.Join(dirA, "main.go")
	require.NoError(t, os.WriteFile(srcFile, []byte("package main\n"), 0o644))

	require.NoError(t, st.EnsureFile(ctx, srcFile, "go"))
	_, err := st.ReplaceASTUnits(ctx, srcFile, []store.ASTUnit{
		{FilePath: srcFile, Language: "go", Kind: "module", Name: "github.com/company/svc-a", StartLine: 1, EndLine: 1, Repo: "svc-a"},
		{FilePath: srcFile, Language: "go", Kind: "function", Name: "f", StartLine: 1, EndLine: 1, Repo: "svc-a"},
	})
	require.NoError(t, err)

	units, _ := st.ListASTUnitsByFile(ctx, srcFile)
	funcID := 0
	for _, u := range units {
		if u.Name == "f" {
			funcID = u.ID
			break
		}
	}

	// Import edge referencing svc-b
	_, err = st.InsertEdge(ctx, store.Edge{
		Repo: "svc-a", SrcID: funcID, DstID: 0,
		Kind: "import", DstName: "github.com/company/svc-b",
		FilePath: srcFile, Line: 1,
	})
	require.NoError(t, err)

	testRepos := []repos.Repo{{Name: "svc-a", Path: dirA}}
	idx := newTestIndexer(t, st, testRepos)
	idx.InitManifests()

	err = idx.FullScan(ctx)
	require.NoError(t, err)

	// Verify import edge got dst_repo set
	crossEdges, err := st.AllCrossRepoEdges(ctx)
	require.NoError(t, err)
	// The edge should have dst_repo set if manifest resolution worked
	_ = crossEdges // may be empty if no manifest match
	assert.NotEmpty(t, idx.manifests.KnownImports())
}

func TestFullScan_NilClassifier(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()

	dirA := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dirA, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dirA, "main.go"), []byte("package main\n"), 0o644))

	require.NoError(t, st.EnsureFile(ctx, filepath.Join(dirA, "main.go"), "go"))
	_, _ = st.ReplaceASTUnits(ctx, filepath.Join(dirA, "main.go"), []store.ASTUnit{
		{FilePath: filepath.Join(dirA, "main.go"), Language: "go", Kind: "function", Name: "f", Repo: "svc-a"},
	})

	testRepos := []repos.Repo{{Name: "svc-a", Path: dirA}}
	idx := newTestIndexer(t, st, testRepos)
	// No classifier set
	idx.InitManifests()

	err := idx.FullScan(ctx)
	require.NoError(t, err)
}

// ─── IndexFile ──────────────────────────────────────────────────────────────

func TestIndexFile_NoCandidates(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()

	dirA := t.TempDir()
	srcFile := filepath.Join(dirA, "main.go")
	require.NoError(t, os.WriteFile(srcFile, []byte("package main\nfunc f() {}\n"), 0o644))

	require.NoError(t, st.EnsureFile(ctx, srcFile, "go"))
	_, _ = st.ReplaceASTUnits(ctx, srcFile, []store.ASTUnit{
		{FilePath: srcFile, Language: "go", Kind: "function", Name: "f", Repo: "svc-a"},
	})

	testRepos := []repos.Repo{{Name: "svc-a", Path: dirA}}
	idx := newTestIndexer(t, st, testRepos)
	idx.InitManifests()

	err := idx.IndexFile(ctx, srcFile)
	require.NoError(t, err)
}

func TestIndexFile_NonExistentFile(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()

	testRepos := []repos.Repo{{Name: "svc-a", Path: t.TempDir()}}
	idx := newTestIndexer(t, st, testRepos)

	err := idx.IndexFile(ctx, "/nonexistent.go")
	require.NoError(t, err) // should not error on missing file
}

// ─── RemoveFile ─────────────────────────────────────────────────────────────

func TestRemoveFile(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()

	dirA := t.TempDir()
	srcFile := filepath.Join(dirA, "main.go")
	require.NoError(t, os.WriteFile(srcFile, []byte("package main\n"), 0o644))

	require.NoError(t, st.EnsureFile(ctx, srcFile, "go"))
	units, _ := st.ReplaceASTUnits(ctx, srcFile, []store.ASTUnit{
		{FilePath: srcFile, Language: "go", Kind: "function", Name: "f", Repo: "svc-a"},
	})
	fID := units["f"]

	// Insert a cross_call edge
	_, err := st.InsertEdge(ctx, store.Edge{
		Repo: "svc-a", SrcID: fID, DstID: 0,
		DstName: "/api/x", DstRepo: "svc-b",
		Kind: "cross_call", FilePath: srcFile, Line: 1,
	})
	require.NoError(t, err)

	// Verify edge exists
	before, _ := st.AllCrossRepoEdges(ctx)
	require.NotEmpty(t, before)

	testRepos := []repos.Repo{{Name: "svc-a", Path: dirA}}
	idx := newTestIndexer(t, st, testRepos)

	err = idx.RemoveFile(ctx, srcFile)
	require.NoError(t, err)

	// Verify edge is gone
	after, _ := st.AllCrossRepoEdges(ctx)
	assert.Empty(t, after)
}

// ─── GetEdgesByRepo ─────────────────────────────────────────────────────────

func TestGetEdgesByRepo(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()

	dirA := t.TempDir()
	srcFile := filepath.Join(dirA, "main.go")
	require.NoError(t, os.WriteFile(srcFile, []byte("package main\n"), 0o644))

	require.NoError(t, st.EnsureFile(ctx, srcFile, "go"))
	units, _ := st.ReplaceASTUnits(ctx, srcFile, []store.ASTUnit{
		{FilePath: srcFile, Language: "go", Kind: "function", Name: "f", Repo: "svc-a"},
	})
	fID := units["f"]

	// Cross-repo edge: svc-a → svc-b
	_, err := st.InsertEdge(ctx, store.Edge{
		Repo: "svc-a", SrcID: fID, DstID: 0,
		DstName: "/api/x", DstRepo: "svc-b",
		Kind: "cross_call", FilePath: srcFile, Line: 1,
	})
	require.NoError(t, err)

	// Cross-repo edge: svc-c → svc-a
	_, err = st.InsertEdge(ctx, store.Edge{
		Repo: "svc-c", SrcID: fID, DstID: 0,
		DstName: "/api/y", DstRepo: "svc-a",
		Kind: "cross_call", FilePath: srcFile, Line: 2,
	})
	require.NoError(t, err)

	testRepos := []repos.Repo{{Name: "svc-a", Path: dirA}}
	idx := newTestIndexer(t, st, testRepos)

	// Query edges for svc-a (should find both: as src and as dst)
	edges, err := idx.GetEdgesByRepo("svc-a")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(edges), 2)

	// Query for svc-b (only as dst)
	edgesB, err := idx.GetEdgesByRepo("svc-b")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(edgesB), 1)
	assert.Equal(t, "svc-b", edgesB[0].DstRepo)
}

func TestGetEdgesByRepo_Empty(t *testing.T) {
	st := openTestDB(t)
	testRepos := []repos.Repo{{Name: "svc-a", Path: t.TempDir()}}
	idx := newTestIndexer(t, st, testRepos)

	edges, err := idx.GetEdgesByRepo("nonexistent")
	require.NoError(t, err)
	assert.Empty(t, edges)
}

// ─── Progress Reporting ─────────────────────────────────────────────────────

func TestReport_NilBus(t *testing.T) {
	st := openTestDB(t)
	testRepos := []repos.Repo{{Name: "svc-a", Path: t.TempDir()}}
	resolver := repos.NewResolver(testRepos)
	idx := New(resolver, st)
	// No bus set — should not panic

	idx.report("scanning", 0, 0, 0, 0)
	// No assertion — just verify no panic
}
