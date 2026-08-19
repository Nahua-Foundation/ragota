package index

import (
	"fmt"
	"sync"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/domain"
)

func TestProjectDropsUnnamedUnits(t *testing.T) {
	syms := ProjectSymbols([]*domain.ASTUnit{
		{Name: "Add", Qualified: "pkg.Add", Kind: "function", StartLine: 3, EndLine: 8},
		nil,
		{Name: "", Kind: "block", StartLine: 1, EndLine: 2},
	})
	if len(syms) != 1 {
		t.Fatalf("Project kept %d symbols, want 1 (the named one)", len(syms))
	}
	want := Symbol{Name: "Add", Qualified: "pkg.Add", Kind: "function", StartLine: 3, EndLine: 8}
	if syms[0] != want {
		t.Errorf("Project = %+v, want %+v", syms[0], want)
	}
}

// A published entry is handed to exactly one consumer: the producer publishes
// once per pass, so leaving it behind would only keep memory alive.
func TestTakeRemovesTheEntry(t *testing.T) {
	c := NewSymbolCache(8)
	c.Put("r", "a.go", "h1", []Symbol{{Name: "A"}})

	got, ok := c.Take("r", "a.go", "h1")
	if !ok || len(got) != 1 || got[0].Name != "A" {
		t.Fatalf("first Take = %v, %v; want the published symbols", got, ok)
	}
	if _, ok := c.Take("r", "a.go", "h1"); ok {
		t.Error("second Take reported a hit; the entry must be gone")
	}
	if c.Len() != 0 {
		t.Errorf("Len = %d after Take, want 0", c.Len())
	}
}

// A file with no symbols is not the same answer as a file nobody published:
// only the second is a reason for the consumer to parse it.
func TestTakeDistinguishesEmptyFromAbsent(t *testing.T) {
	c := NewSymbolCache(8)
	c.Put("r", "empty.go", "h", nil)

	if syms, ok := c.Take("r", "empty.go", "h"); !ok || len(syms) != 0 {
		t.Errorf("Take of an empty publish = %v, %v; want no symbols but a hit", syms, ok)
	}
	if _, ok := c.Take("r", "never.go", "h"); ok {
		t.Error("Take of an unpublished file reported a hit")
	}
}

// The key spans repo, path and content hash: a stale entry from an earlier
// version of a file must never label the new one.
func TestKeyCoversRepoPathAndHash(t *testing.T) {
	c := NewSymbolCache(8)
	c.Put("r1", "a.go", "old", []Symbol{{Name: "Old"}})

	for _, tc := range []struct{ repo, path, hash string }{
		{"r2", "a.go", "old"},
		{"r1", "b.go", "old"},
		{"r1", "a.go", "new"},
	} {
		if _, ok := c.Take(tc.repo, tc.path, tc.hash); ok {
			t.Errorf("Take(%q, %q, %q) hit the entry published for (r1, a.go, old)", tc.repo, tc.path, tc.hash)
		}
	}
	if _, ok := c.Take("r1", "a.go", "old"); !ok {
		t.Error("the published entry was consumed by a differing key")
	}
}

// A producer whose symbols nobody consumes must not grow the cache without
// limit — a pass over a 40k-file repository would otherwise retain every file
// it ever parsed.
func TestCacheIsBounded(t *testing.T) {
	const capacity = 16
	c := NewSymbolCache(capacity)
	for i := 0; i < capacity*10; i++ {
		c.Put("r", fmt.Sprintf("f%d.go", i), "h", []Symbol{{Name: "F"}})
		if c.Len() > capacity {
			t.Fatalf("Len = %d after %d puts, want at most %d", c.Len(), i+1, capacity)
		}
	}
	// Eviction is oldest-first, so the most recent publishes are the ones a
	// consumer running just behind the producer still finds.
	if _, ok := c.Take("r", fmt.Sprintf("f%d.go", capacity*10-1), "h"); !ok {
		t.Error("the newest entry was evicted")
	}
	if _, ok := c.Take("r", "f0.go", "h"); ok {
		t.Error("the oldest entry survived a cache ten times its capacity")
	}
}

// The producer publishes from its parse worker pool while the consumer takes
// from another goroutine. Run with -race.
func TestConcurrentPutAndTake(t *testing.T) {
	c := NewSymbolCache(64)
	const n = 500

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			c.Put("r", fmt.Sprintf("f%d.go", i), "h", []Symbol{{Name: "F", StartLine: i}})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			c.Take("r", fmt.Sprintf("f%d.go", i), "h")
		}
	}()
	wg.Wait()

	if c.Len() > 64 {
		t.Errorf("Len = %d, want at most the capacity 64", c.Len())
	}
}
