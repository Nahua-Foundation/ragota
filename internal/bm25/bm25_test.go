package bm25

// Интеграционные тесты пакета bm25: Bleve работает поверх файловой
// системы (без внешних сервисов), поэтому используем t.TempDir().
// Покрываем happy-path (IndexDocs → Search → Count), фильтры по
// language/kind, удаление (Delete / DeleteByPath), Close-семантику и
// граничные случаи (пустой path, пустой Query.Text, no-op IndexDocs).

import (
	"context"
	"path/filepath"
	"testing"
)

func openTestIndex(t *testing.T) Index {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bm25.bleve")
	idx, err := Open(path, 1.2, 0.75)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func seedDocs() []Doc {
	return []Doc{
		{ID: "a", Path: "internal/api/handler.go", Language: "go", Kind: "function", Symbol: "ServeHTTP", Content: "func ServeHTTP serves http requests"},
		{ID: "b", Path: "internal/api/router.go", Language: "go", Kind: "function", Symbol: "Router", Content: "router dispatches incoming requests to handlers"},
		{ID: "c", Path: "frontend/app.ts", Language: "typescript", Kind: "class", Symbol: "App", Content: "the application root component"},
		{ID: "d", Path: "docs/readme.md", Language: "markdown", Kind: "chunk", Symbol: "", Content: "this project handles http server stuff"},
		{ID: "", Path: "skip", Content: "must be skipped because id is empty"},
	}
}

func TestOpen_EmptyPath(t *testing.T) {
	if _, err := Open("", 1.2, 0.75); err == nil {
		t.Fatalf("Open(\"\"): expected error, got nil")
	}
}

func TestIndexAndSearch_Basic(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()

	if err := idx.IndexDocs(ctx, seedDocs()); err != nil {
		t.Fatalf("IndexDocs: %v", err)
	}
	cnt, err := idx.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	// 4 валидных + 1 пропущенный (пустой ID).
	if cnt != 4 {
		t.Fatalf("Count = %d; want 4", cnt)
	}

	hits, err := idx.Search(ctx, Query{Text: "http", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("Search(http): no hits")
	}
	// Score должен быть > 0 и заполнены ключевые поля.
	h := hits[0]
	if h.Score <= 0 || h.ID == "" || h.Path == "" {
		t.Errorf("Hit looks empty: %+v", h)
	}
}

func TestSearch_FiltersByLanguageAndKind(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()
	_ = idx.IndexDocs(ctx, seedDocs())

	hits, err := idx.Search(ctx, Query{Text: "requests", Language: "go", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, h := range hits {
		if h.Language != "go" {
			t.Errorf("language filter leaked: %+v", h)
		}
	}
	if len(hits) == 0 {
		t.Errorf("Search(requests, go): no hits, want at least one")
	}

	hits2, err := idx.Search(ctx, Query{Text: "application", Kind: "class", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, h := range hits2 {
		if h.Kind != "class" {
			t.Errorf("kind filter leaked: %+v", h)
		}
	}
}

func TestSearch_EmptyText(t *testing.T) {
	idx := openTestIndex(t)
	hits, err := idx.Search(context.Background(), Query{Text: ""})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("Search(empty): %d hits, want 0", len(hits))
	}
}

func TestDelete(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()
	_ = idx.IndexDocs(ctx, seedDocs())

	if err := idx.Delete(ctx, []string{"a", "b"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	cnt, _ := idx.Count(ctx)
	if cnt != 2 {
		t.Errorf("Count after Delete = %d; want 2", cnt)
	}
}

func TestDeleteByPath(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()
	_ = idx.IndexDocs(ctx, seedDocs())

	if err := idx.DeleteByPath(ctx, "internal/api/handler.go"); err != nil {
		t.Fatalf("DeleteByPath: %v", err)
	}
	hits, _ := idx.Search(ctx, Query{Text: "ServeHTTP", Limit: 10})
	for _, h := range hits {
		if h.Path == "internal/api/handler.go" {
			t.Errorf("DeleteByPath did not remove %s: %+v", h.Path, h)
		}
	}
}

func TestIndexDocs_NoOp(t *testing.T) {
	idx := openTestIndex(t)
	if err := idx.IndexDocs(context.Background(), nil); err != nil {
		t.Errorf("IndexDocs(nil): %v", err)
	}
}

func TestClose_AndUseAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bm25.bleve")
	idx, err := Open(path, 1.2, 0.75)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Повторный Close — no-op.
	if err := idx.Close(); err != nil {
		t.Errorf("Close (second): %v", err)
	}
	if err := idx.IndexDocs(context.Background(), []Doc{{ID: "x", Content: "y"}}); err != ErrClosed {
		t.Errorf("IndexDocs after Close: err=%v; want ErrClosed", err)
	}
	if _, err := idx.Search(context.Background(), Query{Text: "y"}); err != ErrClosed {
		t.Errorf("Search after Close: err=%v; want ErrClosed", err)
	}
	if _, err := idx.Count(context.Background()); err != ErrClosed {
		t.Errorf("Count after Close: err=%v; want ErrClosed", err)
	}
}

func TestReopen_PersistsData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bm25.bleve")

	idx, err := Open(path, 1.2, 0.75)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := idx.IndexDocs(context.Background(), seedDocs()); err != nil {
		t.Fatalf("IndexDocs: %v", err)
	}
	_ = idx.Close()

	idx2, err := Open(path, 1.2, 0.75)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer idx2.Close()
	cnt, err := idx2.Count(context.Background())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if cnt != 4 {
		t.Errorf("Count after reopen = %d; want 4", cnt)
	}
}
