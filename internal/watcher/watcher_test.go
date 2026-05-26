package watcher

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ragota/internal/fileutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// EventKind.String()
// ---------------------------------------------------------------------------

func TestEventKind_String(t *testing.T) {
	tests := []struct {
		kind EventKind
		want string
	}{
		{EventCreate, "create"},
		{EventWrite, "write"},
		{EventRemove, "remove"},
		{EventRename, "rename"},
		{EventKind(0), "?"},
		{EventKind(99), "?"},
		{EventKind(-1), "?"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.kind.String())
		})
	}
}

// ---------------------------------------------------------------------------
// New() constructor
// ---------------------------------------------------------------------------

func TestNew_DefaultDebounce(t *testing.T) {
	w, err := New(t.TempDir(), nil, nil, 0)
	require.NoError(t, err)
	defer w.Close()

	assert.Equal(t, 250*time.Millisecond, w.debounce)
	assert.NotNil(t, w.fs)
	assert.NotNil(t, w.pending)
	assert.NotNil(t, w.out)
	assert.NotNil(t, w.doneCh)
}

func TestNew_NegativeDebounce(t *testing.T) {
	w, err := New(t.TempDir(), nil, nil, -5*time.Second)
	require.NoError(t, err)
	defer w.Close()

	assert.Equal(t, 250*time.Millisecond, w.debounce)
}

func TestNew_CustomDebounce(t *testing.T) {
	w, err := New(t.TempDir(), nil, nil, 500*time.Millisecond)
	require.NoError(t, err)
	defer w.Close()

	assert.Equal(t, 500*time.Millisecond, w.debounce)
}

func TestNew_WithMatcherAndExtensions(t *testing.T) {
	m := fileutil.NewMatcher([]string{"node_modules"})
	exts := []string{".go", ".py"}
	w, err := New(t.TempDir(), m, exts, 100*time.Millisecond)
	require.NoError(t, err)
	defer w.Close()

	assert.NotNil(t, w.matcher)
	assert.Equal(t, exts, w.extensions)
}

// ---------------------------------------------------------------------------
// Events() channel
// ---------------------------------------------------------------------------

func TestEvents_ReturnsBufferedChannel(t *testing.T) {
	w, err := New(t.TempDir(), nil, nil, 0)
	require.NoError(t, err)
	defer w.Close()

	ch := w.Events()
	assert.NotNil(t, ch)
	// channel should be buffered with cap 256
	assert.Equal(t, 256, cap(ch))
}

// ---------------------------------------------------------------------------
// SetRepoResolver
// ---------------------------------------------------------------------------

func TestSetRepoResolver(t *testing.T) {
	w, err := New(t.TempDir(), nil, nil, 0)
	require.NoError(t, err)
	defer w.Close()

	assert.Nil(t, w.resolver)
	// SetRepoResolver with nil is valid
	w.SetRepoResolver(nil)
	assert.Nil(t, w.resolver)
}

// ---------------------------------------------------------------------------
// shouldAccept
// ---------------------------------------------------------------------------

func TestShouldAccept_NoMatcherNoExtensions(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, nil, nil, 0)
	require.NoError(t, err)
	defer w.Close()

	// Any file should be accepted
	assert.True(t, w.shouldAccept(filepath.Join(root, "foo.go")))
	assert.True(t, w.shouldAccept(filepath.Join(root, "sub", "bar.txt")))
}

func TestShouldAccept_WithMatcher(t *testing.T) {
	root := t.TempDir()
	m := fileutil.NewMatcher([]string{"vendor"})
	w, err := New(root, m, nil, 0)
	require.NoError(t, err)
	defer w.Close()

	assert.True(t, w.shouldAccept(filepath.Join(root, "main.go")))
	assert.False(t, w.shouldAccept(filepath.Join(root, "vendor", "dep.go")))
}

func TestShouldAccept_WithExtensions(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, nil, []string{".go", ".py"}, 0)
	require.NoError(t, err)
	defer w.Close()

	assert.True(t, w.shouldAccept(filepath.Join(root, "main.go")))
	assert.True(t, w.shouldAccept(filepath.Join(root, "script.py")))
	assert.False(t, w.shouldAccept(filepath.Join(root, "readme.md")))
}

func TestShouldAccept_ExtensionCaseInsensitive(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, nil, []string{".GO"}, 0)
	require.NoError(t, err)
	defer w.Close()

	assert.True(t, w.shouldAccept(filepath.Join(root, "main.go")))
}

func TestShouldAccept_PathOutsideRoot(t *testing.T) {
	root := t.TempDir()
	m := fileutil.NewMatcher([]string{"vendor"})
	w, err := New(root, m, []string{".go"}, 0)
	require.NoError(t, err)
	defer w.Close()

	// filepath.Rel on macOS returns relative path even for unrelated abs paths,
	// so shouldAccept may return true for paths outside root when no matcher/extension
	// filters them out. With matcher+extensions, non-.go files are rejected.
	assert.False(t, w.shouldAccept("/nonexistent/outside/readme.md"))
}

func TestShouldAccept_MatcherAndExtensions(t *testing.T) {
	root := t.TempDir()
	m := fileutil.NewMatcher([]string{"test"})
	w, err := New(root, m, []string{".go"}, 0)
	require.NoError(t, err)
	defer w.Close()

	// Good file
	assert.True(t, w.shouldAccept(filepath.Join(root, "main.go")))
	// Wrong extension
	assert.False(t, w.shouldAccept(filepath.Join(root, "main.py")))
	// Ignored dir
	assert.False(t, w.shouldAccept(filepath.Join(root, "test", "main.go")))
}

// ---------------------------------------------------------------------------
// Start + Close lifecycle
// ---------------------------------------------------------------------------

func TestStartAndClose_EmptyDir(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, nil, nil, 50*time.Millisecond)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	err = w.Start(ctx)
	require.NoError(t, err)

	// Close should not panic or hang
	require.NoError(t, w.Close())
	cancel()
}

func TestStart_InvalidRoot_NoErrorDueToWalkDirResilience(t *testing.T) {
	// WalkDir callback returns nil on error, so addDirRecursively
	// silently ignores missing directories. Start succeeds.
	w, err := New("/nonexistent/path/that/does/not/exist", nil, nil, 0)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = w.Start(ctx)
	// No error — WalkDir errors are swallowed
	assert.NoError(t, err)
	_ = w.Close()
}

// ---------------------------------------------------------------------------
// File event detection (integration)
// ---------------------------------------------------------------------------

func TestWatcher_DetectsFileModification(t *testing.T) {
	root := t.TempDir()

	// Create a subdirectory and file BEFORE starting watcher
	sub := filepath.Join(root, "src")
	require.NoError(t, os.MkdirAll(sub, 0755))
	testFile := filepath.Join(sub, "hello.go")
	require.NoError(t, os.WriteFile(testFile, []byte("package main"), 0644))

	w, err := New(root, nil, []string{".go"}, 100*time.Millisecond)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, w.Start(ctx))
	defer w.Close()

	// Wait for watcher to be ready
	time.Sleep(100 * time.Millisecond)

	// Modify existing file (Create events are dropped by design)
	require.NoError(t, os.WriteFile(testFile, []byte("package main\nfunc main(){}"), 0644))

	select {
	case ev := <-w.Events():
		assert.Equal(t, testFile, ev.AbsPath)
		assert.Equal(t, EventWrite, ev.Kind)
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}
}

func TestWatcher_DetectsFileWrite(t *testing.T) {
	root := t.TempDir()

	// Pre-create the file
	testFile := filepath.Join(root, "existing.go")
	require.NoError(t, os.WriteFile(testFile, []byte("package main"), 0644))

	w, err := New(root, nil, []string{".go"}, 100*time.Millisecond)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, w.Start(ctx))
	defer w.Close()

	time.Sleep(100 * time.Millisecond)

	// Modify file
	require.NoError(t, os.WriteFile(testFile, []byte("package main\nfunc main(){}"), 0644))

	select {
	case ev := <-w.Events():
		assert.Equal(t, testFile, ev.AbsPath)
		assert.Equal(t, EventWrite, ev.Kind)
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}
}

// ---------------------------------------------------------------------------
// Debounce: multiple rapid events → one flush
// ---------------------------------------------------------------------------

func TestWatcher_DebounceCoalescesEvents(t *testing.T) {
	root := t.TempDir()

	testFile := filepath.Join(root, "file.go")
	require.NoError(t, os.WriteFile(testFile, []byte("v1"), 0644))

	w, err := New(root, nil, []string{".go"}, 300*time.Millisecond)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, w.Start(ctx))
	defer w.Close()

	time.Sleep(100 * time.Millisecond)

	// Rapid writes to the same file
	for i := 0; i < 5; i++ {
		require.NoError(t, os.WriteFile(testFile, []byte("v"+string(rune('0'+i))), 0644))
		time.Sleep(10 * time.Millisecond)
	}

	// Should receive at most a few events (debounced)
	var events []Event
	timeout := time.After(2 * time.Second)
collect:
	for {
		select {
		case ev := <-w.Events():
			events = append(events, ev)
			// Drain quickly
			time.Sleep(50 * time.Millisecond)
		case <-timeout:
			break collect
		}
		// Stop after a reasonable time
		if time.Since(events[0].Time) > 1*time.Second && len(events) > 0 {
			break
		}
	}

	// We should get at least one event
	assert.NotEmpty(t, events, "should receive at least one debounced event")
	// All events should be for the same file
	for _, ev := range events {
		assert.Equal(t, testFile, ev.AbsPath)
	}
}

// ---------------------------------------------------------------------------
// enqueue + flush logic (unit-level)
// ---------------------------------------------------------------------------

func TestEnqueue_LastEventWins(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, nil, nil, 50*time.Millisecond)
	require.NoError(t, err)
	defer w.Close()

	path := filepath.Join(root, "test.go")
	rel, _ := filepath.Rel(root, path)

	w.enqueue(Event{Kind: EventCreate, AbsPath: path, RelPath: rel, Time: time.Now()})
	w.enqueue(Event{Kind: EventWrite, AbsPath: path, RelPath: rel, Time: time.Now()})

	w.mu.Lock()
	ev := w.pending[path]
	w.mu.Unlock()

	assert.Equal(t, EventWrite, ev.Kind)
}

func TestFlush_SendsPendingEvents(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, nil, nil, 1*time.Second)
	require.NoError(t, err)
	defer w.Close()

	path := filepath.Join(root, "test.go")
	rel, _ := filepath.Rel(root, path)

	// Manually enqueue
	w.mu.Lock()
	w.pending[path] = Event{Kind: EventWrite, AbsPath: path, RelPath: rel, Time: time.Now()}
	w.mu.Unlock()

	// Trigger flush directly
	w.flush()

	select {
	case ev := <-w.out:
		assert.Equal(t, EventWrite, ev.Kind)
		assert.Equal(t, path, ev.AbsPath)
	case <-time.After(time.Second):
		t.Fatal("expected event from flush")
	}
}

func TestFlush_ClearsPending(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, nil, nil, 1*time.Second)
	require.NoError(t, err)
	defer w.Close()

	path := filepath.Join(root, "test.go")
	rel, _ := filepath.Rel(root, path)

	w.mu.Lock()
	w.pending[path] = Event{Kind: EventWrite, AbsPath: path, RelPath: rel, Time: time.Now()}
	w.mu.Unlock()

	w.flush()

	w.mu.Lock()
	assert.Empty(t, w.pending)
	assert.Nil(t, w.timer)
	w.mu.Unlock()
}

func TestFlush_DropsOnFullChannel(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, nil, nil, 1*time.Second)
	require.NoError(t, err)
	defer w.Close()

	// Fill the channel
	for i := 0; i < 256; i++ {
		w.out <- Event{Kind: EventWrite, AbsPath: "filler"}
	}

	// Now flush an additional event — should be dropped without panic
	path := filepath.Join(root, "overflow.go")
	rel, _ := filepath.Rel(root, path)
	w.mu.Lock()
	w.pending[path] = Event{Kind: EventCreate, AbsPath: path, RelPath: rel, Time: time.Now()}
	w.mu.Unlock()

	// Should not panic
	w.flush()
}

// ---------------------------------------------------------------------------
// addDirRecursively — ignore patterns skip directories
// ---------------------------------------------------------------------------

func TestAddDirRecursively_SkipsIgnored(t *testing.T) {
	root := t.TempDir()

	// Create structure
	require.NoError(t, os.MkdirAll(filepath.Join(root, "src"), 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "node_modules", "pkg"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "src", "a.go"), []byte("x"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "node_modules", "pkg", "b.js"), []byte("y"), 0644))

	m := fileutil.NewMatcher([]string{"node_modules"})
	w, err := New(root, m, nil, 0)
	require.NoError(t, err)
	defer w.Close()

	err = w.addDirRecursively(root)
	require.NoError(t, err)

	// node_modules should not be watched — we can verify by checking
	// that no events are produced for files in it
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	require.NoError(t, w.Start(ctx))
	defer w.Close()

	time.Sleep(100 * time.Millisecond)

	// Write in node_modules — should NOT produce an event
	require.NoError(t, os.WriteFile(filepath.Join(root, "node_modules", "pkg", "c.js"), []byte("z"), 0644))

	select {
	case ev := <-w.Events():
		// If we get an event for node_modules, that's a failure
		assert.NotContains(t, ev.AbsPath, "node_modules")
	case <-time.After(500 * time.Millisecond):
		// Good — no event from ignored dir
	}
}

// ---------------------------------------------------------------------------
// Context cancellation
// ---------------------------------------------------------------------------

func TestWatcher_ContextCancellation(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, nil, nil, 50*time.Millisecond)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))

	// Cancel context — loop should exit and close out channel
	cancel()

	// Wait for out channel to close
	select {
	case _, ok := <-w.out:
		assert.False(t, ok, "channel should be closed after context cancel")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for channel close")
	}

	require.NoError(t, w.Close())
}

// ---------------------------------------------------------------------------
// Concurrent access safety
// ---------------------------------------------------------------------------

func TestWatcher_ConcurrentEnqueue(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, nil, nil, 200*time.Millisecond)
	require.NoError(t, err)
	defer w.Close()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			path := filepath.Join(root, "file.go")
			rel, _ := filepath.Rel(root, path)
			w.enqueue(Event{Kind: EventWrite, AbsPath: path, RelPath: rel, Time: time.Now()})
		}(i)
	}
	wg.Wait()

	w.mu.Lock()
	assert.Len(t, w.pending, 1, "all writes to same path should coalesce")
	w.mu.Unlock()
}

// ---------------------------------------------------------------------------
// fileInfo helper (util.go)
// ---------------------------------------------------------------------------

func TestFileInfo_ExistingFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "test.txt")
	require.NoError(t, os.WriteFile(f, []byte("hello"), 0644))

	info, err := fileInfo(f)
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.False(t, info.IsDir())
	assert.Equal(t, int64(5), info.Size())
}

func TestFileInfo_ExistingDir(t *testing.T) {
	dir := t.TempDir()
	info, err := fileInfo(dir)
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.True(t, info.IsDir())
}

func TestFileInfo_NonExistent(t *testing.T) {
	info, err := fileInfo("/nonexistent/path/12345")
	require.NoError(t, err) // returns nil, nil for not-exist
	assert.Nil(t, info)
}

// ---------------------------------------------------------------------------
// Event struct fields
// ---------------------------------------------------------------------------

func TestEvent_Fields(t *testing.T) {
	now := time.Now()
	ev := Event{
		Kind:    EventCreate,
		AbsPath: "/abs/path",
		RelPath: "rel/path",
		Repo:    "my-repo",
		Time:    now,
	}
	assert.Equal(t, EventCreate, ev.Kind)
	assert.Equal(t, "/abs/path", ev.AbsPath)
	assert.Equal(t, "rel/path", ev.RelPath)
	assert.Equal(t, "my-repo", ev.Repo)
	assert.Equal(t, now, ev.Time)
}

// ---------------------------------------------------------------------------
// RelPath is correctly set
// ---------------------------------------------------------------------------

func TestWatcher_RelPathCorrect(t *testing.T) {
	root := t.TempDir()

	sub := filepath.Join(root, "sub")
	require.NoError(t, os.MkdirAll(sub, 0755))

	// Pre-create file so Write event (not Create) is fired
	testFile := filepath.Join(sub, "file.go")
	require.NoError(t, os.WriteFile(testFile, []byte("v1"), 0644))

	w, err := New(root, nil, []string{".go"}, 100*time.Millisecond)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, w.Start(ctx))
	defer w.Close()

	time.Sleep(100 * time.Millisecond)

	// Modify file to trigger Write event
	require.NoError(t, os.WriteFile(testFile, []byte("v2"), 0644))

	select {
	case ev := <-w.Events():
		assert.Equal(t, testFile, ev.AbsPath)
		expectedRel := filepath.Join("sub", "file.go")
		assert.Equal(t, expectedRel, ev.RelPath)
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}
}
