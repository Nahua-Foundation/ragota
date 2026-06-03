package tui

// Файл содержит bubbletea-модель TUI: структуру model, сообщения tickMsg/
// snapMsg, методы жизненного цикла Init/Update, фабрики команд refreshSnap
// и tick, а также appendLogs — зеркалирование Recent-записей в файл логов.

import (
	"fmt"
	"os"
	"strings"
	"time"

	"ragota/pkg/state"

	tea "github.com/charmbracelet/bubbletea"
)

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
		m.bus.RecordTick()
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
