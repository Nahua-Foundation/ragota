package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/lipgloss"
)

// The string arithmetic the frame is built out of. Everything here measures in
// display columns rather than in bytes or runes: a wide rune is two columns and
// a frame that counts it as one is a column too wide on every line that holds
// one.

// fitPlain is fit for the parts of a single cell, which share one style.
func fitPlain(w int, parts []string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		cand := p
		if out != "" {
			cand = out + " " + p
		}
		if lipgloss.Width(cand) > w {
			break
		}
		out = cand
	}
	if out == "" && len(parts) > 0 {
		out = truncEnd(parts[0], w)
	}
	return out
}

// truncEnd cuts s to at most w display columns, keeping the start. An ellipsis
// marks the cut, so a truncated error message does not read as a complete one.
func truncEnd(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if used+rw > w-1 {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	return b.String() + "…"
}

// truncStart keeps the end of s, which is what a path wants: the last two
// segments say which repository this is, the first ten say whose home
// directory it sits in.
func truncStart(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	runes := []rune(s)
	used, i := 0, len(runes)
	for ; i > 0; i-- {
		rw := lipgloss.Width(string(runes[i-1]))
		if used+rw > w-1 {
			break
		}
		used += rw
	}
	return "…" + string(runes[i:])
}

// pad right-fills s to w display columns. It never shortens, so the caller
// truncates first.
func pad(s string, w int) string {
	if n := w - lipgloss.Width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// sanitize makes an arbitrary string safe to put in a cell. Log messages and
// error strings arrive with newlines and tabs in them, and one that came from a
// subprocess can carry escape sequences; any of the three draws outside its
// cell and corrupts the rest of the frame.
func sanitize(s string) string {
	if strings.IndexFunc(s, unicode.IsControl) < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	gap := true // suppresses a leading space and collapses runs of whitespace
	for _, r := range s {
		switch {
		case unicode.IsControl(r), r == ' ' && gap:
			// The indentation that follows a newline in a wrapped error
			// message is noise once the newline itself is gone.
			if !gap {
				b.WriteByte(' ')
				gap = true
			}
		default:
			b.WriteRune(r)
			gap = r == ' '
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// ago renders an age at one significant unit: the column is a few characters
// wide, and "2h" answers the question "2h13m41.2s" also answers.
func ago(now, then time.Time) string {
	d := now.Sub(then)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// uptime renders how long the process has been up, to the second. Anything
// finer redraws the line every frame and tells nobody anything.
func uptime(now, started time.Time) string {
	d := now.Sub(started)
	if d < 0 {
		d = 0
	}
	return d.Truncate(time.Second).String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
