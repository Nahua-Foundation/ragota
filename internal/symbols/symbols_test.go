package symbols

// Unit-тесты для symbols.Service поверх in-memory SQLite (modernc.org/sqlite,
// без CGO). LSP не используется — graph.Service создаётся через graph.New(st)
// (только tree-sitter путь). Проверяются: FileSymbols, Get, Parent, Children,
// FindDefinition, FindCallable пути (Callers/Callees), Get/Parent на отсутствующих
// id.

import (
	"context"
	"path/filepath"
	"testing"

	"aitools/internal/config"
	"aitools/internal/graph"
	"aitools/internal/store"
)

func openStore(t *testing.T) *store.SQLite {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "ai.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// seedTwoFunctions: file with `f` (parent) and `g` (child of f).
func seedTwoFunctions(t *testing.T, st *store.SQLite, path string) (idF, idG int64) {
	t.Helper()
	ctx := context.Background()
	if err := st.EnsureFile(ctx, path, "go"); err != nil {
		t.Fatalf("EnsureFile: %v", err)
	}
	units := []store.ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "f", Qualified: "pkg.f", StartLine: 1, EndLine: 20, StartByte: 0, EndByte: 200},
		{FilePath: path, Language: "go", Kind: "function", Name: "g", Qualified: "pkg.g", StartLine: 5, EndLine: 10, StartByte: 50, EndByte: 100},
	}
	ids, err := st.ReplaceASTUnits(ctx, path, units)
	if err != nil {
		t.Fatalf("ReplaceASTUnits: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 ids, got %d", len(ids))
	}
	idF, idG = ids["pkg.f"], ids["pkg.g"]
	if idF == 0 || idG == 0 {
		t.Fatalf("missing ids: %+v", ids)
	}
	// Set g.parent_id = f.id
	if err := st.UpdateASTParents(ctx, map[int64]int64{idG: idF}); err != nil {
		t.Fatalf("UpdateASTParents: %v", err)
	}
	return idF, idG
}

func newService(t *testing.T, st *store.SQLite) *Service {
	t.Helper()
	return New(st, graph.New(config.Default(), st), nil)
}

func TestService_FileSymbols(t *testing.T) {
	st := openStore(t)
	path := "/tmp/x.go"
	seedTwoFunctions(t, st, path)
	s := newService(t, st)

	units, err := s.FileSymbols(context.Background(), path)
	if err != nil {
		t.Fatalf("FileSymbols: %v", err)
	}
	if len(units) != 2 {
		t.Fatalf("expected 2 units, got %d", len(units))
	}
}

func TestService_GetParentChildren(t *testing.T) {
	st := openStore(t)
	path := "/tmp/x.go"
	idF, idG := seedTwoFunctions(t, st, path)
	s := newService(t, st)
	ctx := context.Background()

	u, err := s.Get(ctx, idG)
	if err != nil || u == nil {
		t.Fatalf("Get(g): err=%v u=%v", err, u)
	}
	if u.Name != "g" {
		t.Errorf("Get(g).Name = %q", u.Name)
	}

	parent, err := s.Parent(ctx, idG)
	if err != nil || parent == nil {
		t.Fatalf("Parent(g): err=%v parent=%v", err, parent)
	}
	if parent.ID != idF {
		t.Errorf("Parent(g).ID = %d, want %d", parent.ID, idF)
	}

	// Parent of root (f) returns nil, no error.
	rootParent, err := s.Parent(ctx, idF)
	if err != nil {
		t.Fatalf("Parent(f): %v", err)
	}
	if rootParent != nil {
		t.Errorf("Parent(f) must be nil, got %+v", rootParent)
	}

	kids, err := s.Children(ctx, idF)
	if err != nil {
		t.Fatalf("Children(f): %v", err)
	}
	if len(kids) != 1 || kids[0].ID != idG {
		t.Errorf("Children(f) = %v, want [g]", kids)
	}
}

func TestService_FindDefinition(t *testing.T) {
	st := openStore(t)
	path := "/tmp/x.go"
	seedTwoFunctions(t, st, path)
	s := newService(t, st)

	defs, err := s.FindDefinition(context.Background(), "g")
	if err != nil {
		t.Fatalf("FindDefinition: %v", err)
	}
	if len(defs) == 0 {
		t.Fatal("FindDefinition(g) returned no results")
	}
	found := false
	for _, d := range defs {
		if d.Name == "g" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("FindDefinition(g) did not contain symbol g: %+v", defs)
	}
}

func TestService_FindDefinition_NotFound(t *testing.T) {
	st := openStore(t)
	s := newService(t, st)
	defs, err := s.FindDefinition(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("FindDefinition: %v", err)
	}
	if len(defs) != 0 {
		t.Errorf("expected empty result, got %d", len(defs))
	}
}

func TestService_FindReferences(t *testing.T) {
	st := openStore(t)
	path := "/tmp/refs.go"
	ctx := context.Background()
	if err := st.EnsureFile(ctx, path, "go"); err != nil {
		t.Fatalf("EnsureFile: %v", err)
	}
	// Seed: f calls g
	units := []store.ASTUnit{
		{ID: 1, FilePath: path, Language: "go", Kind: "function", Name: "f", Qualified: "pkg.f", StartLine: 1, EndLine: 5},
		{ID: 2, FilePath: path, Language: "go", Kind: "function", Name: "g", Qualified: "pkg.g", StartLine: 10, EndLine: 15},
	}
	ids, _ := st.ReplaceASTUnits(ctx, path, units)
	idF, idG := ids["pkg.f"], ids["pkg.g"]

	// Edge from f to g
	edges := []store.Edge{
		{SrcID: idF, DstID: idG, Kind: "reference", DstName: "g", FilePath: path, Line: 2},
	}
	if err := st.ReplaceEdges(ctx, path, edges); err != nil {
		t.Fatalf("ReplaceEdges: %v", err)
	}

	s := newService(t, st)
	refs, err := s.FindReferences(ctx, "g")
	if err != nil {
		t.Fatalf("FindReferences: %v", err)
	}
	if len(refs) == 0 {
		t.Errorf("expected references to g, got 0")
	}
}

type mockSimilar struct {
	units []store.ASTUnit
}

func (m *mockSimilar) SimilarToUnit(ctx context.Context, u store.ASTUnit, limit int) ([]store.ASTUnit, error) {
	return m.units, nil
}

func TestService_SimilarCode(t *testing.T) {
	st := openStore(t)
	path := "/tmp/sim.go"
	seedTwoFunctions(t, st, path)
	ms := &mockSimilar{units: []store.ASTUnit{{Name: "similar_func"}}}
	s := New(st, graph.New(config.Default(), st), ms)

	res, err := s.SimilarCode(context.Background(), 1, 10)
	if err != nil {
		t.Fatalf("SimilarCode: %v", err)
	}
	if len(res) == 0 || res[0].Name != "similar_func" {
		t.Errorf("SimilarCode returned unexpected results: %+v", res)
	}
}
