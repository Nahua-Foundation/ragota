package main

import (
	"fmt"
	"os"

	"aitools/internal/config"

	"github.com/spf13/cobra"
)

// newGenConfigCmd: ai-tools gen-config [--path <file>] [--force]
//
// Записывает дефолтный YAML-конфиг. Если --path и --local не указаны —
// пишется в глобальный путь (~/.ai-tools/config.yaml).
// Чтобы файл подхватывался автоматически, положите его в .ai-tools/config.yaml
// (внутри проекта) или в ~/.ai-tools/config.yaml.
func newGenConfigCmd() *cobra.Command {
	var (
		path  string
		force bool
		local bool
	)
	c := &cobra.Command{
		Use:   "gen-config",
		Short: "Generate a default YAML config file",
		Long: "Generate a default YAML config with all knobs filled in.\n\n" +
			"By default the file is written to ~/.ai-tools/config.yaml.\n" +
			"Use --local to put it at .ai-tools/config.yaml (project-local search path),\n" +
			"or --path to choose an explicit location.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			target := path
			if local {
				target = config.DefaultConfigPath(".")
			}
			written, err := config.WriteDefault(target, force)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "wrote default config to: %s\n", written)
			return nil
		},
	}
	c.Flags().StringVar(&path, "path", "", "explicit path for the generated config file")
	c.Flags().BoolVar(&local, "local", false, "write to .ai-tools/config.yaml (project-local path)")
	c.Flags().BoolVar(&force, "force", false, "overwrite existing file")
	return c
}
