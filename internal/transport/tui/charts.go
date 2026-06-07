package tui

// Файл содержит функции для рендеринга ASCII-графиков: sparkline, bar charts, progress bars.

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var sparkChars = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

func renderSparkline(values []float64, width int, color lipgloss.Style) string {
	if len(values) == 0 {
		return dimStyle.Render(strings.Repeat(" ", width))
	}
	if width <= 0 {
		return ""
	}
	n := len(values)
	if n > width {
		values = values[n-width:]
		n = width
	}
	minVal, maxVal := values[0], values[0]
	for _, v := range values {
		if v < minVal {
			minVal = v
		}
		if v > maxVal {
			maxVal = v
		}
	}
	var sb strings.Builder
	rangeVal := maxVal - minVal
	for _, v := range values {
		if rangeVal == 0 {
			sb.WriteRune(sparkChars[0])
		} else {
			idx := int((v - minVal) / rangeVal * float64(len(sparkChars)-1))
			if idx < 0 {
				idx = 0
			}
			if idx >= len(sparkChars) {
				idx = len(sparkChars) - 1
			}
			sb.WriteRune(sparkChars[idx])
		}
	}
	return color.Render(sb.String())
}

var barChars = []string{" ", "▏", "▎", "▍", "▌", "▋", "▊", "▉", "█"}

func renderBarChart(values []float64, labels []string, maxWidth, maxHeight int,
	color lipgloss.Style, errColor lipgloss.Style, errValues []float64) string {

	if len(values) == 0 {
		return dimStyle.Render("  no data")
	}
	n := len(values)
	if n > maxWidth {
		start := n - maxWidth
		values = values[start:]
		if len(labels) > start {
			labels = labels[start:]
		} else {
			labels = nil
		}
		if len(errValues) > start {
			errValues = errValues[start:]
		} else {
			errValues = nil
		}
		n = len(values)
	}
	maxVal := 1.0
	for _, v := range values {
		if v > maxVal {
			maxVal = v
		}
	}
	var rows []string
	for row := maxHeight - 1; row >= 0; row-- {
		threshold := float64(row+1) / float64(maxHeight) * maxVal
		var line strings.Builder
		for i := 0; i < n; i++ {
			if values[i] >= threshold {
				if len(errValues) > i && errValues[i] > 0 {
					line.WriteString(errColor.Render("█"))
				} else {
					line.WriteString(color.Render("█"))
				}
			} else {
				line.WriteString(" ")
			}
		}
		rows = append(rows, line.String())
	}
	if len(labels) > 0 && len(labels) <= n {
		var labelLine strings.Builder
		for i := 0; i < n; i++ {
			if i < len(labels) && len(labels[i]) > 0 {
				labelLine.WriteString(string([]rune(labels[i])[0]))
			} else {
				labelLine.WriteString(" ")
			}
		}
		rows = append(rows, dimStyle.Render(labelLine.String()))
	}
	return strings.Join(rows, "\n")
}

func renderProgressBar(current, total int, width int, color lipgloss.Style) string {
	if total == 0 {
		return dimStyle.Render("░░░░░░░░░░")
	}
	pct := float64(current) / float64(total)
	if pct > 1 {
		pct = 1
	}
	filled := int(pct * float64(width))
	if filled > width {
		filled = width
	}
	empty := width - filled
	bar := strings.Repeat("█", filled) + strings.Repeat("░", empty)
	pctStr := fmt.Sprintf("%3.0f%%", pct*100)
	return color.Render(bar) + " " + pctStr
}

func statusIndicator(status string) string {
	switch status {
	case "idle":
		return okStyle.Render("●")
	case "waiting", "scanning", "indexing", "resolving":
		return warnStyle.Render("◐")
	case "error":
		return errStyle.Render("✗")
	default:
		return dimStyle.Render("○")
	}
}
