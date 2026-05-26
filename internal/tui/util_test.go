package tui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ragota/internal/state"
)

// ─── relToCwd ────────────────────────────────────────────────────────────────

func TestRelToCwd_RelativePath(t *testing.T) {
	cwd := "/home/user/project"
	got := relToCwd("/home/user/project/src/main.go", cwd)
	assert.Equal(t, "src/main.go", got)
}

func TestRelToCwd_SameDir(t *testing.T) {
	cwd := "/home/user/project"
	got := relToCwd("/home/user/project", cwd)
	assert.Equal(t, ".", got)
}

func TestRelToCwd_OutsideCwd(t *testing.T) {
	cwd := "/home/user/project"
	got := relToCwd("/other/path/file.go", cwd)
	assert.Equal(t, "/other/path/file.go", got, "path outside cwd should be returned as-is")
}

func TestRelToCwd_EmptyCwd(t *testing.T) {
	got := relToCwd("/some/path", "")
	assert.Equal(t, "/some/path", got)
}

func TestRelToCwd_EmptyPath(t *testing.T) {
	got := relToCwd("", "/home/user")
	assert.Equal(t, "", got)
}

func TestRelToCwd_BothEmpty(t *testing.T) {
	got := relToCwd("", "")
	assert.Equal(t, "", got)
}

func TestRelToCwd_ParentDirectory(t *testing.T) {
	cwd := "/home/user/project/sub"
	got := relToCwd("/home/user/other/file.go", cwd)
	// path goes up with ".." so should be returned as-is
	assert.Equal(t, "/home/user/other/file.go", got)
}

// ─── truncateStart ──────────────────────────────────────────────────────────

func TestTruncateStart_ShortString(t *testing.T) {
	got := truncateStart("hello", 10)
	assert.Equal(t, "hello", got)
}

func TestTruncateStart_ExactLength(t *testing.T) {
	got := truncateStart("hello", 5)
	assert.Equal(t, "hello", got)
}

func TestTruncateStart_Truncated(t *testing.T) {
	got := truncateStart("abcdefghij", 5)
	assert.Equal(t, "…ghij", got)
}

func TestTruncateStart_Unicode(t *testing.T) {
	got := truncateStart("こんにちは世界", 4)
	assert.Equal(t, "…は世界", got)
}

func TestTruncateStart_WidthTwo(t *testing.T) {
	got := truncateStart("abcdef", 2)
	assert.Equal(t, "…f", got)
}

func TestTruncateStart_WidthOne(t *testing.T) {
	got := truncateStart("abcdef", 1)
	// n < 2: returns last n chars without ellipsis
	assert.Equal(t, "f", got)
}

func TestTruncateStart_Empty(t *testing.T) {
	got := truncateStart("", 5)
	assert.Equal(t, "", got)
}

func TestTruncateStart_ZeroWidth(t *testing.T) {
	got := truncateStart("abc", 0)
	assert.Equal(t, "", got)
}

// ─── toFloat64 ───────────────────────────────────────────────────────────────

func TestToFloat64_NonEmpty(t *testing.T) {
	got := toFloat64([]int{1, 2, 3})
	assert.Equal(t, []float64{1.0, 2.0, 3.0}, got)
}

func TestToFloat64_Empty(t *testing.T) {
	got := toFloat64([]int{})
	assert.Equal(t, []float64{}, got)
}

func TestToFloat64_Nil(t *testing.T) {
	var input []int
	got := toFloat64(input)
	assert.Equal(t, []float64{}, got)
}

func TestToFloat64_LargeValues(t *testing.T) {
	got := toFloat64([]int{1000000, -500})
	assert.Equal(t, []float64{1000000.0, -500.0}, got)
}

// ─── clampInt ────────────────────────────────────────────────────────────────

func TestClampInt_InRange(t *testing.T) {
	assert.Equal(t, 5, clampInt(5, 0, 10))
}

func TestClampInt_BelowMin(t *testing.T) {
	assert.Equal(t, 0, clampInt(-5, 0, 10))
}

func TestClampInt_AboveMax(t *testing.T) {
	assert.Equal(t, 10, clampInt(100, 0, 10))
}

func TestClampInt_AtMin(t *testing.T) {
	assert.Equal(t, 0, clampInt(0, 0, 10))
}

func TestClampInt_AtMax(t *testing.T) {
	assert.Equal(t, 10, clampInt(10, 0, 10))
}

func TestClampInt_MinEqualsMax(t *testing.T) {
	assert.Equal(t, 5, clampInt(3, 5, 5))
	assert.Equal(t, 5, clampInt(7, 5, 5))
	assert.Equal(t, 5, clampInt(5, 5, 5))
}

func TestClampInt_Negative(t *testing.T) {
	assert.Equal(t, -5, clampInt(-5, -10, 0))
	assert.Equal(t, -10, clampInt(-100, -10, 0))
	assert.Equal(t, 0, clampInt(5, -10, 0))
}

// ─── prettyDuration ─────────────────────────────────────────────────────────

func TestPrettyDuration_Milliseconds(t *testing.T) {
	assert.Equal(t, "500ms", prettyDuration(500*time.Millisecond))
}

func TestPrettyDuration_Zero(t *testing.T) {
	assert.Equal(t, "0ms", prettyDuration(0))
}

func TestPrettyDuration_Seconds(t *testing.T) {
	assert.Equal(t, "5.0s", prettyDuration(5*time.Second))
}

func TestPrettyDuration_FractionalSeconds(t *testing.T) {
	assert.Equal(t, "1.5s", prettyDuration(1500*time.Millisecond))
}

func TestPrettyDuration_Minutes(t *testing.T) {
	assert.Equal(t, "2m30s", prettyDuration(2*time.Minute+30*time.Second))
}

func TestPrettyDuration_ExactMinute(t *testing.T) {
	assert.Equal(t, "1m0s", prettyDuration(time.Minute))
}

func TestPrettyDuration_SubSecond(t *testing.T) {
	assert.Equal(t, "100ms", prettyDuration(100*time.Millisecond))
}

// ─── computeRecentActivity ──────────────────────────────────────────────────

func TestComputeRecentActivity_Empty(t *testing.T) {
	got := computeRecentActivity(nil, 60)
	assert.Nil(t, got)
}

func TestComputeRecentActivity_SingleEntry(t *testing.T) {
	now := time.Now()
	entries := []state.FileEntry{
		{Path: "a.go", IndexedAt: now},
	}
	got := computeRecentActivity(entries, 10)
	require.Len(t, got, 10)
	assert.Equal(t, 1.0, got[0], "current second bucket should have 1")
	// All other buckets should be 0
	for i := 1; i < 10; i++ {
		assert.Equal(t, 0.0, got[i])
	}
}

func TestComputeRecentActivity_MultipleEntries(t *testing.T) {
	now := time.Now()
	entries := []state.FileEntry{
		{Path: "a.go", IndexedAt: now},
		{Path: "b.go", IndexedAt: now.Add(-1 * time.Second)},
		{Path: "c.go", IndexedAt: now.Add(-1 * time.Second)},
		{Path: "d.go", IndexedAt: now.Add(-5 * time.Second)},
	}
	got := computeRecentActivity(entries, 10)
	require.Len(t, got, 10)
	assert.Equal(t, 1.0, got[0])
	assert.Equal(t, 2.0, got[1])
	assert.Equal(t, 1.0, got[5])
}

func TestComputeRecentActivity_OutsideWindow(t *testing.T) {
	now := time.Now()
	entries := []state.FileEntry{
		{Path: "a.go", IndexedAt: now},
		{Path: "old.go", IndexedAt: now.Add(-120 * time.Second)},
	}
	got := computeRecentActivity(entries, 60)
	require.Len(t, got, 60)
	assert.Equal(t, 1.0, got[0])
	// Entry at 120s ago is outside 60s window
	total := 0.0
	for _, v := range got {
		total += v
	}
	assert.Equal(t, 1.0, total)
}

// ─── renderSparkline ────────────────────────────────────────────────────────

func TestRenderSparkline_Empty(t *testing.T) {
	got := renderSparkline(nil, 10, okStyle)
	assert.NotEmpty(t, got, "empty values should produce dimmed spaces")
}

func TestRenderSparkline_ZeroWidth(t *testing.T) {
	got := renderSparkline([]float64{1, 2, 3}, 0, okStyle)
	assert.Equal(t, "", got)
}

func TestRenderSparkline_NegativeWidth(t *testing.T) {
	got := renderSparkline([]float64{1, 2, 3}, -1, okStyle)
	assert.Equal(t, "", got)
}

func TestRenderSparkline_UniformValues(t *testing.T) {
	got := renderSparkline([]float64{5, 5, 5}, 10, okStyle)
	// All same values → rangeVal==0 → all sparkChars[0]
	assert.NotEmpty(t, got)
}

func TestRenderSparkline_TruncatesToWidth(t *testing.T) {
	values := make([]float64, 100)
	for i := range values {
		values[i] = float64(i)
	}
	// With width=10, only last 10 values should be used
	got := renderSparkline(values, 10, lipgloss.NewStyle())
	// The result should be a styled string of 10 runes
	assert.NotEmpty(t, got)
}

func TestRenderSparkline_IncreasingValues(t *testing.T) {
	got := renderSparkline([]float64{0, 1, 2, 3, 4, 5, 6, 7}, 10, lipgloss.NewStyle())
	assert.NotEmpty(t, got)
}

// ─── renderBarChart ─────────────────────────────────────────────────────────

func TestRenderBarChart_Empty(t *testing.T) {
	got := renderBarChart(nil, nil, 10, 5, okStyle, errStyle, nil)
	assert.Contains(t, got, "no data")
}

func TestRenderBarChart_SingleValue(t *testing.T) {
	got := renderBarChart([]float64{5.0}, []string{"a"}, 10, 3, okStyle, errStyle, nil)
	assert.NotEmpty(t, got)
}

func TestRenderBarChart_WithErrors(t *testing.T) {
	got := renderBarChart(
		[]float64{5.0, 3.0, 1.0},
		[]string{"a", "b", "c"},
		10, 3, okStyle, errStyle,
		[]float64{0, 1, 0},
	)
	assert.NotEmpty(t, got)
}

func TestRenderBarChart_ExceedsMaxWidth(t *testing.T) {
	values := make([]float64, 20)
	for i := range values {
		values[i] = float64(i + 1)
	}
	labels := make([]string, 20)
	for i := range labels {
		labels[i] = "x"
	}
	got := renderBarChart(values, labels, 5, 3, okStyle, errStyle, nil)
	assert.NotEmpty(t, got)
}

func TestRenderBarChart_ZeroValues(t *testing.T) {
	got := renderBarChart([]float64{0, 0, 0}, nil, 10, 3, okStyle, errStyle, nil)
	assert.NotEmpty(t, got)
}

// ─── renderProgressBar ──────────────────────────────────────────────────────

func TestRenderProgressBar_ZeroTotal(t *testing.T) {
	got := renderProgressBar(0, 0, 10, okStyle)
	assert.Contains(t, got, "░")
}

func TestRenderProgressBar_Half(t *testing.T) {
	got := renderProgressBar(5, 10, 10, okStyle)
	assert.Contains(t, got, "50%")
}

func TestRenderProgressBar_Full(t *testing.T) {
	got := renderProgressBar(10, 10, 10, okStyle)
	assert.Contains(t, got, "100%")
}

func TestRenderProgressBar_OverFull(t *testing.T) {
	got := renderProgressBar(15, 10, 10, okStyle)
	// pct > 1 → clamped to 1
	assert.Contains(t, got, "100%")
}

func TestRenderProgressBar_Empty(t *testing.T) {
	got := renderProgressBar(0, 10, 10, okStyle)
	assert.Contains(t, got, "0%")
}

// ─── statusIndicator ────────────────────────────────────────────────────────

func TestStatusIndicator_AllCases(t *testing.T) {
	tests := []struct {
		status string
		want   string
	}{
		{"idle", "●"},
		{"scanning", "◐"},
		{"indexing", "◐"},
		{"error", "✗"},
		{"unknown", "○"},
		{"", "○"},
	}
	for _, tc := range tests {
		t.Run(tc.status, func(t *testing.T) {
			got := statusIndicator(tc.status)
			assert.Contains(t, got, tc.want)
		})
	}
}

// ─── renderBanner ───────────────────────────────────────────────────────────

func TestRenderBanner(t *testing.T) {
	got := renderBanner(80)
	assert.NotEmpty(t, got)
	assert.Contains(t, got, "█")
}

// ─── renderMCPMiniTable ─────────────────────────────────────────────────────

func TestRenderMCPMiniTable_NoServers(t *testing.T) {
	got := renderMCPMiniTable(nil, nil, nil, 60)
	assert.Contains(t, got, "no MCP servers")
}

func TestRenderMCPMiniTable_WithServers(t *testing.T) {
	mcp := map[string]state.MCPStat{
		"vector": {
			Server:  "vector",
			Running: true,
			Calls:   map[string]int{"search": 10},
			Errors:  2,
		},
	}
	history := []int{1, 2, 3, 0, 1}
	errHistory := []int{0, 1, 0, 0, 0}
	got := renderMCPMiniTable(mcp, history, errHistory, 60)
	assert.Contains(t, got, "MCP servers")
	assert.Contains(t, got, "10 calls")
	assert.Contains(t, got, "2 errors")
}

func TestRenderMCPMiniTable_NoErrors(t *testing.T) {
	mcp := map[string]state.MCPStat{
		"ts": {
			Server:  "ts",
			Running: true,
			Calls:   map[string]int{"list": 5},
			Errors:  0,
		},
	}
	history := []int{1, 0, 1}
	errHistory := []int{0, 0, 0}
	got := renderMCPMiniTable(mcp, history, errHistory, 60)
	assert.Contains(t, got, "MCP servers")
	assert.Contains(t, got, "5 calls")
}

// ─── renderDockerSection ────────────────────────────────────────────────────

func TestRenderDockerSection_Error(t *testing.T) {
	d := state.DockerStatus{LastError: "connection refused"}
	got := renderDockerSection(d, 60)
	assert.Contains(t, got, "connection refused")
}

func TestRenderDockerSection_NotStarted(t *testing.T) {
	d := state.DockerStatus{}
	got := renderDockerSection(d, 60)
	assert.Contains(t, got, "not started")
}

func TestRenderDockerSection_Running(t *testing.T) {
	d := state.DockerStatus{
		Running:  true,
		Services: []string{"qdrant(Up)", "ollama(Up)"},
	}
	got := renderDockerSection(d, 60)
	assert.Contains(t, got, "qdrant(Up)")
	assert.Contains(t, got, "ollama(Up)")
}

// ─── renderIndexerDashboard ─────────────────────────────────────────────────

func TestRenderIndexerDashboard_Graph(t *testing.T) {
	idx := state.Indexer{
		Status:       "idle",
		FilesTotal:   100,
		FilesIndexed: 50,
		Chunks:       200,
		Symbols:      300,
	}
	got := renderIndexerDashboard("graph", idx, nil, 60)
	assert.Contains(t, got, "graph")
	assert.Contains(t, got, "edges=200")
	assert.Contains(t, got, "units=300")
}

func TestRenderIndexerDashboard_TreeSitter(t *testing.T) {
	idx := state.Indexer{
		Status:       "scanning",
		FilesTotal:   50,
		FilesIndexed: 25,
		Chunks:       100,
		Symbols:      150,
	}
	got := renderIndexerDashboard("treesitter", idx, nil, 60)
	assert.Contains(t, got, "chunks=100")
	assert.Contains(t, got, "symbols=150")
}

func TestRenderIndexerDashboard_WithError(t *testing.T) {
	idx := state.Indexer{
		Status:    "error",
		LastError: "disk full",
	}
	got := renderIndexerDashboard("vector", idx, nil, 60)
	assert.Contains(t, got, "disk full")
}

// ─── renderErrorsSection ────────────────────────────────────────────────────

func TestRenderErrorsSection_WithErrors(t *testing.T) {
	errs := []state.FileEntry{
		{Path: "/home/user/project/a.go", Kind: "write", Error: "parse failed", IndexedAt: time.Now()},
	}
	got := renderErrorsSection(errs, 80)
	assert.Contains(t, got, "parse failed")
}

func TestRenderErrorsSection_MultilineError(t *testing.T) {
	errs := []state.FileEntry{
		{Path: "/home/user/project/a.go", Kind: "write", Error: "line1\nline2\nline3", IndexedAt: time.Now()},
	}
	got := renderErrorsSection(errs, 80)
	assert.Contains(t, got, "line1")
	assert.NotContains(t, got, "line2")
}

func TestRenderErrorsSection_MoreThanFive(t *testing.T) {
	var errs []state.FileEntry
	for i := 0; i < 8; i++ {
		errs = append(errs, state.FileEntry{
			Path:      filepath.Join("/project", "file.go"),
			Kind:      "write",
			Error:     "error",
			IndexedAt: time.Now(),
		})
	}
	got := renderErrorsSection(errs, 80)
	assert.Contains(t, got, "+3 more")
}

// ─── renderOllamaLatencySection ─────────────────────────────────────────────

func TestRenderOllamaLatencySection_Empty(t *testing.T) {
	got := renderOllamaLatencySection(nil, 60)
	assert.Contains(t, got, "no calls yet")
}

func TestRenderOllamaLatencySection_WithModels(t *testing.T) {
	latency := map[string]*state.OllamaLatency{
		"qwen3:0.6b": {
			Model:      "qwen3:0.6b",
			LatencyMs:  state.MetricsTimeSeries{Values: []float64{100, 200, 150}},
			TotalCalls: 5,
			Errors:     1,
		},
	}
	got := renderOllamaLatencySection(latency, 60)
	assert.Contains(t, got, "qwen3:0.6b")
	assert.Contains(t, got, "calls=5")
	assert.Contains(t, got, "errors=1")
}

// ─── logFilePathHint ────────────────────────────────────────────────────────

func TestLogFilePathHint(t *testing.T) {
	got := logFilePathHint()
	assert.Equal(t, ".ragota/logs/tui.log", got)
}

// ─── openLogFile ────────────────────────────────────────────────────────────

func TestOpenLogFile_CreatesFile(t *testing.T) {
	tmpDir := t.TempDir()
	origWd, _ := os.Getwd()
	require.NoError(t, os.Chdir(tmpDir))
	defer func() { _ = os.Chdir(origWd) }()

	f, path := openLogFile()
	require.NotNil(t, f)
	assert.NotEmpty(t, path)
	assert.FileExists(t, path)

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(content), "ragota TUI started at")

	f.Close()
}

// ─── model appendLogs ───────────────────────────────────────────────────────

func TestAppendLogs_NilLogFile(t *testing.T) {
	m := &model{logFile: nil, seen: make(map[string]struct{})}
	// Should not panic
	m.appendLogs()
}

func TestAppendLogs_NilSeen(t *testing.T) {
	m := &model{logFile: nil, seen: nil}
	// Should not panic
	m.appendLogs()
}

func TestAppendLogs_WritesToFile(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")
	f, err := os.Create(logPath)
	require.NoError(t, err)
	defer f.Close()

	now := time.Now()
	m := &model{
		logFile: f,
		seen:    make(map[string]struct{}),
		snap: state.Snapshot{
			Recent: []state.FileEntry{
				{Path: "/a/b.go", Kind: "write", IndexedAt: now, Chunks: 3, Symbols: 5, DurationMs: 100},
			},
		},
	}
	m.appendLogs()

	content, err := os.ReadFile(logPath)
	require.NoError(t, err)
	s := string(content)
	assert.Contains(t, s, "/a/b.go")
	assert.Contains(t, s, "chunks=3")
	assert.Contains(t, s, "symbols=5")
	assert.Contains(t, s, "100ms")
}

func TestAppendLogs_Deduplicates(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")
	f, err := os.Create(logPath)
	require.NoError(t, err)
	defer f.Close()

	now := time.Now()
	m := &model{
		logFile: f,
		seen:    make(map[string]struct{}),
		snap: state.Snapshot{
			Recent: []state.FileEntry{
				{Path: "/a/b.go", Kind: "write", IndexedAt: now, DurationMs: 50},
			},
		},
	}

	// First call writes
	m.appendLogs()
	// Second call should NOT write (deduplicated)
	m.appendLogs()

	content, err := os.ReadFile(logPath)
	require.NoError(t, err)
	// Should only have one occurrence of the path
	count := 0
	for i := 0; i < len(string(content))-len("/a/b.go"); i++ {
		if string(content)[i:i+len("/a/b.go")] == "/a/b.go" {
			count++
		}
	}
	assert.Equal(t, 1, count, "entry should be logged exactly once")
}

func TestAppendLogs_WithError(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")
	f, err := os.Create(logPath)
	require.NoError(t, err)
	defer f.Close()

	now := time.Now()
	m := &model{
		logFile: f,
		seen:    make(map[string]struct{}),
		snap: state.Snapshot{
			Recent: []state.FileEntry{
				{Path: "/a/b.go", Kind: "scan", IndexedAt: now, Error: "parse\nfailed", DurationMs: 10},
			},
		},
	}
	m.appendLogs()

	content, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "error=parse failed")
}

// ─── model View ─────────────────────────────────────────────────────────────

func TestModelView_ZeroWidth(t *testing.T) {
	m := model{width: 0}
	got := m.View()
	assert.Contains(t, got, "Initializing")
}

func TestModelView_WithWidth(t *testing.T) {
	m := model{
		width:  120,
		height: 40,
		bus:    state.NewBus("/tmp/test"),
		snap: state.Snapshot{
			StartedAt: time.Now().Add(-5 * time.Minute),
			Root:      "/tmp/test",
			Indexers:  make(map[string]state.Indexer),
			MCP:       make(map[string]state.MCPStat),
		},
	}
	got := m.View()
	assert.NotEmpty(t, got)
	assert.Contains(t, got, "press q to quit")
}

// ─── model Update ───────────────────────────────────────────────────────────

func TestModelUpdate_WindowSize(t *testing.T) {
	m := model{width: 0, height: 0, seen: make(map[string]struct{})}
	newM, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
	updated := newM.(model)
	assert.Equal(t, 100, updated.width)
	assert.Equal(t, 50, updated.height)
}

func TestModelUpdate_KeyMsg_Q(t *testing.T) {
	m := model{width: 80, height: 24, seen: make(map[string]struct{})}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	// tea.Quit returns a non-nil cmd
	assert.NotNil(t, cmd)
}

func TestModelUpdate_UnknownMsg(t *testing.T) {
	m := model{width: 80, height: 24, seen: make(map[string]struct{})}
	newM, cmd := m.Update("unknown_msg")
	// Should return model unchanged and nil cmd
	updated := newM.(model)
	assert.Equal(t, 80, updated.width)
	assert.Nil(t, cmd)
}
