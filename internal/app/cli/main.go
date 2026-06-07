// Package cli — cobra-команды бинаря ragota.
// Подкоманды:
//   - up <dir>              — поднять docker, проиндексировать, открыть TUI + MCP SSE сервер
//   - analyze <dir>         — анализ директории и выбор паттернов для .ragotaignore
//   - configure             — интерактивная настройка: зависимости → конфиг → MCP в агенте
//   - stats <dir>           — статистика файлов с учётом игнорирования
//   - clean [dir]           — удалить все индексы и БД
//   - clean cross-repo [dir] — удалить только cross-repo edges
package cli

import (
	"fmt"
	"os"
	"runtime"

	"ragota/pkg/logger"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:           "ragota",
	Short:         "AI dev tool: AST + vector + LSP MCP servers with live indexing",
	SilenceErrors: true,
	SilenceUsage:  true,
}

// Execute — точка входа CLI. Вызывается из cmd/ragota/main.go.
func Execute() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	if f, path, err := logger.OpenLogFile("."); err == nil {
		logger.InitLogger("info", false, f)
		defer f.Close()
		logger.Log().Info().Msgf("ragota: logging to %s", path)
	}

	rootCmd.AddCommand(newUpCmd())
	rootCmd.AddCommand(newAnalyzeCmd())
	rootCmd.AddCommand(newConfigureCmd())
	rootCmd.AddCommand(newStatsCmd())
	rootCmd.AddCommand(newCleanCmd())

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
