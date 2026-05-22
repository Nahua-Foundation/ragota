// Package main для бинаря ai-tools.
// Подкоманды:
//   - watch <dir>          — поднять docker (по флагу), проиндексировать, открыть TUI
//   - run [-t -v -l -w]    — выбранные MCP-серверы и/или watch+TUI в одном процессе
//   - clean                — почистить индексы и БД
//   - gen-config           — сгенерировать дефолтный YAML-конфиг рядом с бинарём
//   - serve-treesitter     — MCP tree-sitter сервер (stdio)
//   - serve-vector         — MCP vector сервер (stdio)
//   - serve-lsp            — MCP LSP сервер (stdio)
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// configPath — глобальный путь к YAML-конфигу (--config / -c).
// Если пустой — используется ./ai-tools/config.yaml.
var configPath string

var rootCmd = &cobra.Command{
	Use:           "ai-tools",
	Short:         "AI dev tool: tree-sitter + vector + LSP MCP servers with file watcher",
	SilenceErrors: true,
	SilenceUsage:  true,
}

func main() {
	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", "",
		"path to YAML config (default search: ./.ai-tools/config.yaml or ~/.ai-tools/config.yaml)")

	rootCmd.AddCommand(newWatchCmd())
	rootCmd.AddCommand(newRunCmd())
	rootCmd.AddCommand(newInstallCmd())
	rootCmd.AddCommand(newCleanCmd())
	rootCmd.AddCommand(newGenConfigCmd())
	rootCmd.AddCommand(newMcpConfigCmd())
	rootCmd.AddCommand(newServeTreesitterCmd())
	rootCmd.AddCommand(newServeVectorCmd())
	rootCmd.AddCommand(newServeLSPCmd())

	// Поддержка слитной записи коротких флагов для run: `-tvlw`, `-tw`, `-lvt` и т.п.
	// Разворачиваем такие токены в набор индивидуальных `-x` ДО передачи cobra.
	os.Args = expandRunShortFlags(os.Args)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// expandRunShortFlags ищет команду run и разворачивает слитные короткие
// bool-флаги (-tvlw → -t -v -l -w). Любой порядок и состав букв из набора
// {t,v,l,w} допустим. Незнакомые буквы оставляют токен как есть.
func expandRunShortFlags(args []string) []string {
	if len(args) < 2 {
		return args
	}
	// Найти позицию подкоманды run (первый не-флаг аргумент после имени программы,
	// пропуская глобальные --config/-c значения).
	runIdx := -1
	i := 1
	for i < len(args) {
		a := args[i]
		if a == "run" {
			runIdx = i
			break
		}
		// Перешагиваем глобальные флаги, ожидающие значение.
		if a == "--config" || a == "-c" {
			i += 2
			continue
		}
		if strings.HasPrefix(a, "--config=") || strings.HasPrefix(a, "-c=") {
			i++
			continue
		}
		// Любой иной токен — не run.
		break
	}
	if runIdx < 0 {
		return args
	}
	const known = "tvlw"
	out := make([]string, 0, len(args)+4)
	out = append(out, args[:runIdx+1]...)
	for _, tok := range args[runIdx+1:] {
		if len(tok) > 2 && tok[0] == '-' && tok[1] != '-' && isKnownLetters(tok[1:], known) {
			for _, r := range tok[1:] {
				out = append(out, "-"+string(r))
			}
			continue
		}
		out = append(out, tok)
	}
	return out
}

func isKnownLetters(s, known string) bool {
	for _, r := range s {
		if !strings.ContainsRune(known, r) {
			return false
		}
	}
	return true
}
