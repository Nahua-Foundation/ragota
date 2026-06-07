package cli

// cli_configure.go — ragota configure: интерактивный TUI-визард для
// установки зависимостей, генерации конфига и настройки MCP агента.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"ragota/pkg/config"
	"ragota/pkg/docker"
	"ragota/pkg/lsp"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

// ─── Стили ───

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63")).MarginBottom(1)
	stepStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).MarginBottom(1)
	checkStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("35"))
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("208"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	boldStyle   = lipgloss.NewStyle().Bold(true)
	dirStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("63")).Bold(true)
	fileStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	selStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(lipgloss.Color("63")).Bold(true)
	promptStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("63")).Bold(true).MarginTop(1)
	inputStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(lipgloss.Color("236")).Padding(0, 1)
	helpStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).MarginTop(1)
	successStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("35")).Bold(true)
)

// ─── Шаги визарда ───

const (
	stepWelcome = iota
	stepDependencies
	stepRagotaConfig
	stepAgentMCP
	stepAgentPath
	stepDone
)

var stepTitles = []string{
	"Welcome",
	"Check dependencies",
	"Ragota config",
	"Agent MCP integration",
	"Select agent config file",
	"Complete",
}

// ─── Модель визарда ───

type wizModel struct {
	step    int
	width   int
	height  int
	quitting bool
	err     string

	// Шаг 1: Dependencies
	depItems  []depItem
	depCursor int

	// Шаг 2: Config
	configExists bool
	configPath   string

	// Шаг 3: Agent MCP
	agentSkip bool

	// Шаг 4: File picker
	picker    *filePickerModel
	agentPath string

	// Context для операций
	ctx    context.Context
	cancel context.CancelFunc
}

type depItem struct {
	name    string
	status  depStatus // pending, ok, installing, done, failed, skipped
	message string
}

type depStatus int

const (
	depPending depStatus = iota
	depOK
	depInstalling
	depDone
	depFailed
	depSkipped
)

func (d depStatus) String() string {
	switch d {
	case depOK:
		return "✓"
	case depInstalling:
		return "⟳"
	case depDone:
		return "✓"
	case depFailed:
		return "✗"
	case depSkipped:
		return "○"
	default:
		return " "
	}
}

func (d depStatus) Style() lipgloss.Style {
	switch d {
	case depOK, depDone:
		return checkStyle
	case depFailed:
		return errStyle
	case depInstalling:
		return warnStyle
	case depSkipped:
		return dimStyle
	default:
		return dimStyle
	}
}

func initialWizModel() wizModel {
	ctx, cancel := context.WithCancel(context.Background())
	return wizModel{
		step: stepWelcome,
		depItems: []depItem{
			{name: "Docker"},
			{name: "Ollama"},
			{name: "Ollama models"},
			{name: "LSP servers"},
		},
		ctx:    ctx,
		cancel: cancel,
	}
}

func (m wizModel) Init() tea.Cmd {
	return nil
}

func (m wizModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			if m.cancel != nil {
				m.cancel()
			}
			return m, tea.Quit
		case "esc":
			m.quitting = true
			if m.cancel != nil {
				m.cancel()
			}
			return m, tea.Quit
		case "enter":
			return m.handleEnter()
		case "tab":
			if m.step == stepAgentPath && m.picker != nil && len(m.picker.filtered) > 0 && m.picker.cursor < len(m.picker.filtered) {
				return m.handlePickerComplete()
			}
		case "up", "k":
			return m.handleUp()
		case "down", "j":
			return m.handleDown()
		case "backspace":
			if m.step == stepAgentPath && m.picker != nil {
				return m.handlePickerBackspace()
			}
		}
		// Любой символ — ввод в file picker
		if m.step == stepAgentPath && m.picker != nil && len(msg.String()) == 1 {
			return m.handlePickerType(msg.String())
		}
	case depsCheckedMsg:
		m.depItems = msg.items
		// Проверяем есть ли ещё pending/installing — если нет, можно перейти дальше
		return m, nil
	case depInstallMsg:
		// После установки перечитываем зависимости
		m.err = ""
		if len(msg.errs) > 0 {
			m.err = strings.Join(msg.errs, "; ")
		}
		return m, checkDependenciesCmd(m.ctx)
	case configCheckedMsg:
		m.configExists = msg.exists
		m.configPath = msg.path
		return m, nil
	}

	// Forward to picker если активен
	if m.step == stepAgentPath && m.picker != nil {
		var cmd tea.Cmd
		m.picker, cmd = m.picker.update(msg)
		return m, cmd
	}

	return m, nil
}

func (m wizModel) handlePickerComplete() (tea.Model, tea.Cmd) {
	if m.picker == nil || len(m.picker.filtered) == 0 || m.picker.cursor >= len(m.picker.filtered) {
		return m, nil
	}
	entry := m.picker.filtered[m.picker.cursor]
	if entry.isDir {
		m.picker.dir = entry.fullPath
		m.picker.input = ""
		m.picker.cursor = 0
		m.picker.scanDir()
	} else {
		m.picker.input = entry.name
		m.picker.filterEntries()
	}
	return m, nil
}


func (m wizModel) handleEnter() (tea.Model, tea.Cmd) {
	switch m.step {
	case stepWelcome:
		m.step = stepDependencies
		return m, checkDependenciesCmd(m.ctx)
	case stepDependencies:
		// Если все dependencies проверены — переходим дальше
		allDone := true
		for _, item := range m.depItems {
			if item.status == depPending || item.status == depInstalling {
				allDone = false
				break
			}
		}
		if allDone {
			m.step = stepRagotaConfig
			return m, checkConfigCmd()
		}
		// Иначе — install selected
		return m, installDepCmd(m.ctx, m.depCursor)
	case stepRagotaConfig:
		m.step = stepAgentMCP
		m.agentSkip = true // по умолчанию — Skip
		return m, nil
	case stepAgentMCP:
		if m.agentSkip {
			m.step = stepDone
			return m, nil
		}
		m.step = stepAgentPath
		m.picker = newFilePickerModel()
		return m, nil
	case stepAgentPath:
		if m.picker != nil && len(m.picker.filtered) > 0 && m.picker.cursor < len(m.picker.filtered) {
			entry := m.picker.filtered[m.picker.cursor]
			if !entry.isDir {
				m.agentPath = entry.fullPath
				m.step = stepDone
				return m, applyAgentMCPCmd(m.ctx, m.agentPath)
			}
		}
	case stepDone:
		m.quitting = true
		if m.cancel != nil {
			m.cancel()
		}
		return m, tea.Quit
	}
	return m, nil
}

func (m wizModel) handleUp() (tea.Model, tea.Cmd) {
	switch m.step {
	case stepDependencies:
		if m.depCursor > 0 {
			m.depCursor--
		}
	case stepAgentMCP:
		m.agentSkip = !m.agentSkip
	case stepAgentPath:
		if m.picker != nil {
			m.picker.cursorUp()
		}
	}
	return m, nil
}

func (m wizModel) handleDown() (tea.Model, tea.Cmd) {
	switch m.step {
	case stepDependencies:
		if m.depCursor < len(m.depItems)-1 {
			m.depCursor++
		}
	case stepAgentMCP:
		m.agentSkip = !m.agentSkip
	case stepAgentPath:
		if m.picker != nil {
			m.picker.cursorDown()
		}
	}
	return m, nil
}

func (m wizModel) handlePickerTab() (tea.Model, tea.Cmd) {
	if m.picker != nil {
		m.picker.cycleComp()
	}
	return m, nil
}

func (m wizModel) handlePickerBackspace() (tea.Model, tea.Cmd) {
	if m.picker != nil {
		m.picker.backspace()
	}
	return m, nil
}

func (m wizModel) handlePickerType(ch string) (tea.Model, tea.Cmd) {
	if m.picker != nil {
		m.picker.input += ch
		m.picker.filterEntries()
	}
	return m, nil
}

func (m wizModel) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	// Progress bar
	b.WriteString(renderProgress(m.step))
	b.WriteString("\n\n")

	// Step title
	title := stepTitles[m.step]
	b.WriteString(titleStyle.Render(fmt.Sprintf("═══ %s ═══", title)))
	b.WriteString("\n\n")

	// Step content
	b.WriteString(m.renderStep())

	// Error
	if m.err != "" {
		b.WriteString(errStyle.Render("Error: "+m.err))
		b.WriteString("\n\n")
	}

	// Help
	b.WriteString(helpStyle.Render(m.renderHelp()))
	b.WriteString("\n")

	return b.String()
}

func (m wizModel) renderStep() string {
	switch m.step {
	case stepWelcome:
		return welcomeView()
	case stepDependencies:
		return m.renderDependencies()
	case stepRagotaConfig:
		return m.renderRagotaConfig()
	case stepAgentMCP:
		return m.renderAgentMCP()
	case stepAgentPath:
		return m.renderAgentPath()
	case stepDone:
		return m.renderDone()
	default:
		return ""
	}
}

func welcomeView() string {
	var b strings.Builder
	b.WriteString(dimStyle.Render("  ragota — AI dev tool for code indexing and search\n\n"))
	b.WriteString("  Setup wizard will:\n")
	b.WriteString("  1. Check and install dependencies (Docker, Ollama, LSP)\n")
	b.WriteString("  2. Generate ~/.ragota/config.yaml\n")
	b.WriteString("  3. Configure MCP in your agent (Claude Desktop, Cursor, etc.)\n")
	return b.String()
}

func (m wizModel) renderDependencies() string {
	var b strings.Builder
	for i, item := range m.depItems {
		cursor := "  "
		if i == m.depCursor {
			cursor = "▸ "
		}
		status := item.status.Style().Render(item.status.String())
		name := item.name
		if i == m.depCursor {
			name = boldStyle.Render(item.name)
		}
		msg := ""
		if item.message != "" {
			msg = dimStyle.Render(" — "+item.message)
		}
		b.WriteString(fmt.Sprintf("  %s %s %s%s\n", cursor, status, name, msg))
	}
	return b.String()
}

func (m wizModel) renderRagotaConfig() string {
	var b strings.Builder
	if m.configExists {
		b.WriteString(checkStyle.Render("  ✓ Config already exists: "))
		b.WriteString(m.configPath)
		b.WriteString("\n")
	} else {
		b.WriteString(warnStyle.Render("  ✗ Config not found"))
		b.WriteString("\n")
	}
	return b.String()
}

func (m wizModel) renderAgentMCP() string {
	var b strings.Builder
	b.WriteString("  Configure MCP servers in your agent config?\n\n")

	options := []string{"Yes, configure MCP", "Skip for now"}
	for i, opt := range options {
		cursor := "  "
		selected := (i == 0 && !m.agentSkip) || (i == 1 && m.agentSkip)
		if selected {
			cursor = "▸ "
		}
		mark := " "
		if selected {
			mark = "◉"
		}
		label := opt
		if selected {
			label = boldStyle.Render(opt)
		}
		b.WriteString(fmt.Sprintf("  %s [%s] %s\n", cursor, mark, label))
	}
	return b.String()
}

func (m wizModel) renderAgentPath() string {
	if m.picker == nil {
		return ""
	}
	return m.picker.view(m.width, m.height)
}

func (m wizModel) renderDone() string {
	var b strings.Builder
	b.WriteString(successStyle.Render("  ✓ Setup complete!\n\n"))
	b.WriteString("  You can now run:\n\n")
	b.WriteString(boldStyle.Render("    ragota up\n\n"))
	b.WriteString(dimStyle.Render("  to start indexing and the MCP server.\n"))

	if m.agentPath != "" {
		b.WriteString("\n")
		b.WriteString(checkStyle.Render("  ✓ MCP configured in: "))
		b.WriteString(m.agentPath)
		b.WriteString("\n")
	}
	return b.String()
}

func renderProgress(step int) string {
	current := step + 1
	total := len(stepTitles) - 1 // не считаем stepDone
	if current > total {
		current = total
	}
	return dimStyle.Render(fmt.Sprintf("  step %d/%d", current, total))
}

func (m wizModel) renderHelp() string {
	switch m.step {
	case stepWelcome:
		return "Enter → start"
	case stepDependencies:
		return "↑/↓ navigate · Enter → check/install"
	case stepRagotaConfig:
		return "Enter → continue"
	case stepAgentMCP:
		return "↑/↓ select · Enter → confirm"
	case stepAgentPath:
		return "↑/↓ navigate · Tab → complete · Enter → select · ← backspace · Esc/q → quit"
	case stepDone:
		return "Enter → exit"
	default:
		return ""
	}
}

// ─── File Picker ───

type fileEntry struct {
	name    string
	isDir   bool
	hidden  bool
	fullPath string
}

type filePickerModel struct {
	dir        string
	entries    []fileEntry
	filtered   []fileEntry
	cursor     int
	input      string
	selected   string
	showComp   bool
	completions []string
	compIndex  int
	width      int
	height     int
	err        string
}

func newFilePickerModel() *filePickerModel {
	m := &filePickerModel{
		dir:   "~",
		input: "",
	}
	if home, err := os.UserHomeDir(); err == nil {
		m.dir = home
	}
	m.scanDir()
	return m
}

func (m *filePickerModel) scanDir() {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		m.err = err.Error()
		m.entries = nil
		m.filtered = nil
		return
	}

	var files []fileEntry
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if !e.IsDir() && !info.Mode().IsRegular() {
			continue
		}
		files = append(files, fileEntry{
			name:     e.Name(),
			isDir:    e.IsDir(),
			hidden:   strings.HasPrefix(e.Name(), "."),
			fullPath: filepath.Join(m.dir, e.Name()),
		})
	}

	sort.Slice(files, func(i, j int) bool {
		if files[i].isDir != files[j].isDir {
			return files[i].isDir
		}
		return files[i].name < files[j].name
	})

	m.entries = files
	m.filterEntries()
}

func (m *filePickerModel) filterEntries() {
	if m.input == "" {
		m.filtered = make([]fileEntry, len(m.entries))
		copy(m.filtered, m.entries)
	} else {
		lower := strings.ToLower(m.input)
		var filtered []fileEntry
		for _, e := range m.entries {
			if strings.Contains(strings.ToLower(e.name), lower) {
				filtered = append(filtered, e)
			}
		}
		m.filtered = filtered
	}

	if m.cursor >= len(m.filtered) {
		if len(m.filtered) > 0 {
			m.cursor = len(m.filtered) - 1
		} else {
			m.cursor = 0
		}
	}
	m.selected = ""
	m.showComp = false
}

func (m *filePickerModel) cursorUp() {
	m.showComp = false
	if m.cursor > 0 {
		m.cursor--
	}
}

func (m *filePickerModel) cursorDown() {
	m.showComp = false
	if m.cursor < len(m.filtered)-1 {
		m.cursor++
	}
}

// complete — автодополнение: если один вариант — дополняет, если несколько — показывает меню
func (m *filePickerModel) complete() {
	if m.input == "" {
		return
	}
	lower := strings.ToLower(m.input)
	var matches []string
	for _, e := range m.entries {
		if strings.HasPrefix(strings.ToLower(e.name), lower) {
			matches = append(matches, e.name)
		}
	}

	if len(matches) == 0 {
		return
	}
	if len(matches) == 1 {
		// Single match — complete it
		m.input = matches[0]
		m.filterEntries()
	} else {
		// Multiple matches — show completion menu, cycle on next Tab
		m.completions = matches
		if m.compIndex >= len(m.completions) {
			m.compIndex = 0
		}
		m.showComp = true
		// Auto-select the current completion
		m.input = m.completions[m.compIndex]
		m.filterEntries()
	}
}

// cycleComp — переключает completion в меню
func (m *filePickerModel) cycleComp() {
	if !m.showComp || len(m.completions) == 0 {
		m.complete()
		return
	}
	m.compIndex = (m.compIndex + 1) % len(m.completions)
	m.input = m.completions[m.compIndex]
	// Move cursor to the matching entry
	m.cursor = 0
	for i, e := range m.filtered {
		if e.name == m.input {
			m.cursor = i
			break
		}
	}
}

func (m *filePickerModel) backspace() {
	if len(m.input) > 0 {
		m.input = m.input[:len(m.input)-1]
		m.filterEntries()
	}
}

func (m *filePickerModel) update(msg tea.Msg) (*filePickerModel, tea.Cmd) {
	if msg, ok := msg.(tea.KeyMsg); ok {
		switch msg.String() {
		case "enter":
			m.showComp = false
			if len(m.filtered) > 0 && m.cursor < len(m.filtered) {
				entry := m.filtered[m.cursor]
				if entry.isDir {
					m.dir = entry.fullPath
					m.input = ""
					m.cursor = 0
					m.scanDir()
				} else {
					m.selected = entry.fullPath
					return m, tea.Quit
				}
			}
		case "left", "h", "backspace2":
			m.showComp = false
			parent := filepath.Dir(m.dir)
			if parent != m.dir {
				m.dir = parent
				m.input = ""
				m.cursor = 0
				m.scanDir()
			}
		}
	}
	return m, nil
}

func (m *filePickerModel) view(w, h int) string {
	m.width = w
	m.height = h

	var b strings.Builder

	// Path breadcrumb with ~ expansion
	relPath := m.dir
	if home, err := os.UserHomeDir(); err == nil {
		if strings.HasPrefix(relPath, home) {
			relPath = "~" + relPath[len(home):]
		}
	}
	b.WriteString(promptStyle.Render("  📁 " + relPath))
	b.WriteString("\n\n")

	// Input line
	filterLabel := "  " + inputStyle.Render(">"+m.input+" ")
	if len(m.filtered) != len(m.entries) {
		filterLabel += dimStyle.Render(fmt.Sprintf("  (%d/%d)", len(m.filtered), len(m.entries)))
	}
	b.WriteString(filterLabel)
	b.WriteString("\n")

	// Completion menu (zsh-style horizontal list)
	if m.showComp && len(m.completions) > 0 {
		b.WriteString("\n")
		maxShow := 10
		show := m.completions
		if len(show) > maxShow {
			show = show[:maxShow]
		}
		for i, c := range show {
			if i == m.compIndex {
				b.WriteString(selStyle.Render(" ► " + c + " "))
			} else {
				b.WriteString("   " + c + "  ")
			}
			if (i+1)%4 == 0 && i < len(show)-1 {
				b.WriteString("\n     ")
			}
		}
		if len(m.completions) > maxShow {
			b.WriteString(dimStyle.Render(fmt.Sprintf("  ... +%d more", len(m.completions)-maxShow)))
		}
		b.WriteString("\n")
	}

	b.WriteString("\n")

	// Error
	if m.err != "" {
		b.WriteString(errStyle.Render("  " + m.err))
		b.WriteString("\n\n")
	}

	// File list
	maxItems := 12
	start := 0
	if m.cursor >= maxItems {
		start = m.cursor - maxItems + 1
	}
	end := start + maxItems
	if end > len(m.filtered) {
		end = len(m.filtered)
		if end-start < maxItems && start > 0 {
			start = end - maxItems
			if start < 0 {
				start = 0
			}
		}
	}

	if len(m.filtered) == 0 {
		b.WriteString(dimStyle.Render("  (no matches)"))
		b.WriteString("\n")
	} else {
		for i := start; i < end; i++ {
			e := m.filtered[i]
			cursor := "  "
			if i == m.cursor {
				cursor = "▸ "
			}

			icon := " "
			if e.isDir {
				icon = "📂"
			}

			name := highlightMatch(e.name, m.input)
			if i == m.cursor {
				name = selStyle.Render(name)
			} else if e.isDir {
				name = dirStyle.Render(name)
			} else {
				name = fileStyle.Render(name)
			}

			b.WriteString(fmt.Sprintf("%s%s %s\n", cursor, icon, name))
		}
	}

	if len(m.filtered) > maxItems {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  ... %d more\n", len(m.filtered)-end)))
	}

	if m.selected != "" {
		b.WriteString("\n")
		b.WriteString(successStyle.Render("  ✓ " + m.selected))
		b.WriteString("\n")
	}

	return b.String()
}

// highlightMatch подсвечивает совпадающую часть имени файла
func highlightMatch(name, pattern string) string {
	if pattern == "" {
		return name
	}
	lower := strings.ToLower(name)
	lowerPat := strings.ToLower(pattern)
	idx := strings.Index(lower, lowerPat)
	if idx < 0 {
		return name
	}
	before := name[:idx]
	match := name[idx : idx+len(pattern)]
	after := name[idx+len(pattern):]
	return before + boldStyle.Foreground(lipgloss.Color("86")).Render(match) + after
}

// ─── Commands (tea.Cmd) ───

func checkDependenciesCmd(ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		var items []depItem

		// Docker
		if err := docker.Available(ctx); err != nil {
			items = append(items, depItem{name: "Docker", status: depFailed, message: err.Error()})
		} else {
			items = append(items, depItem{name: "Docker", status: depOK, message: "installed"})
		}

		// Ollama
		if !isCommandAvailable("ollama") {
			items = append(items, depItem{name: "Ollama", status: depFailed, message: "not installed"})
		} else {
			items = append(items, depItem{name: "Ollama", status: depOK, message: "installed"})
		}

		// Ollama models
		if isCommandAvailable("ollama") {
			cfg := config.Default()
			missing := []string{}
			for _, name := range []string{cfg.CodeCollection().EmbedModel, cfg.TextCollection().EmbedModel} {
				if name == "" {
					continue
				}
				if !isOllamaModelAvailable(ctx, name) {
					missing = append(missing, name)
				}
			}
			if len(missing) > 0 {
				items = append(items, depItem{name: "Ollama models", status: depFailed, message: "missing: " + strings.Join(missing, ", ")})
			} else {
				items = append(items, depItem{name: "Ollama models", status: depOK, message: "all present"})
			}
		} else {
			items = append(items, depItem{name: "Ollama models", status: depSkipped, message: "ollama not installed"})
		}

		// LSP servers
		missing := []string{}
		for _, spec := range lsp.DefaultServers() {
			if _, err := exec.LookPath(spec.Command); err != nil {
				missing = append(missing, spec.Command)
			}
		}
		if len(missing) > 0 {
			items = append(items, depItem{name: "LSP servers", status: depFailed, message: "missing: " + strings.Join(missing, ", ")})
		} else {
			items = append(items, depItem{name: "LSP servers", status: depOK, message: "all installed"})
		}

		return depsCheckedMsg{items: items}
	}
}

type depsCheckedMsg struct{ items []depItem }

func installDepCmd(ctx context.Context, idx int) tea.Cmd {
	return func() tea.Msg {
		// Install all missing dependencies
		var errs []string

		// Docker
		if err := docker.Available(ctx); err != nil {
			if err := installDocker(ctx); err != nil {
				errs = append(errs, "docker: "+err.Error())
			}
		}

		// Ollama
		if !isCommandAvailable("ollama") {
			if err := installOllama(ctx); err != nil {
				errs = append(errs, "ollama: "+err.Error())
			}
		}

		// Ollama models
		if isCommandAvailable("ollama") {
			cfg := config.Default()
			for _, name := range []string{cfg.CodeCollection().EmbedModel, cfg.TextCollection().EmbedModel} {
				if name == "" {
					continue
				}
				if !isOllamaModelAvailable(ctx, name) {
					_ = pullOllamaModel(ctx, name)
				}
			}
		}

		// LSP
		for _, spec := range lsp.DefaultServers() {
			if _, err := exec.LookPath(spec.Command); err != nil {
				_ = installLSP(ctx, spec)
			}
		}

		if len(errs) > 0 {
			return depInstallMsg{errs: errs}
		}
		return depInstallMsg{done: true}
	}
}

type depInstallMsg struct {
	done bool
	errs []string
}

func checkConfigCmd() tea.Cmd {
	return func() tea.Msg {
		path := config.HomeConfigPath()
		exists := false
		if _, err := os.Stat(path); err == nil {
			exists = true
		}
		return configCheckedMsg{exists: exists, path: path}
	}
}

type configCheckedMsg struct {
	exists bool
	path   string
}

func applyAgentMCPCmd(ctx context.Context, agentPath string) tea.Cmd {
	return func() tea.Msg {
		data, err := os.ReadFile(agentPath)
		if err != nil {
			return agentMCPMsg{err: err}
		}

		var agentCfg map[string]any
		if err := json.Unmarshal(data, &agentCfg); err != nil {
			agentCfg = make(map[string]any)
		}

		cfg := config.Default()
		mcpServers := map[string]any{
			"ragota-code": map[string]any{
				"url":   fmt.Sprintf("http://127.0.0.1:%d/sse", cfg.MCPPort),
				"trust": false,
			},
		}

		if existing, ok := agentCfg["mcpServers"]; ok {
			if existingMap, ok := existing.(map[string]any); ok {
				if _, exists := existingMap["ragota-code"]; exists {
					return agentMCPMsg{alreadyExists: true, path: agentPath}
				}
				existingMap["ragota-code"] = mcpServers["ragota-code"]
			} else {
				agentCfg["mcpServers"] = mcpServers
			}
		} else {
			agentCfg["mcpServers"] = mcpServers
		}

		out, err := json.MarshalIndent(agentCfg, "", "  ")
		if err != nil {
			return agentMCPMsg{err: err}
		}
		if err := os.WriteFile(agentPath, append(out, '\n'), 0o644); err != nil {
			return agentMCPMsg{err: err}
		}

		return agentMCPMsg{path: agentPath}
	}
}

type agentMCPMsg struct {
	path          string
	err           error
	alreadyExists bool
}

// ─── cobra command ───

func newConfigureCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "configure",
		Short: "Interactive setup: dependencies, config, and agent MCP integration",
		RunE: func(cmd *cobra.Command, args []string) error {
			m := initialWizModel()
			p := tea.NewProgram(m, tea.WithAltScreen())
			finalModel, err := p.Run()
			if err != nil {
				return err
			}
			wm := finalModel.(wizModel)

			// Post-TUI: если конфиг не существовал — создаём
			if wm.step >= stepRagotaConfig && !wm.configExists {
				path := config.HomeConfigPath()
				if _, err := config.WriteDefault(path, true); err != nil {
					return fmt.Errorf("write config: %w", err)
				}
				fmt.Printf("✓ Config written: %s\n", path)
			}

			return nil
		},
	}
}

// ─── Утилиты ───

func isCommandAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func isOllamaModelAvailable(ctx context.Context, modelName string) bool {
	out, err := exec.CommandContext(ctx, "ollama", "list").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), modelName)
}

func pullOllamaModel(ctx context.Context, modelName string) error {
	cmd := exec.CommandContext(ctx, "ollama", "pull", modelName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func installDocker(ctx context.Context) error {
	switch runtime.GOOS {
	case "darwin":
		if isCommandAvailable("brew") {
			return runCmd(ctx, "brew", "install", "--cask", "docker")
		}
		return fmt.Errorf("homebrew not found, please install Docker Desktop manually")
	case "linux":
		return runCmd(ctx, "sh", "-c", "curl -fsSL https://get.docker.com | sh")
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

func installOllama(ctx context.Context) error {
	switch runtime.GOOS {
	case "darwin":
		if isCommandAvailable("brew") {
			return runCmd(ctx, "brew", "install", "--cask", "ollama")
		}
		return fmt.Errorf("homebrew not found, please install Ollama manually")
	case "linux":
		return runCmd(ctx, "sh", "-c", "curl -fsSL https://ollama.com/install.sh | sh")
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

func installLSP(ctx context.Context, spec lsp.ServerSpec) error {
	switch spec.Command {
	case "gopls":
		return runCmd(ctx, "go", "install", "golang.org/x/tools/gopls@latest")
	case "typescript-language-server":
		return runCmd(ctx, "npm", "install", "-g", "typescript-language-server", "typescript")
	case "pyright-langserver":
		return runCmd(ctx, "npm", "install", "-g", "pyright")
	case "jdtls":
		if runtime.GOOS == "darwin" && isCommandAvailable("brew") {
			return runCmd(ctx, "brew", "install", "jdtls")
		}
		return fmt.Errorf("jdtls installation not supported on this OS")
	default:
		return fmt.Errorf("unknown LSP server: %s", spec.Command)
	}
}

func runCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
