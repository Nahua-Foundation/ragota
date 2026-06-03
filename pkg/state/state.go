// Package state хранит in-memory статистику сессии: статус индексации,
// последние изменённые файлы, число чанков, статистику вызовов MCP-серверов.
package state

import (
	"sync"
	"time"
)

// MetricsTimeSeries — временной ряд для графиков.
type MetricsTimeSeries struct {
	Values []float64
	Label  string
}

func (ts *MetricsTimeSeries) Push(v float64, maxPoints int) {
	if cap(ts.Values) == 0 {
		ts.Values = make([]float64, 0, maxPoints)
	}
	ts.Values = append(ts.Values, v)
	if len(ts.Values) > maxPoints {
		ts.Values = ts.Values[len(ts.Values)-maxPoints:]
	}
}

// OllamaLatency — метрики latency для одной модели ollama.
type OllamaLatency struct {
	Model      string
	LatencyMs  MetricsTimeSeries
	TotalCalls int
	Errors     int
}

// IndexerMetrics — метрики индексатора для графиков.
type IndexerMetrics struct {
	FilesPerSec  MetricsTimeSeries
	ChunksTotal  int
	SymbolsTotal int
}

// FileEntry — запись о последнем (ре)индексированном файле.
type FileEntry struct {
	Path       string    `json:"path"`
	Kind       string    `json:"kind"`
	IndexedAt  time.Time `json:"indexed_at"`
	Chunks     int       `json:"chunks"`
	Symbols    int       `json:"symbols"`
	DurationMs int64     `json:"duration_ms"`
	Error      string    `json:"error,omitempty"`
}

// LSPError — ошибка LSP запроса.
type LSPError struct {
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Line      int       `json:"line"`
	Char      int       `json:"char"`
	Error     string    `json:"error"`
	Timestamp time.Time `json:"timestamp"`
}

// Indexer — статус индексации.
type Indexer struct {
	Name           string    `json:"name"`
	Status         string    `json:"status"`
	LastError      string    `json:"last_error,omitempty"`
	FilesTotal     int       `json:"files_total"`
	FilesIndexed   int       `json:"files_indexed"`
	Chunks         int       `json:"chunks"`
	Symbols        int       `json:"symbols"`
	PendingEdges   int       `json:"pending_edges"`
	ResolvePass    int       `json:"resolve_pass"`
	ResolveTotal   int       `json:"resolve_total"`
	ResolveProgress float64 `json:"resolve_progress"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// MCPStat — статистика по одному MCP-серверу.
type MCPStat struct {
	Server  string         `json:"server"`
	Running bool           `json:"running"`
	Calls   map[string]int `json:"calls"`
	Errors  int            `json:"errors"`
}

// Snapshot — иммутабельный снимок состояния.
type Snapshot struct {
	StartedAt      time.Time                  `json:"started_at"`
	Root           string                     `json:"root"`
	Indexers       map[string]Indexer         `json:"indexers"`
	Recent         []FileEntry                `json:"recent"`
	MCP            map[string]MCPStat         `json:"mcp"`
	Docker         DockerStatus               `json:"docker"`
	LSP            []LSPError                 `json:"lsp,omitempty"`
	IndexerMetrics map[string]*IndexerMetrics `json:"-"`
	MCPCallHistory []int                      `json:"-"`
	MCPErrHistory  []int                      `json:"-"`
	OllamaLatency  map[string]*OllamaLatency  `json:"-"`
}

// DockerStatus — статус сервисов docker.
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
	recent   []FileEntry
	mcp      map[string]MCPStat
	docker   DockerStatus
	lsp      []LSPError
	maxItems int

	indexerMetrics    map[string]*IndexerMetrics
	filesIndexedTotal int
	prevIndexed       map[string]int
	mcpCallHistory    []int
	mcpErrorHistory   []int
	currentMCPsec     int
	currentMCPErrSec  int
	lastTick          time.Time

	ollamaLatency map[string]*OllamaLatency
}

// NewBus создаёт шину состояния.
func NewBus(root string) *Bus {
	return &Bus{
		root:           root,
		started:        time.Now(),
		indexers:       make(map[string]Indexer),
		mcp:            make(map[string]MCPStat),
		maxItems:       50,
		indexerMetrics: make(map[string]*IndexerMetrics),
		prevIndexed:    make(map[string]int),
		lastTick:       time.Now(),
		ollamaLatency:  make(map[string]*OllamaLatency),
	}
}

func (b *Bus) SetIndexer(name string, mutate func(i *Indexer)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cur := b.indexers[name]
	cur.Name = name
	mutate(&cur)
	cur.UpdatedAt = time.Now()
	b.indexers[name] = cur
}

func (b *Bus) AddRecent(e FileEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e.IndexedAt = time.Now()
	b.recent = append([]FileEntry{e}, b.recent...)
	if len(b.recent) > b.maxItems {
		b.recent = b.recent[:b.maxItems]
	}
}

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
		b.currentMCPErrSec++
	}
	b.currentMCPsec++
	cur.Running = true
	b.mcp[server] = cur
}

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

func (b *Bus) SetDocker(s DockerStatus) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s.UpdatedAt = time.Now()
	b.docker = s
}

func (b *Bus) AddLSPError(method, path string, line, char int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := LSPError{Method: method, Path: path, Line: line, Char: char, Error: err.Error(), Timestamp: time.Now()}
	b.lsp = append([]LSPError{e}, b.lsp...)
	if len(b.lsp) > b.maxItems {
		b.lsp = b.lsp[:b.maxItems]
	}
}
