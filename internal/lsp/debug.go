package lsp

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Файл содержит ленивую инициализацию отладочного лог-файла LSP-клиента.
// Лог пишется в `.ragota/logs/lsp-debug.log` относительно текущего каталога
// (корня проекта, где запущен ragota). Путь можно переопределить переменной
// окружения RAGOTA_LSP_LOG (абсолютный путь к файлу).

var (
	lspDebugLog  *os.File
	lspDebugOnce sync.Once
)

// openLspDebugLog лениво открывает файл лога LSP. Папка создаётся при необходимости.
func openLspDebugLog() {
	lspDebugOnce.Do(func() {
		path := os.Getenv("RAGOTA_LSP_LOG")
		if path == "" {
			cwd, err := os.Getwd()
			if err != nil {
				cwd = "."
			}
			dir := filepath.Join(cwd, ".ragota", "logs")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return
			}
			path = filepath.Join(dir, "lsp-debug.log")
		} else {
			if d := filepath.Dir(path); d != "" {
				_ = os.MkdirAll(d, 0o755)
			}
		}
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
			lspDebugLog = f
		}
	})
}

// lspDebug безопасно (best-effort) пишет отладочную запись в файл лога LSP.
// Если лог не удалось открыть — вызов превращается в no-op.
func lspDebug(format string, args ...any) {
	openLspDebugLog()
	if lspDebugLog != nil {
		fmt.Fprintf(lspDebugLog, format, args...)
	}
}
