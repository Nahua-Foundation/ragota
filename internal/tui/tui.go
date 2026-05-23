// Package tui — терминальный дашборд (bubbletea + lipgloss).
// Отображает: статус docker-compose, статус индексаторов, последние файлы,
// статистику вызовов MCP-серверов. Обновляется по тику раз в секунду.
package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"aitools/internal/state"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Run запускает дашборд. Возвращает при выходе пользователем (q/Ctrl+C) или при ctx.Done.
func Run(ctx context.Context, bus *state.Bus) error {
	lf, lfPath := openLogFile()
	if lf != nil {
		defer lf.Close()
		_, _ = fmt.Fprintf(os.Stderr, "ai-tools TUI: logs are mirrored to %s\n", lfPath)
	}
	m := model{bus: bus, logFile: lf, seen: make(map[string]struct{})}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	return err
}

// openLogFile открывает файл логов TUI в ~/.cache/ai-tools/tui.log
// (с фоллбэком в текущую директорию). Если открыть не удалось — возвращает nil.
func openLogFile() (*os.File, string) {
	dir := ""
	if h, err := os.UserCacheDir(); err == nil {
		dir = filepath.Join(h, "ai-tools")
	} else if h, err := os.UserHomeDir(); err == nil {
		dir = filepath.Join(h, ".cache", "ai-tools")
	}
	if dir == "" {
		dir = ".ai-tools"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, ""
	}
	path := filepath.Join(dir, "tui.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, ""
	}
	_, _ = fmt.Fprintf(f, "\n=== ai-tools TUI started at %s ===\n", time.Now().Format(time.RFC3339))
	return f, path
}

type tickMsg time.Time

type model struct {
	bus      *state.Bus
	snap     state.Snapshot
	width    int
	height   int
	lastTick time.Time
	logFile  *os.File
	seen     map[string]struct{} // ключи уже залогированных Recent-записей
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
		m.appendLogs()
		return m, nil
	}
	return m, nil
}

// appendLogs дописывает в файл логов TUI новые записи Recent, которые
// ещё не были залогированы в текущей сессии. Это позволяет видеть всю
// историю даже если в окне TUI помещается лишь несколько последних строк.
func (m *model) appendLogs() {
	if m.logFile == nil || m.seen == nil {
		return
	}
	// Recent хранится newest-first; пишем в файл oldest-first, чтобы файл
	// читался естественно сверху вниз.
	for i := len(m.snap.Recent) - 1; i >= 0; i-- {
		e := m.snap.Recent[i]
		key := fmt.Sprintf("%d|%s|%s", e.IndexedAt.UnixNano(), e.Kind, e.Path)
		if _, ok := m.seen[key]; ok {
			continue
		}
		m.seen[key] = struct{}{}
		extra := ""
		if e.Chunks > 0 {
			extra += fmt.Sprintf(" chunks=%d", e.Chunks)
		}
		if e.Symbols > 0 {
			extra += fmt.Sprintf(" symbols=%d", e.Symbols)
		}
		if e.Error != "" {
			extra += " error=" + strings.ReplaceAll(e.Error, "\n", " ")
		}
		_, _ = fmt.Fprintf(m.logFile, "%s [%s] %s%s (%dms)\n",
			e.IndexedAt.Format("15:04:05"), e.Kind, e.Path, extra, e.DurationMs)
	}
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
	for _, name := range []string{"treesitter", "graph", "vector"} {
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

	// Разделяем Recent на "обычные" и "ошибки" — секция ошибок имеет приоритет
	// и вытесняет Recent снизу, но Recent гарантированно показывает минимум 3 строки.
	var normal, errs []state.FileEntry
	for _, e := range s.Recent {
		if e.Error != "" {
			errs = append(errs, e)
		} else {
			normal = append(normal, e)
		}
	}

	// LSP errors — тоже считаем ошибками для приоритета отображения.
	hasLSPErrors := len(s.LSP) > 0

	// Фиксированная часть body (всё, кроме Recent и Errors).
	fixedSections := []string{
		dockerLine,
		"",
		strings.Join(idxRows, "\n"),
		"",
		strings.Join(mcpRows, "\n"),
		"",
	}
	fixedText := strings.Join(fixedSections, "\n")
	// header (1) + рамка box (2) + fixedText + заголовки (Recent, LSP?, Errors) + safety (1).
	headersOverhead := 2
	if hasLSPErrors {
		headersOverhead = 3
	}
	if len(errs) == 0 && !hasLSPErrors {
		headersOverhead = 1
	}
	usedLines := 1 + 2 + strings.Count(fixedText, "\n") + headersOverhead + 1
	avail := m.height - usedLines
	if avail < 4 {
		avail = 4
	}

	const minRecent = 3
	// Сначала отдадим всё доступное ошибкам (indexer + LSP), но Recent оставим минимум minRecent строк.
	maxErrors := len(errs)
	if hasLSPErrors {
		maxErrors += len(s.LSP) // считаем LSP ошибки вместе с indexer errors
	}
	if maxErrors > avail-minRecent {
		maxErrors = avail - minRecent
	}
	if maxErrors < 0 {
		maxErrors = 0
	}
	maxRecent := avail - maxErrors
	if maxRecent > len(normal) {
		// Если Recent меньше выделенного — отдаём остаток ошибкам.
		extra := maxRecent - len(normal)
		maxRecent = len(normal)
		if maxErrors+extra > len(errs)+len(s.LSP) {
			maxErrors = len(errs) + len(s.LSP)
		} else {
			maxErrors += extra
		}
	}
	if maxRecent > 30 {
		maxRecent = 30
	}

	// cwd для обрезки путей.
	cwd, _ := os.Getwd()
	pathWidth := contentWidth - 40
	if pathWidth < 20 {
		pathWidth = 20
	}

	// LSP errors (под Recent, перед Errors).
	var lspRows []string
	if hasLSPErrors {
		lspRows = append(lspRows, headerStyle.Render(fmt.Sprintf("LSP errors (%d)", len(s.LSP))))
		lspMax := maxErrors / 2 // делим место между LSP и indexer errors
		if lspMax < 3 {
			lspMax = 3
		}
		for i, e := range s.LSP {
			if i >= lspMax {
				lspRows = append(lspRows,
					dimStyle.Render(fmt.Sprintf("  … +%d more (see log file)", len(s.LSP)-i)))
				break
			}
			when := time.Since(e.Timestamp).Truncate(time.Second)
			shownPath := truncateStart(relToCwd(e.Path, cwd), pathWidth)
			line := fmt.Sprintf("  %-12s %s  %s  %s",
				e.Method, shownPath, errStyle.Render(e.Error),
				dimStyle.Render(fmt.Sprintf("%s ago", when)))
			lspRows = append(lspRows, line)
		}
	}

	// Recent (без ошибок).
	var recentRows []string
	recentHeader := headerStyle.Render("Recent files (newest first)")
	if m.logFile != nil {
		recentHeader += dimStyle.Render(fmt.Sprintf("  (full log: %s)", logFilePathHint()))
	}
	recentRows = append(recentRows, recentHeader)
	if len(normal) == 0 {
		recentRows = append(recentRows, dimStyle.Render("  —"))
	}
	for i, e := range normal {
		if i >= maxRecent {
			recentRows = append(recentRows,
				dimStyle.Render(fmt.Sprintf("  … +%d more (see log file)", len(normal)-i)))
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
		shownPath := relToCwd(e.Path, cwd)
		shownPath = truncateStart(shownPath, pathWidth)
		line := fmt.Sprintf("  %-9s %s%s  %s",
			e.Kind, shownPath, extra,
			dimStyle.Render(fmt.Sprintf("%dms, %s ago", e.DurationMs, when)))
		recentRows = append(recentRows, line)
	}

	// Errors (под LSP).
	var errorRows []string
	if len(errs) > 0 {
		errorRows = append(errorRows, headerStyle.Render(fmt.Sprintf("Errors (%d)", len(errs))))
		errMax := maxErrors - len(lspRows) + 1 // оставшееся место после LSP
		if errMax < 2 {
			errMax = 2
		}
		for i, e := range errs {
			if i >= errMax {
				errorRows = append(errorRows,
					dimStyle.Render(fmt.Sprintf("  … +%d more (see log file)", len(errs)-i)))
				break
			}
			when := time.Since(e.IndexedAt).Truncate(time.Second)
			short := e.Error
			if idx := strings.IndexByte(short, '\n'); idx >= 0 {
				short = short[:idx]
			}
			shownPath := truncateStart(relToCwd(e.Path, cwd), pathWidth)
			// Доступная ширина под текст ошибки.
			errWidth := contentWidth - 14 - len([]rune(shownPath))
			if errWidth < 20 {
				errWidth = 20
			}
			if len([]rune(short)) > errWidth {
				r := []rune(short)
				short = string(r[:errWidth-1]) + "…"
			}
			errorRows = append(errorRows, fmt.Sprintf("  %-9s %s  %s  %s",
				e.Kind, shownPath, errStyle.Render(short),
				dimStyle.Render(fmt.Sprintf("%s ago", when))))
		}
	}

	parts := []string{fixedText + strings.Join(recentRows, "\n")}
	if len(lspRows) > 0 {
		parts = append(parts, strings.Join(lspRows, "\n"))
	}
	if len(errorRows) > 0 {
		parts = append(parts, strings.Join(errorRows, "\n"))
	}
	body := strings.Join(parts, "\n")

	return header + "\n" + box.Render(body)
}

// relToCwd возвращает путь относительно cwd. Если путь не лежит под cwd —
// возвращает исходный путь без изменений. cwd может быть пустым.
func relToCwd(path, cwd string) string {
	if cwd == "" || path == "" {
		return path
	}
	if rel, err := filepath.Rel(cwd, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

// logFilePathHint возвращает короткий хинт пути файла логов для отображения
// в TUI. Намеренно без полного пути — экономим место в шапке секции.
func logFilePathHint() string {
	if h, err := os.UserCacheDir(); err == nil {
		return filepath.Join(h, "ai-tools", "tui.log")
	}
	return "~/.cache/ai-tools/tui.log"
}

func dockerLineFmt(d state.DockerStatus, width int) string {
	if d.LastError != "" {
		return errStyle.Render("error: ") + d.LastError
	}
	if !d.Running && len(d.Services) == 0 {
		return dimStyle.Render("not started")
	}
	parts := []string{}
	for _, svc := range d.Services {
		parts = append(parts, okStyle.Render("●")+" "+svc)
	}
	line := strings.Join(parts, "  ")
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

	stats := fmt.Sprintf("files=%d/%d chunks=%d symbols=%d", i.FilesIndexed, i.FilesTotal, i.Chunks, i.Symbols)
	if i.Name == "graph" {
		stats = fmt.Sprintf("files=%d/%d edges=%d units=%d", i.FilesIndexed, i.FilesTotal, i.Chunks, i.Symbols)
	}

	line := fmt.Sprintf("  %-11s %s  %s",
		i.Name,
		statusStyle.Render(fmt.Sprintf("%-9s", i.Status)),
		stats)
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
