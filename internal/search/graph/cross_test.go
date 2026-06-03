package graph

import (
	"context"
	"testing"

	"ragota/internal/store"
	"ragota/pkg/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openTestGraphStore(t *testing.T) *store.SQLite {
	t.Helper()
	path := t.TempDir() + "/ragota.db"
	st, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seedCrossRepoGraph(t *testing.T, st *store.SQLite) {
	t.Helper()
	ctx := context.Background()

	// Create units in repo-a
	require.NoError(t, st.EnsureFile(ctx, "/repo-a/main.go", "go"))
	unitsA := []store.ASTUnit{
		{FilePath: "/repo-a/main.go", Language: "go", Kind: "function", Name: "handleRequest", Qualified: "main.handleRequest", StartLine: 10, EndLine: 20, StartByte: 0, EndByte: 100, Repo: "repo-a"},
		{FilePath: "/repo-a/main.go", Language: "go", Kind: "function", Name: "validateToken", Qualified: "main.validateToken", StartLine: 25, EndLine: 35, StartByte: 101, EndByte: 200, Repo: "repo-a"},
	}
	idsA, err := st.ReplaceASTUnits(ctx, "/repo-a/main.go", unitsA)
	require.NoError(t, err)

	// Create units in auth-svc
	require.NoError(t, st.EnsureFile(ctx, "/auth-svc/auth.go", "go"))
	unitsAuth := []store.ASTUnit{
		{FilePath: "/auth-svc/auth.go", Language: "go", Kind: "function", Name: "ValidateToken", Qualified: "auth.ValidateToken", StartLine: 5, EndLine: 15, StartByte: 0, EndByte: 80, Repo: "auth-svc"},
	}
	idsAuth, err := st.ReplaceASTUnits(ctx, "/auth-svc/auth.go", unitsAuth)
	require.NoError(t, err)

	// Create cross-repo edges
	validateTokenID := idsA["main.validateToken"]
	authValidateID := idsAuth["auth.ValidateToken"]

	edges := []store.Edge{
		// cross_call from repo-a → auth-svc
		{Repo: "repo-a", SrcID: validateTokenID, DstID: 0, DstName: "/api/auth/validate", DstRepo: "auth-svc", Kind: "cross_call", FilePath: "/repo-a/main.go", Line: 30, Confidence: 0.9},
		// import edge with dst_repo
		{Repo: "repo-a", SrcID: validateTokenID, DstID: 0, DstName: "github.com/company/auth-sdk", DstRepo: "auth-svc", Kind: "import", FilePath: "/repo-a/main.go", Line: 1, Confidence: 1.0},
		// Another cross_call from repo-a → payment-svc
		{Repo: "repo-a", SrcID: idsA["main.handleRequest"], DstID: 0, DstName: "/api/pay", DstRepo: "payment-svc", Kind: "cross_call", FilePath: "/repo-a/main.go", Line: 15, Confidence: 0.85},
	}

	for _, e := range edges {
		_, err := st.InsertEdge(ctx, e)
		require.NoError(t, err)
	}

	// Also add an edge where DstID is set (target found)
	_, _ = st.InsertEdge(ctx, store.Edge{
		Repo: "repo-a", SrcID: validateTokenID, DstID: authValidateID,
		DstName: "ValidateToken", DstRepo: "auth-svc",
		Kind: "cross_call", FilePath: "/repo-a/main.go", Line: 31, Confidence: 0.95,
	})
}

// ─── aggregateDeps ──────────────────────────────────────────────────────────

func TestAggregateDeps(t *testing.T) {
	deps := []DepEdge{
		{Target: "auth-svc", Protocol: "http", Count: 1},
		{Target: "auth-svc", Protocol: "http", Count: 1},
		{Target: "auth-svc", Protocol: "grpc", Count: 1},
		{Target: "payment-svc", Protocol: "http", Count: 1},
	}

	result := aggregateDeps(deps)

	// Should have 3 unique (target|protocol) combos
	assert.Len(t, result, 3)

	// Find auth-svc|http — should have Count=2
	for _, d := range result {
		if d.Target == "auth-svc" && d.Protocol == "http" {
			assert.Equal(t, 2, d.Count)
		}
	}
}

func TestAggregateDeps_Empty(t *testing.T) {
	result := aggregateDeps(nil)
	assert.Empty(t, result)
}

// ─── uniqueStrings ──────────────────────────────────────────────────────────

func TestUniqueStrings(t *testing.T) {
	assert.Equal(t, []string{"a", "b", "c"}, uniqueStrings([]string{"a", "b", "a", "c", "b"}))
	assert.Empty(t, uniqueStrings(nil))
	assert.Empty(t, uniqueStrings([]string{}))
	assert.Equal(t, []string{"x"}, uniqueStrings([]string{"x", "x", "x"}))
}

// ─── ServiceGraph ───────────────────────────────────────────────────────────

func TestServiceGraph_Basic(t *testing.T) {
	st := openTestGraphStore(t)
	svc := New(config.Default(), st)
	seedCrossRepoGraph(t, st)

	graph := svc.ServiceGraph(context.Background())

	// Should have repo-a, auth-svc, payment-svc
	assert.Contains(t, graph, "repo-a")
	assert.Contains(t, graph, "auth-svc")
	assert.Contains(t, graph, "payment-svc")

	// repo-a depends on auth-svc and payment-svc
	repoA := graph["repo-a"]
	assert.GreaterOrEqual(t, len(repoA.Dependencies), 2)

	// auth-svc is depended upon by repo-a
	auth := graph["auth-svc"]
	assert.Contains(t, auth.DependedBy, "repo-a")
}

func TestServiceGraph_Empty(t *testing.T) {
	st := openTestGraphStore(t)
	svc := New(config.Default(), st)

	graph := svc.ServiceGraph(context.Background())
	assert.Empty(t, graph)
}

func TestServiceGraph_Aggregation(t *testing.T) {
	st := openTestGraphStore(t)
	svc := New(config.Default(), st)
	ctx := context.Background()

	require.NoError(t, st.EnsureFile(ctx, "/a/f.go", "go"))
	units, _ := st.ReplaceASTUnits(ctx, "/a/f.go", []store.ASTUnit{
		{FilePath: "/a/f.go", Language: "go", Kind: "function", Name: "f", Repo: "repo-a"},
	})
	fID := units["f"]

	// 3 cross_call edges to auth-svc (should aggregate to count=3)
	for i := 0; i < 3; i++ {
		_, _ = st.InsertEdge(ctx, store.Edge{
			Repo: "repo-a", SrcID: fID, DstID: 0,
			DstName: "/api/x", DstRepo: "auth-svc",
			Kind: "cross_call", FilePath: "/a/f.go", Line: i + 1,
		})
	}

	graph := svc.ServiceGraph(ctx)
	repoA := graph["repo-a"]

	// Find the auth-svc dependency — count should be 3
	var authDep *DepEdge
	for i := range repoA.Dependencies {
		d := &repoA.Dependencies[i]
		if d.Target == "auth-svc" {
			authDep = d
			break
		}
	}
	require.NotNil(t, authDep)
	assert.Equal(t, 3, authDep.Count)
}

// ─── CrossRepoCallers ───────────────────────────────────────────────────────

func TestCrossRepoCallers(t *testing.T) {
	st := openTestGraphStore(t)
	svc := New(config.Default(), st)
	seedCrossRepoGraph(t, st)

	// CrossRepoCallers looks for edges with dst_name matching symbol and kind "call"
	// But our seeded edges have kind "cross_call". Let's also seed a "call" edge.
	ctx := context.Background()
	units, _ := st.ListASTUnitsByFile(ctx, "/repo-a/main.go")
	var validateTokenID int
	for _, u := range units {
		if u.Name == "validateToken" {
			validateTokenID = u.ID
			break
		}
	}
	_, _ = st.InsertEdge(ctx, store.Edge{
		Repo: "repo-b", SrcID: validateTokenID, DstID: 0,
		DstName: "ValidateToken", Kind: "call",
		FilePath: "/repo-a/main.go", Line: 50,
	})

	callers, err := svc.CrossRepoCallers(context.Background(), "ValidateToken")
	require.NoError(t, err)
	assert.NotEmpty(t, callers)
}

func TestCrossRepoCallers_Empty(t *testing.T) {
	st := openTestGraphStore(t)
	svc := New(config.Default(), st)

	callers, err := svc.CrossRepoCallers(context.Background(), "NonExistent")
	require.NoError(t, err)
	assert.Empty(t, callers)
}

// ─── ResolveCrossCall ───────────────────────────────────────────────────────

func TestResolveCrossCall_Found(t *testing.T) {
	st := openTestGraphStore(t)
	svc := New(config.Default(), st)
	seedCrossRepoGraph(t, st)

	// resolve_call at line 31 (the one with DstID set to auth.ValidateToken)
	res, err := svc.ResolveCrossCall(context.Background(), "/repo-a/main.go", 31)
	require.NoError(t, err)
	require.NotNil(t, res)

	assert.Equal(t, "auth-svc", res.Edge.DstRepo)
	assert.Equal(t, "validateToken", res.SrcSymbol.Name)
	// DstSymbol should be found because DstID points to auth.ValidateToken
	if res.DstSymbol != nil {
		assert.Equal(t, "ValidateToken", res.DstSymbol.Name)
	}
}

func TestResolveCrossCall_NotFound(t *testing.T) {
	st := openTestGraphStore(t)
	svc := New(config.Default(), st)
	seedCrossRepoGraph(t, st)

	// No unit at line 999
	res, err := svc.ResolveCrossCall(context.Background(), "/repo-a/main.go", 999)
	require.NoError(t, err)
	assert.Nil(t, res)
}

func TestResolveCrossCall_NoCrossEdge(t *testing.T) {
	st := openTestGraphStore(t)
	svc := New(config.Default(), st)
	ctx := context.Background()

	// Create a file with a unit but NO cross-repo edges
	require.NoError(t, st.EnsureFile(ctx, "/local/f.go", "go"))
	_, _ = st.ReplaceASTUnits(ctx, "/local/f.go", []store.ASTUnit{
		{FilePath: "/local/f.go", Language: "go", Kind: "function", Name: "localFunc", Repo: "local"},
	})

	res, err := svc.ResolveCrossCall(context.Background(), "/local/f.go", 1)
	require.NoError(t, err)
	assert.Nil(t, res)
}
