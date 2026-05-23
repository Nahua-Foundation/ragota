package tui

// Файл содержит чистые helper'ы TUI:
// relToCwd — путь относительно cwd (с фоллбэком),
// truncateStart — обрезка строки слева с многоточием.

import (
	"path/filepath"
	"strings"
)

// relToCwd возвращает путь относительно cwd. Если путь не лежит под cwd —
// возвращает исходный путь без изменений. cwd может быть пустым.
func relToCwd(path, cwd string) string {
	if cwd == "" || path == "" {
		return path
	}
	if rel, err := filepath.Rel(cwd, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

func truncateStart(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 2 {
		return string(r[len(r)-n:])
	}
	return "…" + string(r[len(r)-(n-1):])
}
