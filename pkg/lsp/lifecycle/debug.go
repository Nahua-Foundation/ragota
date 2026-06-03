package lifecycle

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var (
	lspDebugLog  *os.File
	lspDebugOnce sync.Once
)

// OpenDebugLog lazily opens the LSP debug log file.
func OpenDebugLog() {
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

// DebugLog writes a debug entry to the LSP log file (best-effort, no-op if log not open).
func DebugLog(format string, args ...any) {
	OpenDebugLog()
	if lspDebugLog != nil {
		fmt.Fprintf(lspDebugLog, format, args...)
	}
}

// CloseDebugLog closes the LSP debug log file (best-effort, no-op if not open).
func CloseDebugLog() {
	lspDebugOnce = sync.Once{}
	if lspDebugLog != nil {
		_ = lspDebugLog.Close()
		lspDebugLog = nil
	}
}
