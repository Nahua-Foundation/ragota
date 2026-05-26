package tui

// Файл содержит функции для рендеринга ASCII-графиков в терминале:
// sparkline-графики, bar charts, прогресс-бары и ASCII-art заголовок RAGOTA.

import (
	"fmt"
	"strings"
	"time"

	"ragota/internal/state"

	"github.com/charmbracelet/lipgloss"
)

// ─── ASCII-art заголовок RAGOTA ──────────────────────────────────────────────

var ragotaBanner = `
██████╗  █████╗  ██████╗  ██████╗ ████████╗ █████╗ 
██╔══██╗██╔══██╗██╔════╝ ██╔═══██╗╚══██╔══╝██╔══██╗
██████╔╝███████║██║  ███╗██║   ██║   ██║   ███████║
██╔══██╗██╔══██║██║   ██║██║   ██║   ██║   ██╔══██║
██║  ██║██║  ██║╚██████╔╝╚██████╔╝   ██║   ██║  ██║
╚═╝  ╚═╝╚═╝  ╚═╝ ╚═════╝  ╚═════╝    ╚═╝   ╚═╝  ╚═╝
`

func renderBanner(width int) string {
	colors := []lipgloss.Style{
		lipgloss.NewStyle().Foreground(lipgloss.Color("#7D56F4")), // purple
		lipgloss.NewStyle().Foreground(lipgloss.Color("#A78BFA")), // light purple
		lipgloss.NewStyle().Foreground(lipgloss.Color("#C4B5FD")), // lighter purple
	}
	rows := strings.Split(strings.TrimSpace(ragotaBanner), "\n")
	var lines []string
	for i, row := range rows {
		c := colors[i%len(colors)]
		lines = append(lines, c.Render(row))
	}
	return strings.Join(lines, "\n")
}

// ─── Sparkline ( Unicode braille-like characters) ────────────────────────────

// sparkChars — символы от низкого к высокому для sparkline.
var sparkChars = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// renderSparkline рисует однострочный sparkline из значений.
// width — максимальная ширина в символах.
func renderSparkline(values []float64, width int, color lipgloss.Style) string {
	if len(values) == 0 {
		return dimStyle.Render(strings.Repeat(" ", width))
	}
	if width <= 0 {
		return ""
	}

	// Берём последние width значений (или меньше).
	n := len(values)
	if n > width {
		values = values[n-width:]
		n = width
	}

	// Находим min/max для нормализации.
	minVal, maxVal := values[0], values[0]
	for _, v := range values {
		if v < minVal {
			minVal = v
		}
		if v > maxVal {
			maxVal = v
		}
	}

	var sb strings.Builder
	rangeVal := maxVal - minVal
	for _, v := range values {
		if rangeVal == 0 {
			sb.WriteRune(sparkChars[0])
		} else {
			idx := int((v - minVal) / rangeVal * float64(len(sparkChars)-1))
			if idx < 0 {
				idx = 0
			}
			if idx >= len(sparkChars) {
				idx = len(sparkChars) - 1
			}
			sb.WriteRune(sparkChars[idx])
		}
	}
	return color.Render(sb.String())
}

// ─── Bar chart (столбчатая диаграмма) ─────────────────────────────────────────

// barChars — символы для вертикальных баров разной высоты (8 уровней).
var barChars = []string{" ", "▏", "▎", "▍", "▌", "▋", "▊", "▉", "█"}

// renderBarChart рисует столбчатую диаграмму высотой maxHeight строк.
// values — значения для каждого столбца, labels — подписи под ними.
// Возвращает многострочную строку.
func renderBarChart(values []float64, labels []string, maxWidth, maxHeight int,
	color lipgloss.Style, errColor lipgloss.Style, errValues []float64) string {

	if len(values) == 0 {
		return dimStyle.Render("  no data")
	}

	n := len(values)
	if n > maxWidth {
		// Сжимаем: берём последние maxWidth.
		start := n - maxWidth
		values = values[start:]
		if len(labels) > start {
			labels = labels[start:]
		} else {
			labels = nil
		}
		if len(errValues) > start {
			errValues = errValues[start:]
		} else {
			errValues = nil
		}
		n = len(values)
	}

	// Находим max для масштабирования.
	maxVal := 1.0
	for _, v := range values {
		if v > maxVal {
			maxVal = v
		}
	}

	// Рисуем по строкам сверху вниз.
	var rows []string
	for row := maxHeight - 1; row >= 0; row-- {
		threshold := float64(row+1) / float64(maxHeight) * maxVal
		var line strings.Builder
		for i := 0; i < n; i++ {
			if values[i] >= threshold {
				// Проверяем, есть ли ошибка в этом столбце.
				if len(errValues) > i && errValues[i] > 0 {
					line.WriteString(errColor.Render("█"))
				} else {
					line.WriteString(color.Render("█"))
				}
			} else {
				line.WriteString(" ")
			}
		}
		rows = append(rows, line.String())
	}

	// Подписи (если помещаются).
	if len(labels) > 0 && len(labels) <= n {
		var labelLine strings.Builder
		for i := 0; i < n; i++ {
			if i < len(labels) && len(labels[i]) > 0 {
				labelLine.WriteString(string([]rune(labels[i])[0]))
			} else {
				labelLine.WriteString(" ")
			}
		}
		rows = append(rows, dimStyle.Render(labelLine.String()))
	}

	return strings.Join(rows, "\n")
}

// ─── Прогресс-бар ─────────────────────────────────────────────────────────────

func renderProgressBar(current, total int, width int, color lipgloss.Style) string {
	if total == 0 {
		return dimStyle.Render("░░░░░░░░░░")
	}
	pct := float64(current) / float64(total)
	if pct > 1 {
		pct = 1
	}
	filled := int(pct * float64(width))
	if filled > width {
		filled = width
	}
	empty := width - filled
	bar := strings.Repeat("█", filled) + strings.Repeat("░", empty)
	pctStr := fmt.Sprintf("%3.0f%%", pct*100)
	return color.Render(bar) + " " + pctStr
}

// ─── Компактный индикатор статуса ────────────────────────────────────────────

func statusIndicator(status string) string {
	switch status {
	case "idle":
		return okStyle.Render("●")
	case "scanning", "indexing":
		return warnStyle.Render("◐")
	case "error":
		return errStyle.Render("✗")
	default:
		return dimStyle.Render("○")
	}
}

// ─── Мини-таблица (для MCP stats) ────────────────────────────────────────────

// renderMCPMiniTable рисует компактную таблицу MCP-серверов с sparkline.
func renderMCPMiniTable(mcp map[string]state.MCPStat, callHistory, errHistory []int, maxWidth int) string {
	if len(mcp) == 0 {
		return dimStyle.Render("  no MCP servers connected")
	}

	var lines []string
	lines = append(lines, headerStyle.Render("MCP servers"))

	// Sparkline для общей активности MCP.
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

	// Per-server stats.
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

// ─── Indexer dashboard row ───────────────────────────────────────────────────

func renderIndexerDashboard(name string, idx state.Indexer, metrics *state.IndexerMetrics, width int) string {
	statusIcon := statusIndicator(idx.Status)

	// Прогресс-бар.
	progressBar := renderProgressBar(idx.FilesIndexed, idx.FilesTotal, 20, okStyle)

	// Статистика.
	stats := ""
	if name == "graph" {
		stats = fmt.Sprintf("edges=%d units=%d", idx.Chunks, idx.Symbols)
	} else {
		stats = fmt.Sprintf("chunks=%d symbols=%d", idx.Chunks, idx.Symbols)
	}

	line := fmt.Sprintf("  %s %-11s %s  %s", statusIcon, name, progressBar, stats)

	if idx.LastError != "" {
		line += "\n" + errStyle.Render("    Error: "+idx.LastError)
	}
	return line
}

// ─── Recent activity sparkline ───────────────────────────────────────────────

// computeRecentActivity считает активность индексации файлов по секундам
// из Recent записей. Возвращает срез значений files/second за последние 60s.
func computeRecentActivity(recent []state.FileEntry, windowSec int) []float64 {
	if len(recent) == 0 {
		return nil
	}

	buckets := make(map[int]int) // second_bucket -> count
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

// ─── Ollama latency section ──────────────────────────────────────────────────

func renderOllamaLatencySection(latency map[string]*state.OllamaLatency, width int) string {
	if len(latency) == 0 {
		return headerStyle.Render("Ollama models") + "\n" + dimStyle.Render("  no calls yet")
	}

	var lines []string
	lines = append(lines, headerStyle.Render("Ollama models"))

	// Sort models by name for stable order.
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
		// Sparkline latency.
		sparkline := renderSparkline(m.LatencyMs.Values, width-18,
			lipgloss.NewStyle().Foreground(lipgloss.Color("214")))

		// Average latency.
		var avgMs float64
		if len(m.LatencyMs.Values) > 0 {
			var sum float64
			for _, v := range m.LatencyMs.Values {
				sum += v
			}
			avgMs = sum / float64(len(m.LatencyMs.Values))
		}

		// Last latency value.
		var lastMs float64
		if len(m.LatencyMs.Values) > 0 {
			lastMs = m.LatencyMs.Values[len(m.LatencyMs.Values)-1]
		}

		line := fmt.Sprintf("  %-20s %s", model, sparkline)
		lines = append(lines, line)
		stats := fmt.Sprintf("    avg=%dms  last=%dms  calls=%d", int(avgMs), int(lastMs), m.TotalCalls)
		if m.Errors > 0 {
			stats += errStyle.Render(fmt.Sprintf("  errors=%d", m.Errors))
		}
		lines = append(lines, dimStyle.Render(stats))
	}

	return strings.Join(lines, "\n")
}

func toFloat64[ints ~[]int](vals ints) []float64 {
	out := make([]float64, len(vals))
	for i, v := range vals {
		out[i] = float64(v)
	}
	return out
}

// clampInt ограничивает значение.
func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// prettyDuration форматирует duration в человекочитаемый вид.
func prettyDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
}
