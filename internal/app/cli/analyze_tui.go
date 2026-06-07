package cli

// analyze_tui.go — TUI для команды ragota analyze (v2).
// Async pipeline с честным прогрессом:
//   Phase 1: Scanning directory... (files found: N)
//   Phase 2: Applying heuristics... (auto-classified: X, remaining: Y)
//   Phase 3: LLM analysis... (pass 1/3, 2/3, 3/3)
//   Phase 4: Selecting patterns
//
// UI features:
//   - Группировка по source (heuristic / llm / negation)
//   - Цветовая дифференциация confidence (green/yellow/red)
//   - Виртуализация списка (scroll window)
//   - Фильтрация (all / llm-only / unselected)
//   - Preview panel с примерами файлов (p key)
//   - Bulk-операции по группе (A/N — group, i — invert)
//   - Подтверждение перед save
//   - Undo последнего действия (u key)
//   - Индикатор «уже в .ragotaignore»

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ragota/internal/analyze"
	"ragota/internal/analyze/heuristic"
	"ragota/internal/analyze/llm"
	"ragota/internal/analyze/output"
	"ragota/internal/analyze/resolve"
	"ragota/pkg/config"
	"ragota/pkg/gitignore"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ── Стили ──────────────────────────────────────────────────────────────────

var (
	analyzeTitleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4"))
	analyzeProgressStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#04B575"))
	analyzeItemStyle     = lipgloss.NewStyle().PaddingLeft(2)
	analyzeCursorStyle   = lipgloss.NewStyle().PaddingLeft(1).Bold(true)
	analyzeFooterStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#888888")).PaddingTop(1)
	analyzeErrorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF0000")).Bold(true)
	analyzePhaseStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#7D56F4")).Bold(true)
	analyzeDimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#666666"))
	analyzeTimerStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#888888"))
	analyzeLogStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#555555")).PaddingLeft(2)
	analyzeGroupStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4"))
	analyzeSummaryStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#04B575")).Bold(true)
	analyzeExistsStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#666666")).PaddingLeft(2)
	analyzePreviewStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#888888")).PaddingLeft(6)
	analyzeFilterStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFD700")).Bold(true)

	// Confidence colors
	analyzeConfHigh = lipgloss.NewStyle().Foreground(lipgloss.Color("#04B575")) // green ≥80
	analyzeConfMid  = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFD700")) // yellow 50-79
	analyzeConfLow  = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF6B6B")) // red <50

	// Checkbox styles (крупные, цветные)
	analyzeCheckOnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#04B575")).Bold(true)
	analyzeCheckOffStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#AAAAAA"))
)

const (
	checkOn  = "[✔]"
	checkOff = "[ ]"
)

// ── Типы ───────────────────────────────────────────────────────────────────

type analyzePhase int

const (
	phaseScanning analyzePhase = iota
	phasePrescreen
	phaseLLM
	phaseSelecting
	phaseConfirm
	phaseDone
	phaseError
)

type filterMode int

const (
	filterAll filterMode = iota
	filterLLM
	filterUnselected
)

func (f filterMode) String() string {
	switch f {
	case filterAll:
		return "all"
	case filterLLM:
		return "llm only"
	case filterUnselected:
		return "unselected"
	}
	return "?"
}

type itemGroup struct {
	stage      string
	title      string
	start, end int // indices in filtered
	count      int // items in group
	selected   int // selected in group
}

// analyzeItem — элемент в списке для выбора.
type analyzeItem struct {
	Path       string
	Pattern    string
	Stage      string
	Reason     string
	Confidence int
	Selected   bool
	Preview    []string // примеры файлов, покрываемых паттерном
	Exists     bool     // уже есть в .ragotaignore
}

type analyzeModel struct {
	cfg   *config.Config
	ctx   context.Context
	noLLM bool
	model string
	phase analyzePhase
	err   string

	// Все элементы (source of truth)
	items []analyzeItem

	// Filtered view
	filtered []int // indices into items (только видимые)

	// Группы (для отрисовки заголовков)
	groups []itemGroup

	// Cursor / scroll
	cursor    int // index into filtered
	scrollOff int

	// Filter
	filter filterMode

	// Collapse state
	collapsed map[string]bool

	// Preview
	showPreview bool

	// Undo
	undoState map[int]bool // item index → old Selected value

	// Existing patterns
	existingPatterns map[string]bool

	// Viewport
	viewportH int
	termWidth int

	// Pipeline progress
	filesScanned   int
	autoIgnored    int
	autoKept       int
	remaining      int
	llmPass        int
	llmTotalPasses int
	llmBatch       int
	llmTotalBatches int
	startTime      time.Time
	logs           []string

	ch chan tea.Msg
}

func newAnalyzeModel(ctx context.Context, cfg *config.Config, noLLM bool, model string) analyzeModel {
	return analyzeModel{
		cfg:              cfg,
		ctx:              ctx,
		noLLM:            noLLM,
		model:            model,
		phase:            phaseScanning,
		llmTotalPasses:   3,
		startTime:        time.Now(),
		ch:               make(chan tea.Msg, 100),
		collapsed:        make(map[string]bool),
		existingPatterns: loadExistingPatterns(cfg.Root),
		viewportH:        15,
	}
}

func (m analyzeModel) Init() tea.Cmd {
	return tea.Batch(m.startAnalysis(), tickCmd())
}

// ── Helpers ────────────────────────────────────────────────────────────────

// loadExistingPatterns загружает паттерны из .ragotaignore для индикации «already exists».
func loadExistingPatterns(root string) map[string]bool {
	result := make(map[string]bool)
	data, err := os.ReadFile(filepath.Join(root, ".ragotaignore"))
	if err != nil {
		return result
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			result[line] = true
		}
	}
	return result
}

// stageOrder для сортировки: heuristic → llm → negation.
func stageOrder(stage string) int {
	switch stage {
	case "heuristic":
		return 0
	case "llm":
		return 1
	case "negation":
		return 2
	}
	return 3
}

func stageTitle(stage string) string {
	switch stage {
	case "heuristic":
		return "Heuristic (auto-detected)"
	case "llm":
		return "LLM Analysis"
	case "negation":
		return "Exceptions (negation)"
	}
	return stage
}

// ── Model methods ──────────────────────────────────────────────────────────

func (m *analyzeModel) markExisting() {
	for i := range m.items {
		m.items[i].Exists = m.existingPatterns[m.items[i].Pattern]
	}
}

func (m *analyzeModel) rebuildFiltered() {
	m.filtered = m.filtered[:0]
	for i, item := range m.items {
		switch m.filter {
		case filterLLM:
			if item.Stage == "heuristic" {
				continue
			}
		case filterUnselected:
			if item.Selected {
				continue
			}
		}
		m.filtered = append(m.filtered, i)
	}
	m.sortItems()
	m.computeGroups()
}

func (m *analyzeModel) sortItems() {
	sort.Slice(m.filtered, func(a, b int) bool {
		ia, ib := m.items[m.filtered[a]], m.items[m.filtered[b]]
		sa, sb := stageOrder(ia.Stage), stageOrder(ib.Stage)
		if sa != sb {
			return sa < sb
		}
		if ia.Confidence != ib.Confidence {
			return ia.Confidence > ib.Confidence
		}
		return ia.Pattern < ib.Pattern
	})
}

func (m *analyzeModel) computeGroups() {
	m.groups = m.groups[:0]
	if len(m.filtered) == 0 {
		return
	}
	start := 0
	stage := m.items[m.filtered[0]].Stage
	for i := 1; i < len(m.filtered); i++ {
		s := m.items[m.filtered[i]].Stage
		if s != stage {
			m.groups = appendGroup(m.groups, m.items, m.filtered, stage, start, i)
			start = i
			stage = s
		}
	}
	m.groups = appendGroup(m.groups, m.items, m.filtered, stage, start, len(m.filtered))
}

func appendGroup(groups []itemGroup, items []analyzeItem, filtered []int, stage string, start, end int) []itemGroup {
	count := end - start
	sel := 0
	for i := start; i < end; i++ {
		if items[filtered[i]].Selected {
			sel++
		}
	}
	return append(groups, itemGroup{
		stage:    stage,
		title:    stageTitle(stage),
		start:    start,
		end:      end,
		count:    count,
		selected: sel,
	})
}

func (m *analyzeModel) selectedCount() int {
	n := 0
	for _, item := range m.items {
		if item.Selected {
			n++
		}
	}
	return n
}

func (m *analyzeModel) filteredSelectedCount() int {
	n := 0
	for _, idx := range m.filtered {
		if m.items[idx].Selected {
			n++
		}
	}
	return n
}

func (m *analyzeModel) cursorItemIdx() int {
	if m.cursor < 0 || m.cursor >= len(m.filtered) {
		return -1
	}
	return m.filtered[m.cursor]
}

func (m *analyzeModel) currentGroup() *itemGroup {
	for i := range m.groups {
		if m.cursor >= m.groups[i].start && m.cursor < m.groups[i].end {
			return &m.groups[i]
		}
	}
	return nil
}

func (m *analyzeModel) skipCollapsed() {
	for m.cursor < len(m.filtered) {
		g := m.currentGroup()
		if g == nil || !m.collapsed[g.stage] {
			return
		}
		m.cursor = g.end
	}
	if m.cursor >= len(m.filtered) && len(m.filtered) > 0 {
		m.cursor = len(m.filtered) - 1
	}
}

// ── Undo operations ────────────────────────────────────────────────────────

func (m *analyzeModel) toggleCurrent() {
	idx := m.cursorItemIdx()
	if idx < 0 {
		return
	}
	m.undoState = map[int]bool{idx: m.items[idx].Selected}
	m.items[idx].Selected = !m.items[idx].Selected
	m.rebuildFiltered()
}

func (m *analyzeModel) selectAll() {
	m.undoState = make(map[int]bool)
	for i, item := range m.items {
		m.undoState[i] = item.Selected
		m.items[i].Selected = true
	}
	m.rebuildFiltered()
}

func (m *analyzeModel) deselectAll() {
	m.undoState = make(map[int]bool)
	for i, item := range m.items {
		m.undoState[i] = item.Selected
		m.items[i].Selected = false
	}
	m.rebuildFiltered()
}

func (m *analyzeModel) selectGroup(sel bool) {
	g := m.currentGroup()
	if g == nil {
		return
	}
	m.undoState = make(map[int]bool)
	for i := g.start; i < g.end; i++ {
		idx := m.filtered[i]
		m.undoState[idx] = m.items[idx].Selected
		m.items[idx].Selected = sel
	}
	m.rebuildFiltered()
}

func (m *analyzeModel) invertGroup() {
	g := m.currentGroup()
	if g == nil {
		return
	}
	m.undoState = make(map[int]bool)
	for i := g.start; i < g.end; i++ {
		idx := m.filtered[i]
		m.undoState[idx] = m.items[idx].Selected
		m.items[idx].Selected = !m.items[idx].Selected
	}
	m.rebuildFiltered()
}

func (m *analyzeModel) undo() {
	if len(m.undoState) == 0 {
		return
	}
	for idx, old := range m.undoState {
		m.items[idx].Selected = old
	}
	m.undoState = nil
	m.rebuildFiltered()
}

// ── Save ───────────────────────────────────────────────────────────────────

func (m *analyzeModel) saveSelected() bool {
	var patterns []string
	for _, item := range m.items {
		if item.Selected {
			patterns = append(patterns, item.Pattern)
		}
	}
	if len(patterns) == 0 {
		return false
	}
	if err := analyze.SavePatterns(m.cfg.Root, patterns); err != nil {
		m.phase = phaseError
		m.err = err.Error()
		return false
	}
	return true
}

// ── Tick ───────────────────────────────────────────────────────────────────

func tickCmd() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

type tickMsg time.Time

func (m analyzeModel) elapsed() string {
	d := time.Since(m.startTime).Truncate(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	mins := int(d.Minutes())
	secs := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm%02ds", mins, secs)
}

func (m *analyzeModel) appendLog(msg string) {
	const maxLogs = 5
	ts := time.Since(m.startTime).Truncate(time.Second)
	entry := fmt.Sprintf("[%s] %s", ts, msg)
	m.logs = append(m.logs, entry)
	if len(m.logs) > maxLogs {
		m.logs = m.logs[len(m.logs)-maxLogs:]
	}
}

// ── Update ─────────────────────────────────────────────────────────────────

func (m analyzeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	// ── Window resize ──────────────────────────────────────────────────
	case tea.WindowSizeMsg:
		m.termWidth = msg.Width
		h := msg.Height - 10 // header + footer + margins
		if h > 5 {
			m.viewportH = h
		}
		if m.showPreview {
			m.viewportH -= 4
		}
		if m.viewportH < 5 {
			m.viewportH = 5
		}
		return m, nil

	// ── Keyboard ───────────────────────────────────────────────────────
	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "ctrl+c":
			return m, tea.Quit
		case "q":
			if m.phase >= phaseSelecting {
				return m, tea.Quit
			}
		}

		if m.phase == phaseSelecting {
			m.updateSelecting(msg)
		} else if m.phase == phaseConfirm {
			m.updateConfirm(msg)
		}

		return m, nil

	// ── Pipeline messages ──────────────────────────────────────────────
	case walkProgressMsg:
		m.filesScanned = msg.files
		return m, m.waitForNext()

	case prescreenStartMsg:
		m.phase = phasePrescreen
		m.remaining = msg.files
		return m, m.waitForNext()

	case prescreenMsg:
		m.autoIgnored = msg.autoIgnored
		m.autoKept = msg.autoKept
		m.remaining = msg.remaining
		return m, m.waitForNext()

	case llmPassMsg:
		m.phase = phaseLLM
		m.llmPass = msg.pass
		return m, m.waitForNext()

	case llmBatchMsg:
		m.phase = phaseLLM
		m.llmBatch = msg.current
		m.llmTotalBatches = msg.total
		m.appendLog(fmt.Sprintf("LLM batch %d/%d: %s", msg.current, msg.total, msg.status))
		return m, m.waitForNext()

	case analysisDoneMsg:
		m.phase = phaseSelecting
		m.items = msg.items
		m.markExisting()
		m.rebuildFiltered()
		return m, nil

	case analysisErrorMsg:
		m.phase = phaseError
		m.err = string(msg)
		return m, nil

	case logMsg:
		m.appendLog(string(msg))
		return m, m.waitForNext()

	case tickMsg:
		return m, tickCmd()
	}

	return m, nil
}

// updateSelecting обрабатывает клавиши в фазе выбора.
func (m *analyzeModel) updateSelecting(msg tea.KeyMsg) {
	switch msg.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
			m.skipCollapsed()
			m.adjustScroll()
		}
	case "down", "j":
		if m.cursor < len(m.filtered)-1 {
			m.cursor++
			m.skipCollapsed()
			m.adjustScroll()
		}
	case "enter", " ":
		m.toggleCurrent()
	case "a":
		m.selectAll()
	case "n":
		m.deselectAll()
	case "A":
		m.selectGroup(true)
	case "N":
		m.selectGroup(false)
	case "i":
		m.invertGroup()
	case "u":
		m.undo()
	case "f":
		m.filter = (m.filter + 1) % 3
		m.cursor = 0
		m.scrollOff = 0
		m.rebuildFiltered()
	case "c":
		if g := m.currentGroup(); g != nil {
			m.collapsed[g.stage] = !m.collapsed[g.stage]
			if m.collapsed[g.stage] {
				m.skipCollapsed()
			}
		}
		m.adjustScroll()
	case "p":
		m.showPreview = !m.showPreview
		// Recalculate viewport
		h := m.viewportH
		if m.showPreview {
			h -= 4
		} else {
			h += 4
		}
		if h < 5 {
			h = 5
		}
		m.viewportH = h
		m.adjustScroll()
	case "y", "s":
		if m.selectedCount() > 0 {
			m.phase = phaseConfirm
		}
	}
}

// updateConfirm обрабатывает клавиши в фазе подтверждения.
func (m *analyzeModel) updateConfirm(msg tea.KeyMsg) {
	switch msg.String() {
	case "y", "enter":
		if m.saveSelected() {
			m.phase = phaseDone
		}
	case "n", "esc":
		m.phase = phaseSelecting
	}
}

// adjustScroll обновляет scroll offset чтобы курсор был в видимой области.
func (m *analyzeModel) adjustScroll() {
	if m.cursor < m.scrollOff {
		m.scrollOff = m.cursor
	}
	if m.cursor >= m.scrollOff+m.viewportH {
		m.scrollOff = m.cursor - m.viewportH + 1
	}
	if m.scrollOff < 0 {
		m.scrollOff = 0
	}
}

func (m analyzeModel) waitForNext() tea.Cmd {
	return func() tea.Msg {
		if m.ch == nil {
			return nil
		}
		msg, ok := <-m.ch
		if !ok {
			return nil
		}
		return msg
	}
}

// ── View ───────────────────────────────────────────────────────────────────

func (m analyzeModel) View() string {
	var b strings.Builder

	switch m.phase {
	case phaseScanning:
		m.viewScanning(&b)
	case phasePrescreen:
		m.viewPrescreen(&b)
	case phaseLLM:
		m.viewLLM(&b)
	case phaseSelecting:
		m.viewSelecting(&b)
	case phaseConfirm:
		m.viewConfirm(&b)
	case phaseDone:
		m.viewDone(&b)
	case phaseError:
		m.viewError(&b)
	}

	return b.String()
}

func (m analyzeModel) viewScanning(b *strings.Builder) {
	b.WriteString(analyzeTitleStyle.Render("ragota analyze"))
	b.WriteString("  ")
	b.WriteString(analyzeTimerStyle.Render(fmt.Sprintf("elapsed: %s", m.elapsed())))
	b.WriteString("\n\n")
	b.WriteString(analyzePhaseStyle.Render("Phase 1: Scanning directory..."))
	b.WriteString("\n\n")
	if m.filesScanned > 0 {
		b.WriteString(fmt.Sprintf("  Files found: %d\n", m.filesScanned))
	} else {
		b.WriteString(analyzeDimStyle.Render("  Starting scan...\n"))
	}
	m.writeLogs(b)
	b.WriteString(analyzeFooterStyle.Render("Press Esc to cancel"))
}

func (m analyzeModel) viewPrescreen(b *strings.Builder) {
	b.WriteString(analyzeTitleStyle.Render("ragota analyze"))
	b.WriteString("  ")
	b.WriteString(analyzeTimerStyle.Render(fmt.Sprintf("elapsed: %s", m.elapsed())))
	b.WriteString("\n\n")
	b.WriteString(analyzePhaseStyle.Render("Phase 2: Applying heuristics..."))
	b.WriteString("\n\n")
	if m.autoIgnored > 0 || m.autoKept > 0 {
		b.WriteString(fmt.Sprintf("  Auto-ignored: %d files\n", m.autoIgnored))
		b.WriteString(fmt.Sprintf("  Auto-kept:    %d files\n", m.autoKept))
		b.WriteString(fmt.Sprintf("  Remaining:    %d files → LLM\n", m.remaining))
	} else {
		b.WriteString(fmt.Sprintf("  Processing %d files...\n", m.remaining))
	}
	m.writeLogs(b)
	b.WriteString(analyzeFooterStyle.Render("Press Esc to cancel"))
}

func (m analyzeModel) viewLLM(b *strings.Builder) {
	b.WriteString(analyzeTitleStyle.Render("ragota analyze"))
	b.WriteString("  ")
	b.WriteString(analyzeTimerStyle.Render(fmt.Sprintf("elapsed: %s", m.elapsed())))
	b.WriteString("\n\n")
	
	// Показываем прогресс батчей если есть
	if m.llmTotalBatches > 0 {
		b.WriteString(analyzePhaseStyle.Render(fmt.Sprintf("Phase 3: LLM analysis (batch %d/%d)...", m.llmBatch, m.llmTotalBatches)))
		b.WriteString("\n\n")
		
		// Прогресс-бар
		barWidth := 40
		filled := (m.llmBatch * barWidth) / m.llmTotalBatches
		bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
		b.WriteString(fmt.Sprintf("  [%s] %d%%", bar, (m.llmBatch*100)/m.llmTotalBatches))
		
		// ETA
		elapsed := time.Since(m.startTime)
		if m.llmBatch > 0 {
			timePerBatch := elapsed / time.Duration(m.llmBatch)
			remaining := time.Duration(m.llmTotalBatches-m.llmBatch) * timePerBatch
			b.WriteString(fmt.Sprintf("  ETA: %s", remaining.Round(time.Second)))
		}
		b.WriteString("\n\n")
	} else {
		b.WriteString(analyzePhaseStyle.Render(fmt.Sprintf("Phase 3: LLM analysis (pass %d/%d)...", m.llmPass, m.llmTotalPasses)))
		b.WriteString("\n\n")
		switch m.llmPass {
		case 1:
			b.WriteString("  Classifying file groups...\n")
		case 2:
			b.WriteString("  Checking for contradictions...\n")
		case 3:
			b.WriteString("  Deep review of uncertain groups...\n")
		}
	}
	
	m.writeLogs(b)
	b.WriteString(analyzeFooterStyle.Render("Press Esc to cancel"))
}

func (m analyzeModel) viewSelecting(b *strings.Builder) {
	// Header
	b.WriteString(analyzeTitleStyle.Render("ragota analyze"))
	b.WriteString("  ")
	b.WriteString(analyzeTimerStyle.Render(fmt.Sprintf("elapsed: %s", m.elapsed())))
	b.WriteString("  ")
	b.WriteString(analyzeFilterStyle.Render(fmt.Sprintf("filter: %s", m.filter)))
	b.WriteString("\n\n")
	b.WriteString(analyzePhaseStyle.Render("Select patterns to add to .ragotaignore"))
	b.WriteString(fmt.Sprintf("  (%d items)\n", len(m.items)))

	// Summary bar
	total := len(m.items)
	sel := m.selectedCount()
	b.WriteString(analyzeSummaryStyle.Render(fmt.Sprintf("  ✔ %d/%d selected", sel, total)))
	b.WriteString("\n\n")

	// Empty list
	if len(m.filtered) == 0 {
		b.WriteString(analyzeDimStyle.Render("  No items match current filter.\n"))
		b.WriteString("\n")
		b.WriteString(analyzeFooterStyle.Render(
			"f=cycle filter  q=quit"))
		return
	}

	// Viewport
	end := m.scrollOff + m.viewportH
	if end > len(m.filtered) {
		end = len(m.filtered)
	}

	for i := m.scrollOff; i < end; i++ {
		m.renderItemRow(b, i)

		// Preview panel (если открыт и это cursor item)
		if m.showPreview && i == m.cursor {
			m.renderPreview(b, i)
		}
	}

	// Scroll indicator
	if len(m.filtered) > m.viewportH {
		b.WriteString(analyzeDimStyle.Render(fmt.Sprintf(
			"  ── items %d-%d of %d ──",
			m.scrollOff+1, end, len(m.filtered))))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(analyzeFooterStyle.Render(
		"↑/↓ navigate  Enter toggle  a/n all  A/N group  i invert  u undo  f filter  c collapse  p preview  y save  q quit"))
}

// renderItemRow отрисовывает одну строку (групповой заголовок + элемент).
func (m analyzeModel) renderItemRow(b *strings.Builder, i int) {
	// Group header (если начало новой группы)
	for _, g := range m.groups {
		if g.start == i {
			arrow := "▾"
			if m.collapsed[g.stage] {
				arrow = "▸"
			}
			header := fmt.Sprintf("  %s %s  (%d/%d)",
				arrow, g.title, g.selected, g.count)
			b.WriteString(analyzeGroupStyle.Render(header))
			b.WriteString("\n")
			break
		}
	}

	// Collapsed — не рисуем элементы
	if g := m.currentGroupAt(i); g != nil && m.collapsed[g.stage] {
		return
	}

	idx := m.filtered[i]
	item := m.items[idx]
	isCursor := i == m.cursor

	// Prefix
	prefix := "  "
	if isCursor {
		prefix = "▸ "
	}

	// Checkbox
	var check string
	if item.Selected {
		check = analyzeCheckOnStyle.Render(checkOn)
	} else {
		check = analyzeCheckOffStyle.Render(checkOff)
	}

	// Confidence with color
	var confStr string
	switch {
	case item.Confidence >= 80:
		confStr = analyzeConfHigh.Render(fmt.Sprintf("%d%%", item.Confidence))
	case item.Confidence >= 50:
		confStr = analyzeConfMid.Render(fmt.Sprintf("%d%%", item.Confidence))
	default:
		confStr = analyzeConfLow.Render(fmt.Sprintf("%d%%", item.Confidence))
	}

	// Reason
	reason := item.Reason
	if item.Confidence < 50 {
		reason += " ❓"
	} else if item.Confidence < 70 {
		reason += " ⚠"
	}

	// Exists badge
	existsBadge := ""
	if item.Exists {
		existsBadge = analyzeDimStyle.Render(" (exists)")
	}

	// Format line
	label := fmt.Sprintf("%s %s  %-24s %s  %s%s",
		check, item.Pattern, "["+item.Stage+"]", confStr, reason, existsBadge)

	// Truncate to terminal width
	maxW := m.termWidth - 4
	if maxW <= 0 {
		maxW = 116
	}
	if len(label) > maxW {
		label = label[:maxW-3] + "..."
	}

	if isCursor {
		b.WriteString(prefix + analyzeCursorStyle.Render(label) + "\n")
	} else if item.Exists {
		b.WriteString(prefix + analyzeExistsStyle.Render(label) + "\n")
	} else {
		b.WriteString(prefix + analyzeItemStyle.Render(label) + "\n")
	}
}

// currentGroupAt возвращает группу, которой принадлежит filtered[i].
func (m analyzeModel) currentGroupAt(i int) *itemGroup {
	for gi := range m.groups {
		if i >= m.groups[gi].start && i < m.groups[gi].end {
			return &m.groups[gi]
		}
	}
	return nil
}

// renderPreview отрисовывает preview panel под текущим элементом.
func (m analyzeModel) renderPreview(b *strings.Builder, i int) {
	idx := m.filtered[i]
	item := m.items[idx]

	if len(item.Preview) == 0 {
		b.WriteString(analyzePreviewStyle.Render("(no preview files)") + "\n")
		return
	}

	max := 5
	if len(item.Preview) < max {
		max = len(item.Preview)
	}
	for j := 0; j < max; j++ {
		b.WriteString(analyzePreviewStyle.Render("  " + item.Preview[j]))
		b.WriteString("\n")
	}
	if len(item.Preview) > 5 {
		b.WriteString(analyzePreviewStyle.Render(fmt.Sprintf("  ... and %d more", len(item.Preview)-5)))
		b.WriteString("\n")
	}
}

func (m analyzeModel) viewConfirm(b *strings.Builder) {
	b.WriteString(analyzeTitleStyle.Render("ragota analyze"))
	b.WriteString("\n\n")
	b.WriteString(analyzePhaseStyle.Render("Confirm save to .ragotaignore"))
	b.WriteString("\n\n")

	// Collect selected
	type entry struct {
		pattern string
		stage   string
	}
	var newOnes, existing []entry
	for _, item := range m.items {
		if item.Selected {
			e := entry{pattern: item.Pattern, stage: item.Stage}
			if item.Exists {
				existing = append(existing, e)
			} else {
				newOnes = append(newOnes, e)
			}
		}
	}

	if len(newOnes) > 0 {
		b.WriteString(analyzeSummaryStyle.Render(fmt.Sprintf("  %d new patterns:", len(newOnes))))
		b.WriteString("\n")
		for _, e := range newOnes {
			b.WriteString(analyzeItemStyle.Render(fmt.Sprintf("+ %s", e.pattern)))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if len(existing) > 0 {
		b.WriteString(analyzeDimStyle.Render(fmt.Sprintf("  %d already exist (skipped)", len(existing))))
		b.WriteString("\n\n")
	}

	if len(newOnes) == 0 && len(existing) == 0 {
		b.WriteString(analyzeDimStyle.Render("  No patterns selected\n"))
	}

	b.WriteString("\n")
	b.WriteString(analyzeFooterStyle.Render("y=confirm  n/esc=go back"))
}

func (m analyzeModel) viewDone(b *strings.Builder) {
	b.WriteString(analyzeTitleStyle.Render("ragota analyze"))
	b.WriteString("  ")
	b.WriteString(analyzeTimerStyle.Render(fmt.Sprintf("done in %s", m.elapsed())))
	b.WriteString("\n\n")
	b.WriteString(analyzeProgressStyle.Render("✔ Patterns saved to .ragotaignore"))
	b.WriteString("\n\n")

	count := 0
	for _, item := range m.items {
		if item.Selected {
			count++
		}
	}
	b.WriteString(fmt.Sprintf("  %d patterns saved\n", count))
	b.WriteString(analyzeFooterStyle.Render("Press Esc or q to quit"))
}

func (m analyzeModel) viewError(b *strings.Builder) {
	b.WriteString(analyzeTitleStyle.Render("ragota analyze"))
	b.WriteString("\n\n")
	b.WriteString(analyzeErrorStyle.Render("Error: " + m.err))
	b.WriteString("\n\n")
	b.WriteString(analyzeFooterStyle.Render("Press Esc or q to quit"))
}

// writeLogs пишет последние лог-записи в буфер.
func (m analyzeModel) writeLogs(b *strings.Builder) {
	if len(m.logs) == 0 {
		return
	}
	b.WriteString("\n")
	for _, log := range m.logs {
		b.WriteString(analyzeLogStyle.Render(log))
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

// ── Pipeline messages ──────────────────────────────────────────────────────

type walkProgressMsg struct {
	files int
}

type prescreenStartMsg struct {
	files int
}

type prescreenMsg struct {
	autoIgnored int
	autoKept    int
	remaining   int
}

type llmPassMsg struct {
	pass int
}

type llmBatchMsg struct {
	current, total int
	status         string
}

type analysisDoneMsg struct {
	items []analyzeItem
}

type analysisErrorMsg string

type logMsg string

// ── Pipeline ───────────────────────────────────────────────────────────────

// startAnalysis запускает async pipeline анализа.
func (m analyzeModel) startAnalysis() tea.Cmd {
	ch := m.ch
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	noLLM := m.noLLM
	cfg := m.cfg

	go func() {
		defer close(ch)

		// Phase 1: Scan
		root := cfg.Root
		gitIgnore, _ := gitignore.Load(root)

		excludedPaths := make(map[string]bool)
		heuristicSeen := make(map[string]bool) // дедупликация по Pattern
		var heuristicEntries []analyze.Entry
		// Для preview: маппинг known-dir паттерн → файлы
		dirPreviewFiles := make(map[string][]string)
		var allFiles []string
		totalFiles := 0

		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}

			rel, _ := filepath.Rel(root, path)
			if rel == "." {
				return nil
			}

			// Пропускаем симлинки и специальные файлы
			if d.Type()&fs.ModeSymlink != 0 || d.Type()&fs.ModeNamedPipe != 0 || d.Type()&fs.ModeSocket != 0 || d.Type()&fs.ModeDevice != 0 {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}

			if d.IsDir() {
				base := filepath.Base(rel)
				if base == ".git" || base == ".hg" || base == ".svn" {
					return filepath.SkipDir
				}
				if heuristic.KnownDirNames[base] {
					excludedPaths[rel] = true
					// Дедупликация: добавляем Entry только если pattern ещё не встречался
					if !heuristicSeen[base] {
						heuristicSeen[base] = true
						heuristicEntries = append(heuristicEntries, analyze.Entry{
							Path:       rel,
							Pattern:    base,
							Stage:      "heuristic",
							Reason:     "known ignore directory",
							Confidence: 100,
						})
					}
					return filepath.SkipDir
				}
				if gitIgnore != nil && gitIgnore.ShouldSkip(rel) {
					return filepath.SkipDir
				}
				return nil
			}

			totalFiles++

			// Собираем preview для excluded директорий (без вложенного WalkDir)
			for exclPath := range excludedPaths {
				if strings.HasPrefix(rel, exclPath+"/") {
					base := filepath.Base(exclPath)
					if len(dirPreviewFiles[base]) < 10 {
						dirPreviewFiles[base] = append(dirPreviewFiles[base], rel)
					}
					break
				}
			}

			if excludedPaths[rel] {
				return nil
			}
			if gitIgnore != nil && gitIgnore.ShouldSkip(rel) {
				return nil
			}

			allFiles = append(allFiles, rel)

			if totalFiles%100 == 0 {
				ch <- walkProgressMsg{files: totalFiles}
			}

			return nil
		})

		ch <- logMsg(fmt.Sprintf("Scan complete: %d files, %d known dirs skipped", totalFiles, len(heuristicEntries)))

		// Compound-файлы (user.controller.ts) НЕ блокируем автоматически — они могут быть бизнес-логикой.
		// Проверка на сгенерированные файлы будет в PreScreen (HasGeneratedMarker).
		var remainingFiles []string
		for _, f := range allFiles {
			remainingFiles = append(remainingFiles, f)
		}

		// Прогресс: pre-screen начат
		ch <- prescreenStartMsg{files: len(remainingFiles)}

		// Phase 2: Pre-screen (только файлы без compound)
		ch <- logMsg(fmt.Sprintf("Heuristics: classifying %d files...", len(remainingFiles)))
		preResult := heuristic.PreScreen(remainingFiles, root)

		ch <- prescreenMsg{
			autoIgnored: len(preResult.AutoIgnored),
			autoKept:    len(preResult.AutoKept),
			remaining:   len(preResult.Remaining),
		}
		ch <- logMsg(fmt.Sprintf("Heuristics done: %d ignored, %d kept, %d → LLM",
			len(preResult.AutoIgnored), len(preResult.AutoKept), len(preResult.Remaining)))

		// Собираем все heuristic entries с preview (дедупликация по Pattern)
		seenPatterns := make(map[string]bool)
		var items []analyzeItem

		for _, e := range heuristicEntries {
			if seenPatterns[e.Pattern] {
				continue
			}
			seenPatterns[e.Pattern] = true
			preview := dirPreviewFiles[e.Pattern]
			if len(preview) == 0 && e.Path != e.Pattern {
				preview = []string{e.Path}
			}
			items = append(items, analyzeItem{
				Path:       e.Path,
				Pattern:    e.Pattern,
				Stage:      e.Stage,
				Reason:     e.Reason,
				Confidence: e.Confidence,
				Selected:   true,
				Preview:    preview,
			})
		}

		// preResult.AutoIgnored — дедупликация по Pattern, мержим preview
		autoIgnoredSeen := make(map[string]int) // pattern → index in items
		for _, e := range preResult.AutoIgnored {
			if idx, exists := autoIgnoredSeen[e.Pattern]; exists {
				// Уже есть — добавляем файл в preview (до 10)
				if len(items[idx].Preview) < 10 {
					items[idx].Preview = append(items[idx].Preview, e.Path)
				}
				continue
			}
			if seenPatterns[e.Pattern] {
				continue
			}
			seenPatterns[e.Pattern] = true
			autoIgnoredSeen[e.Pattern] = len(items)
			items = append(items, analyzeItem{
				Path:       e.Path,
				Pattern:    e.Pattern,
				Stage:      e.Stage,
				Reason:     e.Reason,
				Confidence: e.Confidence,
				Selected:   true,
				Preview:    []string{e.Path},
			})
		}

		if noLLM || len(preResult.Remaining) == 0 {
			ch <- analysisDoneMsg{items: items}
			return
		}

		// Phase 3: LLM
		groups := analyze.GroupFilesByScope(preResult.Remaining)
		ch <- logMsg(fmt.Sprintf("Grouped %d files into %d scoped patterns", len(preResult.Remaining), len(groups)))

		// Построение подгрупп
		ch <- logMsg("Building subgroup trees...")
		for i := range groups {
			analyze.BuildSubGroups(&groups[i], root)
		}
		subGroupCount := 0
		for _, g := range groups {
			subGroupCount += len(g.SubGroups)
		}
		ch <- logMsg(fmt.Sprintf("Built %d subgroups from %d groups", subGroupCount, len(groups)))

		// Выбор модели: флаг --model > config > default
		model := m.model
		if model == "" {
			model = cfg.Ollama.IgnoreModel
		}
		if model == "" {
			model = "qwen3:4b"
		}
		ch <- logMsg(fmt.Sprintf("LLM: loading model %s...", model))

		llmProgress := func(current, total int, status string) {
			ch <- llmBatchMsg{current: current, total: total, status: status}
		}

		decisions, err := llm.Evaluate3Pass(ctx, cfg.Ollama.URL, model, groups, root, llmProgress)

		if err != nil {
			ch <- logMsg(fmt.Sprintf("LLM error: %s", err.Error()))
			ch <- analysisErrorMsg(err.Error())
			return
		}
		ch <- logMsg(fmt.Sprintf("LLM done: %d decisions", len(decisions)))

		// Validate
		llmEntries := resolve.LLMDecisions(decisions, groups)

		// Filter indexed extensions
		var rawPatterns []string
		for _, d := range decisions {
			if d.Action == "ignore" {
				rawPatterns = append(rawPatterns, d.Pattern)
			}
		}
		rawPatterns, llmEntries = output.FilterIndexedExtensions(rawPatterns, llmEntries)

		// Group patterns BEFORE showing in TUI (so user sees compact list)
		groupedPatterns := output.GroupPatternsFromPaths(rawPatterns)

		// Build file map for preview matching (use grouped patterns)
		fileMap := make(map[string][]string) // pattern → matching files
		for _, p := range groupedPatterns {
			matches := analyze.MatchPatternToFiles(p, preResult.Remaining)
			fileMap[p] = matches
		}

		// Добавляем LLM entries с preview (дедупликация через seenPatterns)
		for _, e := range llmEntries {
			if seenPatterns[e.Pattern] {
				continue
			}
			seenPatterns[e.Pattern] = true
			preview := fileMap[e.Pattern]
			if len(preview) == 0 {
				preview = []string{e.Path}
			}
			items = append(items, analyzeItem{
				Path:       e.Path,
				Pattern:    e.Pattern,
				Stage:      e.Stage,
				Reason:     e.Reason,
				Confidence: e.Confidence,
				Selected:   true,
				Preview:    preview,
			})
		}

		// Добавляем grouped patterns с preview (дедупликация через seenPatterns)
		for _, p := range groupedPatterns {
			if seenPatterns[p] {
				continue
			}
			seenPatterns[p] = true
			stage := "llm"
			reason := "LLM suggested"
			confidence := 80
			if strings.HasPrefix(p, "!") {
				stage = "negation"
				reason = "exception from ignore pattern"
				confidence = 90
			}
			preview := fileMap[p]
			items = append(items, analyzeItem{
				Path:       p,
				Pattern:    p,
				Stage:      stage,
				Reason:     reason,
				Confidence: confidence,
				Selected:   true,
				Preview:    preview,
			})
		}

		ch <- analysisDoneMsg{items: items}
	}()

	return func() tea.Msg {
		return <-ch
	}
}

// ── Program ────────────────────────────────────────────────────────────────

// NewAnalyzeProgram создаёт bubbletea программу.
func NewAnalyzeProgram(m analyzeModel) *tea.Program {
	return tea.NewProgram(m, tea.WithAltScreen())
}
