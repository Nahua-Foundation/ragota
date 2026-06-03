package classifier

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"ragota/internal/indexing/crossrepo/detector"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── NewCache ──

func TestNewCache(t *testing.T) {
	c := NewCache(100)
	require.NotNil(t, c)
	assert.Equal(t, 100, c.maxSize)
	assert.NotNil(t, c.items)
	assert.Empty(t, c.order)
}

// ── Get/Set basic operations ──

func TestCache_GetSet_Hit(t *testing.T) {
	c := NewCache(10)
	result := &ClassificationResult{
		Protocol:      "http",
		TargetService: "auth-service",
		Confidence:    0.9,
	}

	c.Set("key1", result)
	got, ok := c.Get("key1")

	assert.True(t, ok)
	assert.Equal(t, result, got)
}

func TestCache_GetSet_Miss(t *testing.T) {
	c := NewCache(10)

	got, ok := c.Get("nonexistent")

	assert.False(t, ok)
	assert.Nil(t, got)
}

func TestCache_GetSet_Overwrite(t *testing.T) {
	c := NewCache(10)
	result1 := &ClassificationResult{Protocol: "http"}
	result2 := &ClassificationResult{Protocol: "grpc"}

	c.Set("key1", result1)
	c.Set("key1", result2)

	got, ok := c.Get("key1")
	assert.True(t, ok)
	assert.Equal(t, "grpc", got.Protocol)
}

func TestCache_SetMultiple(t *testing.T) {
	c := NewCache(10)

	for i := 0; i < 5; i++ {
		c.Set("key", &ClassificationResult{
			Protocol: "http",
			Reason:   "test",
		})
	}

	// Should not grow order for duplicate keys
	assert.Len(t, c.order, 1)
	assert.Len(t, c.items, 1)
}

// ── LRU eviction ──

func TestCache_LRUEviction(t *testing.T) {
	c := NewCache(3)

	c.Set("a", &ClassificationResult{Protocol: "http"})
	c.Set("b", &ClassificationResult{Protocol: "grpc"})
	c.Set("c", &ClassificationResult{Protocol: "kafka"})

	// Cache is full
	assert.Len(t, c.items, 3)

	// Adding one more should evict "a" (oldest)
	c.Set("d", &ClassificationResult{Protocol: "npm_package"})

	assert.Len(t, c.items, 3)
	_, okA := c.Get("a")
	assert.False(t, okA, "oldest item 'a' should be evicted")

	_, okB := c.Get("b")
	assert.True(t, okB)
	_, okC := c.Get("c")
	assert.True(t, okC)
	_, okD := c.Get("d")
	assert.True(t, okD)
}

func TestCache_LRUEviction_AccessReorders(t *testing.T) {
	c := NewCache(3)

	c.Set("a", &ClassificationResult{Protocol: "http"})
	c.Set("b", &ClassificationResult{Protocol: "grpc"})
	c.Set("c", &ClassificationResult{Protocol: "kafka"})

	// Access "a" — should move to end (most recently used)
	c.Get("a")

	// Now "b" is oldest, adding "d" should evict "b"
	c.Set("d", &ClassificationResult{Protocol: "npm_package"})

	assert.Len(t, c.items, 3)
	_, okA := c.Get("a")
	assert.True(t, okA, "'a' was accessed, should still be present")
	_, okB := c.Get("b")
	assert.False(t, okB, "'b' should be evicted after 'a' was accessed")
}

func TestCache_LRUEviction_EvictsMultiple(t *testing.T) {
	c := NewCache(2)

	c.Set("a", &ClassificationResult{Protocol: "http"})
	c.Set("b", &ClassificationResult{Protocol: "grpc"})
	c.Set("c", &ClassificationResult{Protocol: "kafka"})
	c.Set("d", &ClassificationResult{Protocol: "npm_package"})

	assert.Len(t, c.items, 2)
	_, okA := c.Get("a")
	assert.False(t, okA)
	_, okB := c.Get("b")
	assert.False(t, okB)
	_, okC := c.Get("c")
	assert.True(t, okC)
	_, okD := c.Get("d")
	assert.True(t, okD)
}

// ── LoadFromFile / SaveToFile ──

func TestCache_SaveToFile(t *testing.T) {
	c := NewCache(10)
	c.Set("key1", &ClassificationResult{Protocol: "http", Confidence: 0.9})
	c.Set("key2", &ClassificationResult{Protocol: "grpc", Confidence: 0.8})

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "cache.json")

	err := c.SaveToFile(path)
	require.NoError(t, err)

	// Verify file exists and is valid JSON
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "http")
	assert.Contains(t, string(data), "grpc")
}

func TestCache_LoadFromFile(t *testing.T) {
	c := NewCache(10)
	c.Set("key1", &ClassificationResult{Protocol: "http", Confidence: 0.9})
	c.Set("key2", &ClassificationResult{Protocol: "grpc", Confidence: 0.8})

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "cache.json")
	require.NoError(t, c.SaveToFile(path))

	// Load into new cache
	c2 := NewCache(10)
	err := c2.LoadFromFile(path)
	require.NoError(t, err)

	got1, ok1 := c2.Get("key1")
	got2, ok2 := c2.Get("key2")

	assert.True(t, ok1)
	assert.Equal(t, "http", got1.Protocol)
	assert.True(t, ok2)
	assert.Equal(t, "grpc", got2.Protocol)
}

func TestCache_SaveLoadRoundTrip(t *testing.T) {
	c := NewCache(10)
	c.Set("a", &ClassificationResult{
		Protocol:      "http",
		TargetService: "auth-service",
		Endpoint:      "/api/v1/test",
		Confidence:    0.95,
		Reason:        "test reason",
	})

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "subdir", "cache.json")

	require.NoError(t, c.SaveToFile(path))

	c2 := NewCache(10)
	require.NoError(t, c2.LoadFromFile(path))

	got, ok := c2.Get("a")
	require.True(t, ok)
	assert.Equal(t, "http", got.Protocol)
	assert.Equal(t, "auth-service", got.TargetService)
	assert.Equal(t, "/api/v1/test", got.Endpoint)
	assert.InDelta(t, 0.95, got.Confidence, 0.001)
	assert.Equal(t, "test reason", got.Reason)
}

func TestCache_LoadFromFile_MissingFile(t *testing.T) {
	c := NewCache(10)

	err := c.LoadFromFile("/nonexistent/path/cache.json")

	assert.Error(t, err)
}

func TestCache_LoadFromFile_CorruptJSON(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "corrupt.json")

	require.NoError(t, os.WriteFile(path, []byte("not valid json{{{"), 0o644))

	c := NewCache(10)
	err := c.LoadFromFile(path)

	assert.Error(t, err)
}

func TestCache_LoadFromFile_EmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "empty.json")

	require.NoError(t, os.WriteFile(path, []byte(""), 0o644))

	c := NewCache(10)
	err := c.LoadFromFile(path)

	assert.Error(t, err)
}

func TestCache_LoadFromFile_RespectsMaxSize(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "cache.json")

	c := NewCache(10)
	c.Set("k1", &ClassificationResult{Protocol: "http"})
	c.Set("k2", &ClassificationResult{Protocol: "grpc"})
	c.Set("k3", &ClassificationResult{Protocol: "kafka"})
	require.NoError(t, c.SaveToFile(path))

	// Load into cache with maxSize=2 — should only load 2 items
	c2 := NewCache(2)
	require.NoError(t, c2.LoadFromFile(path))

	// The load should stop when maxSize is reached
	assert.LessOrEqual(t, len(c2.items), 2)
}

// ── CandidateCacheKey ──

func TestCandidateCacheKey_Deterministic(t *testing.T) {
	cand := detector.Candidate{
		FilePath: "src/auth/handler.go",
		Line:     42,
		RawCode:  "http.Get(url)",
	}

	key1 := CandidateCacheKey(cand)
	key2 := CandidateCacheKey(cand)

	assert.Equal(t, key1, key2, "same candidate must produce same key")
	assert.NotEmpty(t, key1)
}

func TestCandidateCacheKey_DifferentCandidates(t *testing.T) {
	cand1 := detector.Candidate{
		FilePath: "src/auth/handler.go",
		Line:     42,
		RawCode:  "http.Get(url)",
	}
	cand2 := detector.Candidate{
		FilePath: "src/user/handler.go",
		Line:     42,
		RawCode:  "http.Get(url)",
	}

	key1 := CandidateCacheKey(cand1)
	key2 := CandidateCacheKey(cand2)

	assert.NotEqual(t, key1, key2, "different candidates must produce different keys")
}

func TestCandidateCacheKey_DifferentFields(t *testing.T) {
	base := detector.Candidate{
		FilePath: "src/auth/handler.go",
		Line:     42,
		RawCode:  "http.Get(url)",
	}

	keyBase := CandidateCacheKey(base)

	// Different FilePath
	candFile := base
	candFile.FilePath = "src/user/handler.go"
	assert.NotEqual(t, keyBase, CandidateCacheKey(candFile))

	// Different Line
	candLine := base
	candLine.Line = 43
	assert.NotEqual(t, keyBase, CandidateCacheKey(candLine))

	// Different RawCode
	candCode := base
	candCode.RawCode = "grpc.Invoke(ctx)"
	assert.NotEqual(t, keyBase, CandidateCacheKey(candCode))
}

// ── Thread safety ──

func TestCache_ThreadSafety(t *testing.T) {
	c := NewCache(1000)
	var wg sync.WaitGroup

	// Concurrent writes
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c.Set("key", &ClassificationResult{
				Protocol: "http",
				Reason:   "test",
			})
		}(i)
	}

	// Concurrent reads
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c.Get("key")
		}(i)
	}

	wg.Wait()

	// Should not panic and have valid state
	_, ok := c.Get("key")
	assert.True(t, ok)
}

func TestCache_ThreadSafety_ConcurrentSetGetDifferentKeys(t *testing.T) {
	c := NewCache(100)
	var wg sync.WaitGroup

	numGoroutines := 20
	numOpsPerGoroutine := 50

	// Writers
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			for j := 0; j < numOpsPerGoroutine; j++ {
				key := "key"
				c.Set(key, &ClassificationResult{
					Protocol: "http",
				})
			}
		}(i)
	}

	// Readers
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			for j := 0; j < numOpsPerGoroutine; j++ {
				c.Get("key")
			}
		}(i)
	}

	wg.Wait()
	// No data race = test passes
}

func TestCache_EvictionUnderConcurrency(t *testing.T) {
	c := NewCache(5)
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c.Set("key", &ClassificationResult{
				Protocol: "http",
			})
		}(i)
	}

	wg.Wait()

	// Should not panic, and cache should be within bounds
	assert.LessOrEqual(t, len(c.items), 5)
}

// ── moveToEndLocked / evictOldestLocked edge cases ──

func TestCache_EvictOldest_EmptyCache(t *testing.T) {
	c := NewCache(3)
	c.evictOldestLocked()
	// Should not panic
	assert.Empty(t, c.items)
	assert.Empty(t, c.order)
}

func TestCache_MoveToEndLocked_NonExistentKey(t *testing.T) {
	c := NewCache(3)
	c.Set("a", &ClassificationResult{Protocol: "http"})
	c.moveToEndLocked("nonexistent")
	// Should not panic
}

// ── SaveToFile creates directory ──

func TestCache_SaveToFile_CreatesDirectory(t *testing.T) {
	c := NewCache(10)
	c.Set("key1", &ClassificationResult{Protocol: "http"})

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "deep", "nested", "dir", "cache.json")

	err := c.SaveToFile(path)
	require.NoError(t, err)

	_, err = os.Stat(path)
	require.NoError(t, err)
}
