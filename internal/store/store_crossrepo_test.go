package store

// Тесты новых cross-repo методов в edges.go:
// AllEdgesByKind, AllCrossRepoEdges, InsertEdge, DeleteEdgesByFilePath,
// ResolvePendingEdgesCrossRepo.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── AllEdgesByKind ─────────────────────────────────────────────────────────

func TestAllEdgesByKind_AllKinds(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	idF, _ := seedFile(t, st, "/test/a.go")

	edges := []Edge{
		{Repo: "repo-a", SrcID: idF, DstID: 0, Kind: "call", DstName: "external.Func", FilePath: "/test/a.go", Line: 2},
		{Repo: "repo-a", SrcID: idF, DstID: 0, Kind: "import", DstName: "github.com/pkg/x", FilePath: "/test/a.go", Line: 1},
		{Repo: "repo-a", SrcID: idF, DstID: 0, Kind: "cross_call", DstName: "/api/validate", DstRepo: "auth-svc", FilePath: "/test/a.go", Line: 3},
	}
	require.NoError(t, st.ReplaceEdges(ctx, "/test/a.go", edges))

	// All edges
	all, err := st.AllEdgesByKind(ctx, "")
	require.NoError(t, err)
	assert.Len(t, all, 3)

	// Filtered by kind
	calls, err := st.AllEdgesByKind(ctx, "call")
	require.NoError(t, err)
	assert.Len(t, calls, 1)
	assert.Equal(t, "call", calls[0].Kind)

	imports, err := st.AllEdgesByKind(ctx, "import")
	require.NoError(t, err)
	assert.Len(t, imports, 1)

	// Non-existent kind
	none, err := st.AllEdgesByKind(ctx, "nonexistent")
	require.NoError(t, err)
	assert.Empty(t, none)
}

// ─── AllCrossRepoEdges ──────────────────────────────────────────────────────

func TestAllCrossRepoEdges(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	idF, _ := seedFile(t, st, "/test/a.go")

	edges := []Edge{
		{Repo: "repo-a", SrcID: idF, DstID: 0, Kind: "call", DstName: "local.Func", FilePath: "/test/a.go", Line: 2},                        // no dst_repo
		{Repo: "repo-a", SrcID: idF, DstID: 0, Kind: "cross_call", DstName: "/api/auth", DstRepo: "auth-svc", FilePath: "/test/a.go", Line: 3}, // has dst_repo
		{Repo: "repo-a", SrcID: idF, DstID: 0, Kind: "import", DstName: "github.com/x/y", DstRepo: "lib-svc", FilePath: "/test/a.go", Line: 1}, // has dst_repo
	}
	require.NoError(t, st.ReplaceEdges(ctx, "/test/a.go", edges))

	cross, err := st.AllCrossRepoEdges(ctx)
	require.NoError(t, err)
	assert.Len(t, cross, 2) // only edges with dst_repo != ""

	for _, e := range cross {
		assert.NotEmpty(t, e.DstRepo)
	}
}

func TestAllCrossRepoEdges_Empty(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	cross, err := st.AllCrossRepoEdges(ctx)
	require.NoError(t, err)
	assert.Empty(t, cross)
}

// ─── InsertEdge ─────────────────────────────────────────────────────────────

func TestInsertEdge_Basic(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	idF, _ := seedFile(t, st, "/test/a.go")

	edge := Edge{
		Repo: "repo-a", SrcID: idF, DstID: 0,
		Kind: "cross_call", DstName: "/api/auth", DstRepo: "auth-svc",
		FilePath: "/test/a.go", Line: 5, Confidence: 0.95,
	}

	id, err := st.InsertEdge(ctx, edge)
	require.NoError(t, err)
	assert.Greater(t, id, 0)

	// Verify persisted
	all, err := st.AllEdgesByKind(ctx, "cross_call")
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, "auth-svc", all[0].DstRepo)
	assert.Equal(t, 0.95, all[0].Confidence)
}

func TestInsertEdge_DefaultConfidence(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	idF, _ := seedFile(t, st, "/test/a.go")

	edge := Edge{
		Repo: "repo-a", SrcID: idF, DstID: 0,
		Kind: "cross_call", DstName: "/api/x", DstRepo: "svc",
		FilePath: "/test/a.go", Line: 1,
		// Confidence = 0 → should default to 1.0
	}

	_, err := st.InsertEdge(ctx, edge)
	require.NoError(t, err)

	all, _ := st.AllEdgesByKind(ctx, "cross_call")
	require.Len(t, all, 1)
	assert.Equal(t, 1.0, all[0].Confidence)
}

// ─── DeleteEdgesByFilePath ──────────────────────────────────────────────────

func TestDeleteEdgesByFilePath(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	idF, _ := seedFile(t, st, "/test/a.go")
	idG, _ := seedFile(t, st, "/test/b.go")

	edges := []Edge{
		{Repo: "repo-a", SrcID: idF, DstID: 0, Kind: "cross_call", DstName: "/api/x", DstRepo: "svc", FilePath: "/test/a.go", Line: 1},
		{Repo: "repo-a", SrcID: idF, DstID: 0, Kind: "call", DstName: "local.F", FilePath: "/test/a.go", Line: 2},
		{Repo: "repo-a", SrcID: idG, DstID: 0, Kind: "cross_call", DstName: "/api/y", DstRepo: "svc", FilePath: "/test/b.go", Line: 1},
	}
	require.NoError(t, st.ReplaceEdges(ctx, "/test/a.go", edges[:2]))
	require.NoError(t, st.ReplaceEdges(ctx, "/test/b.go", edges[2:]))

	// Delete only cross_call edges from /test/a.go
	err := st.DeleteEdgesByFilePath(ctx, "/test/a.go", "cross_call")
	require.NoError(t, err)

	remaining, _ := st.AllEdgesByKind(ctx, "")
	assert.Len(t, remaining, 2) // call from a.go + cross_call from b.go

	// Verify: cross_call from a.go gone, call from a.go still there
	calls, _ := st.AllEdgesByKind(ctx, "call")
	assert.Len(t, calls, 1)
	assert.Equal(t, "/test/a.go", calls[0].FilePath)
}

func TestDeleteEdgesByFilePath_AllKinds(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	idF, _ := seedFile(t, st, "/test/a.go")

	edges := []Edge{
		{Repo: "repo-a", SrcID: idF, DstID: 0, Kind: "cross_call", DstName: "/api/x", DstRepo: "svc", FilePath: "/test/a.go", Line: 1},
		{Repo: "repo-a", SrcID: idF, DstID: 0, Kind: "call", DstName: "local.F", FilePath: "/test/a.go", Line: 2},
	}
	require.NoError(t, st.ReplaceEdges(ctx, "/test/a.go", edges))

	// Delete all edges (kind="")
	err := st.DeleteEdgesByFilePath(ctx, "/test/a.go", "")
	require.NoError(t, err)

	remaining, _ := st.AllEdgesByKind(ctx, "")
	assert.Empty(t, remaining)
}

func TestDeleteEdgesByFilePath_NonExistentFile(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	err := st.DeleteEdgesByFilePath(ctx, "/nonexistent.go", "cross_call")
	require.NoError(t, err) // should be no-op, not error
}

// ─── ResolvePendingEdgesCrossRepo ───────────────────────────────────────────

// mockResolver implements CrossRepoResolver for testing.
type mockResolver struct {
	m map[string]string
}

func (m *mockResolver) ResolveImport(path string) string {
	return m.m[path]
}

func TestResolvePendingEdgesCrossRepo_NilResolver(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	idF, _ := seedFile(t, st, "/test/a.go")

	edges := []Edge{
		{Repo: "repo-a", SrcID: idF, DstID: 0, Kind: "import", DstName: "github.com/x/y", FilePath: "/test/a.go", Line: 1},
	}
	require.NoError(t, st.ReplaceEdges(ctx, "/test/a.go", edges))

	resolved, err := st.ResolvePendingEdgesCrossRepo(ctx, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, resolved)
}

func TestResolvePendingEdgesCrossRepo_Success(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	idF, _ := seedFile(t, st, "/test/a.go")

	edges := []Edge{
		{Repo: "repo-a", SrcID: idF, DstID: 0, Kind: "import", DstName: "github.com/company/auth-sdk", FilePath: "/test/a.go", Line: 1},
		{Repo: "repo-a", SrcID: idF, DstID: 0, Kind: "import", DstName: "github.com/other/lib", FilePath: "/test/a.go", Line: 2},
		{Repo: "repo-a", SrcID: idF, DstID: 0, Kind: "call", DstName: "local.Func", FilePath: "/test/a.go", Line: 3}, // not import kind
	}
	require.NoError(t, st.ReplaceEdges(ctx, "/test/a.go", edges))

	resolver := &mockResolver{m: map[string]string{
		"github.com/company/auth-sdk": "auth-service",
	}}

	resolved, err := st.ResolvePendingEdgesCrossRepo(ctx, resolver, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, resolved) // only auth-sdk matched

	// Verify dst_repo was set
	cross, _ := st.AllCrossRepoEdges(ctx)
	require.Len(t, cross, 1)
	assert.Equal(t, "auth-service", cross[0].DstRepo)
	assert.Equal(t, 1.0, cross[0].Confidence)
}

func TestResolvePendingEdgesCrossRepo_ProgressCallback(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	idF, _ := seedFile(t, st, "/test/a.go")

	// Create 3 resolvable import edges
	for i := 0; i < 3; i++ {
		edge := Edge{
			Repo: "repo-a", SrcID: idF, DstID: 0,
			Kind: "import", DstName: "github.com/company/svc",
			FilePath: "/test/a.go", Line: i + 1,
		}
		_, err := st.InsertEdge(ctx, edge)
		require.NoError(t, err)
	}

	resolver := &mockResolver{m: map[string]string{
		"github.com/company/svc": "my-service",
	}}

	var calls []int
	progressFn := func(pass int, resolved, remaining int64) {
		calls = append(calls, int(resolved))
	}

	resolved, err := st.ResolvePendingEdgesCrossRepo(ctx, resolver, progressFn)
	require.NoError(t, err)
	assert.Equal(t, 3, resolved)
	assert.NotEmpty(t, calls)
}

func TestResolvePendingEdgesCrossRepo_AlreadyResolved(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	idF, idG := seedFile(t, st, "/test/a.go")
	_ = idG

	// Edge with dst_id > 0 (already resolved)
	edge := Edge{
		Repo: "repo-a", SrcID: idF, DstID: idG, Kind: "import",
		DstName: "github.com/company/auth-sdk", FilePath: "/test/a.go", Line: 1,
	}
	_, err := st.InsertEdge(ctx, edge)
	require.NoError(t, err)

	resolver := &mockResolver{m: map[string]string{
		"github.com/company/auth-sdk": "auth-service",
	}}

	resolved, err := st.ResolvePendingEdgesCrossRepo(ctx, resolver, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, resolved) // already resolved, skipped

	// dst_repo should NOT be set (edge was skipped)
	cross, _ := st.AllCrossRepoEdges(ctx)
	assert.Empty(t, cross)
}
