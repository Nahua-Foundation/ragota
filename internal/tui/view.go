package tui

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/status"
	"github.com/charmbracelet/lipgloss"
)

// Layout limits. The frame is drawn on the alt screen, which has no scrollback
// to absorb a line that does not fit, so these are hard bounds and not
// preferences.
const (
	nameMin  = 6  // a repository name is never squeezed below this
	nameMax  = 20 // ...nor given more than this, however long the names are
	phaseW   = 8  // "indexing", the longest phase word
	statMax  = 24 // "[##########] 12345/12345"
	pathMin  = 12 // below this a path column says nothing, so it is dropped
	logRepoW = 10 // repository column in the log pane
	logPathW = 40 // the log file's path in the footer
	maxLog   = 12 // log lines to show when the frame height is unknown
)

// frame is everything one redraw needs, and render is a pure function of it.
// That is the point of the split: the whole layout can be asserted from a test
// without a terminal, a bus or a bubbletea program.
type frame struct {
	snap   status.Snapshot
	width  int
	height int // 0 or less means "no vertical bound"
	now    time.Time
	home   string // shortened to ~ in displayed paths; empty disables it

	source   string // --source directory, empty when there was none
	addr     string // host:port the API listens on
	logPath  string // file the process log went to, empty when it is discarded
	watch    bool   // --watch was asked for
	problems bool   // the log pane is filtered to warnings and errors

	theme theme
}

// cell is text plus how to paint it. Every width measurement in this file is
// taken on the text and the style is applied to the finished cell: truncating a
// string that already holds escape sequences cuts one in half, and the terminal
// then eats the rest of the line.
type cell struct {
	text  string
	style lipgloss.Style
}

// render draws the whole dashboard. No line it returns is wider than f.width
// display columns, and when f.height is positive it returns no more than
// f.height lines.
func render(f frame) string {
	// Normally this lasts one frame, until the terminal reports its size. It
	// lasts forever on a terminal that reports 0 columns, which is why it says
	// something rather than drawing an empty screen.
	if f.width <= 0 {
		return "ragota-core: waiting for the terminal size"
	}

	repos := orderRepos(f.snap.Repos)
	entries := f.entries()

	// Two header lines, a footer, and a blank line above each of the two
	// sections. The sections carry their own headers.
	const chrome = 2 + 1 + 2
	repoWant := 1 + max(1, len(repos))
	if f.snap.Dormant > 0 {
		repoWant++ // the line accounting for the repositories not in this run
	}
	logWant := 1 + max(1, len(entries))
	repoRows, logRows := repoWant, min(logWant, 1+maxLog)
	if f.height > 0 {
		// With a height to fill, the log pane takes whatever the table leaves:
		// a tall terminal showing three repositories should show history, not
		// three rows and a screen of nothing.
		repoRows, logRows = budget(f.height-chrome, repoWant, logWant)
	}

	lines := []string{f.title(), f.counters()}
	if body := f.repoSection(repos, repoRows); len(body) > 0 {
		lines = append(lines, "")
		lines = append(lines, body...)
	}
	if body := f.logSection(entries, logRows); len(body) > 0 {
		lines = append(lines, "")
		lines = append(lines, body...)
	}
	lines = append(lines, f.footer())

	// The height bound is enforced here as well as in budget: a section that
	// grew one line past its allowance would scroll the alt screen, and every
	// frame after it would be drawn over the wrong rows.
	if f.height > 0 && len(lines) > f.height {
		lines = lines[:f.height]
	}
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n")
}

// budget splits avail lines between the repository table and the log pane.
// Both get what they ask for when both fit; otherwise the log pane is held to
// half the frame — the table is the part that answers "what is it doing right
// now" — and whatever the smaller pane does not want goes back to the other.
func budget(avail, repoWant, logWant int) (repoRows, logRows int) {
	if avail <= 0 {
		return 0, 0
	}
	if repoWant+logWant <= avail {
		return repoWant, logWant
	}
	logRows = min(avail/2, logWant)
	repoRows = avail - logRows
	if repoRows > repoWant {
		repoRows = repoWant
		logRows = min(avail-repoRows, logWant)
	}
	return repoRows, logRows
}

// title is the "what is this process doing" line.
func (f frame) title() string {
	cells := []cell{{text: "ragota-core", style: headStyle}, f.activity()}
	if !f.snap.StartedAt.IsZero() {
		cells = append(cells, cell{text: "up " + uptime(f.now, f.snap.StartedAt)})
	}
	if f.addr != "" {
		cells = append(cells, cell{text: "api " + f.addr, style: dimStyle})
	}
	return f.theme.fit(f.width, cells)
}

// activity names the phase the process as a whole is in, so that "is anything
// happening?" is answered without reading the table below.
func (f frame) activity() cell {
	var indexing []string
	queued, watched := 0, 0
	for _, r := range f.snap.Repos {
		switch r.Phase {
		case status.PhaseIndexing:
			indexing = append(indexing, r.Name)
		case status.PhaseQueued:
			queued++
		}
		if r.Watched {
			watched++
		}
	}
	switch {
	case len(indexing) == 1:
		return cell{text: "indexing " + indexing[0], style: runStyle}
	case len(indexing) > 1:
		return cell{text: fmt.Sprintf("indexing %s +%d", indexing[0], len(indexing)-1), style: runStyle}
	case queued > 0:
		return cell{text: fmt.Sprintf("%d queued", queued)}
	case watched > 0:
		return cell{text: "watching", style: runStyle}
	default:
		return cell{text: "idle", style: dimStyle}
	}
}

// counters is the running totals line. Errors and warnings come first and are
// left out when there are none: they are the fields that have to survive a
// narrow terminal, and fit drops from the end.
func (f frame) counters() string {
	m := f.snap.Metrics
	var cells []cell
	if m.Errors > 0 {
		cells = append(cells, cell{text: plural(m.Errors, "error", "errors"), style: badStyle})
	}
	if m.Warnings > 0 {
		cells = append(cells, cell{text: plural(m.Warnings, "warning", "warnings"), style: warnStyle})
	}
	cells = append(cells,
		cell{text: plural(m.Repos, "repo", "repos")},
		cell{text: plural(m.Passes, "pass", "passes")},
		cell{text: fmt.Sprintf("%d indexed", m.FilesIndexed)})
	if f.watch {
		cells = append(cells, cell{text: fmt.Sprintf("watch %d/%d", f.watched(), len(f.snap.Repos))})
	}
	if m.WatchBatches > 0 {
		cells = append(cells, cell{text: fmt.Sprintf("%s, %d changed, %d deleted",
			plural(m.WatchBatches, "batch", "batches"), m.WatchChanged, m.WatchDeleted)})
	}
	return f.theme.fit(f.width, cells)
}

func (f frame) watched() int {
	n := 0
	for _, r := range f.snap.Repos {
		if r.Watched {
			n++
		}
	}
	return n
}

// footer holds the keys and the static context — where the repositories came
// from, and where the log went while the dashboard has the terminal.
func (f frame) footer() string {
	toggle := "w warnings only"
	if f.problems {
		toggle = "w all entries"
	}
	logs := "logs off"
	if f.logPath != "" {
		// Shortened here rather than left for fit to drop: a truncated path
		// still names the file to tail, and a missing field says nothing.
		logs = "logs " + truncStart(f.short(f.logPath), logPathW)
	}
	// Where the log went comes before where the repositories came from: the
	// second is what the user typed, the first is the thing they have no other
	// way to find out.
	cells := []cell{
		{text: "q quit", style: dimStyle},
		{text: toggle, style: dimStyle},
		{text: logs, style: dimStyle},
	}
	if f.source != "" {
		cells = append(cells, cell{text: "src " + f.short(f.source), style: dimStyle})
	}
	return f.theme.fit(f.width, cells)
}

// cols is the repository table's column widths in display columns. A zero
// width means the column is not drawn at all.
type cols struct {
	watch int
	name  int
	phase int
	stat  int
	path  int
}

// layout sizes the table for the terminal it has. Columns are dropped in
// reverse order of what they answer: the path goes first (on one machine a name
// identifies a repository), then the phase word — which the progress cell
// implies anyway, since "waiting", a bar and "1204 indexed" are three different
// phases — then the watch marker, which the header line counts regardless.
func layout(width int, repos []status.RepoState) cols {
	longest := 0
	for _, r := range repos {
		if w := lipgloss.Width(r.Name); w > longest {
			longest = w
		}
	}
	c := cols{watch: 1, phase: phaseW}
	c.name = clamp(longest, nameMin, min(nameMax, max(nameMin, width/3)))

	rest := width - (c.watch + 1 + c.name + 1 + c.phase + 1)
	if rest < 10 {
		c.phase = 0
		rest = width - (c.watch + 1 + c.name + 1)
	}
	if rest < 8 {
		c.watch = 0
		rest = width - (c.name + 1)
	}
	if rest < 1 {
		c.name = max(1, width)
		return c
	}
	c.stat = min(rest, statMax)
	if left := rest - c.stat - 1; left >= pathMin {
		c.path = left
	} else {
		c.stat = rest
	}
	return c
}

// repoSection renders the table into at most rows lines, its header included.
// Fewer than two rows is not a table, so it renders nothing and the frame
// spends the line elsewhere.
func (f frame) repoSection(repos []status.RepoState, rows int) []string {
	if rows < 2 {
		return nil
	}
	c := layout(f.width, repos)
	out := []string{f.row(c,
		cell{text: "W", style: headStyle},
		cell{text: "REPO", style: headStyle},
		cell{text: "PHASE", style: headStyle},
		cell{text: "PROGRESS / LAST PASS", style: headStyle},
		cell{text: "PATH", style: headStyle})}
	rows--

	// The dormant line is reserved before the table is trimmed. It is the only
	// thing on screen saying the index holds more than this run is about, so it
	// outranks the last repository row — down to a table of one, where the
	// repository itself says more than the count does.
	dormant := ""
	if n := f.snap.Dormant; n > 0 && rows > 1 {
		dormant = fmt.Sprintf("+%d dormant in the index, not in this run", n)
		rows--
	}
	if len(repos) == 0 {
		// "registered" would be a lie with dormant repositories in the store,
		// and the difference is what the reader does next: register some, or
		// widen the working set they already have.
		if f.snap.Dormant == 0 {
			return append(out, f.note("no repositories registered"))
		}
		out = append(out, f.note("no repositories in this run"))
		if dormant != "" {
			out = append(out, f.note(dormant))
		}
		return out
	}
	shown := repos
	if len(repos) > rows {
		// One row goes on saying how many are hidden — unless a single row is
		// left, when the repository itself is worth more than the count, which
		// the counters line carries as "N repos" regardless.
		if rows > 1 {
			shown = repos[:rows-1]
		} else {
			shown = repos[:1]
		}
	}
	for _, r := range shown {
		out = append(out, f.repoRow(c, r))
	}
	if len(shown) < len(repos) {
		out = append(out, f.note(more(repos[len(shown):])))
	}
	if dormant != "" {
		out = append(out, f.note(dormant))
	}
	return out
}

// note is an indented, dimmed line inside a section — a count of what did not
// fit, or the reason a section is empty. Like every other line it is clipped to
// the frame.
func (f frame) note(text string) string {
	return f.theme.style(dimStyle, truncEnd("  "+text, f.width))
}

func (f frame) repoRow(c cols, r status.RepoState) string {
	watch := cell{}
	if r.Watched {
		watch = cell{text: "w", style: runStyle}
	}
	return f.row(c,
		watch,
		cell{text: r.Name},
		cell{text: string(r.Phase), style: phaseStyle(r.Phase)},
		f.statusCell(r, c.stat),
		cell{text: f.short(r.Path), style: dimStyle})
}

// row assembles one table line. It truncates and pads before styling, and cuts
// the path from the left because the last segments are the ones that identify
// a repository.
func (f frame) row(c cols, watch, name, phase, stat, path cell) string {
	var b strings.Builder
	write := func(cl cell, w int, last bool) {
		text := truncEnd(cl.text, w)
		if !last {
			text = pad(text, w)
		}
		b.WriteString(f.theme.style(cl.style, text))
	}
	if c.watch > 0 {
		write(watch, c.watch, false)
		b.WriteByte(' ')
	}
	write(name, c.name, c.phase == 0 && c.stat == 0)
	if c.phase > 0 {
		b.WriteByte(' ')
		write(phase, c.phase, false)
	}
	if c.stat > 0 {
		b.WriteByte(' ')
		write(stat, c.stat, c.path == 0)
	}
	if c.path > 0 {
		b.WriteByte(' ')
		b.WriteString(f.theme.style(path.style, truncStart(path.text, c.path)))
	}
	return b.String()
}

// statusCell answers "how far has the running pass got" for a repository being
// indexed and "how did the last one end" for one that is not. The bus keeps
// those two pairs of counters apart on purpose and so does this: an idle
// repository reporting zero finished with nothing to do, which is not the same
// as one this process has never touched.
func (f frame) statusCell(r status.RepoState, w int) cell {
	switch r.Phase {
	case status.PhaseIndexing:
		if r.Total <= 0 {
			return cell{text: fmt.Sprintf("%d files", r.Files), style: runStyle}
		}
		counts := fmt.Sprintf("%d/%d", r.Files, r.Total)
		if barW := w - lipgloss.Width(counts) - 1; barW >= 5 {
			return cell{text: bar(r.Files, r.Total, barW) + " " + counts, style: runStyle}
		}
		return cell{text: counts, style: runStyle}
	case status.PhaseQueued:
		return cell{text: "waiting", style: dimStyle}
	case status.PhaseError:
		var parts []string
		if r.Failed > 0 {
			parts = append(parts, plural(r.Failed, "file failed", "files failed"))
		}
		parts = append(parts, sanitize(r.Err))
		return cell{text: fitPlain(w, parts), style: badStyle}
	default:
		if r.FinishedAt.IsZero() && r.StartedAt.IsZero() {
			return cell{text: "-", style: dimStyle}
		}
		parts := []string{fmt.Sprintf("%d indexed", r.Indexed)}
		if r.Failed > 0 {
			parts = append(parts, fmt.Sprintf("%d failed", r.Failed))
		}
		if !r.FinishedAt.IsZero() {
			parts = append(parts, ago(f.now, r.FinishedAt))
		}
		style := plainStyle
		if r.Failed > 0 {
			style = warnStyle
		}
		return cell{text: fitPlain(w, parts), style: style}
	}
}

// logSection renders the tail of the ring buffer, oldest first, so that it
// reads like the log it mirrors.
func (f frame) logSection(entries []status.Entry, rows int) []string {
	if rows < 2 {
		return nil
	}
	head := "RECENT"
	if f.problems {
		head = "WARNINGS AND ERRORS"
	}
	out := []string{f.theme.style(headStyle, truncEnd(head, f.width))}
	rows--

	if len(entries) == 0 {
		if f.problems {
			return append(out, f.note("none"))
		}
		return append(out, f.note("nothing yet"))
	}
	if len(entries) > rows {
		entries = entries[len(entries)-rows:]
	}
	names := make(map[string]string, len(f.snap.Repos))
	for _, r := range f.snap.Repos {
		names[r.ID] = r.Name
	}
	for _, e := range entries {
		out = append(out, f.logRow(e, names))
	}
	return out
}

func (f frame) logRow(e status.Entry, names map[string]string) string {
	var b strings.Builder
	rest := f.width
	if !e.At.IsZero() && rest > 30 {
		b.WriteString(f.theme.style(dimStyle, e.At.Format("15:04:05")) + " ")
		rest -= 9
	}
	label, style := levelLabel(e.Level)
	b.WriteString(f.theme.style(style, label) + " ")
	rest -= lipgloss.Width(label) + 1

	if name := names[e.RepoID]; name != "" && rest > logRepoW+20 {
		w := min(logRepoW, rest/3)
		b.WriteString(pad(truncEnd(name, w), w) + " ")
		rest -= w + 1
	}
	b.WriteString(truncEnd(sanitize(e.Msg), rest))
	return b.String()
}

// entries applies the pane's filter. The bus tees warnings and errors into the
// same ring as the lifecycle entries, so a filter is all the "problems only"
// view needs.
func (f frame) entries() []status.Entry {
	if !f.problems {
		return f.snap.Log
	}
	out := make([]status.Entry, 0, len(f.snap.Log))
	for _, e := range f.snap.Log {
		if e.Level >= slog.LevelWarn {
			out = append(out, e)
		}
	}
	return out
}

// short replaces the home directory with ~, so that a path column which would
// otherwise be all prefix carries the part identifying the repository.
func (f frame) short(path string) string {
	if f.home == "" || f.home == "/" || path == "" {
		return path
	}
	if path == f.home {
		return "~"
	}
	if strings.HasPrefix(path, f.home+"/") {
		return "~" + path[len(f.home):]
	}
	return path
}

// orderRepos puts the interesting repositories first. The bus sorts by name,
// which is the right contract for a stable table but the wrong one for a
// terminal showing eight rows of forty: the pass in flight has to be on screen.
// The sort is stable, so within a phase the bus's name order survives.
func orderRepos(in []status.RepoState) []status.RepoState {
	out := append([]status.RepoState(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		return phaseRank(out[i].Phase) < phaseRank(out[j].Phase)
	})
	return out
}

func phaseRank(p status.Phase) int {
	switch p {
	case status.PhaseIndexing:
		return 0
	case status.PhaseError:
		return 1
	case status.PhaseQueued:
		return 2
	default:
		return 3
	}
}

func phaseStyle(p status.Phase) lipgloss.Style {
	switch p {
	case status.PhaseIndexing:
		return runStyle
	case status.PhaseError:
		return badStyle
	case status.PhaseQueued:
		return plainStyle
	default:
		return dimStyle
	}
}

// more describes the rows that did not fit, by phase, so that hiding a row does
// not hide that a repository is queued or has failed.
func more(hidden []status.RepoState) string {
	counts := map[status.Phase]int{}
	for _, r := range hidden {
		counts[r.Phase]++
	}
	var parts []string
	for _, p := range []status.Phase{status.PhaseIndexing, status.PhaseError, status.PhaseQueued, status.PhaseIdle} {
		if n := counts[p]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, p))
		}
	}
	return fmt.Sprintf("+%d more: %s", len(hidden), strings.Join(parts, ", "))
}

func levelLabel(l slog.Level) (string, lipgloss.Style) {
	switch {
	case l >= slog.LevelError:
		return "err ", badStyle
	case l >= slog.LevelWarn:
		return "warn", warnStyle
	case l >= slog.LevelInfo:
		return "info", plainStyle
	default:
		return "dbg ", dimStyle
	}
}

// bar draws a fixed-width progress bar in ASCII: the frame has to be readable
// on a terminal with no colour, where a bar of coloured blocks is a blank.
func bar(done, total, w int) string {
	if w < 3 {
		return ""
	}
	inner := w - 2
	filled := 0
	if total > 0 {
		filled = clamp(done*inner/total, 0, inner)
	}
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", inner-filled) + "]"
}
