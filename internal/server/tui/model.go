package tui

import (
	"os"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/server/progress"
	tea "github.com/charmbracelet/bubbletea"
)

// clockInterval is how often the frame is redrawn with nothing new to show. It
// exists for the elapsed times — uptime, "2m ago" — which no publisher can
// announce a change to.
const clockInterval = time.Second

type (
	// changedMsg says the bus published something.
	changedMsg struct{}
	// snapMsg carries the copy of the surface that the next frame renders.
	snapMsg progress.Snapshot
	// clockMsg is the periodic redraw.
	clockMsg time.Time
)

// model is the dashboard state. Everything that decides what is on screen is a
// field here and render is a pure function of them, so the interesting
// assertions are about frames rather than about bubbletea.
type model struct {
	bus    *progress.Bus
	events <-chan struct{}
	opts   Options
	home   string

	snap     progress.Snapshot
	width    int
	height   int
	now      time.Time
	problems bool
	theme    theme
}

// newModel reads a first snapshot rather than waiting for one, so that the
// frame drawn before the first wakeup is the surface as it stands and not an
// empty one.
func newModel(bus *progress.Bus, events <-chan struct{}, opts Options) model {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return model{
		bus:    bus,
		events: events,
		opts:   opts,
		home:   home,
		snap:   bus.Snapshot(),
		now:    time.Now(),
		theme:  theme{colour: true},
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(readSnapshot(m.bus), waitForChange(m.events), clock())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "w":
			m.problems = !m.problems
			return m, nil
		}
	case changedMsg:
		// Read on every wakeup and rearm, with no throttle in between. The
		// bus coalesces into a one-slot channel, so a publisher cannot wake
		// this loop faster than it redraws: the rate limit is the renderer's,
		// and pacing it here would only add latency to the last change.
		return m, tea.Batch(readSnapshot(m.bus), waitForChange(m.events))
	case snapMsg:
		m.snap = progress.Snapshot(msg)
		return m, nil
	case clockMsg:
		m.now = time.Time(msg)
		return m, clock()
	}
	return m, nil
}

func (m model) View() string { return render(m.frame()) }

// frame is the whole input to a redraw, assembled in one place so that a test
// can build the same value directly.
func (m model) frame() frame {
	return frame{
		snap:     m.snap,
		width:    m.width,
		height:   m.height,
		now:      m.now,
		home:     m.home,
		source:   m.opts.Source,
		addr:     m.opts.Addr,
		logPath:  m.opts.LogPath,
		watch:    m.opts.Watch,
		problems: m.problems,
		theme:    m.theme,
	}
}

func readSnapshot(bus *progress.Bus) tea.Cmd {
	return func() tea.Msg { return snapMsg(bus.Snapshot()) }
}

// waitForChange blocks until the bus publishes. A closed channel means the
// subscription was cancelled, which happens as the program comes down: it
// returns no message then, so the command is not rearmed and the goroutine
// ends instead of spinning on a closed channel.
func waitForChange(events <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		if _, ok := <-events; !ok {
			return nil
		}
		return changedMsg{}
	}
}

func clock() tea.Cmd {
	return tea.Tick(clockInterval, func(t time.Time) tea.Msg { return clockMsg(t) })
}
