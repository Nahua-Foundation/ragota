package symbols

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"ragota/internal/indexing/ast"
	"ragota/pkg/config"
	"ragota/internal/search/graph"
	"ragota/internal/store"
)

func TestComprehensiveProjectSupport(t *testing.T) {
	_, filename, _, _ := runtime.Caller(0)
	projectRoot := filepath.Join(filepath.Dir(filename), "..", "..", "..")
	testDataDir := filepath.Join(projectRoot, "tests", "testprojects")

	tmpDir, err := os.MkdirTemp("", "ragota-comprehensive")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.Default()
	idx := astindex.New(cfg, st)
	gs := graph.New(cfg, st)
	ctx := context.Background()

	// Индексируем всё
	err = filepath.Walk(testDataDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext == ".go" || ext == ".java" || ext == ".ts" || ext == ".js" {
			return idx.IndexFile(ctx, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Failed to index test data: %v", err)
	}

	if _, err := st.ResolvePendingEdges(ctx, nil); err != nil {
		t.Fatalf("Failed to resolve edges: %v", err)
	}

	svc := New(st, gs, nil)

	t.Run("GoSupport", func(t *testing.T) {
		// 1. Definition
		defs, _ := svc.FindDefinition(ctx, "Equaler")
		if len(defs) == 0 {
			t.Error("Should find Go interface Equaler")
		}

		// 2. Implicit Implementation
		impls, _ := svc.FindImplementations(ctx, "Equaler")
		found := false
		for _, impl := range impls {
			if impl.Name == "MyInt" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Should find MyInt as implicit implementation of Equaler, found %d", len(impls))
		}

		// 3. Callers
		callers, _ := svc.FindCallers(ctx, "Equal")
		if len(callers) == 0 {
			t.Error("Should find callers of Equal")
		}

		// 4. Callees
		mainDef, _ := svc.FindDefinition(ctx, "main")
		if len(mainDef) > 0 {
			callees, _ := svc.FindCallees(ctx, "main")
			found := false
			for _, c := range callees {
				if c.Name == "Equal" {
					found = true
					break
				}
			}
			if !found {
				t.Error("Should find Equal as callee of main")
			}
		}

		// 5. Method references
		refs, _ := svc.FindReferences(ctx, "MyInt.Equal")
		foundCall := false
		for _, r := range refs {
			if strings.Contains(r.FilePath, "main.go") {
				foundCall = true
				break
			}
		}
		if !foundCall {
			t.Error("FindReferences(MyInt.Equal) should find call in main.go")
		}
	})

	t.Run("JavaSupport", func(t *testing.T) {
		// 1. Qualified Definition
		defs, _ := svc.FindDefinition(ctx, "com.example.api.Service")
		if len(defs) == 0 {
			t.Error("Should find Java interface by qualified name")
		}

		// 2. Implementation
		impls, _ := svc.FindImplementations(ctx, "Service")
		found := false
		for _, impl := range impls {
			if impl.Name == "ServiceImpl" {
				found = true
				break
			}
		}
		if !found {
			t.Error("Should find ServiceImpl as implementation of Service")
		}

		// 3. References
		refs, _ := svc.FindReferences(ctx, "Service")
		if len(refs) == 0 {
			t.Error("Should find references to Service")
		}

		// 4. References should include implementations
		foundImpl := false
		for _, r := range refs {
			if strings.Contains(r.FilePath, "ServiceImpl.java") {
				foundImpl = true
				break
			}
		}
		if !foundImpl {
			t.Error("FindReferences(Service) should include ServiceImpl.java")
		}
		// 5. Method references (should find calls)
		refs, _ = svc.FindReferences(ctx, "ServiceImpl.execute")
		foundCall := false
		for _, r := range refs {
			if strings.Contains(r.FilePath, "Main.java") {
				foundCall = true
				break
			}
		}
		if !foundCall {
			t.Error("FindReferences(ServiceImpl.execute) should find call in Main.java")
		}
	})

	t.Run("TypeScriptSupport", func(t *testing.T) {
		// 1. Namespace Definition
		defs, _ := svc.FindDefinition(ctx, "Data.UserStore")
		if len(defs) == 0 {
			t.Error("Should find TS class inside namespace")
		}

		// 2. Callers
		callers, _ := svc.FindCallers(ctx, "getUser")
		if len(callers) == 0 {
			t.Error("Should find callers of getUser in app.ts")
		}

		// 3. References from another file
		refs, _ := svc.FindReferences(ctx, "User")
		if len(refs) == 0 {
			t.Error("Should find references to User interface")
		}

		// 4. References should include implementations/usages
		foundUsage := false
		for _, r := range refs {
			if strings.Contains(r.FilePath, "UserService.ts") {
				foundUsage = true
				break
			}
		}
		if !foundUsage {
			t.Error("FindReferences(User) should include UserService.ts")
		}

		// 5. Method references
		refs, _ = svc.FindReferences(ctx, "UserStore.getUser")
		foundCall := false
		for _, r := range refs {
			if strings.Contains(r.FilePath, "app.ts") {
				foundCall = true
				break
			}
		}
		if !foundCall {
			t.Error("FindReferences(UserStore.getUser) should find call in app.ts")
		}
	})

	t.Run("GraphTools", func(t *testing.T) {
		// Test ExpandNeighbors
		defs, _ := svc.FindDefinition(ctx, "Data.UserStore")
		if len(defs) > 0 {
			target := defs[0]
			nb, err := gs.ExpandNeighbors(ctx, target.ID, 1, nil)
			if err != nil {
				t.Fatalf("ExpandNeighbors failed: %v", err)
			}
			if len(nb.Nodes) == 0 {
				t.Error("ExpandNeighbors should return some nodes")
			}

			// Test GetExecutionContext
			execCtx, err := gs.GetExecutionContext(ctx, target.ID)
			if err != nil {
				t.Fatalf("GetExecutionContext failed: %v", err)
			}
			if execCtx.Definition.ID != target.ID {
				t.Errorf("GetExecutionContext returned wrong definition: %d != %d", execCtx.Definition.ID, target.ID)
			}

			// Test DependencyGraph
			t.Run("GoDep", func(t *testing.T) {
				// В Go "модулем" считается пакет.
				gh, err := gs.DependencyGraph(ctx, "main", 1)
				if err != nil {
					t.Fatalf("DependencyGraph failed: %v", err)
				}
				if len(gh.Nodes) == 0 {
					t.Error("DependencyGraph(main) should not be empty")
				}
			})

			t.Run("JavaDep", func(t *testing.T) {
				gh, err := gs.DependencyGraph(ctx, "Main.java", 1)
				if err != nil {
					t.Fatalf("DependencyGraph failed: %v", err)
				}
				found := false
				for _, e := range gh.Edges {
					if e.Kind == graph.EdgeImport {
						found = true
						break
					}
				}
				if !found {
					t.Error("DependencyGraph(Main.java) should have import edges")
				}
			})

			t.Run("TSDepRelative", func(t *testing.T) {
				gh, err := gs.DependencyGraph(ctx, "app.ts", 1)
				if err != nil {
					t.Fatalf("DependencyGraph failed: %v", err)
				}
				foundResolved := false
				for _, e := range gh.Edges {
					if e.Kind == graph.EdgeImport && e.DstID != 0 {
						foundResolved = true
						break
					}
				}
				if !foundResolved {
					t.Error("DependencyGraph(app.ts) should have resolved import edges")
				}
			})
			getUserDefs, _ := svc.FindDefinition(ctx, "getUser")
			if len(getUserDefs) > 0 {
				target := getUserDefs[0]
				cg, err := gs.CallGraph(ctx, target.ID, 2)
				if err != nil {
					t.Fatalf("CallGraph failed: %v", err)
				}
				if len(cg.Nodes) == 0 {
					t.Error("CallGraph should return nodes")
				}

				// Test Semantic tools (without LLM, should return deterministic part)
				summary, err := gs.GetSymbolSummary(ctx, target.ID)
				if err != nil {
					t.Fatalf("GetSymbolSummary failed: %v", err)
				}
				if summary.Name != target.Name {
					t.Errorf("GetSymbolSummary name mismatch: got %s, want %s", summary.Name, target.Name)
				}

				intent, err := gs.GetFileIntent(ctx, target.FilePath)
				if err != nil {
					t.Fatalf("GetFileIntent failed: %v", err)
				}
				if len(intent.Symbols) == 0 {
					t.Error("GetFileIntent should return some symbols")
				}

				neighborhood, err := gs.GetSemanticNeighborhood(ctx, target.ID)
				if err != nil {
					t.Fatalf("GetSemanticNeighborhood failed: %v", err)
				}
				if neighborhood.Center != target.Name {
					t.Errorf("GetSemanticNeighborhood center mismatch: got %s, want %s", neighborhood.Center, target.Name)
				}
			}

			// Test TraverseGraph
			tr, err := gs.TraverseGraph(ctx, target.ID, 2, []string{"reference", "call"})
			if err != nil {
				t.Fatalf("TraverseGraph failed: %v", err)
			}
			if len(tr.Nodes) == 0 {
				t.Error("TraverseGraph should return nodes")
			}
		}

		// Test AST tools
		fileSyms, _ := svc.FileSymbols(ctx, filepath.Join(testDataDir, "ts", "services", "UserService.ts"))
		if len(fileSyms) == 0 {
			t.Error("FileSymbols should return symbols")
		}
		var userStore *store.ASTUnit
		for _, s := range fileSyms {
			if s.Name == "UserStore" {
				userStore = &s
				break
			}
		}
		if userStore != nil {
			parent, _ := svc.Parent(ctx, userStore.ID)
			if parent == nil || parent.Name != "Data" {
				t.Errorf("Parent of UserStore should be Data, got %v", parent)
			}

			children, _ := svc.Children(ctx, userStore.ID)
			found := false
			for _, c := range children {
				if c.Name == "getUser" {
					found = true
					break
				}
			}
			if !found {
				t.Error("Children of UserStore should include getUser")
			}
		}

		// Test SurroundingContext
		if len(defs) > 0 {
			ctxText, err := svc.SurroundingContext(ctx, defs[0].ID, 2, 2)
			if err != nil {
				t.Fatalf("SurroundingContext failed: %v", err)
			}
			if !strings.Contains(ctxText, "class UserStore") {
				t.Error("SurroundingContext should contain class definition")
			}
		}
	})
}
