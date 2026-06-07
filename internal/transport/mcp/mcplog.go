package mcp

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/rs/zerolog"
)

// mcpLog — логгер для MCP-вызовов.
var (
	mcpLog     zerolog.Logger
	mcpLogOnce sync.Once
)

// InitMCPLog инициализирует файловый логгер для MCP.
// Пишет в {root}/logs/mcp.log. Безопасно для многократного вызова.
func InitMCPLog(root string) {
	mcpLogOnce.Do(func() {
		logDir := filepath.Join(root, "logs")
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			mcpLog = zerolog.Nop()
			return
		}
		path := filepath.Join(logDir, "mcp.log")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			mcpLog = zerolog.Nop()
			return
		}
		mcpLog = zerolog.New(zerolog.ConsoleWriter{Out: f, TimeFormat: "15:04:05.000"}).
			Level(zerolog.DebugLevel).With().Timestamp().Str("module", "mcp").Logger()
	})
}
