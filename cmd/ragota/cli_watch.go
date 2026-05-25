package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"ragota/internal/astindex"
	"ragota/internal/config"
	"ragota/internal/docker"
	"ragota/internal/embedder"
	"ragota/internal/fileutil"
	"ragota/internal/index"
	"ragota/internal/qdrant"
	"ragota/internal/repos"
	"ragota/internal/state"
	"ragota/internal/store"
	"ragota/internal/tui"
	"ragota/internal/watcher"

	"github.com/spf13/cobra"
)

// shutdownState хранит ресурсы для graceful shutdown.
type shutdownState struct {
	mu           sync.Mutex
	watcher      *watcher.Watcher
	vectorIdx    *index.Vector
	tsIdx        *index.TreeSitter
	astIdx       *astindex.Indexer
	dockerRunner *docker.Runner
	started      bool
	shutdownOnce sync.Once
	indexersWg   sync.WaitGroup
}

// stopIndexers останавливает индексацию и watcher перед остановкой docker.
// Возвращает true, если индексаторы завершились успешно.
func (s *shutdownState) stopIndexers(ctx context.Context) bool {
	completed := false
	s.shutdownOnce.Do(func() {
		s.mu.Lock()
		defer s.mu.Unlock()

		if !s.started {
			completed = true
			return
		}

		// 1. Сначала останавливаем watcher — он генерирует события для индексаторов
		if s.watcher != nil {
			fmt.Fprintf(os.Stderr, "graceful: stopping file watcher...\n")
			s.watcher.Close()
		}

		// 2. Ждём завершения всех горутин индексации
		// FullScan и IndexFile проверяют ctx.Done(), поэтому завершатся
		fmt.Fprintf(os.Stderr, "graceful: waiting for indexers to complete (up to 15s)...\n")
		done := make(chan struct{})
		go func() {
			s.indexersWg.Wait()
			close(done)
		}()
		select {
		case <-done:
			fmt.Fprintf(os.Stderr, "graceful: indexers completed normally\n")
			completed = true
		case <-time.After(15 * time.Second):
			fmt.Fprintf(os.Stderr, "graceful: timeout waiting for indexers\n")
		}

		fmt.Fprintf(os.Stderr, "graceful: indexers stopped\n")
	})
	return completed
}

// stopDocker останавливает docker контейнеры после остановки индексаторов.
func (s *shutdownState) stopDocker(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.dockerRunner == nil {
		return
	}

	fmt.Fprintf(os.Stderr, "docker: stopping containers...\n")
	shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_ = s.dockerRunner.Down(shutdownCtx)
	fmt.Fprintf(os.Stderr, "docker: containers stopped\n")
}

// newWatchCmd: ai-tools watch [dir]
//   - запускает docker compose up -d
//   - индексирует tree-sitter в SQLite
//   - индексирует векторно в Qdrant
//   - подписывается на изменения файлов
//   - запускает TUI-дашборд
func newWatchCmd() *cobra.Command {
	var (
		envMode    string
		skipVector bool
		skipTS     bool
		noTUI      bool
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
			// НЕ вызываем defer cancel() — отменим контекст явно после остановки индексаторов

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

			// Состояние для graceful shutdown
			shutdown := &shutdownState{}

			// 1. docker — по флагу --env docker (все сервисы) или --env local (только Qdrant).
			// По умолчанию (пустой envMode) — local режим.
			startDockerAll := envMode == "docker"
			startDockerQdrant := envMode == "" || envMode == "local" || startDockerAll

			if startDockerQdrant || startDockerAll {
				runner := docker.New(cfg.Root, cfg.Docker)
				shutdown.dockerRunner = runner

				if err := docker.Available(ctx); err != nil {
					fmt.Fprintf(os.Stderr, "docker: check failed: %v\n", err)
					bus.SetDocker(state.DockerStatus{LastError: err.Error()})
					// В режиме --env docker Docker обязателен
					if startDockerAll {
						return fmt.Errorf("docker required but unavailable: %w", err)
					}
				} else {
					fmt.Fprintf(os.Stderr, "docker: starting containers...\n")
					bus.SetDocker(state.DockerStatus{LastError: "starting..."})
					if err := runner.Up(ctx, startDockerAll); err != nil {
						fmt.Fprintf(os.Stderr, "docker: error starting containers: %v\n", err)
						bus.SetDocker(state.DockerStatus{LastError: err.Error()})
						// В режиме --env docker ошибка запуска контейнеров фатальна
						if startDockerAll {
							return fmt.Errorf("docker containers failed to start: %w", err)
						}
					} else {
						fmt.Fprintf(os.Stderr, "docker: containers are up\n")
						go startDockerMonitor(ctx, runner, bus)
					}
				}
			}

			if startDockerQdrant || startDockerAll {
				cfg.Qdrant.Host = "localhost"
			}

			// 3. tree-sitter + ast graph
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
				shutdown.tsIdx = tsIdx
				shutdown.astIdx = astIdx
			}

			// 3. vector
			var vIdx *index.Vector
			if !skipVector {
				qd := qdrant.New(fmt.Sprintf("http://%s:%d", cfg.Qdrant.Host, cfg.Qdrant.Port))
				emb := embedder.New(cfg.Ollama.URL, cfg.Ollama.EmbedModel)
				// Ограничиваем параллелизм запросов к Ollama (2 одновременных запроса)
				// чтобы не перегружать CPU и избегать contention
				emb.SetSemaphore(make(chan struct{}, 2))
				vIdx = index.NewVector(cfg, qd, emb, st, bus)
				vIdx.SetRepoResolver(repoResolver)
				shutdown.vectorIdx = vIdx
				shutdown.indexersWg.Add(1)
				// ждём готовности qdrant/ollama (10 попыток по 2с)
				go func() {
					defer shutdown.indexersWg.Done()
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
			shutdown.watcher = w
			defer w.Close()
			if err := w.Start(ctx); err != nil {
				return err
			}
			if tsIdx != nil {
				shutdown.indexersWg.Add(1)
				go func() {
					defer shutdown.indexersWg.Done()
					_ = tsIdx.FullScan(ctx)
				}()
			}
			if astIdx != nil {
				shutdown.indexersWg.Add(1)
				go func() {
					defer shutdown.indexersWg.Done()
					_ = astIdx.FullScan(ctx)
				}()
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
			} else {
				if err := tui.Run(ctx, bus); err != nil && !errors.Is(err, context.Canceled) {
					return err
				}
			}

			// 6. Graceful shutdown: сначала останавливаем индексацию, потом docker
			shutdown.mu.Lock()
			shutdown.started = true
			shutdown.mu.Unlock()

			// Отменяем контекст — индексаторы начнут завершаться (проверяют ctx.Done())
			cancel()
			// Ждём завершения индексаторов ПЕРЕД остановкой Docker
			if !shutdown.stopIndexers(ctx) {
				fmt.Fprintf(os.Stderr, "graceful: warning: indexers did not complete cleanly\n")
			}
			// Теперь можно безопасно остановить Docker — все запросы завершены
			shutdown.stopDocker(ctx)

			return nil
		},
	}
	c.Flags().StringVar(&envMode, "env", "", "environment mode: 'local' (Qdrant in Docker, rest on host) or 'docker' (all in Docker containers)")
	c.Flags().BoolVar(&skipVector, "skip-vector", false, "disable vector (qdrant) indexing")
	c.Flags().BoolVar(&skipTS, "skip-treesitter", false, "disable tree-sitter indexing")
	c.Flags().BoolVar(&noTUI, "no-tui", false, "no TUI dashboard, just run in background")
	return c
}
