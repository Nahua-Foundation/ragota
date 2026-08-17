package tui

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/status"
	"github.com/charmbracelet/lipgloss"
)

var testNow = time.Date(2026, 8, 17, 14, 3, 20, 0, time.UTC)

// sampleFrame is a run with one repository being indexed, one that finished,
// one that failed, one queued, one registered but never touched, and one that
// finished with nothing to do.
func sampleFrame() frame {
	return frame{
		snap: status.Snapshot{
			StartedAt: testNow.Add(-2*time.Minute - 14*time.Second),
			Metrics: status.Metrics{
				Repos: 6, Passes: 4, FilesIndexed: 4312,
				WatchBatches: 5, WatchChanged: 18, WatchDeleted: 2,
				Warnings: 2, Errors: 1,
			},
			Repos: []status.RepoState{
				{ID: "a", Name: "alpha", Path: "/home/dev/projects/alpha", Phase: status.PhaseIndexing,
					Files: 412, Total: 1000, Watched: true, StartedAt: testNow.Add(-20 * time.Second)},
				{ID: "b", Name: "beta", Path: "/home/dev/projects/beta", Phase: status.PhaseIdle,
					Indexed: 1204, Watched: true, StartedAt: testNow.Add(-5 * time.Minute), FinishedAt: testNow.Add(-2 * time.Minute)},
				{ID: "c", Name: "gamma", Path: "/home/dev/projects/gamma", Phase: status.PhaseError,
					Failed: 3, Err: "walk: permission denied", StartedAt: testNow.Add(-9 * time.Minute), FinishedAt: testNow.Add(-8 * time.Minute)},
				{ID: "d", Name: "delta", Path: "/home/dev/projects/delta", Phase: status.PhaseQueued},
				{ID: "e", Name: "epsilon", Path: "/srv/other/epsilon", Phase: status.PhaseIdle},
				{ID: "f", Name: "zeta", Path: "/home/dev/projects/zeta", Phase: status.PhaseIdle,
					StartedAt: testNow.Add(-30 * time.Minute), FinishedAt: testNow.Add(-29 * time.Minute)},
			},
			Log: []status.Entry{
				{At: testNow.Add(-90 * time.Second), Level: slog.LevelInfo, RepoID: "b", Msg: "indexing beta"},
				{At: testNow.Add(-80 * time.Second), Level: slog.LevelWarn, RepoID: "c", Msg: "cannot read file: permission denied"},
				{At: testNow.Add(-70 * time.Second), Level: slog.LevelError, RepoID: "c", Msg: "index failed: walk: permission denied"},
				{At: testNow.Add(-20 * time.Second), Level: slog.LevelInfo, RepoID: "a", Msg: "indexing alpha"},
			},
		},
		width: 100, height: 24,
		now: testNow, home: "/home/dev",
		source: "/home/dev/projects", addr: "127.0.0.1:8080",
		logPath: "/tmp/ragota-core.log", watch: true,
	}
}

// The frame has to answer, without scrolling and without colour: which
// repositories are known and what each is doing, how far the running pass has
// got, whether the watcher is live, what went wrong, and the running totals.
func TestRenderAnswersTheQuestions(t *testing.T) {
	out := render(sampleFrame())

	for _, want := range []string{
		"alpha", "beta", "gamma", "delta", "epsilon", "zeta", // every repository is on screen
		"indexing", "idle", "error", "queued", // ...with its phase
		"412/1000",                // how far the running pass has got
		"[",                       // ...as a bar that survives a colourless terminal
		"1204 indexed",            // how the last finished pass ended
		"walk: permission denied", // what went wrong
		"watch 2/6",               // whether the watcher is live, and over how much
		"6 repos", "4 passes",     // running totals
		"4312 indexed",          //
		"1 error", "2 warnings", //
		"5 batches, 18 changed, 2 deleted",
		"up 2m14s",
		"cannot read file: permission denied", // the recent warnings and errors
		"index failed: walk: permission denied",
		"q quit", "/tmp/ragota-core.log", // how to leave, and where the log went
	} {
		if !strings.Contains(out, want) {
			t.Errorf("frame does not mention %q:\n%s", want, out)
		}
	}
}

// Files/Total describe the running pass and Indexed/Failed the last finished
// one. Conflating them made "0 files" mean both "not started" and "finished
// with nothing to do", so the frame has to keep them apart too.
func TestRenderSeparatesRunningFromFinished(t *testing.T) {
	f := sampleFrame()
	f.width, f.height = 100, 40

	rows := map[string]string{}
	for _, line := range strings.Split(render(f), "\n") {
		if name := strings.Fields(strings.TrimPrefix(line, "w ")); len(name) > 0 {
			rows[name[0]] = line
		}
	}

	if got := rows["zeta"]; !strings.Contains(got, "0 indexed") {
		t.Errorf("a pass that finished with nothing to do renders as %q, want a zero count", got)
	}
	if got := rows["epsilon"]; strings.Contains(got, "indexed") {
		t.Errorf("a repository this process never indexed renders as %q, want no pass result", got)
	}
	if got := rows["alpha"]; !strings.Contains(got, "412/1000") || strings.Contains(got, "indexed") {
		t.Errorf("a running pass renders as %q, want its progress and not a pass result", got)
	}
	if got := rows["delta"]; !strings.Contains(got, "waiting") {
		t.Errorf("a queued repository renders as %q, want it marked as waiting", got)
	}
}

// The alt screen has no scrollback: a line wider than the terminal wraps and
// every frame after it is drawn over the wrong rows.
func TestRenderFitsTheTerminal(t *testing.T) {
	base := sampleFrame()
	base.snap.Repos = append(base.snap.Repos, status.RepoState{
		ID:    "long",
		Name:  strings.Repeat("very-long-repository-name-", 8),
		Path:  "/home/dev/" + strings.Repeat("deeply/nested/", 30) + "repo",
		Phase: status.PhaseIndexing,
		Files: 1, Total: 999999,
	})
	base.snap.Log = append(base.snap.Log, status.Entry{
		At: testNow, Level: slog.LevelError, RepoID: "long",
		Msg: strings.Repeat("an error that will not stop talking. ", 20),
	})
	base.source = "/home/dev/" + strings.Repeat("nested/", 40)
	base.logPath = "/tmp/" + strings.Repeat("x", 300) + ".log"
	base.snap.Dormant = 118 // the line that accounts for them is bound too

	sizes := []struct{ w, h int }{
		{200, 60}, {120, 40}, {100, 24}, {80, 24}, {60, 20}, {40, 12}, {30, 10}, {20, 8}, {12, 6}, {8, 4}, {4, 3},
	}
	for _, sz := range sizes {
		f := base
		f.width, f.height = sz.w, sz.h
		for _, problems := range []bool{false, true} {
			f.problems = problems
			lines := strings.Split(render(f), "\n")
			if len(lines) > sz.h {
				t.Errorf("%dx%d: rendered %d lines, want at most %d", sz.w, sz.h, len(lines), sz.h)
			}
			for i, line := range lines {
				if w := lipgloss.Width(line); w > sz.w {
					t.Errorf("%dx%d: line %d is %d columns wide:\n%q", sz.w, sz.h, i, w, line)
				}
			}
		}
	}
}

// A message with a newline, a tab or an escape sequence in it — an error from
// a subprocess, a manifest parse failure — would otherwise draw outside its
// cell and corrupt the rest of the frame.
func TestRenderSurvivesControlCharacters(t *testing.T) {
	f := sampleFrame()
	f.snap.Log = []status.Entry{{
		At: testNow, Level: slog.LevelError, RepoID: "c",
		Msg: "yaml:\n  line 3:\tdid not find\x1b[31m expected key\r\n",
	}}
	f.snap.Repos[2].Err = "boom\nand\ta second line"

	out := render(f)
	if strings.ContainsAny(strings.ReplaceAll(out, "\n", ""), "\r\t\x1b") {
		t.Errorf("frame carries control characters through:\n%q", out)
	}
	if !strings.Contains(out, "yaml: line 3: did not find") {
		t.Errorf("the message did not survive sanitizing:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if lipgloss.Width(line) > f.width {
			t.Errorf("line wider than the frame after sanitizing: %q", line)
		}
	}
}

// A terminal that shows eight of forty rows has to show the pass in flight.
func TestRenderKeepsTheActiveRepositoriesOnScreen(t *testing.T) {
	f := sampleFrame()
	f.height = 14
	for i := 0; i < 40; i++ {
		f.snap.Repos = append(f.snap.Repos, status.RepoState{
			ID: string(rune('A' + i%26)), Name: "idle-repo", Phase: status.PhaseIdle,
		})
	}
	out := render(f)

	if !strings.Contains(out, "412/1000") {
		t.Errorf("the repository being indexed fell off the table:\n%s", out)
	}
	if !strings.Contains(out, "walk: permission denied") {
		t.Errorf("the failed repository fell off the table:\n%s", out)
	}
	if !strings.Contains(out, "more:") {
		t.Errorf("the rows that did not fit are not accounted for:\n%s", out)
	}
}

// The table is the working set, and the repositories the run is not about are
// one line saying how many there are. A row each would say they are waiting
// their turn, which is exactly what they are not; no line at all would hide
// half an index behind a table that looks complete.
func TestRenderCountsTheRepositoriesOutsideTheRun(t *testing.T) {
	f := sampleFrame()
	f.snap.Dormant = 17
	out := render(f)

	if !strings.Contains(out, "17 dormant") {
		t.Errorf("the repositories outside the run are not accounted for:\n%s", out)
	}
	// The line is a count and not a table: the six rows are still the six
	// repositories the run is about.
	rows := 0
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimPrefix(line, "w "))
		if len(fields) > 0 && slices.Contains([]string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta"}, fields[0]) {
			rows++
		}
	}
	if rows != 6 {
		t.Errorf("%d repository rows, want the 6 in the working set:\n%s", rows, out)
	}

	// With none of them, the line is not there at all — an installation that
	// never narrowed anything has nothing to be told.
	f.snap.Dormant = 0
	if out := render(f); strings.Contains(out, "dormant") {
		t.Errorf("a run with nothing dormant still mentions it:\n%s", out)
	}

	// A working set that is empty while the index is not: "no repositories
	// registered" would send the user off to register the ones they have.
	f.snap.Repos, f.snap.Dormant = nil, 3
	out = render(f)
	if !strings.Contains(out, "no repositories in this run") || !strings.Contains(out, "3 dormant") {
		t.Errorf("an empty working set over a full index renders as:\n%s", out)
	}
	if strings.Contains(out, "no repositories registered") {
		t.Errorf("an index of 3 repositories renders as empty:\n%s", out)
	}
}

// The 'w' toggle exists so that a busy run can be read at all: the ring holds
// lifecycle entries and the teed warnings and errors in one sequence.
func TestRenderFiltersToProblems(t *testing.T) {
	f := sampleFrame()
	f.problems = true
	out := render(f)

	if strings.Contains(out, "indexing beta") {
		t.Errorf("an info entry survived the warnings-only filter:\n%s", out)
	}
	if !strings.Contains(out, "cannot read file: permission denied") {
		t.Errorf("a warning is missing from the warnings-only view:\n%s", out)
	}
	if !strings.Contains(out, "index failed: walk: permission denied") {
		t.Errorf("an error is missing from the warnings-only view:\n%s", out)
	}

	f.snap.Log = f.snap.Log[:1] // one info entry and nothing else
	if out := render(f); !strings.Contains(out, "none") {
		t.Errorf("an empty warnings-only view says nothing at all:\n%s", out)
	}
}

// The zero snapshot is what a dashboard started before anything has published
// renders — including one attached to a nil bus, whose Snapshot is the zero
// value by contract.
func TestRenderEmptySnapshot(t *testing.T) {
	f := frame{snap: (*status.Bus)(nil).Snapshot(), width: 80, height: 24, now: testNow}
	out := render(f)

	if !strings.Contains(out, "no repositories registered") {
		t.Errorf("an empty surface does not say so:\n%s", out)
	}
	if !strings.Contains(out, "nothing yet") {
		t.Errorf("an empty log does not say so:\n%s", out)
	}
	if strings.Contains(out, "up ") {
		t.Errorf("a zero start time was rendered as an uptime:\n%s", out)
	}
	// A terminal that reports no size at all never sends one later either, so
	// the frame before the first size has to say something.
	if got := render(frame{snap: status.Snapshot{}, width: 0, height: 24}); !strings.Contains(got, "ragota-core") {
		t.Errorf("render before the first window size = %q, want a line naming the process", got)
	}
}

func TestShortenHome(t *testing.T) {
	f := frame{home: "/home/dev"}
	tests := []struct{ in, want string }{
		{"/home/dev/projects/alpha", "~/projects/alpha"},
		{"/home/dev", "~"},
		{"/home/developer/x", "/home/developer/x"}, // a prefix match is not a parent
		{"/srv/other", "/srv/other"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := f.short(tt.in); got != tt.want {
			t.Errorf("short(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	none := frame{home: ""}
	if got := none.short("/home/dev/x"); got != "/home/dev/x" {
		t.Errorf("short() with no home = %q, want the path unchanged", got)
	}
}

func TestTruncation(t *testing.T) {
	if got := truncEnd("abcdefgh", 4); got != "abc…" {
		t.Errorf("truncEnd() = %q, want %q", got, "abc…")
	}
	if got := truncEnd("abc", 4); got != "abc" {
		t.Errorf("truncEnd() shortened a string that fits: %q", got)
	}
	if got := truncStart("/a/b/c/repo", 6); got != "…/repo" {
		t.Errorf("truncStart() = %q, want %q", got, "…/repo")
	}
	// Wide runes are measured in columns, not in runes, or the frame is one
	// column too wide on every CJK path.
	if got := lipgloss.Width(truncEnd("日本語のリポジトリ", 7)); got > 7 {
		t.Errorf("truncEnd() of wide runes is %d columns, want at most 7", got)
	}
	if got := lipgloss.Width(truncStart("日本語のリポジトリ", 7)); got > 7 {
		t.Errorf("truncStart() of wide runes is %d columns, want at most 7", got)
	}
	for _, w := range []int{0, 1, 2} {
		if got := lipgloss.Width(truncEnd("abcdef", w)); got > w {
			t.Errorf("truncEnd(_, %d) is %d columns wide", w, got)
		}
		if got := lipgloss.Width(truncStart("abcdef", w)); got > w {
			t.Errorf("truncStart(_, %d) is %d columns wide", w, got)
		}
	}
}

func TestBudgetSplitsTheFrame(t *testing.T) {
	tests := []struct {
		name                     string
		avail, repoWant, logWant int
		wantRepo, wantLog        int
	}{
		{"both fit", 20, 5, 6, 5, 6},
		{"the log pane is held to half", 10, 20, 20, 5, 5},
		{"a small table hands the rest to the log", 10, 3, 20, 3, 7},
		{"a small log hands the rest to the table", 10, 20, 2, 8, 2},
		{"nothing to give", 0, 5, 5, 0, 0},
		{"negative", -3, 5, 5, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, log := budget(tt.avail, tt.repoWant, tt.logWant)
			if repo != tt.wantRepo || log != tt.wantLog {
				t.Errorf("budget(%d, %d, %d) = %d, %d; want %d, %d",
					tt.avail, tt.repoWant, tt.logWant, repo, log, tt.wantRepo, tt.wantLog)
			}
			if tt.avail > 0 && repo+log > tt.avail {
				t.Errorf("budget(%d, ...) handed out %d lines", tt.avail, repo+log)
			}
		})
	}
}
