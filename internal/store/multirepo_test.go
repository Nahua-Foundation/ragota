package store

// Интеграционные тесты multi-repo: гарантия, что граф не пересекает
// границы репо при поиске и резолве (ResolvePendingEdges,
// EdgesByDstNameForLangRepo).
//
// Сценарий: две «репы» (alpha и beta) с одноимённой функцией Save и по
// одному «caller» в каждой. Ожидается:
//   - ResolvePendingEdges резолвит caller'а alpha только в Save из alpha;
//   - EdgesByDstNameForLangRepo с repo="alpha" возвращает только рёбра,
//     порождённые в репе alpha;
//   - FindASTUnits с repo="alpha" не видит Save из beta.

import (
	"context"
	"database/sql"
	"testing"
)

// seedRepo создаёт минимальный набор юнитов одной репы:
//
//	module callsite.go → caller (function)
//	module callee.go   → Save   (function)
//
// и одно неразрешённое ребро caller --call--> Save (dst_id=0,
// dst_name="Save"). Возвращает (callerID, saveID).
func seedRepo(t *testing.T, st *SQLite, repo, root string) (callerID, saveID int64) {
	t.Helper()
	ctx := context.Background()
	callsite := root + "/callsite.go"
	callee := root + "/callee.go"

	if err := st.EnsureFile(ctx, callsite, "go"); err != nil {
		t.Fatalf("EnsureFile callsite: %v", err)
	}
	if err := st.EnsureFile(ctx, callee, "go"); err != nil {
		t.Fatalf("EnsureFile callee: %v", err)
	}

	idsCaller, err := st.ReplaceASTUnits(ctx, callsite, []ASTUnit{
		{Repo: repo, FilePath: callsite, Language: "go", Kind: "function", Name: "caller", StartLine: 1, EndLine: 3},
	})
	if err != nil {
		t.Fatalf("ReplaceASTUnits callsite: %v", err)
	}
	callerID = idsCaller["caller"]

	idsCallee, err := st.ReplaceASTUnits(ctx, callee, []ASTUnit{
		{Repo: repo, FilePath: callee, Language: "go", Kind: "function", Name: "Save", StartLine: 1, EndLine: 3},
	})
	if err != nil {
		t.Fatalf("ReplaceASTUnits callee: %v", err)
	}
	saveID = idsCallee["Save"]

	// Edge: caller --call--> "Save" (резолв отложен).
	if err := st.ReplaceEdges(ctx, callsite, []Edge{
		{Repo: repo, SrcID: callerID, DstID: 0, Kind: "call", DstName: "Save", FilePath: callsite, Line: 2},
	}); err != nil {
		t.Fatalf("ReplaceEdges: %v", err)
	}
	return callerID, saveID
}

func TestMultiRepoResolvePendingDoesNotCrossRepos(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	_, saveA := seedRepo(t, st, "alpha", "/tmp/alpha")
	_, saveB := seedRepo(t, st, "beta", "/tmp/beta")

	n, err := st.ResolvePendingEdges(ctx)
	if err != nil {
		t.Fatalf("ResolvePendingEdges: %v", err)
	}
	// Должны резолвиться оба ребра, каждое — в свой Save.
	if n != 2 {
		t.Errorf("resolved=%d, want 2", n)
	}

	// Проверим, что caller alpha смотрит на Save alpha, а не на Save beta.
	rows, err := st.GetDB().QueryContext(ctx,
		`SELECT edges.dst_id, edges.repo FROM edges
		   JOIN ast_units AS src ON src.id = edges.src_id
		  WHERE src.repo = ? AND edges.kind = 'call'`, "alpha")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	got := []struct {
		dst  int64
		repo string
	}{}
	for rows.Next() {
		var d sql.NullInt64
		var r string
		if err := rows.Scan(&d, &r); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, struct {
			dst  int64
			repo string
		}{d.Int64, r})
	}
	if len(got) != 1 {
		t.Fatalf("ожидалось 1 ребро из alpha, получили %d: %+v", len(got), got)
	}
	if got[0].dst != saveA {
		t.Errorf("caller alpha должен указывать на Save alpha (id=%d), а получил dst_id=%d (Save beta id=%d)",
			saveA, got[0].dst, saveB)
	}
	if got[0].repo != "alpha" {
		t.Errorf("edge.repo=%q, want alpha", got[0].repo)
	}
}

func TestMultiRepoEdgesByDstNameForLangRepoScoped(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	seedRepo(t, st, "alpha", "/tmp/alpha")
	seedRepo(t, st, "beta", "/tmp/beta")
	if _, err := st.ResolvePendingEdges(ctx); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Без repo-фильтра — оба ребра (alpha+beta).
	all, err := st.EdgesByDstNameForLang(ctx, "Save", "call", "go")
	if err != nil {
		t.Fatalf("EdgesByDstNameForLang: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("без repo-фильтра ожидалось 2 ребра, получили %d", len(all))
	}

	// С repo="alpha" — только alpha.
	only, err := st.EdgesByDstNameForLangRepo(ctx, "Save", "call", "go", "alpha")
	if err != nil {
		t.Fatalf("EdgesByDstNameForLangRepo: %v", err)
	}
	if len(only) != 1 {
		t.Fatalf("с repo=alpha ожидалось 1 ребро, получили %d: %+v", len(only), only)
	}
	if only[0].Repo != "alpha" {
		t.Errorf("edge.repo=%q, want alpha", only[0].Repo)
	}
}

func TestMultiRepoFindASTUnitsScoped(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	seedRepo(t, st, "alpha", "/tmp/alpha")
	seedRepo(t, st, "beta", "/tmp/beta")

	all, err := st.FindASTUnits(ctx, "Save", "function", "go", "", 10)
	if err != nil {
		t.Fatalf("FindASTUnits all: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("без repo-фильтра ожидалось 2 Save, получили %d", len(all))
	}

	only, err := st.FindASTUnits(ctx, "Save", "function", "go", "alpha", 10)
	if err != nil {
		t.Fatalf("FindASTUnits alpha: %v", err)
	}
	if len(only) != 1 || only[0].Repo != "alpha" {
		t.Errorf("с repo=alpha ожидалось 1 unit alpha, получили %+v", only)
	}

	// '*' эквивалентно "" — без фильтра.
	star, _ := st.FindASTUnits(ctx, "Save", "function", "go", "*", 10)
	if len(star) != 2 {
		t.Errorf("с repo='*' ожидалось 2, получили %d", len(star))
	}
}
