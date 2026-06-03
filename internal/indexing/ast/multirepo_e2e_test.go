package astindex_test

// End-to-end multi-repo тест: воспроизводит сценарий из issue —
// два mock-репо (alpha и beta) в tmp dir с реальным маркером .git,
// общий FullScan через публичный Indexer и проверка, что граф никогда
// не пересекает границы репо.
//
// Сознательно не подключаем Qdrant/Ollama (vec.search_hybrid в e2e не
// проверяется — это требует реальных бэкендов). Здесь верифицируется
// то, что лежит в основе изоляции: репо-разметка AST units и резолв
// edges в пределах одной репы.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"ragota/internal/indexing/ast"
	"ragota/pkg/config"
	"ragota/pkg/repos"
	"ragota/internal/store"
)

const alphaCallee = `package alpha

// Save — определение в репе alpha.
func Save(x int) int { return x + 1 }
`

const alphaCaller = `package alpha

// AlphaCaller вызывает alpha.Save.
func AlphaCaller(v int) int {
	return Save(v)
}
`

const betaCallee = `package beta

// Save — одноимённая функция в репе beta.
func Save(s string) string { return s + "!" }
`

const betaCaller = `package beta

// BetaCaller вызывает beta.Save.
func BetaCaller(s string) string {
	return Save(s)
}
`

// mkRepo создаёт минимальную «репу»: каталог + пустой маркер .git + Go-модуль.
func mkRepo(t *testing.T, root, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git for %s: %v", name, err)
	}
	// Минимальный go.mod (не обязателен для парсера, но помогает findWorkspaceRoot).
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/"+name+"\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatalf("write go.mod for %s: %v", name, err)
	}
	for fname, content := range files {
		if err := os.WriteFile(filepath.Join(dir, fname), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s/%s: %v", name, fname, err)
		}
	}
	return dir
}

func TestMultiRepoE2E_FullScanIsolatesGraph(t *testing.T) {
	tmp := t.TempDir()

	mkRepo(t, tmp, "alpha", map[string]string{
		"callee.go":   alphaCallee,
		"callsite.go": alphaCaller,
	})
	mkRepo(t, tmp, "beta", map[string]string{
		"callee.go":   betaCallee,
		"callsite.go": betaCaller,
	})

	// 1. Auto-discovery — должна определить multi-repo workspace.
	discovered, err := repos.Discover(tmp)
	if err != nil {
		t.Fatalf("repos.Discover: %v", err)
	}
	if len(discovered) != 2 {
		t.Fatalf("ожидалось 2 репо, получено %d: %+v", len(discovered), discovered)
	}
	names := map[string]bool{}
	for _, r := range discovered {
		names[r.Name] = true
	}
	if !names["alpha"] || !names["beta"] {
		t.Fatalf("ожидались репо alpha и beta, получено %v", names)
	}
	resolver := repos.NewResolver(discovered)

	// 2. SQLite + Indexer.
	cfg := config.Default()
	cfg.Root = tmp
	cfg.Extensions = []string{".go"}

	dbPath := filepath.Join(tmp, "ragota.db")
	st, err := store.OpenFresh(dbPath, repos.Signature(discovered))
	if err != nil {
		t.Fatalf("store.OpenFresh: %v", err)
	}
	defer st.Close()

	idx := astindex.New(cfg, st)
	idx.SetRepoResolver(resolver)

	ctx := context.Background()
	if err := idx.FullScan(ctx); err != nil {
		t.Fatalf("FullScan: %v", err)
	}

	// 3. Проверка: FindASTUnits("Save", "*") видит обе репы.
	all, err := st.FindASTUnits(ctx, "Save", "function", "go", "*", 0)
	if err != nil {
		t.Fatalf("FindASTUnits: %v", err)
	}
	repoCount := map[string]int{}
	for _, u := range all {
		if u.Name == "Save" {
			repoCount[u.Repo]++
		}
	}
	if repoCount["alpha"] < 1 || repoCount["beta"] < 1 {
		t.Fatalf("ожидалось Save в обеих репах, получено %+v (units=%+v)", repoCount, all)
	}

	// 4. FindASTUnits с repo="alpha" не должен видеть Save из beta.
	alphaOnly, err := st.FindASTUnits(ctx, "Save", "function", "go", "alpha", 0)
	if err != nil {
		t.Fatalf("FindASTUnits alpha: %v", err)
	}
	for _, u := range alphaOnly {
		if u.Repo == "beta" {
			t.Fatalf("FindASTUnits(repo=alpha) вернул узел из beta: %+v", u)
		}
	}
	if len(alphaOnly) == 0 {
		t.Fatalf("FindASTUnits(repo=alpha) не нашёл Save из alpha")
	}

	// 5. EdgesByDstNameForLangRepo("Save","call","go","alpha") — только из alpha.
	alphaEdges, err := st.EdgesByDstNameForLangRepo(ctx, "Save", "call", "go", "alpha")
	if err != nil {
		t.Fatalf("EdgesByDstNameForLangRepo alpha: %v", err)
	}
	if len(alphaEdges) == 0 {
		t.Fatalf("ожидалось хотя бы одно ребро call->Save в alpha, edges=%v", alphaEdges)
	}
	for _, e := range alphaEdges {
		if e.Repo != "alpha" {
			t.Fatalf("ребро из чужой репы при фильтре alpha: repo=%q edge=%+v", e.Repo, e)
		}
	}

	// 6. Аналогично для beta — изоляция симметричная.
	betaEdges, err := st.EdgesByDstNameForLangRepo(ctx, "Save", "call", "go", "beta")
	if err != nil {
		t.Fatalf("EdgesByDstNameForLangRepo beta: %v", err)
	}
	for _, e := range betaEdges {
		if e.Repo != "beta" {
			t.Fatalf("ребро из чужой репы при фильтре beta: repo=%q edge=%+v", e.Repo, e)
		}
	}
}
