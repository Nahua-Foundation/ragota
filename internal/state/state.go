// Package state хранит in-memory статистику сессии: статус индексации,
// последние изменённые файлы, число чанков, статистику вызовов MCP-серверов.
// Используется TUI-дашбордом и опционально сериализуется в ragota/stats.json.
package state

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// MetricsTimeSeries — временной ряд для графиков (60 последних точек, 1 точка/сек).
type MetricsTimeSeries struct {
	Values []float64
	Label  string
}

// Push добавляет значение в кольцевой буфер (макс maxPoints точек).
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
	LatencyMs  MetricsTimeSeries // ms за последние 60 точек
	TotalCalls int
	Errors     int
}

// IndexerMetrics — метрики индексатора для графиков.
type IndexerMetrics struct {
	FilesPerSec MetricsTimeSeries // файлов в секунду
	ChunksTotal int
	SymbolsTotal int
}

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

// LSPError — ошибка LSP запроса.
type LSPError struct {
	Method    string    `json:"method"` // definition/hover/references
	Path      string    `json:"path"`
	Line      int       `json:"line"`
	Char      int       `json:"char"`
	Error     string    `json:"error"`
	Timestamp time.Time `json:"timestamp"`
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
	LSP       []LSPError         `json:"lsp,omitempty"`

	// Метрики для графиков.
	IndexerMetrics map[string]*IndexerMetrics `json:"-"`
	MCPCallHistory []int                      `json:"-"` // last 60s
	MCPErrHistory  []int                      `json:"-"` // last 60s
	OllamaLatency  map[string]*OllamaLatency  `json:"-"` // model -> latency
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
	lsp      []LSPError // ring buffer (newest first)
	maxItems int

	// Метрики для графиков.
	indexerMetrics map[string]*IndexerMetrics // per-indexer time series
	filesIndexedTotal int                      // кумулятивный счётчик
	prevIndexed    map[string]int             // предыдущее значение для расчёта delta
	mcpCallHistory []int                      // MCP calls per second (last 60s)
	mcpErrorHistory []int                     // MCP errors per second (last 60s)
	currentMCPsec  int                        // calls in current second bucket
	currentMCPErrSec int                      // errors in current second bucket
	lastTick       time.Time                  // последний tick для расчёта rate

	// Ollama latency metrics.
	ollamaLatency map[string]*OllamaLatency // model -> latency metrics
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
		b.currentMCPErrSec++
	}
	b.currentMCPsec++
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

// RecordTick — вызывается каждый тик (1 сек) для обновления метрик.
// Фиксирует delta файлов и MCP-вызовов за секунду.
func (b *Bus) RecordTick() {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(b.lastTick).Seconds()
	if elapsed < 0.5 {
		return // слишком часто
	}
	b.lastTick = now

	// Indexer metrics: delta files per second.
	for name, idx := range b.indexers {
		prev := b.prevIndexed[name]
		delta := idx.FilesIndexed - prev
		b.prevIndexed[name] = idx.FilesIndexed

		m, ok := b.indexerMetrics[name]
		if !ok {
			m = &IndexerMetrics{}
			b.indexerMetrics[name] = m
		}
		if elapsed > 0 {
			rate := float64(delta) / elapsed
			m.FilesPerSec.Push(rate, 60)
		}
		m.ChunksTotal = idx.Chunks
		m.SymbolsTotal = idx.Symbols
	}

	// MCP metrics: flush current second bucket.
	b.mcpCallHistory = append(b.mcpCallHistory, b.currentMCPsec)
	b.mcpErrorHistory = append(b.mcpErrorHistory, b.currentMCPErrSec)
	if len(b.mcpCallHistory) > 60 {
		b.mcpCallHistory = b.mcpCallHistory[len(b.mcpCallHistory)-60:]
	}
	if len(b.mcpErrorHistory) > 60 {
		b.mcpErrorHistory = b.mcpErrorHistory[len(b.mcpErrorHistory)-60:]
	}
	b.currentMCPsec = 0
	b.currentMCPErrSec = 0
}

// SetOllamaLatency записывает latency вызова ollama модели.
func (b *Bus) SetOllamaLatency(model string, latencyMs float64, isError bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	cur, ok := b.ollamaLatency[model]
	if !ok {
		cur = &OllamaLatency{Model: model}
		b.ollamaLatency[model] = cur
	}
	cur.LatencyMs.Push(latencyMs, 60)
	cur.TotalCalls++
	if isError {
		cur.Errors++
	}
}

// SetDocker обновляет статус docker-compose.
func (b *Bus) SetDocker(s DockerStatus) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s.UpdatedAt = time.Now()
	b.docker = s
}

// AddLSPError добавляет ошибку LSP запроса в ring buffer.
func (b *Bus) AddLSPError(method, path string, line, char int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := LSPError{
		Method:    method,
		Path:      path,
		Line:      line,
		Char:      char,
		Error:     err.Error(),
		Timestamp: time.Now(),
	}
	b.lsp = append([]LSPError{e}, b.lsp...)
	if len(b.lsp) > b.maxItems {
		b.lsp = b.lsp[:b.maxItems]
	}
}

// Snapshot возвращает копию текущего состояния.
func (b *Bus) Snapshot() Snapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()
	snap := Snapshot{
		StartedAt:      b.started,
		Root:           b.root,
		Indexers:       make(map[string]Indexer, len(b.indexers)),
		MCP:            make(map[string]MCPStat, len(b.mcp)),
		Recent:         append([]FileEntry(nil), b.recent...),
		Docker:         b.docker,
		LSP:            append([]LSPError(nil), b.lsp...),
		IndexerMetrics: make(map[string]*IndexerMetrics, len(b.indexerMetrics)),
		MCPCallHistory: append([]int(nil), b.mcpCallHistory...),
		MCPErrHistory:  append([]int(nil), b.mcpErrorHistory...),
		OllamaLatency:  make(map[string]*OllamaLatency, len(b.ollamaLatency)),
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
	for k, v := range b.indexerMetrics {
		m := &IndexerMetrics{
			ChunksTotal:  v.ChunksTotal,
			SymbolsTotal: v.SymbolsTotal,
		}
		m.FilesPerSec.Values = append([]float64(nil), v.FilesPerSec.Values...)
		snap.IndexerMetrics[k] = m
	}
	for k, v := range b.ollamaLatency {
		m := &OllamaLatency{
			Model:      v.Model,
			TotalCalls: v.TotalCalls,
			Errors:     v.Errors,
		}
		m.LatencyMs.Values = append([]float64(nil), v.LatencyMs.Values...)
		snap.OllamaLatency[k] = m
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
