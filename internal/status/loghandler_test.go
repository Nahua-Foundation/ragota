package status

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestLogHandlerTeesWarningsAndErrors(t *testing.T) {
	var out bytes.Buffer
	bus := NewBus(16)
	inner := slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(NewLogHandler(inner, bus, slog.LevelWarn))

	logger.Info("routine progress")
	logger.Warn("manifest unreadable", RepoIDKey, "r1")
	logger.Error("index failed", RepoIDKey, "r2")

	// Everything still reaches the handler it wraps: the bus is an addition to
	// the process log, not a replacement for it.
	text := out.String()
	for _, want := range []string{"routine progress", "manifest unreadable", "index failed"} {
		if !strings.Contains(text, want) {
			t.Errorf("the wrapped handler did not receive %q; got:\n%s", want, text)
		}
	}

	log := bus.Snapshot().Log
	if len(log) != 2 {
		t.Fatalf("bus log holds %d entries, want only the warning and the error: %+v", len(log), log)
	}
	if log[0].Msg != "manifest unreadable" || log[0].RepoID != "r1" || log[0].Level != slog.LevelWarn {
		t.Errorf("log[0] = %+v, want the warning attributed to r1", log[0])
	}
	if log[1].Msg != "index failed" || log[1].RepoID != "r2" || log[1].Level != slog.LevelError {
		t.Errorf("log[1] = %+v, want the error attributed to r2", log[1])
	}

	m := bus.Snapshot().Metrics
	if m.Warnings != 1 || m.Errors != 1 {
		t.Errorf("metrics = %+v, want 1 warning and 1 error", m)
	}
}

// A logger derived with slog's With carries repo_id in its attributes rather
// than in each record, which is how the indexing path is written in places.
func TestLogHandlerKeepsRepoIDFromWithAttrs(t *testing.T) {
	bus := NewBus(8)
	inner := slog.NewTextHandler(&bytes.Buffer{}, nil)
	logger := slog.New(NewLogHandler(inner, bus, slog.LevelWarn)).With(RepoIDKey, "r9")

	logger.Warn("something soft failed")

	log := bus.Snapshot().Log
	if len(log) != 1 || log[0].RepoID != "r9" {
		t.Errorf("log = %+v, want one entry attributed to r9", log)
	}
}

// Without a bus there is nothing to wrap, and the caller gets its handler back
// unchanged rather than a layer that forwards to nowhere.
func TestNewLogHandlerWithoutABusReturnsTheInnerHandler(t *testing.T) {
	inner := slog.NewTextHandler(&bytes.Buffer{}, nil)
	if got := NewLogHandler(inner, nil, slog.LevelWarn); got != slog.Handler(inner) {
		t.Errorf("NewLogHandler(nil bus) = %T, want the inner handler itself", got)
	}
}
