// Package tui draws the status bus as a full-screen terminal dashboard, which
// is what --interactive turns on.
//
// It reads internal/server/progress and nothing else: what a repository is doing, how
// far the running pass has got, whether the watcher is live, the recent
// warnings and errors, and the running totals all arrive as snapshots, and no
// part of the indexer knows this package exists.
//
// The split is between Run, which owns a terminal, and render, which owns a
// string. render is a pure function of a frame value — a snapshot plus a size
// plus the process facts the bus does not carry — so the layout is asserted in
// tests without a terminal, a bus or a bubbletea program anywhere near it.
package tui

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/Nahua-Foundation/ragota/internal/server/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"
)

// ErrNoTerminal is returned by Run when the output is not a terminal — a pipe,
// a redirect to a file, a CI job. The caller is expected to carry on without a
// dashboard rather than to fail: a full-screen renderer writing escape
// sequences into a log file helps nobody.
var ErrNoTerminal = errors.New("output is not a terminal")

// Options are the process facts the status bus does not carry, plus the
// terminal to draw on.
type Options struct {
	// Out is the terminal the dashboard draws on; nil means os.Stdout.
	Out *os.File
	// Source is the --source directory, empty when there was none.
	Source string
	// Addr is the host:port the HTTP API listens on.
	Addr string
	// LogPath is the file the process log is being written to while the
	// dashboard owns the terminal; empty means it is being discarded.
	LogPath string
	// Watch says whether --watch was asked for, which is not the same as a
	// watcher having started: the frame shows how many repositories are
	// actually being followed.
	Watch bool
}

// Run draws the dashboard until the user quits it or ctx is cancelled, and
// returns nil in both cases — neither is a failure. On a non-terminal output it
// returns ErrNoTerminal without writing anything at all.
//
// Quitting is a return, not an exit: the caller's shutdown continues where it
// left off, which is the whole reason this does not call os.Exit. By the time
// it returns the terminal has been given back, so the caller may print again.
func Run(ctx context.Context, bus *progress.Bus, opts Options) error {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	if !IsTerminal(out) {
		return ErrNoTerminal
	}

	// Cancelling the subscription closes the channel, which releases the
	// command blocked on it rather than leaving a goroutine parked for the
	// life of the process.
	events, cancel := bus.Subscribe()
	defer cancel()

	p := tea.NewProgram(newModel(bus, events, opts),
		tea.WithContext(ctx),
		tea.WithOutput(out),
		tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		// A cancelled context and a SIGINT are how this program is asked to
		// stop; only a real rendering or terminal failure is worth reporting.
		if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, tea.ErrInterrupted) {
			return nil
		}
		return fmt.Errorf("dashboard: %w", err)
	}
	return nil
}

// IsTerminal reports whether f is an interactive terminal, and is the check
// that decides whether a dashboard may start at all. The caller needs it before
// Run, because where the process log goes depends on the answer.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	fd := f.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}
