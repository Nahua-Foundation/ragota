package cli

// cli_watch.go — ai-tools watch: docker + индексация + watcher + TUI.

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"ragota/internal/indexing/ast"
	"ragota/internal/indexing/crossrepo/classifier"
	"ragota/internal/indexing/crossrepoindex"
	"ragota/pkg/config"
	"ragota/internal/indexing/vector"
	"ragota/pkg/state"
	"ragota/internal/transport/tui"

	"github.com/spf13/cobra"
)

// newWatchCmd: ai-tools watch [dir]
func newWatchCmd() *cobra.Command {
	var (
		envMode    string
		skipVector bool
		noTUI      bool
	)
	c := &cobra.Command{
		Use:   "watch [directory]",
		Short: "Start docker, indexing and a live dashboard",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			cfg, err := config.Load(dir, configPath)
			if err != nil {
				return err
			}
			if err := cfg.EnsureDataDir(); err != nil {
				return err
			}

			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			bus := state.NewBus(cfg.Root)

			// Shared bootstrap: Docker + store + repo discovery.
			boot, err := bootstrap(ctx, cfg, bus, envMode)
			if err != nil {
				cancel()
				return err
			}
			defer boot.cleanup()

			// AST indexer — always created.
			astIdx := astindex.New(cfg, boot.st)
			astIdx.SetBus(bus)
			astIdx.SetRepoResolver(boot.resolver)

			// Cross-repo indexer.
			crIdx := crossrepoindex.New(boot.resolver, boot.st)
			crIdx.InitManifests()
			if cfg.Ollama.URL != "" {
				crIdx.SetClassifier(classifier.New(cfg.Ollama.URL, "qwen2.5-coder:3b"))
			}
			crIdx.SetBus(bus)

			// Vector indexer — optional.
			var vIdx *vector.Vector
			if !skipVector {
				vIdx = boot.createVectorIndexer(ctx, cfg, bus)
			}

			// Watcher + FullScan + event fanout.
			w, err := boot.createWatcher(ctx, cfg)
			if err != nil {
				cancel()
				return err
			}
			defer w.Close()

			go func() {
				_ = astIdx.FullScan(ctx)
				_ = crIdx.FullScan(ctx)
			}()
			go fanoutWatchEvents(ctx, w, vIdx, astIdx, crIdx)

			// TUI or wait for signal.
			if noTUI {
				<-ctx.Done()
			} else {
				if err := tui.Run(ctx, bus); err != nil && !errors.Is(err, context.Canceled) {
					cancel()
					return err
				}
			}

			cancel()
			boot.stopDocker(ctx)
			return nil
		},
	}
	c.Flags().StringVar(&envMode, "env", "", "environment: 'local' or 'docker'")
	c.Flags().BoolVar(&skipVector, "skip-vector", false, "disable vector indexing")
	c.Flags().BoolVar(&noTUI, "no-tui", false, "no TUI dashboard")
	return c
}
