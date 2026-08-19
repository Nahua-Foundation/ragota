package tui

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/server/progress"
)

// A piped or redirected stdout — a shell pipeline, a nohup, a CI job — must not
// get a full-screen UI. Run has to say so and, above all, write nothing: escape
// sequences in a log file are worse than no dashboard.
func TestRunRefusesANonTerminal(t *testing.T) {
	bus := progress.NewBus(8)
	bus.RepoRegistered("r1", "alpha", "/src/alpha")

	t.Run("a pipe", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe() error = %v", err)
		}
		defer func() { _ = r.Close() }()

		if err := Run(context.Background(), bus, Options{Out: w}); !errors.Is(err, ErrNoTerminal) {
			t.Fatalf("Run() error = %v, want ErrNoTerminal", err)
		}
		_ = w.Close()
		written, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("read pipe: %v", err)
		}
		if len(written) > 0 {
			t.Errorf("Run() wrote %q to a pipe, want nothing", written)
		}
	})

	t.Run("a redirect to a file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "out.log")
		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		defer func() { _ = f.Close() }()

		if err := Run(context.Background(), bus, Options{Out: f}); !errors.Is(err, ErrNoTerminal) {
			t.Fatalf("Run() error = %v, want ErrNoTerminal", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if info.Size() != 0 {
			t.Errorf("Run() wrote %d bytes to a redirected stdout, want 0", info.Size())
		}
	})
}

// IsTerminal is what the caller asks before it decides where the process log
// goes, so it has to answer for the same cases without a Run in between.
func TestIsTerminal(t *testing.T) {
	if IsTerminal(nil) {
		t.Error("IsTerminal(nil) = true")
	}
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer func() { _ = f.Close() }()
	if IsTerminal(f) {
		t.Errorf("IsTerminal(%s) = true", os.DevNull)
	}
}

// A dashboard on a nil bus is not a crash: Subscribe hands back a nil channel
// that blocks instead of spinning, and Snapshot the zero value.
func TestModelWithoutABus(t *testing.T) {
	var bus *progress.Bus
	events, cancel := bus.Subscribe()
	defer cancel()

	m := newModel(bus, events, Options{Addr: "127.0.0.1:8080"})
	m.width, m.height = 80, 24
	out := m.View()
	if !strings.Contains(out, "no repositories registered") || !strings.Contains(out, "127.0.0.1:8080") {
		t.Errorf("View() with no bus = %q, want the empty frame", out)
	}
}
