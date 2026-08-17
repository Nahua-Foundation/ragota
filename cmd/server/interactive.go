package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/service"
	"github.com/Nahua-Foundation/ragota/internal/status"
)

// interactiveLogName is the file the process log goes to while the dashboard
// owns the terminal. The name is fixed rather than per-process so that
// `tail -f` in another pane needs no argument hunting, and it is opened for
// append so that starting a second run does not truncate the log the first one
// is still writing.
const interactiveLogName = "ragota-core.log"

// logSink routes the process log at runtime. It starts on stderr, moves to a
// file for as long as the dashboard owns the terminal, and returns to stderr —
// still writing the file — once the dashboard has handed the terminal back.
//
// The moving part is the point. Log records and a full-screen renderer on one
// terminal interleave into garbage, but a shutdown can take as long as the
// index pass in flight, and finishing that behind a silent blank screen is
// worse than not showing it at all.
type logSink struct {
	mu   sync.Mutex
	w    io.Writer
	file *os.File // nil when no log file could be opened
}

func newLogSink() *logSink { return &logSink{w: os.Stderr} }

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// toFile sends everything to f, and to nothing at all when f is nil: with no
// file to hold them, the records below warn are dropped rather than smeared
// over the dashboard. The warnings and errors are on screen either way — the
// bus is fed by a slog handler that tees them, not by this writer.
//
// It takes an *os.File rather than an io.Writer so that "no file" is a nil
// argument and not an interface holding a nil pointer, which is not nil and
// would panic on the first record.
func (s *logSink) toFile(f *os.File) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.file = f
	if f == nil {
		s.w = io.Discard
		return
	}
	s.w = f
}

// toTerminal is called once the dashboard has given the terminal back.
func (s *logSink) toTerminal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		s.w = io.MultiWriter(s.file, os.Stderr)
		return
	}
	s.w = os.Stderr
}

// interactiveLogPath is where that file lives: beside the data the built-in
// local profile writes, not in the temporary directory. The path is then stable
// and the user's own — a shared /tmp is somewhere any other account can plant a
// symlink under a name this predictable. Only a process with no home directory
// to resolve falls back to the temporary one.
func interactiveLogPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), interactiveLogName)
	}
	return filepath.Join(home, ".ragota-core", "logs", interactiveLogName)
}

// openInteractiveLog opens the file the log is parked in while the dashboard
// runs. A failure is not fatal and not even reported here: the caller falls
// back to discarding, and a process that cannot write this file has worse
// problems than a missing log.
func openInteractiveLog() (*os.File, string) {
	path := interactiveLogPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, ""
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, ""
	}
	// A run marker, so that an appended file reads as a sequence of sessions
	// rather than as one log with jumps in it.
	_, _ = fmt.Fprintf(f, "=== ragota-core %s started %s ===\n", version, time.Now().Format(time.RFC3339))
	return f, path
}

// primeStatusBus tells the surface which repositories the run is about.
//
// --source does this as it registers them, but --interactive on its own has
// nothing that would: the table would stay empty until something happened, and
// an index pass started from the API would then appear under a repository id
// instead of a name, since the bus names a repository it has not been told
// about after its id. Re-registering is explicitly harmless — it refreshes the
// name and path without touching the phase, so a pass in flight is undisturbed.
//
// Only the working set is announced. A dormant repository is one this run will
// never touch, and a row for it would sit in the table showing a dash where a
// progress bar goes — which reads as "queued", the exact confusion the working
// set exists to remove. It is not hidden either: the count goes to the bus, and
// the renderer spends a line on it. Filtering here rather than in the renderer
// keeps one answer to "what is on screen" — a snapshot carrying rows nobody
// draws is a snapshot that grows a second, contradictory filter later.
//
// One ListRepos call gives both halves. Asking for the active rows and the
// total separately would let a write between the two calls report a count that
// contradicts the table.
func primeStatusBus(ctx context.Context, svc *service.Service, bus *status.Bus) {
	all, err := svc.ListRepos(ctx)
	if err != nil {
		slog.Warn("interactive: cannot list repositories", "err", err)
		return
	}
	dormant := 0
	for _, r := range all {
		if !r.Active {
			dormant++
			continue
		}
		bus.RepoRegistered(r.ID, r.Name, r.Path)
	}
	bus.ReposDormant(dormant)
}
