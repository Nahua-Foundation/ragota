// Package tui — терминальный дашборд (bubbletea + lipgloss).
// Отображает: статус docker-compose, статус индексаторов, последние файлы,
// статистику вызовов MCP-серверов. Обновляется по тику раз в секунду.
package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"aitools/internal/state"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Run запускает дашборд. Возвращает при выходе пользователем (q/Ctrl+C) или при ctx.Done.
func Run(ctx context.Context, bus *state.Bus) error {
	m := model{bus: bus}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	return err
}

type tickMsg time.Time

type model struct {
	bus      *state.Bus
	snap     state.Snapshot
	width    int
	height   int
	lastTick time.Time
}

func (m model) Init() tea.Cmd {
	return tea.Batch(refreshSnap(m.bus), tick())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		}
	case tickMsg:
		m.lastTick = time.Time(msg)
		return m, tea.Batch(refreshSnap(m.bus), tick())
	case snapMsg:
		m.snap = state.Snapshot(msg)
		return m, nil
	}
	return m, nil
}

type snapMsg state.Snapshot

func refreshSnap(bus *state.Bus) tea.Cmd {
	return func() tea.Msg { return snapMsg(bus.Snapshot()) }
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4")).Padding(0, 1)
	headerStyle = lipgloss.NewStyle().Bold(true).Underline(true)
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	box         = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
)

func (m model) View() string {
	if m.width == 0 {
		return "  Initializing..."
	}
	s := m.snap
	uptime := time.Since(s.StartedAt).Truncate(time.Second)
	header := titleStyle.Render("ai-tools watch") + "  " +
		dimStyle.Render(fmt.Sprintf("root=%s  uptime=%s  press q to quit", s.Root, uptime))

	// Docker
	contentWidth := m.width - 4
	if contentWidth < 20 {
		contentWidth = 20
	}
	dockerLine := "docker: " + dockerLineFmt(s.Docker, contentWidth)

	// Indexers
	var idxRows []string
	idxRows = append(idxRows, headerStyle.Render("Indexers"))
	for _, name := range []string{"treesitter", "vector"} {
		if i, ok := s.Indexers[name]; ok {
			idxRows = append(idxRows, renderIndexer(i, contentWidth))
		} else {
			idxRows = append(idxRows, fmt.Sprintf("  %-11s %s", name, dimStyle.Render("not started")))
		}
	}

	// MCP stats
	var mcpRows []string
	mcpRows = append(mcpRows, headerStyle.Render("MCP servers (this session)"))
	names := make([]string, 0, len(s.MCP))
	for k := range s.MCP {
		names = append(names, k)
	}
	sort.Strings(names)
	if len(names) == 0 {
		mcpRows = append(mcpRows, dimStyle.Render("  no calls yet"))
	}
	for _, name := range names {
		st := s.MCP[name]
		var total int
		var calls []string
		// детерминированный порядок tools
		toolNames := make([]string, 0, len(st.Calls))
		for t := range st.Calls {
			toolNames = append(toolNames, t)
		}
		sort.Strings(toolNames)
		for _, t := range toolNames {
			total += st.Calls[t]
			calls = append(calls, fmt.Sprintf("%s=%d", t, st.Calls[t]))
		}
		status := okStyle.Render("●")
		if !st.Running {
			status = dimStyle.Render("○")
		}
		mcpRows = append(mcpRows,
			fmt.Sprintf("  %s %-12s total=%d errors=%d  %s",
				status, name, total, st.Errors, dimStyle.Render(strings.Join(calls, " "))))
	}

	// Recent
	var recentRows []string
	recentRows = append(recentRows, headerStyle.Render("Recent files (newest first)"))
	if len(s.Recent) == 0 {
		recentRows = append(recentRows, dimStyle.Render("  —"))
	}
	maxRecent := 12
	for i, e := range s.Recent {
		if i >= maxRecent {
			break
		}
		when := time.Since(e.IndexedAt).Truncate(time.Second)
		extra := ""
		if e.Chunks > 0 {
			extra = fmt.Sprintf(" chunks=%d", e.Chunks)
		}
		if e.Symbols > 0 {
			extra += fmt.Sprintf(" symbols=%d", e.Symbols)
		}
		line := fmt.Sprintf("  %-9s %s%s  %s",
			e.Kind, truncateStart(e.Path, 60), extra,
			dimStyle.Render(fmt.Sprintf("%dms, %s ago", e.DurationMs, when)))
		if e.Error != "" {
			line += renderError(e.Error, contentWidth-4)
		}
		recentRows = append(recentRows, line)
	}

	body := strings.Join([]string{
		dockerLine,
		"",
		strings.Join(idxRows, "\n"),
		"",
		strings.Join(mcpRows, "\n"),
		"",
		strings.Join(recentRows, "\n"),
	}, "\n")

	return header + "\n" + box.Render(body)
}

func dockerLineFmt(d state.DockerStatus, width int) string {
	if !d.Running && len(d.Services) == 0 {
		return dimStyle.Render("not started")
	}
	parts := []string{}
	for _, svc := range d.Services {
		parts = append(parts, okStyle.Render("●")+" "+svc)
	}
	line := strings.Join(parts, "  ")
	if d.LastError != "" {
		line += renderError(d.LastError, width-4)
	}
	return line
}

func renderIndexer(i state.Indexer, width int) string {
	statusStyle := dimStyle
	switch i.Status {
	case "indexing", "scanning":
		statusStyle = warnStyle
	case "idle":
		statusStyle = okStyle
	case "error":
		statusStyle = errStyle
	}
	line := fmt.Sprintf("  %-11s %s  files=%d/%d chunks=%d symbols=%d",
		i.Name,
		statusStyle.Render(fmt.Sprintf("%-9s", i.Status)),
		i.FilesIndexed, i.FilesTotal, i.Chunks, i.Symbols)
	if i.LastError != "" {
		line += renderError(i.LastError, width-4)
	}
	return line
}

func renderError(err string, width int) string {
	if err == "" {
		return ""
	}
	return "\n" + errStyle.Copy().Width(width).Render("    Error: "+err)
}

func truncateStart(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 2 {
		return string(r[len(r)-n:])
	}
	return "…" + string(r[len(r)-(n-1):])
}
