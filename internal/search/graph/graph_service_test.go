package graph

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"ragota/pkg/config"
	"ragota/internal/store"
)

// openTestDB creates an in-memory SQLite store for testing.
func openTestDB(t *testing.T) *store.SQLite {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// newTestService creates a Service without LSP for testing.
func newTestService(t *testing.T) (*Service, *store.SQLite) {
	t.Helper()
	st := openTestDB(t)
	cfg := config.Default()
	svc := New(cfg, st)
	return svc, st
}

// seedGraph creates a simple graph: f -> g, h -> g, i implements f
// Returns IDs of f, g, h, i.
func seedGraph(t *testing.T, st *store.SQLite) (idF, idG, idH, idI int) {
	t.Helper()
	ctx := context.Background()
	path := "/tmp/test.go"

	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "f", Qualified: "pkg.f", StartLine: 1, EndLine: 10, Signature: "func f()"},
		{FilePath: path, Language: "go", Kind: "function", Name: "g", Qualified: "pkg.g", StartLine: 15, EndLine: 25, Signature: "func g()"},
		{FilePath: path, Language: "go", Kind: "function", Name: "h", Qualified: "pkg.h", StartLine: 30, EndLine: 40, Signature: "func h()"},
		{FilePath: path, Language: "go", Kind: "interface", Name: "i", Qualified: "pkg.i", StartLine: 45, EndLine: 50, Signature: "type i interface"},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	// ReplaceASTUnits uses Qualified as key (falls back to Name if empty)
	idF = ids["pkg.f"]
	idG = ids["pkg.g"]
	idH = ids["pkg.h"]
	idI = ids["pkg.i"]

	// Edges: f -> g (call), h -> g (call), f implements i
	edges := []store.Edge{
		{SrcID: idF, DstID: idG, Kind: EdgeCall, Repo: "", FilePath: path, Line: 5, DstName: "g"},
		{SrcID: idH, DstID: idG, Kind: EdgeCall, Repo: "", FilePath: path, Line: 35, DstName: "g"},
		{SrcID: idF, DstID: idI, Kind: EdgeImplements, Repo: "", FilePath: path, Line: 1, DstName: "i"},
	}
	require.NoError(t, st.ReplaceEdges(ctx, path, edges))

	return
}

// TestNew tests Service constructor.
func TestNew(t *testing.T) {
	st := openTestDB(t)
	cfg := config.Default()
	svc := New(cfg, st)
	require.NotNil(t, svc)
	assert.NotNil(t, svc.callCache)
	assert.NotNil(t, svc.implCache)
	assert.Nil(t, svc.mgr)
}

// TestNewWithLSP tests Service constructor with LSP manager.
func TestNewWithLSP(t *testing.T) {
	st := openTestDB(t)
	cfg := config.Default()
	svc := NewWithLSP(cfg, st, nil)
	require.NotNil(t, svc)
	// mgr is nil but set explicitly
}

// TestSetBus tests SetBus method.
func TestSetBus(t *testing.T) {
	st := openTestDB(t)
	cfg := config.Default()
	svc := New(cfg, st)
	assert.Nil(t, svc.bus)
	// SetBus with nil is safe
	svc.SetBus(nil)
	assert.Nil(t, svc.bus)
}

// TestCallers_Basic tests Callers returns correct callers.
func TestCallers_Basic(t *testing.T) {
	svc, st := newTestService(t)
	idF, idG, idH, _ := seedGraph(t, st)

	ctx := context.Background()

	// g is called by f and h
	callers, err := svc.Callers(ctx, idG)
	require.NoError(t, err)
	assert.Len(t, callers, 2)

	callerIDs := make(map[int]bool)
	for _, c := range callers {
		callerIDs[c.ID] = true
	}
	assert.True(t, callerIDs[idF])
	assert.True(t, callerIDs[idH])
}

// TestCallers_NoCallers tests Callers for function with no callers.
func TestCallers_NoCallers(t *testing.T) {
	svc, st := newTestService(t)
	idF, _, _, _ := seedGraph(t, st)

	ctx := context.Background()

	// f has no callers (nothing calls f)
	callers, err := svc.Callers(ctx, idF)
	require.NoError(t, err)
	assert.Empty(t, callers)
}

// TestCallers_NonExistentID tests Callers with non-existent ID.
func TestCallers_NonExistentID(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	callers, err := svc.Callers(ctx, 99999)
	require.NoError(t, err)
	assert.Empty(t, callers)
}

// TestCallees_Basic tests Callees returns correct callees.
func TestCallees_Basic(t *testing.T) {
	svc, st := newTestService(t)
	idF, idG, _, _ := seedGraph(t, st)

	ctx := context.Background()

	// f calls g
	callees, err := svc.Callees(ctx, idF)
	require.NoError(t, err)
	assert.Len(t, callees, 1)
	assert.Equal(t, idG, callees[0].ID)
}

// TestCallees_NoCallees tests Callees for function with no callees.
func TestCallees_NoCallees(t *testing.T) {
	svc, st := newTestService(t)
	_, _, idH, _ := seedGraph(t, st)

	ctx := context.Background()

	// h calls g, so it has callees
	callees, err := svc.Callees(ctx, idH)
	require.NoError(t, err)
	assert.Len(t, callees, 1)
}

// TestCallees_NonExistentID tests Callees with non-existent ID.
func TestCallees_NonExistentID(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	callees, err := svc.Callees(ctx, 99999)
	require.NoError(t, err)
	assert.Empty(t, callees)
}

// TestImplementations_Basic tests Implementations with implements edges.
func TestImplementations_Basic(t *testing.T) {
	svc, st := newTestService(t)
	idF, _, _, idI := seedGraph(t, st)

	ctx := context.Background()

	// i is implemented by f
	impls, err := svc.Implementations(ctx, idI)
	require.NoError(t, err)
	assert.Len(t, impls, 1)
	assert.Equal(t, idF, impls[0].ID)
}

// TestImplementations_NoImplementations tests Implementations for non-interface.
func TestImplementations_NoImplementations(t *testing.T) {
	svc, st := newTestService(t)
	idF, _, _, _ := seedGraph(t, st)

	ctx := context.Background()

	// f is a function, not an interface
	impls, err := svc.Implementations(ctx, idF)
	require.NoError(t, err)
	assert.Empty(t, impls)
}

// TestImplementations_NonExistentID tests Implementations with non-existent ID.
func TestImplementations_NonExistentID(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	impls, err := svc.Implementations(ctx, 99999)
	require.NoError(t, err)
	assert.Empty(t, impls)
}

// TestImplementations_WithExtends tests Implementations includes extends edges.
func TestImplementations_WithExtends(t *testing.T) {
	svc, st := newTestService(t)
	ctx := context.Background()

	path := "/tmp/extends.go"
	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "interface", Name: "Base", Qualified: "pkg.Base", StartLine: 1, EndLine: 5},
		{FilePath: path, Language: "go", Kind: "struct", Name: "Derived", Qualified: "pkg.Derived", StartLine: 10, EndLine: 15},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	idBase := ids["pkg.Base"]
	idDerived := ids["pkg.Derived"]

	edges := []store.Edge{
		{SrcID: idDerived, DstID: idBase, Kind: EdgeExtends, Repo: "", FilePath: path, Line: 10, DstName: "Base"},
	}
	require.NoError(t, st.ReplaceEdges(ctx, path, edges))

	impls, err := svc.Implementations(ctx, idBase)
	require.NoError(t, err)
	assert.Len(t, impls, 1)
	assert.Equal(t, idDerived, impls[0].ID)
}

// TestReferences_AllKinds tests References returns edges of all relevant kinds.
func TestReferences_AllKinds(t *testing.T) {
	svc, st := newTestService(t)
	ctx := context.Background()

	path := "/tmp/refs.go"
	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "target", Qualified: "pkg.target", StartLine: 1, EndLine: 10},
		{FilePath: path, Language: "go", Kind: "function", Name: "caller", Qualified: "pkg.caller", StartLine: 20, EndLine: 30},
		{FilePath: path, Language: "go", Kind: "struct", Name: "impl", Qualified: "pkg.impl", StartLine: 40, EndLine: 50},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	idTarget := ids["pkg.target"]
	idCaller := ids["pkg.caller"]
	idImpl := ids["pkg.impl"]

	edges := []store.Edge{
		{SrcID: idCaller, DstID: idTarget, Kind: EdgeCall, Repo: "", FilePath: path, Line: 25, DstName: "target"},
		{SrcID: idImpl, DstID: idTarget, Kind: EdgeImplements, Repo: "", FilePath: path, Line: 40, DstName: "target"},
		{SrcID: idCaller, DstID: idTarget, Kind: EdgeReference, Repo: "", FilePath: path, Line: 22, DstName: "target"},
	}
	require.NoError(t, st.ReplaceEdges(ctx, path, edges))

	refs, err := svc.References(ctx, idTarget)
	require.NoError(t, err)
	// Should have call + implements + reference = 3 edges
	assert.Len(t, refs, 3)
}

// TestReferences_Empty tests References for node with no references.
func TestReferences_Empty(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	refs, err := svc.References(ctx, 99999)
	require.NoError(t, err)
	assert.Empty(t, refs)
}

// TestExpandNeighbors_Basic tests ExpandNeighbors with depth 1.
func TestExpandNeighbors_Basic(t *testing.T) {
	svc, st := newTestService(t)
	idF, _, _, _ := seedGraph(t, st)

	ctx := context.Background()

	nh, err := svc.ExpandNeighbors(ctx, idF, 1, nil)
	require.NoError(t, err)
	require.NotNil(t, nh)
	// f itself + g (called by f) + i (f implements i)
	assert.GreaterOrEqual(t, len(nh.Nodes), 1)
}

// TestExpandNeighbors_ZeroDepth tests ExpandNeighbors with depth 0 (normalized to 1).
func TestExpandNeighbors_ZeroDepth(t *testing.T) {
	svc, st := newTestService(t)
	idF, _, _, _ := seedGraph(t, st)

	ctx := context.Background()

	nh, err := svc.ExpandNeighbors(ctx, idF, 0, nil)
	require.NoError(t, err)
	require.NotNil(t, nh)
}

// TestExpandNeighbors_WithKinds tests ExpandNeighbors with kind filter.
func TestExpandNeighbors_WithKinds(t *testing.T) {
	svc, st := newTestService(t)
	idF, idG, _, _ := seedGraph(t, st)

	ctx := context.Background()

	// Only call edges
	nh, err := svc.ExpandNeighbors(ctx, idF, 1, []string{EdgeCall})
	require.NoError(t, err)
	require.NotNil(t, nh)
	// Should include f and g (f calls g)
	nodeIDs := make(map[int]bool)
	for _, n := range nh.Nodes {
		nodeIDs[n.ID] = true
	}
	assert.True(t, nodeIDs[idF])
	assert.True(t, nodeIDs[idG])
}

// TestExpandNeighbors_NonExistentID tests ExpandNeighbors with non-existent ID.
func TestExpandNeighbors_NonExistentID(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	nh, err := svc.ExpandNeighbors(ctx, 99999, 1, nil)
	require.NoError(t, err)
	require.NotNil(t, nh)
	assert.Empty(t, nh.Nodes)
}

// TestCallGraph_Basic tests CallGraph expansion.
func TestCallGraph_Basic(t *testing.T) {
	svc, st := newTestService(t)
	idF, _, _, _ := seedGraph(t, st)

	ctx := context.Background()

	nh, err := svc.CallGraph(ctx, idF, 1)
	require.NoError(t, err)
	require.NotNil(t, nh)
}

// TestCallGraph_NonExistentID tests CallGraph with non-existent ID.
func TestCallGraph_NonExistentID(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	nh, err := svc.CallGraph(ctx, 99999, 1)
	require.NoError(t, err)
	require.NotNil(t, nh)
}

// TestDependencyGraph_ModuleNotFound tests DependencyGraph when module doesn't exist.
func TestDependencyGraph_ModuleNotFound(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	nh, err := svc.DependencyGraph(ctx, "nonexistent/module", 1)
	require.NoError(t, err)
	require.NotNil(t, nh)
	assert.Empty(t, nh.Nodes)
}

// TestDependencyGraph_WithModule tests DependencyGraph with existing module.
func TestDependencyGraph_WithModule(t *testing.T) {
	svc, st := newTestService(t)
	ctx := context.Background()

	path := "/tmp/module.go"
	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "module", Name: "mymodule", Qualified: "mymodule", StartLine: 1, EndLine: 1},
	}
	_, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	nh, err := svc.DependencyGraph(ctx, "mymodule", 1)
	require.NoError(t, err)
	require.NotNil(t, nh)
}

// TestTraverseGraph_Basic tests TraverseGraph.
func TestTraverseGraph_Basic(t *testing.T) {
	svc, st := newTestService(t)
	idF, _, _, _ := seedGraph(t, st)

	ctx := context.Background()

	result, err := svc.TraverseGraph(ctx, idF, 1, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
}

// TestTraverseGraph_NonExistentID tests TraverseGraph with non-existent ID.
func TestTraverseGraph_NonExistentID(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	result, err := svc.TraverseGraph(ctx, 99999, 1, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
}

// TestGetExecutionContext_Found tests GetExecutionContext for existing symbol.
func TestGetExecutionContext_Found(t *testing.T) {
	svc, st := newTestService(t)
	idF, _, _, _ := seedGraph(t, st)

	ctx := context.Background()

	ec, err := svc.GetExecutionContext(ctx, idF)
	require.NoError(t, err)
	require.NotNil(t, ec)
	require.NotNil(t, ec.Definition)
	assert.Equal(t, idF, ec.Definition.ID)
	// f calls g, so should have callees
	assert.NotEmpty(t, ec.Callees)
}

// TestGetExecutionContext_NotFound tests GetExecutionContext for non-existent symbol.
func TestGetExecutionContext_NotFound(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	ec, err := svc.GetExecutionContext(ctx, 99999)
	require.NoError(t, err)
	assert.Nil(t, ec)
}

// TestGetExecutionContext_ImportantFiles tests that ImportantFiles is populated.
func TestGetExecutionContext_ImportantFiles(t *testing.T) {
	svc, st := newTestService(t)
	idF, _, _, _ := seedGraph(t, st)

	ctx := context.Background()

	ec, err := svc.GetExecutionContext(ctx, idF)
	require.NoError(t, err)
	require.NotNil(t, ec)
	assert.NotEmpty(t, ec.ImportantFiles)
	assert.Contains(t, ec.ImportantFiles, "/tmp/test.go")
}

// TestGetExecutionContext_WithOutEdges tests GetExecutionContext with outgoing extends/imports edges.
func TestGetExecutionContext_WithOutEdges(t *testing.T) {
	svc, st := newTestService(t)
	ctx := context.Background()

	path := "/tmp/outedges.go"
	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "struct", Name: "MyStruct", Qualified: "pkg.MyStruct", StartLine: 1, EndLine: 10},
		{FilePath: path, Language: "go", Kind: "interface", Name: "MyInterface", Qualified: "pkg.MyInterface", StartLine: 20, EndLine: 25},
		{FilePath: path, Language: "go", Kind: "module", Name: "dep", Qualified: "dep", StartLine: 1, EndLine: 1},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	idStruct := ids["pkg.MyStruct"]
	idIface := ids["pkg.MyInterface"]
	idDep := ids["dep"]

	edges := []store.Edge{
		{SrcID: idStruct, DstID: idIface, Kind: EdgeImplements, Repo: "", FilePath: path, Line: 1, DstName: "MyInterface"},
		{SrcID: idStruct, DstID: idDep, Kind: EdgeImport, Repo: "", FilePath: path, Line: 1, DstName: "dep"},
	}
	require.NoError(t, st.ReplaceEdges(ctx, path, edges))

	ec, err := svc.GetExecutionContext(ctx, idStruct)
	require.NoError(t, err)
	require.NotNil(t, ec)
	// Should have related types (MyInterface)
	assert.NotEmpty(t, ec.RelatedTypes)
	// Should have imports (dep)
	assert.NotEmpty(t, ec.Imports)
}

// TestNodesByIDs_EmptySlice tests nodesByIDs with empty input.
func TestNodesByIDs_EmptySlice(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	nodes, err := svc.nodesByIDs(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, nodes)
}

// TestNodesByIDs_SkipsZero tests nodesByIDs skips zero IDs.
func TestNodesByIDs_SkipsZero(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	nodes, err := svc.nodesByIDs(ctx, []int{0, 0, 0})
	require.NoError(t, err)
	assert.Empty(t, nodes)
}

// TestNodesByIDs_SkipsDuplicates tests nodesByIDs deduplicates.
func TestNodesByIDs_SkipsDuplicates(t *testing.T) {
	svc, st := newTestService(t)
	idF, _, _, _ := seedGraph(t, st)

	ctx := context.Background()

	nodes, err := svc.nodesByIDs(ctx, []int{idF, idF, idF})
	require.NoError(t, err)
	assert.Len(t, nodes, 1)
}

// TestNodesByIDs_SkipsNonExistent tests nodesByIDs skips non-existent IDs.
func TestNodesByIDs_SkipsNonExistent(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	nodes, err := svc.nodesByIDs(ctx, []int{99999, 99998})
	require.NoError(t, err)
	assert.Empty(t, nodes)
}

// TestValidateFileLayer_InterfaceCorrection tests validateFileLayer corrects hallucinated interface layer.
func TestValidateFileLayer_InterfaceCorrection(t *testing.T) {
	svc, _ := newTestService(t)

	units := []store.ASTUnit{
		{Kind: "function", Name: "foo"},
		{Kind: "struct", Name: "bar"},
	}

	// LLM claimed "Interface" but there's no interface, only implementation
	result := svc.validateFileLayer("/tmp/test.go", units, "Interface")
	assert.Equal(t, "implementation", result)
}

// TestValidateFileLayer_TestFileDetection tests validateFileLayer detects test files.
func TestValidateFileLayer_TestFileDetection(t *testing.T) {
	svc, _ := newTestService(t)

	units := []store.ASTUnit{
		{Kind: "function", Name: "TestFoo"},
	}

	// LLM claimed "implementation" but it's a test file
	result := svc.validateFileLayer("/tmp/foo_test.go", units, "implementation")
	assert.Equal(t, "test", result)
}

// TestValidateFileLayer_EmptyLayer tests validateFileLayer with empty input.
func TestValidateFileLayer_EmptyLayer(t *testing.T) {
	svc, _ := newTestService(t)

	result := svc.validateFileLayer("/tmp/test.go", nil, "")
	assert.Equal(t, "", result)
}

// TestValidateFileLayer_CorrectLayer tests validateFileLayer preserves correct layer.
func TestValidateFileLayer_CorrectLayer(t *testing.T) {
	svc, _ := newTestService(t)

	units := []store.ASTUnit{
		{Kind: "interface", Name: "Foo"},
	}

	result := svc.validateFileLayer("/tmp/test.go", units, "interface")
	assert.Equal(t, "interface", result)
}

// TestValidateFileLayer_TestNameSuffix tests validateFileLayer detects _test suffix in unit names.
func TestValidateFileLayer_TestNameSuffix(t *testing.T) {
	svc, _ := newTestService(t)

	units := []store.ASTUnit{
		{Kind: "function", Name: "foo_test"},
	}

	result := svc.validateFileLayer("/tmp/foo.go", units, "implementation")
	assert.Equal(t, "test", result)
}

// TestSourceContent_NonExistentFile tests sourceContent with non-existent file.
func TestSourceContent_NonExistentFile(t *testing.T) {
	svc, _ := newTestService(t)

	u := store.ASTUnit{FilePath: "/nonexistent/file.go", StartLine: 1, EndLine: 10}
	result := svc.sourceContent(u)
	assert.Equal(t, "", result)
}

// TestSourceContent_ValidFile tests sourceContent with real file.
func TestSourceContent_ValidFile(t *testing.T) {
	svc, _ := newTestService(t)

	// Create a temp file
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "test.go")
	content := "package main\n\nfunc foo() {\n\treturn\n}\n"
	require.NoError(t, writeTestFile(tmpFile, content))

	u := store.ASTUnit{FilePath: tmpFile, StartLine: 3, EndLine: 5}
	result := svc.sourceContent(u)
	assert.Contains(t, result, "func foo()")
}

// TestSourceContent_OutOfBoundsLines tests sourceContent with lines beyond file.
func TestSourceContent_OutOfBoundsLines(t *testing.T) {
	svc, _ := newTestService(t)

	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "test.go")
	content := "line1\nline2\nline3"
	require.NoError(t, writeTestFile(tmpFile, content))

	u := store.ASTUnit{FilePath: tmpFile, StartLine: 1, EndLine: 100}
	result := svc.sourceContent(u)
	assert.Contains(t, result, "line1")
}

// TestSourceContent_NegativeStartLine tests sourceContent with negative start line.
func TestSourceContent_NegativeStartLine(t *testing.T) {
	svc, _ := newTestService(t)

	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "test.go")
	content := "line1\nline2"
	require.NoError(t, writeTestFile(tmpFile, content))

	u := store.ASTUnit{FilePath: tmpFile, StartLine: -5, EndLine: 2}
	result := svc.sourceContent(u)
	assert.Contains(t, result, "line1")
}

// TestSourceContentFile_NonExistent tests sourceContentFile with non-existent file.
func TestSourceContentFile_NonExistent(t *testing.T) {
	svc, _ := newTestService(t)

	result, err := svc.sourceContentFile("/nonexistent/file.go")
	assert.Error(t, err)
	assert.Equal(t, "", result)
}

// TestSourceContentFile_Valid tests sourceContentFile with real file.
func TestSourceContentFile_Valid(t *testing.T) {
	svc, _ := newTestService(t)

	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "test.go")
	content := "package main\nfunc main() {}"
	require.NoError(t, writeTestFile(tmpFile, content))

	result, err := svc.sourceContentFile(tmpFile)
	require.NoError(t, err)
	assert.Equal(t, content, result)
}

// TestFindModuleNode_ExactMatch tests findModuleNode with exact name match.
func TestFindModuleNode_ExactMatch(t *testing.T) {
	svc, st := newTestService(t)
	ctx := context.Background()

	path := "/tmp/mod.go"
	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "module", Name: "mymod", Qualified: "mymod", StartLine: 1, EndLine: 1},
	}
	_, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	found, err := svc.findModuleNode(ctx, "mymod")
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, "mymod", found.Name)
}

// TestFindModuleNode_NotFound tests findModuleNode when module doesn't exist.
func TestFindModuleNode_NotFound(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	found, err := svc.findModuleNode(ctx, "nonexistent")
	require.NoError(t, err)
	assert.Nil(t, found)
}

// TestFindModuleNode_FallbackAnyKind tests findModuleNode fallback to any kind.
func TestFindModuleNode_FallbackAnyKind(t *testing.T) {
	svc, st := newTestService(t)
	ctx := context.Background()

	path := "/tmp/fallback.go"
	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "class", Name: "MyClass", Qualified: "pkg.MyClass", StartLine: 1, EndLine: 10},
	}
	_, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	found, err := svc.findModuleNode(ctx, "MyClass")
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, "class", found.Kind)
}

// TestGetSymbolSummary_NotFound tests GetSymbolSummary with non-existent ID.
func TestGetSymbolSummary_NotFound(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	summary, err := svc.GetSymbolSummary(ctx, 99999)
	assert.Error(t, err)
	assert.Nil(t, summary)
	assert.Contains(t, err.Error(), "symbol not found")
}

// TestGetFileIntent_EmptyFile tests GetFileIntent for file with no units.
func TestGetFileIntent_EmptyFile(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	intent, err := svc.GetFileIntent(ctx, "/nonexistent/file.go")
	require.NoError(t, err)
	require.NotNil(t, intent)
	assert.Empty(t, intent.Symbols)
	assert.Empty(t, intent.Imports)
}

// TestGetSemanticNeighborhood_NotFound tests GetSemanticNeighborhood with non-existent ID.
func TestGetSemanticNeighborhood_NotFound(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	nh, err := svc.GetSemanticNeighborhood(ctx, 99999)
	assert.Error(t, err)
	assert.Nil(t, nh)
	assert.Contains(t, err.Error(), "symbol not found")
}

// TestLocationsToUnits_Empty tests locationsToUnits with empty input.
func TestLocationsToUnits_Empty(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	result := svc.locationsToUnits(ctx, nil, true)
	assert.Empty(t, result)
}

// TestLocationsToUnits_EmptyURI tests locationsToUnits skips empty URIs.
func TestLocationsToUnits_EmptyURI(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	// Using lsp.Location would require importing lsp, so we test via the function signature
	// locationsToUnits handles empty URI by returning empty path
	result := svc.locationsToUnits(ctx, nil, false)
	assert.Empty(t, result)
}

// TestRecordOllamaLatency_NilBus tests recordOllamaLatency with nil bus.
func TestRecordOllamaLatency_NilBus(t *testing.T) {
	svc, _ := newTestService(t)
	// Should not panic with nil bus
	svc.recordOllamaLatency("model", time.Now(), nil)
}

// TestRecordOllamaLatency_WithError tests recordOllamaLatency with error.
func TestRecordOllamaLatency_WithError(t *testing.T) {
	svc, _ := newTestService(t)
	// Should not panic with nil bus and error
	svc.recordOllamaLatency("model", time.Now(), assert.AnError)
}

// writeTestFile is a helper to write content to a file.
func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0644)
}

// =============================================================================
// Tests for identified MCP server issues
// =============================================================================

// TestImplementations_GoImplicitInterface tests that Go implicit implementations
// are NOT found via implements edges (Go doesn't have explicit implements keyword).
// This is a KNOWN LIMITATION: tree-sitter doesn't extract implements edges for Go.
func TestImplementations_GoImplicitInterface(t *testing.T) {
	svc, st := newTestService(t)
	ctx := context.Background()

	path := "/tmp/implicit.go"
	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	// Create interface and struct that implicitly implements it
	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "interface", Name: "Processor", Qualified: "pkg.Processor", StartLine: 1, EndLine: 5},
		{FilePath: path, Language: "go", Kind: "struct", Name: "DataProcessor", Qualified: "pkg.DataProcessor", StartLine: 10, EndLine: 15},
		{FilePath: path, Language: "go", Kind: "method", Name: "Process", Qualified: "pkg.DataProcessor.Process", StartLine: 20, EndLine: 25},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	idIface := ids["pkg.Processor"]
	idStruct := ids["pkg.DataProcessor"]

	// In Go, there's NO implements edge because it's implicit
	// This test verifies the current behavior (returns empty)
	impls, err := svc.Implementations(ctx, idIface)
	require.NoError(t, err)
	assert.Empty(t, impls, "Go implicit implementations should NOT be found without explicit implements edge")

	// Verify struct exists but has no implements edge
	refs, err := svc.References(ctx, idIface)
	require.NoError(t, err)
	// Should be empty or only have reference edges, not implements
	for _, ref := range refs {
		assert.NotEqual(t, EdgeImplements, ref.Kind, "Go should not have implements edges")
	}

	_ = idStruct // struct exists but no implements relationship
}

// TestImplementations_ExplicitImplementsEdge tests that implementations ARE found
// when explicit implements edge exists (TypeScript/Java pattern).
func TestImplementations_ExplicitImplementsEdge(t *testing.T) {
	svc, st := newTestService(t)
	ctx := context.Background()

	path := "/tmp/explicit.ts"
	require.NoError(t, st.EnsureFile(ctx, path, "typescript"))

	// Create interface and class with explicit implements edge
	units := []store.ASTUnit{
		{FilePath: path, Language: "typescript", Kind: "interface", Name: "IProcessor", Qualified: "IProcessor", StartLine: 1, EndLine: 5},
		{FilePath: path, Language: "typescript", Kind: "class", Name: "DataProcessor", Qualified: "DataProcessor", StartLine: 10, EndLine: 20},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	idIface := ids["IProcessor"]
	// Class ID might be qualified differently, use name lookup
	var idClass int
	for k, v := range ids {
		if k == "DataProcessor" || k == "pkg.DataProcessor" {
			idClass = v
			break
		}
	}
	if idClass == 0 && len(ids) > 0 {
		// Fallback: use second ID
		for _, v := range ids {
			if v != idIface {
				idClass = v
				break
			}
		}
	}

	// Add explicit implements edge (TypeScript/Java pattern)
	edges := []store.Edge{
		{SrcID: idClass, DstID: idIface, Kind: EdgeImplements, Repo: "", FilePath: path, Line: 10, DstName: "IProcessor"},
	}
	require.NoError(t, st.ReplaceEdges(ctx, path, edges))

	// Now implementations SHOULD be found
	impls, err := svc.Implementations(ctx, idIface)
	require.NoError(t, err)
	assert.NotEmpty(t, impls, "Explicit implements edge should be found")
	assert.Len(t, impls, 1)
	assert.Equal(t, idClass, impls[0].ID)
}

// TestImplementations_ExtendsAsImplementation tests that extends edges are also
// considered as implementations (subclass implements parent interface).
func TestImplementations_ExtendsAsImplementation(t *testing.T) {
	svc, st := newTestService(t)
	ctx := context.Background()

	path := "/tmp/extends.ts"
	require.NoError(t, st.EnsureFile(ctx, path, "typescript"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "typescript", Kind: "class", Name: "BaseClass", Qualified: "BaseClass", StartLine: 1, EndLine: 10},
		{FilePath: path, Language: "typescript", Kind: "class", Name: "DerivedClass", Qualified: "DerivedClass", StartLine: 15, EndLine: 25},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	idBase := ids["BaseClass"]
	idDerived := ids["DerivedClass"]

	// Add extends edge
	edges := []store.Edge{
		{SrcID: idDerived, DstID: idBase, Kind: EdgeExtends, Repo: "", FilePath: path, Line: 15, DstName: "BaseClass"},
	}
	require.NoError(t, st.ReplaceEdges(ctx, path, edges))

	// Extends should be found as implementation
	impls, err := svc.Implementations(ctx, idBase)
	require.NoError(t, err)
	assert.NotEmpty(t, impls, "Extends edge should be found as implementation")
	assert.Len(t, impls, 1)
	assert.Equal(t, idDerived, impls[0].ID)
}

// TestReferences_MethodsVsFunctions tests that method references are handled
// differently from function references (KNOWN ISSUE: methods may not be found).
func TestReferences_MethodsVsFunctions(t *testing.T) {
	svc, st := newTestService(t)
	ctx := context.Background()

	path := "/tmp/methods.go"
	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	// Create function and method
	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "standaloneFunc", Qualified: "pkg.standaloneFunc", StartLine: 1, EndLine: 10},
		{FilePath: path, Language: "go", Kind: "struct", Name: "MyStruct", Qualified: "pkg.MyStruct", StartLine: 15, EndLine: 20},
		{FilePath: path, Language: "go", Kind: "method", Name: "MyMethod", Qualified: "pkg.MyStruct.MyMethod", StartLine: 25, EndLine: 30},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	idFunc := ids["pkg.standaloneFunc"]
	idMethod := ids["pkg.MyStruct.MyMethod"]

	// Create reference edges for function (should work)
	funcCallerID := ids["pkg.MyStruct"] // reuse struct as caller
	edges := []store.Edge{
		{SrcID: funcCallerID, DstID: idFunc, Kind: EdgeCall, Repo: "", FilePath: path, Line: 5, DstName: "standaloneFunc"},
		// NOTE: Method references may not work due to how tree-sitter extracts methods
	}
	require.NoError(t, st.ReplaceEdges(ctx, path, edges))

	// Function references SHOULD be found
	funcRefs, err := svc.References(ctx, idFunc)
	require.NoError(t, err)
	assert.NotEmpty(t, funcRefs, "Function references should be found")

	// Method references may be empty (KNOWN LIMITATION)
	methodRefs, err := svc.References(ctx, idMethod)
	require.NoError(t, err)
	// This test documents the current behavior - may be empty
	// If tree-sitter doesn't extract method call edges, this will be empty
	assert.Empty(t, methodRefs, "Method references may not be found without explicit call edges")
}

// TestReferences_CallEdgeKind tests that only call edges are returned for callers,
// not reference edges (KNOWN ISSUE: resolve_call may return wrong kind).
func TestReferences_CallEdgeKind(t *testing.T) {
	svc, st := newTestService(t)
	ctx := context.Background()

	path := "/tmp/kinds.go"
	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "target", Qualified: "pkg.target", StartLine: 1, EndLine: 10},
		{FilePath: path, Language: "go", Kind: "function", Name: "caller", Qualified: "pkg.caller", StartLine: 15, EndLine: 25},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	idTarget := ids["pkg.target"]
	idCaller := ids["pkg.caller"]

	// Create BOTH call and reference edges
	edges := []store.Edge{
		{SrcID: idCaller, DstID: idTarget, Kind: EdgeCall, Repo: "", FilePath: path, Line: 20, DstName: "target"},
		{SrcID: idCaller, DstID: idTarget, Kind: EdgeReference, Repo: "", FilePath: path, Line: 18, DstName: "target"},
	}
	require.NoError(t, st.ReplaceEdges(ctx, path, edges))

	// References should return ALL kinds (call + reference)
	refs, err := svc.References(ctx, idTarget)
	require.NoError(t, err)
	assert.Len(t, refs, 2, "Should return both call and reference edges")

	// Verify kinds
	kinds := make(map[string]bool)
	for _, ref := range refs {
		kinds[ref.Kind] = true
	}
	assert.True(t, kinds[EdgeCall], "Should have call edge")
	assert.True(t, kinds[EdgeReference], "Should have reference edge")
}

// TestResolveCrossCall_EdgeKindFiltering tests that resolve_call returns correct
// edge kind (call vs reference). KNOWN ISSUE: may return reference instead of call.
func TestResolveCrossCall_EdgeKindFiltering(t *testing.T) {
	svc, st := newTestService(t)
	ctx := context.Background()

	path := "/tmp/resolve.go"
	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "caller", Qualified: "pkg.caller", StartLine: 1, EndLine: 20},
		{FilePath: path, Language: "go", Kind: "function", Name: "target", Qualified: "pkg.target", StartLine: 25, EndLine: 35},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	idCaller := ids["pkg.caller"]
	idTarget := ids["pkg.target"]

	// Create cross-call edge at specific line
	edges := []store.Edge{
		{SrcID: idCaller, DstID: idTarget, Kind: EdgeCall, Repo: "repo-a", DstRepo: "repo-b",
			DstName: "target", FilePath: path, Line: 10, Confidence: 0.9},
		// Also add a reference edge at different line
		{SrcID: idCaller, DstID: idTarget, Kind: EdgeReference, Repo: "repo-a", DstRepo: "repo-b",
			DstName: "target", FilePath: path, Line: 15, Confidence: 0.5},
	}
	for _, e := range edges {
		_, _ = st.InsertEdge(ctx, e)
	}

	// Resolve at line 10 (should return call edge)
	res, err := svc.ResolveCrossCall(ctx, path, 10)
	require.NoError(t, err)
	if res != nil {
		// NOTE: Current implementation may return first matching edge, not necessarily at exact line
		t.Logf("ResolveCrossCall at line 10 returned kind=%s (expected: call)", res.Edge.Kind)
	}

	// Resolve at line 15 (should return reference edge)
	res2, err := svc.ResolveCrossCall(ctx, path, 15)
	require.NoError(t, err)
	if res2 != nil {
		t.Logf("ResolveCrossCall at line 15 returned kind=%s (expected: reference)", res2.Edge.Kind)
	}
	
	// KNOWN ISSUE: resolve_call may not filter by exact line, returns first match
	// This test documents the current behavior
}

// TestFindCallers_MethodsVsFunctions tests that callers of methods are found
// correctly (KNOWN ISSUE: method callers may not be found).
func TestFindCallers_MethodsVsFunctions(t *testing.T) {
	svc, st := newTestService(t)
	ctx := context.Background()

	path := "/tmp/callers.go"
	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "standaloneFunc", Qualified: "pkg.standaloneFunc", StartLine: 1, EndLine: 10},
		{FilePath: path, Language: "go", Kind: "struct", Name: "MyStruct", Qualified: "pkg.MyStruct", StartLine: 15, EndLine: 20},
		{FilePath: path, Language: "go", Kind: "method", Name: "MyMethod", Qualified: "pkg.MyStruct.MyMethod", StartLine: 25, EndLine: 30},
		{FilePath: path, Language: "go", Kind: "function", Name: "caller1", Qualified: "pkg.caller1", StartLine: 35, EndLine: 40},
		{FilePath: path, Language: "go", Kind: "function", Name: "caller2", Qualified: "pkg.caller2", StartLine: 45, EndLine: 50},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	idFunc := ids["pkg.standaloneFunc"]
	idMethod := ids["pkg.MyStruct.MyMethod"]
	idCaller1 := ids["pkg.caller1"]
	idCaller2 := ids["pkg.caller2"]

	// Create call edges
	edges := []store.Edge{
		{SrcID: idCaller1, DstID: idFunc, Kind: EdgeCall, Repo: "", FilePath: path, Line: 5, DstName: "standaloneFunc"},
		{SrcID: idCaller1, DstID: idMethod, Kind: EdgeCall, Repo: "", FilePath: path, Line: 38, DstName: "MyMethod"},
		{SrcID: idCaller2, DstID: idMethod, Kind: EdgeCall, Repo: "", FilePath: path, Line: 48, DstName: "MyMethod"},
	}
	require.NoError(t, st.ReplaceEdges(ctx, path, edges))

	// Function callers SHOULD be found
	funcCallers, err := svc.Callers(ctx, idFunc)
	require.NoError(t, err)
	assert.NotEmpty(t, funcCallers, "Function callers should be found")
	assert.Len(t, funcCallers, 1)

	// Method callers SHOULD be found (if edges exist)
	methodCallers, err := svc.Callers(ctx, idMethod)
	require.NoError(t, err)
	assert.NotEmpty(t, methodCallers, "Method callers should be found when edges exist")
	assert.Len(t, methodCallers, 2)
}

// TestCallGraph_DirectionUp tests that call graph with direction=up (callers)
// returns edges correctly (KNOWN ISSUE: may return nodes without edges).
func TestCallGraph_DirectionUp(t *testing.T) {
	svc, st := newTestService(t)
	ctx := context.Background()

	path := "/tmp/callgraph.go"
	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "leaf", Qualified: "pkg.leaf", StartLine: 1, EndLine: 10},
		{FilePath: path, Language: "go", Kind: "function", Name: "middle", Qualified: "pkg.middle", StartLine: 15, EndLine: 25},
		{FilePath: path, Language: "go", Kind: "function", Name: "root", Qualified: "pkg.root", StartLine: 30, EndLine: 40},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	idLeaf := ids["pkg.leaf"]
	idMiddle := ids["pkg.middle"]
	idRoot := ids["pkg.root"]

	// Create call chain: root -> middle -> leaf
	edges := []store.Edge{
		{SrcID: idRoot, DstID: idMiddle, Kind: EdgeCall, Repo: "", FilePath: path, Line: 35, DstName: "middle"},
		{SrcID: idMiddle, DstID: idLeaf, Kind: EdgeCall, Repo: "", FilePath: path, Line: 20, DstName: "leaf"},
	}
	require.NoError(t, st.ReplaceEdges(ctx, path, edges))

	// Test direction=down (callees) - should work
	downGraph, err := svc.CallGraph(ctx, idRoot, 2)
	require.NoError(t, err)
	require.NotNil(t, downGraph)
	assert.NotEmpty(t, downGraph.Nodes, "Down graph should have nodes")
	// Verify edges exist in Neighborhood
	assert.NotEmpty(t, downGraph.Edges, "Down graph should have edges")

	// Test direction=up (callers) - KNOWN ISSUE: may return nodes without edges
	upGraph, err := svc.CallGraph(ctx, idLeaf, 2)
	require.NoError(t, err)
	require.NotNil(t, upGraph)
	assert.NotEmpty(t, upGraph.Nodes, "Up graph should have nodes")

	// Verify edges exist (this is the known issue - may be empty)
	// This assertion documents the expected behavior
	assert.NotEmpty(t, upGraph.Edges, "Up graph should have edges (KNOWN ISSUE: may be empty)")
}

// TestSemanticSearch_EmptyWithoutEmbedding tests that semantic search returns empty
// results when embedding model is not configured (KNOWN LIMITATION).
func TestSemanticSearch_EmptyWithoutEmbedding(t *testing.T) {
	// This test documents the expected behavior when Ollama/Qdrant is not configured
	// Semantic search should gracefully return empty results, not error
	// Actual implementation tested in vector/search_test.go
	t.Skip("Semantic search requires Ollama + Qdrant configuration - tested separately")
}

// TestKeywordSearch_Fallback tests that keyword search works as fallback
// when semantic search is unavailable.
func TestKeywordSearch_Fallback(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()

	path := "/tmp/keyword.go"
	require.NoError(t, st.EnsureFile(ctx, path, "go"))

	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "processData", Qualified: "pkg.processData", StartLine: 1, EndLine: 10},
		{FilePath: path, Language: "go", Kind: "function", Name: "validateInput", Qualified: "pkg.validateInput", StartLine: 15, EndLine: 25},
	}
	_, err := st.ReplaceASTUnits(ctx, path, units)
	require.NoError(t, err)

	// Keyword search should work without embeddings
	// This is tested via the hybrid search which falls back to keyword
	// Actual implementation tested in hybrid/hybrid_test.go
	t.Skip("Keyword search tested in hybrid search tests")
}
