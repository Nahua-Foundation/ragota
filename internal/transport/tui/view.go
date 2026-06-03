package tui

// Файл реализует новый дашборд RAGOTA с ASCII-art заголовком,
// sparkline-графиками, прогресс-барами и компактным отображением ошибок.

import (
	"fmt"
	"os"
	"strings"
	"time"

	"ragota/pkg/state"

	"github.com/charmbracelet/lipgloss"
)

var (
	dimStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	okStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	warnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	errStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	box       = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(1, 2)
)

func (m model) View() string {
	if m.width == 0 {
		return "  Initializing..."
	}
	s := m.snap
	contentWidth := m.width - 8 // padding box
	if contentWidth < 40 {
		contentWidth = 40
	}

	var sections []string

	// 1. Banner RAGOTA
	sections = append(sections, renderBanner(contentWidth))

	// 2. Subtitle: root path + uptime
	uptime := time.Since(s.StartedAt).Truncate(time.Second)
	subtitle := dimStyle.Render(fmt.Sprintf("root: %s  |  uptime: %s  |  press q to quit", s.Root, uptime))
	sections = append(sections, subtitle)
	sections = append(sections, "")

	// 3. Docker status
	sections = append(sections, renderDockerSection(s.Docker, contentWidth))
	sections = append(sections, "")

	// 4. Two-column layout: Indexers (left) + Ollama latency (right)
	leftWidth := contentWidth / 2
	rightWidth := contentWidth - leftWidth
	leftCol := renderIndexersSection(s, leftWidth)
	rightCol := renderOllamaLatencySection(s.OllamaLatency, rightWidth)
	sections = append(sections, lipgloss.JoinHorizontal(lipgloss.Top, leftCol, "    ", rightCol))
	sections = append(sections, "")

	// 5. MCP servers с графиками
	sections = append(sections, renderMCPMiniTable(s.MCP, s.MCPCallHistory, s.MCPErrHistory, contentWidth))
	sections = append(sections, "")

	// 6. Recent activity sparkline
	sections = append(sections, renderRecentActivitySection(s.Recent, contentWidth))

	// 7. LSP errors (компактно)
	if len(s.LSP) > 0 {
		sections = append(sections, "")
		sections = append(sections, renderLSPErrorsSection(s.LSP, contentWidth))
	}

	// 8. Errors (компактно)
	var errs []state.FileEntry
	for _, e := range s.Recent {
		if e.Error != "" {
			errs = append(errs, e)
		}
	}
	if len(errs) > 0 {
		sections = append(sections, "")
		sections = append(sections, renderErrorsSection(errs, contentWidth))
	}

	body := strings.Join(sections, "\n")
	body = strings.TrimRight(body, "\n")
	return box.Render(body)
}

// ─── Docker section ───────────────────────────────────────────────────────────

func renderDockerSection(d state.DockerStatus, width int) string {
	var lines []string
	lines = append(lines, headerStyle.Render("Docker"))

	if d.LastError != "" {
		lines = append(lines, errStyle.Render("  ✗ "+d.LastError))
		return strings.Join(lines, "\n")
	}
	if !d.Running && len(d.Services) == 0 {
		lines = append(lines, dimStyle.Render("  not started"))
		return strings.Join(lines, "\n")
	}

	var svcLines []string
	for _, svc := range d.Services {
		svcLines = append(svcLines, okStyle.Render("●")+" "+svc)
	}
	lines = append(lines, "  "+strings.Join(svcLines, "  "))
	return strings.Join(lines, "\n")
}

// ─── Indexers section ────────────────────────────────────────────────────────

func renderIndexersSection(s state.Snapshot, width int) string {
	var lines []string
	lines = append(lines, headerStyle.Render("Indexers"))

	for _, name := range []string{"ast", "vector", "crossrepo"} {
		if idx, ok := s.Indexers[name]; ok {
			metrics := s.IndexerMetrics[name]
			lines = append(lines, renderIndexerDashboard(name, idx, metrics, width))
		} else {
			lines = append(lines, fmt.Sprintf("  ○ %-11s %s", name, dimStyle.Render("not started")))
		}
	}

	return strings.Join(lines, "\n")
}

// ─── LSP errors section ──────────────────────────────────────────────────────

func renderLSPErrorsSection(lspErrors []state.LSPError, width int) string {
	var lines []string
	lines = append(lines, headerStyle.Render(fmt.Sprintf("LSP errors (%d)", len(lspErrors))))

	cwd, _ := os.Getwd()
	maxShow := 5
	for i, e := range lspErrors {
		if i >= maxShow {
			lines = append(lines, dimStyle.Render(fmt.Sprintf("  … +%d more (see log file)", len(lspErrors)-i)))
			break
		}
		when := time.Since(e.Timestamp).Truncate(time.Second)
		shownPath := truncateStart(relToCwd(e.Path, cwd), width-40)
		line := fmt.Sprintf("  %-12s %s  %s  %s",
			e.Method, shownPath, errStyle.Render(e.Error),
			dimStyle.Render(fmt.Sprintf("%s ago", when)))
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// ─── Errors section ──────────────────────────────────────────────────────────

func renderErrorsSection(errs []state.FileEntry, width int) string {
	var lines []string
	lines = append(lines, headerStyle.Render(fmt.Sprintf("Errors (%d)", len(errs))))

	cwd, _ := os.Getwd()
	maxShow := 5
	for i, e := range errs {
		if i >= maxShow {
			lines = append(lines, dimStyle.Render(fmt.Sprintf("  … +%d more (see log file)", len(errs)-i)))
			break
		}
		when := time.Since(e.IndexedAt).Truncate(time.Second)
		short := e.Error
		if idx := strings.IndexByte(short, '\n'); idx >= 0 {
			short = short[:idx]
		}
		shownPath := truncateStart(relToCwd(e.Path, cwd), width-50)
		lines = append(lines, fmt.Sprintf("  %-9s %s  %s  %s",
			e.Kind, shownPath, errStyle.Render(short),
			dimStyle.Render(fmt.Sprintf("%s ago", when))))
	}
	return strings.Join(lines, "\n")
}

// ─── Recent activity section ─────────────────────────────────────────────────

func renderRecentActivitySection(recent []state.FileEntry, width int) string {
	var lines []string

	// Считаем суммарную статистику.
	totalFiles := len(recent)
	var totalChunks, totalSymbols int
	var avgDuration int64
	for _, e := range recent {
		totalChunks += e.Chunks
		totalSymbols += e.Symbols
		avgDuration += e.DurationMs
	}
	if totalFiles > 0 {
		avgDuration /= int64(totalFiles)
	}

	lines = append(lines, headerStyle.Render("Recent activity"))

	// Summary stats.
	summary := fmt.Sprintf("  %d files  |  %d chunks  |  %d symbols  |  avg %dms",
		totalFiles, totalChunks, totalSymbols, avgDuration)
	lines = append(lines, dimStyle.Render(summary))

	// Последние 3 файла.
	if len(recent) > 0 {
		lines = append(lines, "")
		lines = append(lines, dimStyle.Render("  latest:"))
		show := 3
		if show > len(recent) {
			show = len(recent)
		}
		cwd, _ := os.Getwd()
		for i := 0; i < show; i++ {
			e := recent[i]
			shownPath := truncateStart(relToCwd(e.Path, cwd), width-25)
			lines = append(lines, fmt.Sprintf("    %s %-9s %s  %dms",
				okStyle.Render("●"), e.Kind, shownPath, e.DurationMs))
		}
	}

	return strings.Join(lines, "\n")
}

// ─── Header style (определён здесь чтобы не дублировать) ──────────────────────

var headerStyle = lipgloss.NewStyle().Bold(true).Underline(true)
