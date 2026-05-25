package tui

// Файл содержит мелкие рендеры отдельных секций дашборда:
// dockerLineFmt — строка статуса docker-compose,
// renderIndexer — строка одного индексатора,
// renderError — многострочный блок ошибки.

import (
	"fmt"
	"strings"

	"ragota/internal/state"
)

func dockerLineFmt(d state.DockerStatus, width int) string {
	if d.LastError != "" {
		return errStyle.Render("error: ") + d.LastError
	}
	if !d.Running && len(d.Services) == 0 {
		return dimStyle.Render("not started")
	}
	parts := []string{}
	for _, svc := range d.Services {
		parts = append(parts, okStyle.Render("●")+" "+svc)
	}
	return strings.Join(parts, "  ")
}

func renderIndexer(i state.Indexer, width int) string {
	statusStyle := dimStyle
	switch i.Status {
	case "indexing", "scanning":
		statusStyle = warnStyle
	case "idle":
		statusStyle = okStyle
	case "error":
		statusStyle = errStyle
	}

	stats := fmt.Sprintf("files=%d/%d chunks=%d symbols=%d", i.FilesIndexed, i.FilesTotal, i.Chunks, i.Symbols)
	if i.Name == "graph" {
		stats = fmt.Sprintf("files=%d/%d edges=%d units=%d", i.FilesIndexed, i.FilesTotal, i.Chunks, i.Symbols)
	}

	line := fmt.Sprintf("  %-11s %s  %s",
		i.Name,
		statusStyle.Render(fmt.Sprintf("%-9s", i.Status)),
		stats)
	if i.LastError != "" {
		line += renderError(i.LastError, width-4)
	}
	return line
}

func renderError(err string, width int) string {
	if err == "" {
		return ""
	}
	return "\n" + errStyle.Copy().Width(width).Render("    Error: "+err)
}
