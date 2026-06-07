package tui

// Файл содержит секции TUI-дашборда: indexer dashboard, recent activity,
// ollama latency, MCP mini table.

import (
	"fmt"
	"strings"
	"time"

	"ragota/pkg/state"
)

func renderIndexerDashboard(name string, idx state.Indexer, metrics *state.IndexerMetrics, width int) string {
	statusIcon := statusIndicator(idx.Status)

	// Прогресс и статистика зависят от статуса.
	var progressStr, statsStr string

	switch idx.Status {
	case "waiting":
		progressStr = dimStyle.Render("waiting...")
		if idx.LastError != "" {
			statsStr = warnStyle.Render(idx.LastError)
		}

	case "scanning":
		progressStr = dimStyle.Render("scanning files...")
		statsStr = ""

	case "indexing":
		progressBar := renderProgressBar(idx.FilesIndexed, idx.FilesTotal, 20, okStyle)
		progressStr = fmt.Sprintf("%s  %d/%d", progressBar, idx.FilesIndexed, idx.FilesTotal)
		if idx.Chunks > 0 {
			// Vector indexer: только chunks (units/edges — это AST)
			statsStr = dimStyle.Render(fmt.Sprintf("chunks=%s", prettyCount(idx.Chunks)))
		} else if idx.Symbols > 0 {
			// AST indexer: units + edges
			statsStr = dimStyle.Render(fmt.Sprintf("units=%d edges=%d", idx.Symbols, idx.Chunks))
		}

	case "resolving":
		// Resolving — только для AST индексера (vector не резолвит edges).
		pass := idx.ResolvePass
		if pass < 1 {
			pass = 1
		}
		totalPasses := idx.ResolveTotal
		if totalPasses < 4 {
			totalPasses = 4
		}
		pending := idx.PendingEdges
		bar := renderProgressBar(idx.FilesIndexed, idx.FilesTotal, 20, warnStyle)

		if pending > 0 {
			progressStr = fmt.Sprintf("%s  %d/%d  pass %d/%d", bar, idx.FilesIndexed, idx.FilesTotal, pass, totalPasses)
		} else {
			progressStr = fmt.Sprintf("%s  %d/%d  finalizing...", bar, idx.FilesIndexed, idx.FilesTotal)
		}
		statsStr = dimStyle.Render(fmt.Sprintf("%s edges pending", prettyCount(pending)))

	case "detecting", "classifying", "writing":
		// Cross-repo indexer statuses
		if idx.Status == "classifying" && idx.FilesTotal > 0 {
			progressBar := renderProgressBar(idx.FilesIndexed, idx.FilesTotal, 20, okStyle)
			pct := int(float64(idx.FilesIndexed) / float64(idx.FilesTotal) * 100)
			progressStr = fmt.Sprintf("%s  %d/%d (%d%%)", progressBar, idx.FilesIndexed, idx.FilesTotal, pct)
		} else {
			progressStr = dimStyle.Render(fmt.Sprintf("%s...", idx.Status))
		}
		if idx.Symbols > 0 || idx.Chunks > 0 {
			statsStr = dimStyle.Render(fmt.Sprintf("import_edges=%d call_edges=%d", idx.Symbols, idx.Chunks))
		}

	case "idle":
		// Cross-repo idle: показываем edges вместо файлов
		if name == "crossrepo" {
			progressStr = okStyle.Render("✓ indexed")
			statsStr = dimStyle.Render(fmt.Sprintf("import_edges=%d call_edges=%d", idx.Symbols, idx.Chunks))
		} else {
			progressStr = okStyle.Render(fmt.Sprintf("✓ %d files", idx.FilesTotal))
			if idx.Chunks > 0 && idx.Symbols == 0 {
				// Vector indexer: только chunks
				statsStr = dimStyle.Render(fmt.Sprintf("chunks=%s", prettyCount(idx.Chunks)))
			} else {
				// AST indexer: units + edges
				statsStr = dimStyle.Render(fmt.Sprintf("units=%d edges=%d", idx.Symbols, idx.Chunks))
			}
		}

	case "error":
		progressStr = errStyle.Render(fmt.Sprintf("✗ %d/%d files", idx.FilesIndexed, idx.FilesTotal))
		statsStr = errStyle.Render("Error: " + idx.LastError)

	default:
		progressStr = dimStyle.Render("not started")
		statsStr = ""
	}

	line := fmt.Sprintf("  %s %-11s %s", statusIcon, name, progressStr)
	if statsStr != "" {
		line += "  " + statsStr
	}
	return line
}

func prettyCount(n int) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func computeRecentActivity(recent []state.FileEntry, windowSec int) []float64 {
	if len(recent) == 0 {
		return nil
	}
	buckets := make(map[int]int)
	now := recent[0].IndexedAt
	for _, e := range recent {
		secAgo := int(now.Sub(e.IndexedAt).Seconds())
		if secAgo < windowSec {
			buckets[secAgo]++
		}
	}
	result := make([]float64, windowSec)
	for i := 0; i < windowSec; i++ {
		result[i] = float64(buckets[i])
	}
	return result
}

func renderOllamaLatencySection(latency map[string]*state.OllamaLatency, width int) string {
	if len(latency) == 0 {
		return headerStyle.Render("Ollama models") + "\n" + dimStyle.Render("  no calls yet")
	}
	var lines []string
	lines = append(lines, headerStyle.Render("Ollama models"))
	models := make([]string, 0, len(latency))
	for k := range latency {
		models = append(models, k)
	}
	for i := 0; i < len(models); i++ {
		for j := i + 1; j < len(models); j++ {
			if models[i] > models[j] {
				models[i], models[j] = models[j], models[i]
			}
		}
	}
	for _, model := range models {
		m := latency[model]
		var avgMs float64
		if len(m.LatencyMs.Values) > 0 {
			var sum float64
			for _, v := range m.LatencyMs.Values {
				sum += v
			}
			avgMs = sum / float64(len(m.LatencyMs.Values))
		}
		var lastMs float64
		if len(m.LatencyMs.Values) > 0 {
			lastMs = m.LatencyMs.Values[len(m.LatencyMs.Values)-1]
		}
		line := fmt.Sprintf("  %-20s avg=%dms  last=%dms  calls=%d", model, int(avgMs), int(lastMs), m.TotalCalls)
		if m.Errors > 0 {
			line += errStyle.Render(fmt.Sprintf("  errors=%d", m.Errors))
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func renderMCPMiniTable(mcp map[string]state.MCPStat, callHistory, errHistory []int, maxWidth int) string {
	if len(mcp) == 0 {
		return dimStyle.Render("  no MCP servers connected")
	}
	var lines []string
	lines = append(lines, headerStyle.Render("MCP servers"))
	if len(callHistory) > 0 {
		line := fmt.Sprintf("  calls/s: %s", renderSparkline(toFloat64(callHistory), maxWidth-12, okStyle))
		lines = append(lines, line)
	}
	if len(errHistory) > 0 {
		hasErrors := false
		for _, v := range errHistory {
			if v > 0 {
				hasErrors = true
				break
			}
		}
		if hasErrors {
			line := fmt.Sprintf("  errors:  %s", renderSparkline(toFloat64(errHistory), maxWidth-12, errStyle))
			lines = append(lines, line)
		}
	}
	totalCalls := 0
	totalErrors := 0
	for _, st := range mcp {
		for _, c := range st.Calls {
			totalCalls += c
		}
		totalErrors += st.Errors
	}
	summary := fmt.Sprintf("  total: %d calls, %d errors", totalCalls, totalErrors)
	lines = append(lines, dimStyle.Render(summary))
	return strings.Join(lines, "\n")
}

func toFloat64[ints ~[]int](vals ints) []float64 {
	out := make([]float64, len(vals))
	for i, v := range vals {
		out[i] = float64(v)
	}
	return out
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// renderLogsSection рендерит секцию логов (warn/error) для TUI.
// Максимум 15 строк, каждая строка обрезается до width.
func renderLogsSection(logs []state.LogEntry, width int) string {
	var lines []string
	lines = append(lines, headerStyle.Render("Logs"))

	for _, entry := range logs {
		// Формат: HH:MM:SS [LEVEL] message
		timeStr := entry.Timestamp.Format("15:04:05")
		levelStr := entry.Level

		// Стилизуем уровень
		var levelStyled string
		if entry.Level == "error" {
			levelStr = "[ERR]"
			levelStyled = errStyle.Render(levelStr)
		} else {
			levelStr = "[WRN]"
			levelStyled = warnStyle.Render(levelStr)
		}

		// Формируем строку: "  HH:MM:SS [LEVEL] message"
		prefix := fmt.Sprintf("  %s %s ", dimStyle.Render(timeStr), levelStyled)
		// Обрезаем message чтобы вся строка влезла в width
		// Примерная длина prefix: 2 + 8 + 1 + 5 + 1 = 17 символов (без стилей)
		msgMaxLen := width - 20 // 20 = "  HH:MM:SS [WRN] " + запас
		msg := entry.Message
		if len(msg) > msgMaxLen && msgMaxLen > 3 {
			msg = msg[:msgMaxLen-3] + "..."
		}

		lines = append(lines, prefix+msg)
	}

	return strings.Join(lines, "\n")
}

func prettyDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
}
