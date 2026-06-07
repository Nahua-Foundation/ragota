package cli

// cli_analyze.go — ragota analyze [dir]: анализ директории, TUI для выбора
// паттернов игнорирования и сохранение в .ragotaignore.

import (
	"context"
	"fmt"
	"path/filepath"

	"ragota/pkg/config"

	"github.com/spf13/cobra"
)

func newAnalyzeCmd() *cobra.Command {
	var noLLM bool
	var model string

	c := &cobra.Command{
		Use:   "analyze [directory]",
		Short: "Analyze directory and interactively select ignore patterns for .ragotaignore",
		Long: `Analyze directory structure and suggest .ragotaignore patterns.

By default, uses a 3-pass LLM pipeline for high accuracy:
  Pass 1: Classify file groups
  Pass 2: Check for contradictions
  Pass 3: Deep review uncertain groups

Use --no-llm for fast heuristic-only analysis (no Ollama required).
Use --model to specify Ollama model (default: qwen3:4b).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			// Преобразуем в абсолютный путь ДО загрузки конфига
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			cfg, err := config.Load(absDir)
			if err != nil {
				return err
			}

			if !noLLM && cfg.Ollama.URL == "" {
				return fmt.Errorf("ollama URL not configured — run 'ragota configure' first, or use --no-llm for heuristic-only analysis")
			}

			ctx := cmd.Context()
			return runAnalyzeTUI(ctx, cfg, noLLM, model)
		},
	}

	c.Flags().BoolVar(&noLLM, "no-llm", false, "skip LLM analysis, use heuristics only (fast, no Ollama required)")
	c.Flags().StringVar(&model, "model", "", "Ollama model to use (default: qwen3:4b)")

	return c
}

// runAnalyzeTUI запускает TUI для анализа и выбора паттернов.
func runAnalyzeTUI(ctx context.Context, cfg *config.Config, noLLM bool, model string) error {
	m := newAnalyzeModel(ctx, cfg, noLLM, model)
	p := NewAnalyzeProgram(m)
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("analyze TUI: %w", err)
	}
	return nil
}
