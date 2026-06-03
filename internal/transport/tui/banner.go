package tui

// ASCII-art баннер RAGOTA.

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var ragotaBanner = `
██████╗  █████╗  ██████╗  ██████╗ ████████╗ █████╗
██╔══██╗██╔══██╗██╔════╝ ██╔═══██╗╚══██╔══╝██╔══██╗
██████╔╝███████║██║  ███╗██║   ██║   ██║   ███████║
██╔══██╗██╔══██║██║   ██║██║   ██║   ██║   ██╔══██║
██║  ██║██║  ██║╚██████╔╝╚██████╔╝   ██║   ██║  ██║
╚═╝  ╚═╝╚═╝  ╚═╝ ╚═════╝  ╚═════╝    ╚═╝   ╚═╝  ╚═╝
`

func renderBanner(width int) string {
	colors := []lipgloss.Style{
		lipgloss.NewStyle().Foreground(lipgloss.Color("#7D56F4")),
		lipgloss.NewStyle().Foreground(lipgloss.Color("#A78BFA")),
		lipgloss.NewStyle().Foreground(lipgloss.Color("#C4B5FD")),
	}
	rows := strings.Split(strings.TrimSpace(ragotaBanner), "\n")
	var lines []string
	for i, row := range rows {
		c := colors[i%len(colors)]
		lines = append(lines, c.Render(row))
	}
	return strings.Join(lines, "\n")
}
