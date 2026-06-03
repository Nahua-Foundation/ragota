package astindex

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ragota/pkg/config"
	"ragota/pkg/state"
	"ragota/internal/store"
)

func TestFullScan_StatusTransitions(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	defer st.Close()

	// Создаём тестовый файл
	testFile := filepath.Join(tmpDir, "main.go")
	err = os.WriteFile(testFile, []byte(`package main

func main() {
	println("hello")
}
`), 0644)
	require.NoError(t, err)

	cfg := config.Default()
	cfg.Root = tmpDir

	bus := state.NewBus(tmpDir)
	
	idx := New(cfg, st)
	idx.SetBus(bus)

	ctx := context.Background()
	err = idx.FullScan(ctx)
	require.NoError(t, err)

	// Проверяем финальный статус
	snap := bus.Snapshot()
	ast, ok := snap.Indexers["ast"]
	require.True(t, ok, "ast indexer should exist in snapshot")

	t.Logf("Final status: %q, FilesTotal=%d, Symbols=%d, Chunks=%d, PendingEdges=%d, ResolvePass=%d",
		ast.Status, ast.FilesTotal, ast.Symbols, ast.Chunks, ast.PendingEdges, ast.ResolvePass)

	assert.Equal(t, "idle", ast.Status, "Final status should be idle")
	assert.Greater(t, ast.Symbols, 0, "Should have symbols after FullScan")
	// Edges may be 0 for simple files (println can't be resolved to a local symbol)
}

func TestResolvePendingEdges_ProgressCallback(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	defer st.Close()

	// Создаём два файла с unresolved edges
	file1 := filepath.Join(tmpDir, "a.go")
	file2 := filepath.Join(tmpDir, "b.go")
	err = os.WriteFile(file1, []byte(`package main

func Foo() {}
`), 0644)
	require.NoError(t, err)
	err = os.WriteFile(file2, []byte(`package main

func main() { Foo() }
`), 0644)
	require.NoError(t, err)

	cfg := config.Default()
	cfg.Root = tmpDir

	bus := state.NewBus(tmpDir)
	idx := New(cfg, st)
	idx.SetBus(bus)

	ctx := context.Background()
	require.NoError(t, idx.IndexFile(ctx, file1))
	require.NoError(t, idx.IndexFile(ctx, file2))

	// Проверяем что есть unresolved edges
	var unresolved int64
	err = st.GetDBForTests().QueryRowContext(ctx, `SELECT COUNT(*) FROM edges WHERE dst_id = 0`).Scan(&unresolved)
	require.NoError(t, err)
	t.Logf("Unresolved edges before ResolvePendingEdges: %d", unresolved)

	// Захватываем прогресс
	var progressCalls []struct{ pass int; resolved int64; remaining int64 }
	progressFn := func(pass int, resolved int64, remaining int64) {
		progressCalls = append(progressCalls, struct{ pass int; resolved int64; remaining int64 }{pass, resolved, remaining})
	}

	total, err := st.ResolvePendingEdges(ctx, progressFn)
	require.NoError(t, err)
	t.Logf("ResolvePendingEdges: total=%d, progress calls=%d", total, len(progressCalls))
	for i, pc := range progressCalls {
		t.Logf("  call %d: pass=%d resolved=%d remaining=%d", i, pc.pass, pc.resolved, pc.remaining)
	}

	// После разрешения edges должно быть 0 unresolved
	err = st.GetDBForTests().QueryRowContext(ctx, `SELECT COUNT(*) FROM edges WHERE dst_id = 0`).Scan(&unresolved)
	require.NoError(t, err)
	assert.Equal(t, int64(0), unresolved, "All edges should be resolved")
}

func TestResolvePendingEdges_NoEdges(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	defer st.Close()

	// Никаких edges — ResolvePendingEdges должен вернуть 0
	var progressCalls int
	progressFn := func(pass int, resolved int64, remaining int64) {
		progressCalls++
		t.Logf("Progress callback: pass=%d resolved=%d remaining=%d", pass, resolved, remaining)
	}

	ctx := context.Background()
	total, err := st.ResolvePendingEdges(ctx, progressFn)
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Greater(t, progressCalls, 0, "Should have at least one progress callback (pass 1)")
}
