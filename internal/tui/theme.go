package tui

import "github.com/charmbracelet/lipgloss"

// The frame carries its meaning in words and ASCII glyphs; these only tint it.
// Nothing in the layout is legible only in colour, which is what makes the
// no-colour path (and every rendering test) the same frame rather than a
// degraded one.
var (
	plainStyle = lipgloss.NewStyle()
	headStyle  = lipgloss.NewStyle().Bold(true)
	dimStyle   = lipgloss.NewStyle().Faint(true)
	runStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))  // blue: in flight
	warnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214")) // amber
	badStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("196")) // red
)

// theme decides whether a cell is styled at all. The zero value renders plain
// text, so a test asserts the frame it can see and no escape sequence has to be
// stripped to compare a line.
//
// When it is on, lipgloss still has the last word: its renderer inspects the
// terminal — TERM, NO_COLOR, whether stdout is a tty — and emits nothing for
// one that cannot show colour. So "on" means "offer colour", not "assume it".
type theme struct {
	colour bool
}

// style paints text, which the caller has already sized: an escape sequence has
// no display width, and a cell measured after styling measures wrong.
func (t theme) style(s lipgloss.Style, text string) string {
	if !t.colour || text == "" {
		return text
	}
	return s.Render(text)
}

// fit joins as many leading cells as fit in w columns. The caller orders them
// by importance, so a narrow terminal loses the least useful field instead of
// wrapping the line. It stops at the first cell that does not fit rather than
// skipping over it: a line whose middle silently vanishes reads as a different
// line.
func (t theme) fit(w int, cells []cell) string {
	plain, styled := "", ""
	for _, c := range cells {
		if c.text == "" {
			continue
		}
		p, s := c.text, t.style(c.style, c.text)
		if plain != "" {
			p, s = plain+"  "+p, styled+"  "+s
		}
		if lipgloss.Width(p) > w {
			break
		}
		plain, styled = p, s
	}
	if plain != "" {
		return styled
	}
	for _, c := range cells {
		if c.text != "" {
			return t.style(c.style, truncEnd(c.text, w))
		}
	}
	return ""
}
