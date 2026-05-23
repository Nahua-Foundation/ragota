// Package tui — терминальный дашборд (bubbletea + lipgloss).
// Отображает: статус docker-compose, статус индексаторов, последние файлы,
// статистику вызовов MCP-серверов. Обновляется по тику раз в секунду.
//
// Реализация декомпозирована по доменам (все файлы — package tui):
//
//   - tui.go     — публичная точка входа Run + открытие лог-файла TUI
//     (openLogFile, logFilePathHint).
//   - model.go   — bubbletea-модель (model, tickMsg, snapMsg, Init/Update,
//     refreshSnap, tick) и appendLogs (зеркалирование Recent
//     в файл логов).
//   - view.go    — рендеринг главного экрана (model.View) и стили
//     (titleStyle/headerStyle/dimStyle/...).
//   - render.go  — мелкие рендеры секций: dockerLineFmt, renderIndexer,
//     renderError.
//   - util.go    — чистые helper'ы: relToCwd, truncateStart.
package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"aitools/internal/state"

	tea "github.com/charmbracelet/bubbletea"
)

// Run запускает дашборд. Возвращает при выходе пользователем (q/Ctrl+C) или при ctx.Done.
func Run(ctx context.Context, bus *state.Bus) error {
	lf, lfPath := openLogFile()
	if lf != nil {
		defer lf.Close()
		_, _ = fmt.Fprintf(os.Stderr, "ai-tools TUI: logs are mirrored to %s\n", lfPath)
	}
	m := model{bus: bus, logFile: lf, seen: make(map[string]struct{})}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	return err
}

// openLogFile открывает файл логов TUI в ~/.cache/ai-tools/tui.log
// (с фоллбэком в текущую директорию). Если открыть не удалось — возвращает nil.
func openLogFile() (*os.File, string) {
	dir := ""
	if h, err := os.UserCacheDir(); err == nil {
		dir = filepath.Join(h, "ai-tools")
	} else if h, err := os.UserHomeDir(); err == nil {
		dir = filepath.Join(h, ".cache", "ai-tools")
	}
	if dir == "" {
		dir = ".ai-tools"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, ""
	}
	path := filepath.Join(dir, "tui.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, ""
	}
	_, _ = fmt.Fprintf(f, "\n=== ai-tools TUI started at %s ===\n", time.Now().Format(time.RFC3339))
	return f, path
}

// logFilePathHint возвращает короткий хинт пути файла логов для отображения
// в TUI. Намеренно без полного пути — экономим место в шапке секции.
func logFilePathHint() string {
	if h, err := os.UserCacheDir(); err == nil {
		return filepath.Join(h, "ai-tools", "tui.log")
	}
	return "~/.cache/ai-tools/tui.log"
}
