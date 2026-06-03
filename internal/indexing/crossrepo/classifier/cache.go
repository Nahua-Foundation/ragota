package classifier

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"ragota/internal/indexing/crossrepo/detector"
)

// Cache — LRU-кэш результатов LLM-классификации.
type Cache struct {
	mu      sync.RWMutex
	items   map[string]*ClassificationResult
	order   []string       // для LRU eviction
	maxSize int
}

// NewCache создаёт кэш с лимитом записей.
func NewCache(maxSize int) *Cache {
	return &Cache{
		items:   make(map[string]*ClassificationResult),
		maxSize: maxSize,
	}
}

// Get возвращает закэшированный результат.
func (c *Cache) Get(key string) (*ClassificationResult, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	val, ok := c.items[key]
	if ok {
		// Move to end (most recently used)
		c.moveToEndLocked(key)
	}
	return val, ok
}

// Set добавляет результат в кэш.
func (c *Cache) Set(key string, val *ClassificationResult) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.items[key]; !ok {
		// Evict if full
		if len(c.items) >= c.maxSize {
			c.evictOldestLocked()
		}
		c.order = append(c.order, key)
	}
	c.items[key] = val
}

// LoadFromFile загружает кэш из JSON файла.
func (c *Cache) LoadFromFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	var items map[string]*ClassificationResult
	if err := json.Unmarshal(data, &items); err != nil {
		return err
	}

	for k, v := range items {
		if len(c.items) < c.maxSize {
			c.items[k] = v
			c.order = append(c.order, k)
		}
	}
	return nil
}

// SaveToFile сохраняет кэш в JSON файл.
func (c *Cache) SaveToFile(path string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(c.items, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0o644)
}

// CandidateCacheKey вычисляет хэш кандидата для кэша.
func CandidateCacheKey(cand detector.Candidate) string {
	h := sha256.Sum256([]byte(cand.FilePath + string(rune(cand.Line)) + cand.RawCode))
	return hex.EncodeToString(h[:8])
}

func (c *Cache) moveToEndLocked(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			c.order = append(c.order, key)
			return
		}
	}
}

func (c *Cache) evictOldestLocked() {
	if len(c.order) == 0 {
		return
	}
	oldest := c.order[0]
	c.order = c.order[1:]
	delete(c.items, oldest)
}
