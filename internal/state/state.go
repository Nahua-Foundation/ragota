// Package state хранит in-memory статистику сессии: статус индексации,
// последние изменённые файлы, число чанков, статистику вызовов MCP-серверов.
// Используется TUI-дашбордом и опционально сериализуется в ai-tools/stats.json.
package state

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// FileEntry — запись о последнем (ре)индексированном файле.
type FileEntry struct {
	Path       string    `json:"path"`
	Kind       string    `json:"kind"` // create/write/remove/scan
	IndexedAt  time.Time `json:"indexed_at"`
	Chunks     int       `json:"chunks"`
	Symbols    int       `json:"symbols"`
	DurationMs int64     `json:"duration_ms"`
	Error      string    `json:"error,omitempty"`
}

// Indexer — статус индексации (общий для tree-sitter и vector).
type Indexer struct {
	Name         string    `json:"name"`
	Status       string    `json:"status"` // idle/scanning/indexing/error
	LastError    string    `json:"last_error,omitempty"`
	FilesTotal   int       `json:"files_total"`
	FilesIndexed int       `json:"files_indexed"`
	Chunks       int       `json:"chunks"`
	Symbols      int       `json:"symbols"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// MCPStat — статистика по одному MCP-серверу.
type MCPStat struct {
	Server  string         `json:"server"`
	Running bool           `json:"running"`
	Calls   map[string]int `json:"calls"` // tool name -> count
	Errors  int            `json:"errors"`
}

// Snapshot — иммутабельный снимок состояния для рендера UI.
type Snapshot struct {
	StartedAt time.Time          `json:"started_at"`
	Root      string             `json:"root"`
	Indexers  map[string]Indexer `json:"indexers"`
	Recent    []FileEntry        `json:"recent"`
	MCP       map[string]MCPStat `json:"mcp"`
	Docker    DockerStatus       `json:"docker"`
}

// DockerStatus — статус сервисов docker-compose.
type DockerStatus struct {
	Running   bool      `json:"running"`
	Services  []string  `json:"services"`
	UpdatedAt time.Time `json:"updated_at"`
	LastError string    `json:"last_error,omitempty"`
}

// Bus — потокобезопасный агрегатор состояния.
type Bus struct {
	mu       sync.RWMutex
	root     string
	started  time.Time
	indexers map[string]Indexer
	recent   []FileEntry // ring buffer (newest first)
	mcp      map[string]MCPStat
	docker   DockerStatus
	maxItems int
}

// NewBus создаёт шину состояния.
func NewBus(root string) *Bus {
	return &Bus{
		root:     root,
		started:  time.Now(),
		indexers: make(map[string]Indexer),
		mcp:      make(map[string]MCPStat),
		maxItems: 50,
	}
}

// SetIndexer обновляет статус индексатора (имя — "treesitter"/"vector").
func (b *Bus) SetIndexer(name string, mutate func(i *Indexer)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cur := b.indexers[name]
	cur.Name = name
	mutate(&cur)
	cur.UpdatedAt = time.Now()
	b.indexers[name] = cur
}

// AddRecent добавляет запись о файле в начало ring buffer.
func (b *Bus) AddRecent(e FileEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e.IndexedAt = time.Now()
	b.recent = append([]FileEntry{e}, b.recent...)
	if len(b.recent) > b.maxItems {
		b.recent = b.recent[:b.maxItems]
	}
}

// IncMCPCall увеличивает счётчик вызова MCP-инструмента.
func (b *Bus) IncMCPCall(server, tool string, isError bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cur, ok := b.mcp[server]
	if !ok {
		cur = MCPStat{Server: server, Calls: make(map[string]int)}
	}
	if cur.Calls == nil {
		cur.Calls = make(map[string]int)
	}
	cur.Calls[tool]++
	if isError {
		cur.Errors++
	}
	cur.Running = true
	b.mcp[server] = cur
}

// SetMCPRunning отмечает MCP-сервер как (не) запущенный.
func (b *Bus) SetMCPRunning(server string, running bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cur, ok := b.mcp[server]
	if !ok {
		cur = MCPStat{Server: server, Calls: make(map[string]int)}
	}
	cur.Running = running
	b.mcp[server] = cur
}

// SetDocker обновляет статус docker-compose.
func (b *Bus) SetDocker(s DockerStatus) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s.UpdatedAt = time.Now()
	b.docker = s
}

// Snapshot возвращает копию текущего состояния.
func (b *Bus) Snapshot() Snapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()
	snap := Snapshot{
		StartedAt: b.started,
		Root:      b.root,
		Indexers:  make(map[string]Indexer, len(b.indexers)),
		MCP:       make(map[string]MCPStat, len(b.mcp)),
		Recent:    append([]FileEntry(nil), b.recent...),
		Docker:    b.docker,
	}
	for k, v := range b.indexers {
		snap.Indexers[k] = v
	}
	for k, v := range b.mcp {
		// клонируем map calls
		calls := make(map[string]int, len(v.Calls))
		for kk, vv := range v.Calls {
			calls[kk] = vv
		}
		v.Calls = calls
		snap.MCP[k] = v
	}
	return snap
}

// Persist сохраняет snapshot в файл (для дебага и обмена между процессами).
func (b *Bus) Persist(path string) error {
	snap := b.Snapshot()
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
