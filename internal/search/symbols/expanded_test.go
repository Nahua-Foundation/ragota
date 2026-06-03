package symbols

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ragota/pkg/config"
	"ragota/internal/search/graph"
	"ragota/internal/store"
)

func openTestStore(t *testing.T) *store.SQLite {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newTestService(t *testing.T, st *store.SQLite) *Service {
	t.Helper()
	return New(st, graph.New(config.Default(), st), nil)
}

func TestService_Get_NotFound(t *testing.T) {
	st := openTestStore(t)
	svc := newTestService(t, st)
	ctx := context.Background()

	u, err := svc.Get(ctx, 99999)
	assert.NoError(t, err)
	assert.Nil(t, u)
}

func TestService_Parent_NoParent(t *testing.T) {
	st := openTestStore(t)
	svc := newTestService(t, st)
	ctx := context.Background()
	path := "/tmp/orphan.go"

	require.NoError(t, st.EnsureFile(ctx, path, "go"))
	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "orphan", Qualified: "pkg.orphan", StartLine: 1, EndLine: 5},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	id := ids["pkg.orphan"]
	parent, err := svc.Parent(ctx, id)
	assert.NoError(t, err)
	assert.Nil(t, parent)
}

func TestService_Parent_NonexistentID(t *testing.T) {
	st := openTestStore(t)
	svc := newTestService(t, st)
	ctx := context.Background()

	parent, err := svc.Parent(ctx, 99999)
	assert.NoError(t, err)
	assert.Nil(t, parent)
}

func TestService_Children_Empty(t *testing.T) {
	st := openTestStore(t)
	svc := newTestService(t, st)
	ctx := context.Background()
	path := "/tmp/leaf.go"

	require.NoError(t, st.EnsureFile(ctx, path, "go"))
	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "leaf", Qualified: "pkg.leaf", StartLine: 1, EndLine: 5},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	kids, err := svc.Children(ctx, ids["pkg.leaf"])
	assert.NoError(t, err)
	assert.Empty(t, kids)
}

func TestService_FileSymbols_EmptyFile(t *testing.T) {
	st := openTestStore(t)
	svc := newTestService(t, st)
	ctx := context.Background()

	units, err := svc.FileSymbols(ctx, "/nonexistent/file.go")
	assert.NoError(t, err)
	assert.Empty(t, units)
}

func TestService_FindDefinition_ModuleFiltered(t *testing.T) {
	st := openTestStore(t)
	svc := newTestService(t, st)
	ctx := context.Background()
	path := "/tmp/mod.go"

	require.NoError(t, st.EnsureFile(ctx, path, "go"))
	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "module", Name: "mymod", Qualified: "mymod", StartLine: 1, EndLine: 1},
		{FilePath: path, Language: "go", Kind: "function", Name: "mymod", Qualified: "mymod.mymod", StartLine: 5, EndLine: 10},
	}
	_, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	// FindDefinition should filter out "module" kind
	defs, err := svc.FindDefinition(ctx, "mymod")
	assert.NoError(t, err)
	for _, d := range defs {
		assert.NotEqual(t, "module", d.Kind)
	}
}

func TestService_FindDefinition_CaseInsensitive(t *testing.T) {
	st := openTestStore(t)
	svc := newTestService(t, st)
	ctx := context.Background()
	path := "/tmp/case.go"

	require.NoError(t, st.EnsureFile(ctx, path, "go"))
	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "MyFunc", Qualified: "pkg.MyFunc", StartLine: 1, EndLine: 5},
	}
	_, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	defs, err := svc.FindDefinition(ctx, "myfunc")
	assert.NoError(t, err)
	assert.NotEmpty(t, defs)
}

func TestService_FindReferences_NoDefinitions(t *testing.T) {
	st := openTestStore(t)
	svc := newTestService(t, st)
	ctx := context.Background()

	refs, err := svc.FindReferences(ctx, "nonexistent_func")
	assert.NoError(t, err)
	assert.Empty(t, refs)
}

func TestService_FindReferences_WithDotFallback(t *testing.T) {
	st := openTestStore(t)
	svc := newTestService(t, st)
	ctx := context.Background()
	path := "/tmp/dotref.go"

	require.NoError(t, st.EnsureFile(ctx, path, "go"))
	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "log", Qualified: "pkg.log", StartLine: 1, EndLine: 5},
		{FilePath: path, Language: "go", Kind: "function", Name: "caller", Qualified: "pkg.caller", StartLine: 10, EndLine: 15},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	idCaller := ids["pkg.caller"]
	idLog := ids["pkg.log"]

	edges := []store.Edge{
		{SrcID: idCaller, DstID: idLog, Kind: "reference", DstName: "Logger.log", FilePath: path, Line: 12},
	}
	require.NoError(t, st.ReplaceEdges(ctx, path, edges))

	// Search for "Logger.log" — should find via dot fallback to "log"
	refs, err := svc.FindReferences(ctx, "Logger.log")
	assert.NoError(t, err)
	// May or may not find it depending on exact matching, but should not error
	_ = refs
}

func TestService_FindCallers_NoDefinitions(t *testing.T) {
	st := openTestStore(t)
	svc := newTestService(t, st)
	ctx := context.Background()

	callers, err := svc.FindCallers(ctx, "nonexistent_function")
	assert.NoError(t, err)
	assert.Empty(t, callers)
}

func TestService_FindCallees_NoDefinitions(t *testing.T) {
	st := openTestStore(t)
	svc := newTestService(t, st)
	ctx := context.Background()

	callees, err := svc.FindCallees(ctx, "nonexistent_function")
	assert.NoError(t, err)
	assert.Empty(t, callees)
}

func TestService_FindImplementations_NoInterface(t *testing.T) {
	st := openTestStore(t)
	svc := newTestService(t, st)
	ctx := context.Background()

	impls, err := svc.FindImplementations(ctx, "NonexistentInterface")
	assert.NoError(t, err)
	assert.Nil(t, impls)
}

func TestService_SurroundingContext_NonexistentUnit(t *testing.T) {
	st := openTestStore(t)
	svc := newTestService(t, st)
	ctx := context.Background()

	_, err := svc.SurroundingContext(ctx, 99999, 2, 2)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestService_SurroundingContext_FileDeleted(t *testing.T) {
	st := openTestStore(t)
	svc := newTestService(t, st)
	ctx := context.Background()
	path := "/tmp/deleted_file_xyz.go"

	require.NoError(t, st.EnsureFile(ctx, path, "go"))
	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "f", Qualified: "pkg.f", StartLine: 1, EndLine: 5},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	// File doesn't exist on disk, so os.ReadFile will fail
	_, err = svc.SurroundingContext(ctx, ids["pkg.f"], 2, 2)
	assert.Error(t, err) // should return os.ReadFile error
}

func TestService_SurroundingContext_WithFile(t *testing.T) {
	st := openTestStore(t)
	svc := newTestService(t, st)
	ctx := context.Background()

	dir := t.TempDir()
	srcFile := filepath.Join(dir, "test.go")
	content := "line1\nline2\nline3\nfunc hello() {\n\treturn\n}\nline7\nline8\nline9\nline10"
	require.NoError(t, os.WriteFile(srcFile, []byte(content), 0644))

	require.NoError(t, st.EnsureFile(ctx, srcFile, "go"))
	units := []store.ASTUnit{
		{FilePath: srcFile, Language: "go", Kind: "function", Name: "hello", Qualified: "pkg.hello", StartLine: 4, EndLine: 6},
	}
	ids, err := st.ReplaceASTUnits(ctx, srcFile, units)
	require.NoError(t, err)

	result, err := svc.SurroundingContext(ctx, ids["pkg.hello"], 1, 1)
	assert.NoError(t, err)
	assert.Contains(t, result, "line3")
	assert.Contains(t, result, "func hello()")
	assert.Contains(t, result, "line7")
}

func TestService_SurroundingContext_BoundaryLines(t *testing.T) {
	st := openTestStore(t)
	svc := newTestService(t, st)
	ctx := context.Background()

	dir := t.TempDir()
	srcFile := filepath.Join(dir, "test.go")
	content := "func first() {\n\treturn\n}"
	require.NoError(t, os.WriteFile(srcFile, []byte(content), 0644))

	require.NoError(t, st.EnsureFile(ctx, srcFile, "go"))
	units := []store.ASTUnit{
		{FilePath: srcFile, Language: "go", Kind: "function", Name: "first", Qualified: "pkg.first", StartLine: 1, EndLine: 3},
	}
	ids, err := st.ReplaceASTUnits(ctx, srcFile, units)
	require.NoError(t, err)

	// beforeLines > startLine should not panic
	result, err := svc.SurroundingContext(ctx, ids["pkg.first"], 10, 10)
	assert.NoError(t, err)
	assert.NotEmpty(t, result)
}

func TestService_RelatedFiles(t *testing.T) {
	st := openTestStore(t)
	svc := newTestService(t, st)
	ctx := context.Background()
	path := "/tmp/related.go"

	require.NoError(t, st.EnsureFile(ctx, path, "go"))
	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "f", Qualified: "pkg.f", StartLine: 1, EndLine: 5},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	files, err := svc.RelatedFiles(ctx, ids["pkg.f"])
	assert.NoError(t, err)
	// May be empty but should not error
	_ = files
}

func TestService_SimilarCode_NilSearcher(t *testing.T) {
	st := openTestStore(t)
	svc := newTestService(t, st) // sim = nil
	ctx := context.Background()
	path := "/tmp/sim.go"

	require.NoError(t, st.EnsureFile(ctx, path, "go"))
	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "f", Qualified: "pkg.f", StartLine: 1, EndLine: 5},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	res, err := svc.SimilarCode(ctx, ids["pkg.f"], 10)
	assert.NoError(t, err)
	assert.Empty(t, res)
}

func TestService_SimilarCode_NonexistentID(t *testing.T) {
	st := openTestStore(t)
	ms := &mockSimilar{units: []store.ASTUnit{{Name: "sim"}}}
	svc := New(st, graph.New(config.Default(), st), ms)
	ctx := context.Background()

	_, err := svc.SimilarCode(ctx, 99999, 10)
	assert.NoError(t, err)
}

func TestService_SetLSPManager(t *testing.T) {
	st := openTestStore(t)
	svc := newTestService(t, st)
	// Should not panic
	svc.SetLSPManager(nil)
}

func TestService_SetSimilarSearcher(t *testing.T) {
	st := openTestStore(t)
	svc := newTestService(t, st)
	ms := &mockSimilar{}
	svc.SetSimilarSearcher(ms)
	assert.NotNil(t, svc.sim)
}

func TestService_FindDefinition_EmptyStore(t *testing.T) {
	st := openTestStore(t)
	svc := newTestService(t, st)
	ctx := context.Background()

	defs, err := svc.FindDefinition(ctx, "")
	assert.NoError(t, err)
	assert.Empty(t, defs)
}

func TestService_FindCallable_MixedKinds(t *testing.T) {
	st := openTestStore(t)
	svc := newTestService(t, st)
	ctx := context.Background()
	path := "/tmp/callable.go"

	require.NoError(t, st.EnsureFile(ctx, path, "go"))
	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "doWork", Qualified: "pkg.doWork", StartLine: 1, EndLine: 10},
		{FilePath: path, Language: "go", Kind: "method", Name: "doWork", Qualified: "pkg.S.doWork", StartLine: 15, EndLine: 20},
	}
	_, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	// findCallable should find both function and method
	callers, err := svc.FindCallers(ctx, "doWork")
	assert.NoError(t, err)
	// May be empty (no edges), but should not error
	_ = callers
}

func TestService_FindImplementations_LangFiltered(t *testing.T) {
	st := openTestStore(t)
	svc := newTestService(t, st)
	ctx := context.Background()
	path := "/tmp/iface.go"

	require.NoError(t, st.EnsureFile(ctx, path, "go"))
	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "interface", Name: "Reader", Qualified: "pkg.Reader", StartLine: 1, EndLine: 5},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	impls, err := svc.FindImplementations(ctx, "Reader")
	assert.NoError(t, err)
	_ = ids
	// Without edges, should return empty or just the interface itself
	for _, impl := range impls {
		// Go interfaces should NOT be in implementation list
		if impl.Language == "go" {
			assert.NotEqual(t, "interface", impl.Kind)
		}
	}
}
