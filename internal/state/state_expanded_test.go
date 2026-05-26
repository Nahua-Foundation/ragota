package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// MetricsTimeSeries.Push — ring buffer behaviour
// ---------------------------------------------------------------------------

func TestMetricsTimeSeries_Push_InitializesSlice(t *testing.T) {
	var ts MetricsTimeSeries
	assert.Nil(t, ts.Values)
	ts.Push(1.0, 5)
	assert.Len(t, ts.Values, 1)
	assert.Equal(t, 1.0, ts.Values[0])
}

func TestMetricsTimeSeries_Push_RespectsMaxPoints(t *testing.T) {
	var ts MetricsTimeSeries
	for i := 0; i < 10; i++ {
		ts.Push(float64(i), 5)
	}
	assert.Len(t, ts.Values, 5)
	// Last 5 values: 5, 6, 7, 8, 9.
	assert.Equal(t, []float64{5, 6, 7, 8, 9}, ts.Values)
}

func TestMetricsTimeSeries_Push_ExactlyMaxPoints(t *testing.T) {
	var ts MetricsTimeSeries
	for i := 1; i <= 3; i++ {
		ts.Push(float64(i), 3)
	}
	assert.Equal(t, []float64{1, 2, 3}, ts.Values)
}

func TestMetricsTimeSeries_Push_MaxOne(t *testing.T) {
	var ts MetricsTimeSeries
	ts.Push(10, 1)
	ts.Push(20, 1)
	ts.Push(30, 1)
	assert.Equal(t, []float64{30}, ts.Values)
}

func TestMetricsTimeSeries_Push_ZeroValues(t *testing.T) {
	var ts MetricsTimeSeries
	ts.Push(0, 3)
	ts.Push(0, 3)
	assert.Equal(t, []float64{0, 0}, ts.Values)
}

func TestMetricsTimeSeries_Push_NegativeValues(t *testing.T) {
	var ts MetricsTimeSeries
	ts.Push(-5.5, 10)
	assert.Equal(t, -5.5, ts.Values[0])
}

// ---------------------------------------------------------------------------
// Bus.SetIndexer — edge cases
// ---------------------------------------------------------------------------

func TestSetIndexer_MultipleIndexers(t *testing.T) {
	b := NewBus("/r")
	b.SetIndexer("vector", func(i *Indexer) { i.Status = "idle" })
	b.SetIndexer("treesitter", func(i *Indexer) { i.Status = "scanning" })

	snap := b.Snapshot()
	assert.Equal(t, "idle", snap.Indexers["vector"].Status)
	assert.Equal(t, "scanning", snap.Indexers["treesitter"].Status)
}

func TestSetIndexer_PreservesFieldsAcrossUpdates(t *testing.T) {
	b := NewBus("/r")
	b.SetIndexer("vector", func(i *Indexer) {
		i.Status = "scanning"
		i.FilesTotal = 100
		i.FilesIndexed = 50
		i.Chunks = 200
		i.Symbols = 500
	})
	b.SetIndexer("vector", func(i *Indexer) {
		i.Status = "idle"
		// Other fields should be preserved from previous call.
	})

	snap := b.Snapshot()
	v := snap.Indexers["vector"]
	assert.Equal(t, "idle", v.Status)
	assert.Equal(t, 100, v.FilesTotal)
	assert.Equal(t, 50, v.FilesIndexed)
	assert.Equal(t, 200, v.Chunks)
	assert.Equal(t, 500, v.Symbols)
}

func TestSetIndexer_SetLastError(t *testing.T) {
	b := NewBus("/r")
	b.SetIndexer("vector", func(i *Indexer) {
		i.Status = "error"
		i.LastError = "connection refused"
	})
	snap := b.Snapshot()
	assert.Equal(t, "error", snap.Indexers["vector"].Status)
	assert.Equal(t, "connection refused", snap.Indexers["vector"].LastError)
}

// ---------------------------------------------------------------------------
// Bus.AddRecent — edge cases
// ---------------------------------------------------------------------------

func TestAddRecent_EmptyEntry(t *testing.T) {
	b := NewBus("/r")
	b.AddRecent(FileEntry{})
	snap := b.Snapshot()
	require.Len(t, snap.Recent, 1)
	assert.Empty(t, snap.Recent[0].Path)
}

func TestAddRecent_AllFields(t *testing.T) {
	b := NewBus("/r")
	b.AddRecent(FileEntry{
		Path:       "foo.go",
		Kind:       "write",
		Chunks:     5,
		Symbols:    10,
		DurationMs: 123,
		Error:      "timeout",
	})
	snap := b.Snapshot()
	require.Len(t, snap.Recent, 1)
	r := snap.Recent[0]
	assert.Equal(t, "foo.go", r.Path)
	assert.Equal(t, "write", r.Kind)
	assert.Equal(t, 5, r.Chunks)
	assert.Equal(t, 10, r.Symbols)
	assert.Equal(t, int64(123), r.DurationMs)
	assert.Equal(t, "timeout", r.Error)
	assert.False(t, r.IndexedAt.IsZero())
}

func TestAddRecent_ExactlyMaxItems(t *testing.T) {
	b := NewBus("/r")
	b.maxItems = 2
	b.AddRecent(FileEntry{Path: "a"})
	b.AddRecent(FileEntry{Path: "b"})
	snap := b.Snapshot()
	assert.Len(t, snap.Recent, 2)
	assert.Equal(t, "b", snap.Recent[0].Path) // newest first
	assert.Equal(t, "a", snap.Recent[1].Path)
}

// ---------------------------------------------------------------------------
// Bus.IncMCPCall — edge cases
// ---------------------------------------------------------------------------

func TestIncMCPCall_MultipleServers(t *testing.T) {
	b := NewBus("/r")
	b.IncMCPCall("srv1", "search", false)
	b.IncMCPCall("srv2", "definition", true)
	b.IncMCPCall("srv2", "definition", false)

	snap := b.Snapshot()
	assert.Equal(t, 1, snap.MCP["srv1"].Calls["search"])
	assert.Equal(t, 0, snap.MCP["srv1"].Errors)
	assert.Equal(t, 2, snap.MCP["srv2"].Calls["definition"])
	assert.Equal(t, 1, snap.MCP["srv2"].Errors)
}

func TestIncMCPCall_AfterSetMCPRunningFalse(t *testing.T) {
	b := NewBus("/r")
	b.SetMCPRunning("srv", false)
	b.IncMCPCall("srv", "tool", false)
	// IncMCPCall sets Running=true.
	snap := b.Snapshot()
	assert.True(t, snap.MCP["srv"].Running)
}

func TestIncMCPCall_NilCallsMapInitialization(t *testing.T) {
	b := NewBus("/r")
	// Manually create entry with nil Calls map.
	b.mcp["srv"] = MCPStat{Server: "srv"}
	// IncMCPCall should initialize the map.
	b.IncMCPCall("srv", "tool", false)
	snap := b.Snapshot()
	assert.Equal(t, 1, snap.MCP["srv"].Calls["tool"])
}

// ---------------------------------------------------------------------------
// Bus.SetMCPRunning — edge cases
// ---------------------------------------------------------------------------

func TestSetMCPRunning_Idempotent(t *testing.T) {
	b := NewBus("/r")
	b.SetMCPRunning("srv", true)
	b.SetMCPRunning("srv", true)
	snap := b.Snapshot()
	assert.True(t, snap.MCP["srv"].Running)
}

// ---------------------------------------------------------------------------
// Bus.SetOllamaLatency
// ---------------------------------------------------------------------------

func TestSetOllamaLatency_Basic(t *testing.T) {
	b := NewBus("/r")
	b.SetOllamaLatency("llama3", 150.0, false)
	b.SetOllamaLatency("llama3", 200.0, false)
	b.SetOllamaLatency("llama3", 300.0, true)

	snap := b.Snapshot()
	require.Contains(t, snap.OllamaLatency, "llama3")
	ol := snap.OllamaLatency["llama3"]
	assert.Equal(t, "llama3", ol.Model)
	assert.Equal(t, 3, ol.TotalCalls)
	assert.Equal(t, 1, ol.Errors)
	assert.Len(t, ol.LatencyMs.Values, 3)
	assert.Equal(t, 150.0, ol.LatencyMs.Values[0])
}

func TestSetOllamaLatency_MultipleModels(t *testing.T) {
	b := NewBus("/r")
	b.SetOllamaLatency("model-a", 100, false)
	b.SetOllamaLatency("model-b", 200, true)

	snap := b.Snapshot()
	assert.Contains(t, snap.OllamaLatency, "model-a")
	assert.Contains(t, snap.OllamaLatency, "model-b")
	assert.Equal(t, 0, snap.OllamaLatency["model-a"].Errors)
	assert.Equal(t, 1, snap.OllamaLatency["model-b"].Errors)
}

func TestSetOllamaLatency_MaxPointsRingBuffer(t *testing.T) {
	b := NewBus("/r")
	for i := 0; i < 70; i++ {
		b.SetOllamaLatency("m", float64(i), false)
	}
	snap := b.Snapshot()
	// Push uses maxPoints=60, so should be capped at 60.
	assert.Len(t, snap.OllamaLatency["m"].LatencyMs.Values, 60)
	assert.Equal(t, 70, snap.OllamaLatency["m"].TotalCalls)
}

// ---------------------------------------------------------------------------
// Bus.SetDocker — edge cases
// ---------------------------------------------------------------------------

func TestSetDocker_EmptyServices(t *testing.T) {
	b := NewBus("/r")
	b.SetDocker(DockerStatus{Running: false})
	snap := b.Snapshot()
	assert.False(t, snap.Docker.Running)
	assert.Nil(t, snap.Docker.Services)
}

func TestSetDocker_WithError(t *testing.T) {
	b := NewBus("/r")
	b.SetDocker(DockerStatus{Running: false, LastError: "docker not found"})
	snap := b.Snapshot()
	assert.Equal(t, "docker not found", snap.Docker.LastError)
	assert.False(t, snap.Docker.UpdatedAt.IsZero())
}

// ---------------------------------------------------------------------------
// Bus.AddLSPError — edge cases
// ---------------------------------------------------------------------------

func TestAddLSPError_NilError(t *testing.T) {
	// Calling with a nil error would panic on err.Error() — let's verify
	// the caller is expected to pass non-nil. We test with a real error.
	b := NewBus("/r")
	b.AddLSPError("hover", "/x", 10, 5, errors.New("timeout"))
	snap := b.Snapshot()
	require.Len(t, snap.LSP, 1)
	assert.Equal(t, "timeout", snap.LSP[0].Error)
}

func TestAddLSPError_ExactMaxItems(t *testing.T) {
	b := NewBus("/r")
	b.maxItems = 3
	for i := 0; i < 3; i++ {
		b.AddLSPError("def", "/p", i, 0, errors.New("e"))
	}
	assert.Len(t, b.Snapshot().LSP, 3)
}

// ---------------------------------------------------------------------------
// Bus.RecordTick
// ---------------------------------------------------------------------------

func TestRecordTick_IndexerMetrics(t *testing.T) {
	b := NewBus("/r")
	b.SetIndexer("vector", func(i *Indexer) {
		i.FilesIndexed = 10
		i.Chunks = 50
		i.Symbols = 100
	})

	// Force lastTick to the past so elapsed >= 0.5.
	b.mu.Lock()
	b.lastTick = time.Now().Add(-2 * time.Second)
	b.mu.Unlock()

	b.RecordTick()

	snap := b.Snapshot()
	require.Contains(t, snap.IndexerMetrics, "vector")
	m := snap.IndexerMetrics["vector"]
	assert.Equal(t, 50, m.ChunksTotal)
	assert.Equal(t, 100, m.SymbolsTotal)
	assert.NotEmpty(t, m.FilesPerSec.Values)
}

func TestRecordTick_SkipsWhenTooSoon(t *testing.T) {
	b := NewBus("/r")
	b.SetIndexer("vector", func(i *Indexer) { i.FilesIndexed = 5 })
	// lastTick was just set by NewBus — elapsed < 0.5s.
	b.RecordTick()
	snap := b.Snapshot()
	// No metrics should have been recorded because elapsed < 0.5.
	assert.Empty(t, snap.IndexerMetrics)
}

func TestRecordTick_MCPCallHistory(t *testing.T) {
	b := NewBus("/r")
	b.IncMCPCall("srv", "tool", false)
	b.IncMCPCall("srv", "tool", true)

	b.mu.Lock()
	b.lastTick = time.Now().Add(-2 * time.Second)
	b.mu.Unlock()

	b.RecordTick()

	snap := b.Snapshot()
	require.NotEmpty(t, snap.MCPCallHistory)
	require.NotEmpty(t, snap.MCPErrHistory)
	// The current bucket should have captured 2 calls and 1 error.
	assert.Equal(t, 2, snap.MCPCallHistory[len(snap.MCPCallHistory)-1])
	assert.Equal(t, 1, snap.MCPErrHistory[len(snap.MCPErrHistory)-1])
}

func TestRecordTick_MCPCallHistoryCapAt60(t *testing.T) {
	b := NewBus("/r")
	for i := 0; i < 70; i++ {
		b.mu.Lock()
		b.lastTick = time.Now().Add(-2 * time.Second)
		b.mu.Unlock()
		b.RecordTick()
	}
	snap := b.Snapshot()
	assert.LessOrEqual(t, len(snap.MCPCallHistory), 60)
	assert.LessOrEqual(t, len(snap.MCPErrHistory), 60)
}

// ---------------------------------------------------------------------------
// Bus.Snapshot — deep copy verification
// ---------------------------------------------------------------------------

func TestSnapshot_LSPDeepCopy(t *testing.T) {
	b := NewBus("/r")
	b.AddLSPError("hover", "/x", 1, 2, errors.New("err"))
	snap := b.Snapshot()
	snap.LSP[0].Method = "MUTATED"

	snap2 := b.Snapshot()
	assert.Equal(t, "hover", snap2.LSP[0].Method)
}

func TestSnapshot_DockerStatusFields(t *testing.T) {
	b := NewBus("/r")
	b.SetDocker(DockerStatus{Running: true, Services: []string{"qdrant"}})
	snap := b.Snapshot()
	// DockerStatus is copied as a value struct; scalar fields are independent.
	assert.True(t, snap.Docker.Running)
	assert.Equal(t, []string{"qdrant"}, snap.Docker.Services)

	// Changing Running on the snapshot doesn't affect the bus.
	snap.Docker.Running = false
	snap2 := b.Snapshot()
	assert.True(t, snap2.Docker.Running)
}

func TestSnapshot_IndexerMetricsDeepCopy(t *testing.T) {
	b := NewBus("/r")
	b.SetIndexer("vector", func(i *Indexer) {
		i.FilesIndexed = 5
		i.Chunks = 10
		i.Symbols = 20
	})
	b.mu.Lock()
	b.lastTick = time.Now().Add(-2 * time.Second)
	b.mu.Unlock()
	b.RecordTick()

	snap := b.Snapshot()
	snap.IndexerMetrics["vector"].ChunksTotal = 999

	snap2 := b.Snapshot()
	assert.Equal(t, 10, snap2.IndexerMetrics["vector"].ChunksTotal)
}

func TestSnapshot_OllamaLatencyDeepCopy(t *testing.T) {
	b := NewBus("/r")
	b.SetOllamaLatency("m", 100, false)

	snap := b.Snapshot()
	snap.OllamaLatency["m"].TotalCalls = 999

	snap2 := b.Snapshot()
	assert.Equal(t, 1, snap2.OllamaLatency["m"].TotalCalls)
}

func TestSnapshot_EmptyBus(t *testing.T) {
	b := NewBus("/root")
	snap := b.Snapshot()
	assert.Equal(t, "/root", snap.Root)
	assert.Empty(t, snap.Indexers)
	assert.Empty(t, snap.MCP)
	assert.Empty(t, snap.Recent)
	assert.Empty(t, snap.LSP)
	assert.Empty(t, snap.MCPCallHistory)
	assert.Empty(t, snap.MCPErrHistory)
	assert.Empty(t, snap.OllamaLatency)
	assert.False(t, snap.StartedAt.IsZero())
}

// ---------------------------------------------------------------------------
// Bus.Persist — edge cases
// ---------------------------------------------------------------------------

func TestPersist_RoundTrip(t *testing.T) {
	b := NewBus("/repo")
	b.SetIndexer("ts", func(i *Indexer) { i.Status = "idle"; i.FilesTotal = 42 })
	b.AddRecent(FileEntry{Path: "main.go", Kind: "scan", Chunks: 3, Symbols: 7})
	b.IncMCPCall("sym", "get_symbol", false)
	b.SetOllamaLatency("embed", 50.5, false)
	b.SetDocker(DockerStatus{Running: true, Services: []string{"qdrant"}})
	b.AddLSPError("definition", "/a.go", 10, 5, errors.New("timeout"))

	path := filepath.Join(t.TempDir(), "state.json")
	require.NoError(t, b.Persist(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var snap Snapshot
	require.NoError(t, json.Unmarshal(data, &snap))

	assert.Equal(t, "/repo", snap.Root)
	assert.Equal(t, "idle", snap.Indexers["ts"].Status)
	assert.Equal(t, 42, snap.Indexers["ts"].FilesTotal)
	require.Len(t, snap.Recent, 1)
	assert.Equal(t, "main.go", snap.Recent[0].Path)
	assert.Equal(t, 1, snap.MCP["sym"].Calls["get_symbol"])
	assert.True(t, snap.Docker.Running)
	require.Len(t, snap.LSP, 1)
	assert.Equal(t, "timeout", snap.LSP[0].Error)
}

func TestPersist_InvalidDir(t *testing.T) {
	b := NewBus("/r")
	err := b.Persist("/nonexistent_dir_12345/stats.json")
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// Concurrent access safety
// ---------------------------------------------------------------------------

func TestBus_ConcurrentAccess(t *testing.T) {
	b := NewBus("/r")
	var wg sync.WaitGroup

	// Writers.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b.SetIndexer("vector", func(idx *Indexer) {
				idx.FilesIndexed = i
			})
			b.AddRecent(FileEntry{Path: "file.go"})
			b.IncMCPCall("srv", "tool", false)
			b.SetMCPRunning("srv", true)
			b.SetOllamaLatency("m", float64(i), false)
			b.SetDocker(DockerStatus{Running: true})
			b.AddLSPError("def", "/p", i, 0, errors.New("e"))
		}(i)
	}

	// Readers.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap := b.Snapshot()
			_ = snap.Root
		}()
	}

	wg.Wait()
	// If we reach here without race detector complaints, concurrent safety is OK.
	snap := b.Snapshot()
	assert.NotEmpty(t, snap.Indexers)
}

// ---------------------------------------------------------------------------
// NewBus defaults
// ---------------------------------------------------------------------------

func TestNewBus_InitializesOllamaLatency(t *testing.T) {
	b := NewBus("/r")
	assert.NotNil(t, b.ollamaLatency)
	assert.NotNil(t, b.indexerMetrics)
	assert.NotNil(t, b.prevIndexed)
	assert.False(t, b.lastTick.IsZero())
}
