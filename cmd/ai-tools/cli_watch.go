package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"aitools/internal/astindex"
	"aitools/internal/config"
	"aitools/internal/docker"
	"aitools/internal/embedder"
	"aitools/internal/fileutil"
	"aitools/internal/index"
	"aitools/internal/qdrant"
	"aitools/internal/repos"
	"aitools/internal/state"
	"aitools/internal/store"
	"aitools/internal/tui"
	"aitools/internal/watcher"

	"github.com/spf13/cobra"
)

// newWatchCmd: ai-tools watch [dir]
//   - запускает docker compose up -d
//   - индексирует tree-sitter в SQLite
//   - индексирует векторно в Qdrant
//   - подписывается на изменения файлов
//   - запускает TUI-дашборд
func newWatchCmd() *cobra.Command {
	var (
		startDocker bool
		skipVector  bool
		skipTS      bool
		noTUI       bool
	)
	c := &cobra.Command{
		Use:   "watch [directory]",
		Short: "Start docker, indexing and a live dashboard for the directory",
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
			defer cancel()

			bus := state.NewBus(cfg.Root)

			// 0. Auto-discovery репо в workspace. Один резолвер пробрасывается
			// во все индексаторы и watcher для multi-repo маршрутизации.
			discovered, err := repos.Discover(cfg.Root)
			if err != nil {
				return err
			}
			if len(discovered) > 1 {
				fmt.Fprintf(os.Stderr, "repos: найдено %d репо в workspace:\n", len(discovered))
				for _, r := range discovered {
					fmt.Fprintf(os.Stderr, "  - %s (%s)\n", r.Name, r.Path)
				}
			}
			repoResolver := repos.NewResolver(discovered)

			// 1. docker — только по флагу --start-docker (контейнеры из конфига).
			if startDocker {
				runner := docker.New(cfg.Root, cfg.Docker)
				// Гарантируем остановку при выходе
				defer func() {
					fmt.Fprintf(os.Stderr, "docker: stopping containers...\n")
					_ = runner.Down(context.Background())
				}()

				if err := docker.Available(ctx); err != nil {
					fmt.Fprintf(os.Stderr, "docker: check failed: %v\n", err)
					bus.SetDocker(state.DockerStatus{LastError: err.Error()})
				} else {
					fmt.Fprintf(os.Stderr, "docker: starting containers...\n")
					bus.SetDocker(state.DockerStatus{LastError: "starting..."})
					if err := runner.Up(ctx); err != nil {
						fmt.Fprintf(os.Stderr, "docker: error starting containers: %v\n", err)
						bus.SetDocker(state.DockerStatus{LastError: err.Error()})
					} else {
						fmt.Fprintf(os.Stderr, "docker: all containers are up\n")
						go startDockerMonitor(ctx, runner, bus)
					}
				}
			}

			// 2. tree-sitter + ast graph
			var tsIdx *index.TreeSitter
			var astIdx *astindex.Indexer
			var st *store.SQLite
			if !skipTS {
				st, err = store.Open(cfg.SQLitePath())
				if err != nil {
					return fmt.Errorf("sqlite open: %w", err)
				}
				defer st.Close()
				tsIdx = index.NewTreeSitter(cfg, st, bus)
				astIdx = astindex.New(cfg, st)
				astIdx.SetBus(bus)
				astIdx.SetRepoResolver(repoResolver)
			}

			// 3. vector
			var vIdx *index.Vector
			if !skipVector {
				qd := qdrant.New(fmt.Sprintf("http://%s:%d", cfg.Qdrant.Host, cfg.Qdrant.Port))
				emb := embedder.New(cfg.Ollama.URL, cfg.Ollama.EmbedModel)
				vIdx = index.NewVector(cfg, qd, emb, st, bus)
				vIdx.SetRepoResolver(repoResolver)
				// ждём готовности qdrant/ollama (10 попыток по 2с)
				go func() {
					for i := 0; i < 30; i++ {
						if ctx.Err() != nil {
							return
						}
						pCtx, c2 := context.WithTimeout(ctx, 3*time.Second)
						qErr := qd.Ping(pCtx)
						oErr := emb.Ping(pCtx)
						c2()
						if qErr == nil && oErr == nil {
							if err := vIdx.Init(ctx); err != nil {
								bus.SetIndexer("vector", func(i *state.Indexer) {
									i.Status = "error"
									i.LastError = "qdrant init: " + err.Error()
								})
								return
							}
							_ = vIdx.FullScan(ctx)
							return
						}
						bus.SetIndexer("vector", func(i *state.Indexer) {
							i.Status = "scanning"
							if qErr != nil {
								i.LastError = "waiting qdrant: " + qErr.Error()
							} else if oErr != nil {
								i.LastError = "waiting ollama: " + oErr.Error()
							}
						})
						time.Sleep(2 * time.Second)
					}
				}()
			}

			// 4. ts full scan и затем watcher
			matcher := fileutil.NewMatcher(cfg.Ignore)
			w, err := watcher.New(cfg.Root, matcher, cfg.Extensions, 300*time.Millisecond)
			if err != nil {
				return err
			}
			w.SetRepoResolver(repoResolver)
			defer w.Close()
			if err := w.Start(ctx); err != nil {
				return err
			}
			if tsIdx != nil {
				go func() { _ = tsIdx.FullScan(ctx) }()
			}
			if astIdx != nil {
				go func() { _ = astIdx.FullScan(ctx) }()
			}
			// Реакция на события: один watcher → оба индексатора.
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case ev, ok := <-w.Events():
						if !ok {
							return
						}
						switch ev.Kind {
						case watcher.EventRemove, watcher.EventRename:
							if tsIdx != nil {
								_ = tsIdx.RemoveFile(ctx, ev.AbsPath)
							}
							if vIdx != nil {
								_ = vIdx.RemoveFile(ctx, ev.AbsPath)
							}
							if astIdx != nil {
								_ = astIdx.RemoveFile(ctx, ev.AbsPath)
							}
						default:
							if tsIdx != nil {
								_ = tsIdx.IndexFile(ctx, ev.AbsPath)
							}
							if vIdx != nil {
								_ = vIdx.IndexFile(ctx, ev.AbsPath)
							}
							if astIdx != nil {
								_ = astIdx.IndexFile(ctx, ev.AbsPath)
							}
						}
					}
				}
			}()

			// 5. TUI или просто блок на сигнал
			if noTUI {
				<-ctx.Done()
				return nil
			}
			if err := tui.Run(ctx, bus); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		},
	}
	c.Flags().BoolVar(&startDocker, "start-docker", false, "start qdrant container from config (off by default)")
	c.Flags().BoolVar(&skipVector, "skip-vector", false, "disable vector (qdrant) indexing")
	c.Flags().BoolVar(&skipTS, "skip-treesitter", false, "disable tree-sitter indexing")
	c.Flags().BoolVar(&noTUI, "no-tui", false, "no TUI dashboard, just run in background")
	return c
}
