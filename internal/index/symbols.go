// Symbol spans.
//
// A file's symbol spans are handed from the indexer that parsed it to the
// indexers that only need to know which symbol covers a line.
//
// The AST indexer parses every file of a window; the keyword indexer used to
// parse the same bytes a second time purely to label its chunks — 14.8% of the
// CPU of a full pass over Elasticsearch. The indexers run concurrently over
// the same window, so the handoff is a cache and never a rendezvous: a
// consumer takes what has been published and parses the rest itself. Nothing
// waits for anything, and an indexer running without the AST indexer sees an
// empty cache and behaves exactly as it did before.
package index

import (
	"strings"
	"sync"

	"github.com/Nahua-Foundation/ragota/internal/domain"
)

// Symbol is one AST unit reduced to what a line-range annotation needs. It is a
// copy rather than a reference to the parsed unit: the producing indexer goes
// on to rewrite its units (repo, path and storage id) while the consumer is
// reading them.
type Symbol struct {
	Name      string
	Qualified string
	Kind      string
	StartLine int
	EndLine   int
}

// annotatedLanguages are the languages whose units are cheap and reliable
// enough to label chunks with. Producers publish only these, so the cache is
// not filled with entries no consumer will ever ask for.
var annotatedLanguages = map[string]bool{
	"go":         true,
	"java":       true,
	"kotlin":     true,
	"csharp":     true,
	"typescript": true,
	"javascript": true,
	"python":     true,
	"proto":      true,
}

// SymbolsAnnotated reports whether a language's symbols are worth carrying.
func SymbolsAnnotated(language string) bool { return annotatedLanguages[language] }

// ProjectSymbols reduces parsed units to symbols, dropping the unnamed ones no
// annotation can use.
func ProjectSymbols(units []*domain.ASTUnit) []Symbol {
	out := make([]Symbol, 0, len(units))
	for _, u := range units {
		if u == nil || u.Name == "" {
			continue
		}
		out = append(out, Symbol{
			Name:      u.Name,
			Qualified: u.Qualified,
			Kind:      u.Kind,
			StartLine: u.StartLine,
			EndLine:   u.EndLine,
		})
	}
	return out
}

// defaultSymbolCacheCapacity bounds how many files' symbols the shared cache
// holds. One index window is 512 files and a producer never runs more than a
// window ahead of a consumer, so this is several windows of slack; the bound is
// what keeps a pass over a 40k-file repository from accumulating every file it
// ever parsed, and it also caps what an unconsumed producer (no keyword indexer
// configured) can retain.
const defaultSymbolCacheCapacity = 4096

// SharedSymbols is the process-wide cache the indexers hand symbols through. It
// is a package variable because the indexers are constructed independently and
// know nothing about each other; entries are keyed by repo, path and content
// hash, so two repositories — or two passes over the same file — never see each
// other's symbols.
var SharedSymbols = NewSymbolCache(defaultSymbolCacheCapacity)

// SymbolCache is a bounded, concurrency-safe store of per-file symbols. Entries
// are taken out on read and evicted oldest-first when the capacity is reached,
// so a producer whose symbols nobody consumes cannot grow it without limit.
type SymbolCache struct {
	mu    sync.Mutex
	byKey map[string][]Symbol
	// order is the insertion order of the live keys. Taken keys are left in it
	// and skipped during eviction, which keeps Take O(1).
	order []string
	cap   int
}

// NewSymbolCache returns a cache holding at most capacity files' symbols.
func NewSymbolCache(capacity int) *SymbolCache {
	if capacity < 1 {
		capacity = 1
	}
	return &SymbolCache{byKey: make(map[string][]Symbol), cap: capacity}
}

func symbolKey(repoID, path, hash string) string {
	var b strings.Builder
	b.Grow(len(repoID) + len(path) + len(hash) + 2)
	b.WriteString(repoID)
	b.WriteByte('\x00')
	b.WriteString(path)
	b.WriteByte('\x00')
	b.WriteString(hash)
	return b.String()
}

// Put publishes a file's symbols. Publishing the same key twice replaces the
// entry rather than duplicating it.
func (c *SymbolCache) Put(repoID, path, hash string, syms []Symbol) {
	k := symbolKey(repoID, path, hash)

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.byKey[k]; !ok {
		c.evictLocked()
		c.order = append(c.order, k)
	}
	c.byKey[k] = syms
}

// Take returns a file's symbols and removes them from the cache. The second
// result distinguishes "nothing was published" from "the file has no symbols":
// only the first is a reason to parse it.
func (c *SymbolCache) Take(repoID, path, hash string) ([]Symbol, bool) {
	k := symbolKey(repoID, path, hash)

	c.mu.Lock()
	defer c.mu.Unlock()
	syms, ok := c.byKey[k]
	if ok {
		delete(c.byKey, k)
	}
	return syms, ok
}

// evictLocked drops the oldest entries until there is room for one more.
func (c *SymbolCache) evictLocked() {
	for len(c.byKey) >= c.cap && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.byKey, oldest)
	}
	// Keys taken by a consumer leave a hole in order; compact it once it is
	// mostly holes, so a long pass does not grow the slice without bound.
	if len(c.order) > 2*c.cap {
		live := c.order[:0]
		for _, k := range c.order {
			if _, ok := c.byKey[k]; ok {
				live = append(live, k)
			}
		}
		c.order = live
	}
}

// Len reports how many entries are held; it exists for tests.
func (c *SymbolCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.byKey)
}
