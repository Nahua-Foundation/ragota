package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/status"
)

// The rule the dashboard depends on: while it owns the terminal nothing may be
// written there, and once it has handed the terminal back everything is written
// there again — a shutdown that takes as long as the index pass in flight is
// not something to run behind a blank screen.
func TestLogSinkKeepsTheTerminalClear(t *testing.T) {
	onTerminal := captureStderr(t)

	path := filepath.Join(t.TempDir(), "log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = file.Close() }()

	sink := newLogSink()
	log := slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelInfo}))

	log.Info("before the dashboard")
	sink.toFile(file)
	log.Info("while the dashboard is up")
	sink.toTerminal()
	log.Info("after the dashboard")

	stderr := onTerminal()
	if !strings.Contains(stderr, "before the dashboard") {
		t.Errorf("stderr lost the ordinary log line: %q", stderr)
	}
	if strings.Contains(stderr, "while the dashboard is up") {
		t.Errorf("a log record reached the terminal the dashboard owns: %q", stderr)
	}
	if !strings.Contains(stderr, "after the dashboard") {
		t.Errorf("stderr did not come back after the dashboard: %q", stderr)
	}

	inFile, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range []string{"while the dashboard is up", "after the dashboard"} {
		if !strings.Contains(string(inFile), want) {
			t.Errorf("the log file is missing %q: %q", want, inFile)
		}
	}
}

// With nowhere to park the log it is dropped rather than smeared over the
// dashboard. Warnings and errors are on screen either way: they reach the bus
// through the slog tee, which sits above this writer.
func TestLogSinkWithoutAFileDiscards(t *testing.T) {
	onTerminal := captureStderr(t)

	sink := newLogSink()
	bus := status.NewBus(8)
	log := slog.New(status.NewLogHandler(
		slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelInfo}), bus, slog.LevelWarn))

	sink.toFile(nil)
	log.Info("routine progress")
	log.Warn("something to show")

	if stderr := onTerminal(); stderr != "" {
		t.Errorf("stderr = %q, want nothing while the dashboard owns the terminal", stderr)
	}
	snap := bus.Snapshot()
	if len(snap.Log) != 1 || snap.Log[0].Msg != "something to show" {
		t.Errorf("bus log = %+v, want the warning and nothing else", snap.Log)
	}
}

// The log file is appended to, not truncated: a second run must not wipe the
// log a first one is still writing, and each session says where it begins.
func TestOpenInteractiveLogAppends(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	first, path := openInteractiveLog()
	if first == nil {
		t.Fatal("openInteractiveLog() opened nothing")
	}
	if _, err := first.WriteString("kept\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = first.Close()

	second, path2 := openInteractiveLog()
	if second == nil {
		t.Fatal("openInteractiveLog() opened nothing the second time")
	}
	_ = second.Close()

	if want := filepath.Join(dir, ".ragota-core", "logs", interactiveLogName); path != want || path2 != want {
		t.Errorf("log paths = %q and %q, want one stable %q", path, path2, want)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n := strings.Count(string(body), "started"); n != 2 {
		t.Errorf("session markers = %d, want 2 (the second run appended): %q", n, body)
	}
	if !strings.Contains(string(body), "kept") {
		t.Errorf("the first run's output was truncated: %q", body)
	}
}

// newHandler takes its destination now, because --interactive moves the log off
// the terminal. The format and level decisions must not have moved with it.
func TestNewHandlerHonoursFormatAndLevel(t *testing.T) {
	var buf strings.Builder
	slog.New(newHandler(&config.LogConfig{Format: "json"}, &buf)).Info("hello")
	if !strings.HasPrefix(buf.String(), "{") {
		t.Errorf("json format produced %q", buf.String())
	}

	buf.Reset()
	slog.New(newHandler(&config.LogConfig{Level: "warn"}, &buf)).Info("hello")
	if buf.String() != "" {
		t.Errorf("an info record passed a warn level: %q", buf.String())
	}
}

// captureStderr redirects os.Stderr to a pipe and returns a function yielding
// everything written to it. The redirect is in place before the sink is built,
// because the sink resolves os.Stderr as it goes.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	old := os.Stderr
	os.Stderr = w

	read := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		read <- string(b)
	}()

	var (
		once sync.Once
		out  string
	)
	drain := func() string {
		once.Do(func() {
			os.Stderr = old
			_ = w.Close()
			out = <-read
			_ = r.Close()
		})
		return out
	}
	t.Cleanup(func() { drain() })
	return drain
}
