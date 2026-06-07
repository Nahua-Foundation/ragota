package state

// Файл содержит метрики: RecordTick, SetOllamaLatency, Snapshot, Persist.

import (
	"encoding/json"
	"os"
	"time"
)

// RecordTick вызывается каждый тик (1 сек) для обновления метрик.
func (b *Bus) RecordTick() {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(b.lastTick).Seconds()
	if elapsed < 0.5 {
		return
	}
	b.lastTick = now

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
		Logs:           append([]LogEntry(nil), b.logs...),
		IndexerMetrics: make(map[string]*IndexerMetrics, len(b.indexerMetrics)),
		MCPCallHistory: append([]int(nil), b.mcpCallHistory...),
		MCPErrHistory:  append([]int(nil), b.mcpErrorHistory...),
		OllamaLatency:  make(map[string]*OllamaLatency, len(b.ollamaLatency)),
	}
	for k, v := range b.indexers {
		snap.Indexers[k] = v
	}
	for k, v := range b.mcp {
		calls := make(map[string]int, len(v.Calls))
		for kk, vv := range v.Calls {
			calls[kk] = vv
		}
		v.Calls = calls
		snap.MCP[k] = v
	}
	for k, v := range b.indexerMetrics {
		m := &IndexerMetrics{ChunksTotal: v.ChunksTotal, SymbolsTotal: v.SymbolsTotal}
		m.FilesPerSec.Values = append([]float64(nil), v.FilesPerSec.Values...)
		snap.IndexerMetrics[k] = m
	}
	for k, v := range b.ollamaLatency {
		m := &OllamaLatency{Model: v.Model, TotalCalls: v.TotalCalls, Errors: v.Errors}
		m.LatencyMs.Values = append([]float64(nil), v.LatencyMs.Values...)
		snap.OllamaLatency[k] = m
	}
	return snap
}

// Persist сохраняет snapshot в файл.
func (b *Bus) Persist(path string) error {
	snap := b.Snapshot()
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
