package store

// Интеграционные тесты SQLite-хранилища. Используется pure-Go драйвер
// modernc.org/sqlite (без CGO), поэтому тесты работают на CI без
// дополнительных зависимостей. БД создаётся во временной директории
// t.TempDir(), полный flow без моков:
//
//   Open → EnsureFile → ReplaceASTUnits → ReplaceEdges →
//   ResolvePendingEdges → EdgesFrom/EdgesTo/EdgesByDstName →
//   ExpandNeighbors → SetEmbedMeta/GetEmbedMeta.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *SQLite {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ai-tools.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// seedFile создаёт минимальный «файл с двумя AST units» (function f → g)
// и возвращает их id. Удобно для большинства тестов.
func seedFile(t *testing.T, st *SQLite, path string) (idF, idG int64) {
	t.Helper()
	ctx := context.Background()
	if err := st.EnsureFile(ctx, path, "go"); err != nil {
		t.Fatalf("EnsureFile: %v", err)
	}
	units := []ASTUnit{
		{FilePath: path, Language: "go", Kind: "function", Name: "f", Qualified: "pkg.f", StartLine: 1, EndLine: 5, StartByte: 0, EndByte: 50},
		{FilePath: path, Language: "go", Kind: "function", Name: "g", Qualified: "pkg.g", StartLine: 10, EndLine: 15, StartByte: 80, EndByte: 130},
	}
	if _, err := st.ReplaceASTUnits(ctx, path, units); err != nil {
		t.Fatalf("ReplaceASTUnits: %v", err)
	}
	persisted, err := st.ListASTUnitsByFile(ctx, path)
	if err != nil {
		t.Fatalf("ListASTUnitsByFile: %v", err)
	}
	if len(persisted) != 2 {
		t.Fatalf("ListASTUnitsByFile: %d units, want 2", len(persisted))
	}
	return persisted[0].ID, persisted[1].ID
}

func TestOpen_AndClose(t *testing.T) {
	st := openTestStore(t)
	if st == nil || st.db == nil {
		t.Fatal("Open returned nil store")
	}
}

func TestReplaceASTUnits_AndList(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	idF, idG := seedFile(t, st, "/tmp/a.go")

	u, err := st.GetASTUnit(ctx, idF)
	if err != nil || u == nil {
		t.Fatalf("GetASTUnit(idF): %v %v", u, err)
	}
	if u.Name != "f" || u.Kind != "function" || u.Qualified != "pkg.f" {
		t.Errorf("GetASTUnit: %+v", u)
	}

	// Повторный Replace полностью заменяет (старые id больше не существуют).
	if _, err := st.ReplaceASTUnits(ctx, "/tmp/a.go", []ASTUnit{
		{FilePath: "/tmp/a.go", Language: "go", Kind: "function", Name: "f2", StartLine: 1, EndLine: 2},
	}); err != nil {
		t.Fatalf("ReplaceASTUnits #2: %v", err)
	}
	if u, _ := st.GetASTUnit(ctx, idF); u != nil {
		t.Errorf("old idF still found: %+v", u)
	}
	if u, _ := st.GetASTUnit(ctx, idG); u != nil {
		t.Errorf("old idG still found: %+v", u)
	}
	left, _ := st.ListASTUnitsByFile(ctx, "/tmp/a.go")
	if len(left) != 1 || left[0].Name != "f2" {
		t.Errorf("after replace: %+v", left)
	}
}

func TestFindASTUnits(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	seedFile(t, st, "/tmp/a.go")

	hits, err := st.FindASTUnits(ctx, "f", "", "", "", 10)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	// Точное совпадение по name должно быть первым (см. ORDER BY в SQL).
	if len(hits) == 0 || hits[0].Name != "f" {
		t.Errorf("Find(f): %+v", hits)
	}

	hits2, err := st.FindASTUnits(ctx, "f", "function", "go", "", 10)
	if err != nil || len(hits2) == 0 {
		t.Errorf("Find(f, kind=function, lang=go): %+v %v", hits2, err)
	}

	hits3, _ := st.FindASTUnits(ctx, "no-such-symbol", "", "", "", 10)
	if len(hits3) != 0 {
		t.Errorf("Find(no-such): %+v", hits3)
	}
}

func TestUpdateASTParents_AndChildrenOf(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	idF, idG := seedFile(t, st, "/tmp/a.go")

	if err := st.UpdateASTParents(ctx, map[int64]int64{idG: idF}); err != nil {
		t.Fatalf("UpdateASTParents: %v", err)
	}
	kids, err := st.ChildrenOf(ctx, idF)
	if err != nil {
		t.Fatalf("ChildrenOf: %v", err)
	}
	if len(kids) != 1 || kids[0].ID != idG {
		t.Errorf("ChildrenOf(idF) = %+v; want [idG]", kids)
	}

	// Самоссылка должна игнорироваться.
	if err := st.UpdateASTParents(ctx, map[int64]int64{idF: idF}); err != nil {
		t.Fatalf("UpdateASTParents self: %v", err)
	}
	u, _ := st.GetASTUnit(ctx, idF)
	if u == nil || (u.ParentID.Valid && u.ParentID.Int64 == idF) {
		t.Errorf("self parent should be ignored: %+v", u)
	}

	// Пустой map — no-op.
	if err := st.UpdateASTParents(ctx, nil); err != nil {
		t.Errorf("UpdateASTParents(nil): %v", err)
	}
}

func TestReplaceEdges_AndEdgesFromTo(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	idF, idG := seedFile(t, st, "/tmp/a.go")

	edges := []Edge{
		{SrcID: idF, DstID: idG, Kind: "call", DstName: "pkg.g", FilePath: "/tmp/a.go", Line: 3},
		{SrcID: idF, DstID: 0, Kind: "call", DstName: "pkg.external", FilePath: "/tmp/a.go", Line: 4},
	}
	if err := st.ReplaceEdges(ctx, "/tmp/a.go", edges); err != nil {
		t.Fatalf("ReplaceEdges: %v", err)
	}

	out, err := st.EdgesFrom(ctx, idF, "call")
	if err != nil {
		t.Fatalf("EdgesFrom: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("EdgesFrom(idF, call) = %d; want 2", len(out))
	}

	in, err := st.EdgesTo(ctx, idG, "call")
	if err != nil {
		t.Fatalf("EdgesTo: %v", err)
	}
	if len(in) != 1 || in[0].SrcID != idF {
		t.Errorf("EdgesTo(idG): %+v", in)
	}

	// Без фильтра по kind — то же самое (других видов нет).
	noFilter, _ := st.EdgesFrom(ctx, idF, "")
	if len(noFilter) != 2 {
		t.Errorf("EdgesFrom(idF, no-kind) = %d; want 2", len(noFilter))
	}
}

func TestResolvePendingEdges(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	idF, idG := seedFile(t, st, "/tmp/a.go")

	// dst_id=0 + dst_name=pkg.g — должно срезолвиться в idG.
	if err := st.ReplaceEdges(ctx, "/tmp/a.go", []Edge{
		{SrcID: idF, DstID: 0, Kind: "call", DstName: "pkg.g", FilePath: "/tmp/a.go", Line: 1},
		{SrcID: idF, DstID: 0, Kind: "call", DstName: "pkg.unknown", FilePath: "/tmp/a.go", Line: 2},
	}); err != nil {
		t.Fatalf("ReplaceEdges: %v", err)
	}

	n, err := st.ResolvePendingEdges(ctx)
	if err != nil {
		t.Fatalf("ResolvePendingEdges: %v", err)
	}
	if n != 1 {
		t.Errorf("ResolvePendingEdges = %d; want 1", n)
	}

	out, _ := st.EdgesFrom(ctx, idF, "call")
	var resolved, pending int
	for _, e := range out {
		if e.DstID == idG {
			resolved++
		} else if e.DstID == 0 {
			pending++
		}
	}
	if resolved != 1 || pending != 1 {
		t.Errorf("after resolve: resolved=%d pending=%d", resolved, pending)
	}
}

func TestEdgesByDstName(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	idF, _ := seedFile(t, st, "/tmp/a.go")
	_ = st.ReplaceEdges(ctx, "/tmp/a.go", []Edge{
		{SrcID: idF, DstID: 0, Kind: "call", DstName: "pkg.target", FilePath: "/tmp/a.go", Line: 1},
		{SrcID: idF, DstID: 0, Kind: "import", DstName: "pkg.target", FilePath: "/tmp/a.go", Line: 2},
	})

	all, err := st.EdgesByDstName(ctx, "pkg.target", "")
	if err != nil {
		t.Fatalf("EdgesByDstName: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("EdgesByDstName(no kind) = %d; want 2", len(all))
	}

	calls, _ := st.EdgesByDstName(ctx, "pkg.target", "call")
	if len(calls) != 1 || calls[0].Kind != "call" {
		t.Errorf("EdgesByDstName(call): %+v", calls)
	}

	// Поиск по короткому имени должен матчить суффикс .name.
	short, _ := st.EdgesByDstName(ctx, "target", "")
	if len(short) != 2 {
		t.Errorf("EdgesByDstName(short) = %d; want 2", len(short))
	}
}

func TestExpandNeighbors(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	idF, idG := seedFile(t, st, "/tmp/a.go")
	// h в другом файле, для проверки depth>1.
	if err := st.EnsureFile(ctx, "/tmp/b.go", "go"); err != nil {
		t.Fatalf("EnsureFile b: %v", err)
	}
	if _, err := st.ReplaceASTUnits(ctx, "/tmp/b.go", []ASTUnit{
		{FilePath: "/tmp/b.go", Language: "go", Kind: "function", Name: "h", Qualified: "pkg.h", StartLine: 1, EndLine: 2},
	}); err != nil {
		t.Fatalf("ReplaceASTUnits b: %v", err)
	}
	hUnits, _ := st.ListASTUnitsByFile(ctx, "/tmp/b.go")
	idH := hUnits[0].ID

	// f → g, g → h
	_ = st.ReplaceEdges(ctx, "/tmp/a.go", []Edge{{SrcID: idF, DstID: idG, Kind: "call"}})
	_ = st.ReplaceEdges(ctx, "/tmp/b.go", []Edge{{SrcID: idG, DstID: idH, Kind: "call"}})

	// depth=1: только f и g.
	nodes, edges, err := st.ExpandNeighbors(ctx, idF, 1, []string{"call"})
	if err != nil {
		t.Fatalf("ExpandNeighbors depth=1: %v", err)
	}
	ids := map[int64]bool{}
	for _, n := range nodes {
		ids[n.ID] = true
	}
	if !ids[idF] || !ids[idG] || ids[idH] {
		t.Errorf("depth=1 nodes = %v; want {idF, idG}", ids)
	}
	if len(edges) != 1 {
		t.Errorf("depth=1 edges = %d; want 1", len(edges))
	}

	// depth=2: должен подтянуться и h.
	nodes2, _, err := st.ExpandNeighbors(ctx, idF, 2, []string{"call"})
	if err != nil {
		t.Fatalf("ExpandNeighbors depth=2: %v", err)
	}
	ids2 := map[int64]bool{}
	for _, n := range nodes2 {
		ids2[n.ID] = true
	}
	if !ids2[idH] {
		t.Errorf("depth=2 nodes missing idH: %v", ids2)
	}

	// Фильтр kinds: рёбер kind="import" нет → только сам корень.
	nodes3, _, _ := st.ExpandNeighbors(ctx, idF, 3, []string{"import"})
	if len(nodes3) != 1 || nodes3[0].ID != idF {
		t.Errorf("ExpandNeighbors(import): %+v", nodes3)
	}
}

func TestEmbedMeta(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	got, err := st.GetEmbedMeta(ctx, "missing")
	if err != nil {
		t.Fatalf("GetEmbedMeta(missing): %v", err)
	}
	if got != nil {
		t.Errorf("GetEmbedMeta(missing) = %+v; want nil", got)
	}

	if err := st.SetEmbedMeta(ctx, EmbedMeta{Collection: "code", Model: "nomic-embed-text", Dim: 768}); err != nil {
		t.Fatalf("SetEmbedMeta: %v", err)
	}
	got, err = st.GetEmbedMeta(ctx, "code")
	if err != nil || got == nil {
		t.Fatalf("GetEmbedMeta: %v %v", got, err)
	}
	if got.Model != "nomic-embed-text" || got.Dim != 768 {
		t.Errorf("GetEmbedMeta: %+v", got)
	}

	// Upsert: меняем модель, проверяем что обновилось.
	if err := st.SetEmbedMeta(ctx, EmbedMeta{Collection: "code", Model: "qwen3-embedding", Dim: 1024}); err != nil {
		t.Fatalf("SetEmbedMeta upsert: %v", err)
	}
	got2, _ := st.GetEmbedMeta(ctx, "code")
	if got2 == nil || got2.Model != "qwen3-embedding" || got2.Dim != 1024 {
		t.Errorf("after upsert: %+v", got2)
	}
}

// TestNullParentID — sanity-check: sql.NullInt64{Valid:false} корректно
// сохраняется как NULL и читается обратно.
func TestNullParentID(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	_ = st.EnsureFile(ctx, "/tmp/c.go", "go")
	_, err := st.ReplaceASTUnits(ctx, "/tmp/c.go", []ASTUnit{
		{FilePath: "/tmp/c.go", Language: "go", Kind: "function", Name: "x", StartLine: 1, EndLine: 1, ParentID: sql.NullInt64{Valid: false}},
	})
	if err != nil {
		t.Fatalf("ReplaceASTUnits: %v", err)
	}
	units, _ := st.ListASTUnitsByFile(ctx, "/tmp/c.go")
	if len(units) != 1 || units[0].ParentID.Valid {
		t.Errorf("ParentID should be NULL: %+v", units)
	}
}
