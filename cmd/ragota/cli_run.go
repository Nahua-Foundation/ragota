package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"ragota/internal/astindex"
	"ragota/internal/bm25"
	"ragota/internal/config"
	"ragota/internal/docker"
	"ragota/internal/embedder"
	"ragota/internal/fileutil"
	"ragota/internal/graph"
	"ragota/internal/index"
	"ragota/internal/lsp"
	mcppkg "ragota/internal/mcp"
	"ragota/internal/qdrant"
	"ragota/internal/repos"
	"ragota/internal/rerank"
	"ragota/internal/state"
	"ragota/internal/store"
	"ragota/internal/symbols"
	"ragota/internal/tui"
	"ragota/internal/watcher"

	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
)

// newRunCmd: ai-tools run [-t] [-v] [-l] [-w] [--env local|docker] [directory]
//
// Запускает выбранные MCP-сервера (SSE на отдельных портах) и при -w —
// индексаторы + TUI.
//
// Флаг --env управляет окружением:
//   - local (по умолчанию): Qdrant в Docker, LSP/Ollama на хосте
//   - docker: все сервисы (LSP, Ollama, Qdrant) в Docker-контейнерах
//
// Поддерживается слитная запись коротких флагов вида -tvlw, -lvw, -tw
// в любом порядке (см. main.go).
func newRunCmd() *cobra.Command {
	var (
		enableTS     bool
		enableVector bool
		enableLSP    bool
		enableSymbol bool
		doWatch      bool
		envMode      string
		noTUI        bool
	)
	c := &cobra.Command{
		Use:   "run [directory]",
		Short: "Run selected MCP servers (-t -v -l) and/or -w indexing of the directory",
		Long: "Run any combination of MCP servers and the watcher in a single process.\n" +
			"Short flags can be combined: `ai-tools run -tvlw .` is equivalent to\n" +
			"`ai-tools run -t -v -l -w .`. MCP servers are exposed over SSE on ports\n" +
			"from the YAML config (mcp.*) or defaults 7771/7772/7773.\n\n" +
			"Environment mode (--env):\n" +
			"  - local (default): Qdrant in Docker, LSP/Ollama on host\n" +
			"  - docker: Qdrant and LSP in Docker containers",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !enableTS && !enableVector && !enableLSP && !enableSymbol && !doWatch {
				return errors.New("nothing to run: pass at least one of -t, -v, -l, -s, -w")
			}
			// Validate --env flag
			if envMode != "" && envMode != "local" && envMode != "docker" {
				return fmt.Errorf("invalid --env value: %s (must be 'local' or 'docker')", envMode)
			}
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

			// Multi-repo discovery: определяем, какие репозитории
			// обслуживает workspace. См. internal/repos.Discover.
			discovered, err := repos.Discover(cfg.Root)
			if err != nil {
				return fmt.Errorf("repos discover: %w", err)
			}
			if len(discovered) > 1 {
				fmt.Fprintf(os.Stderr, "ai-tools: multi-repo workspace, найдено %d репо:\n", len(discovered))
				for _, r := range discovered {
					mark := ""
					if !r.HasGit {
						mark = " (без .git)"
					}
					fmt.Fprintf(os.Stderr, "  - %s -> %s%s\n", r.Name, r.Path, mark)
				}
			}
			repoResolver := repos.NewResolver(discovered)
			repoSig := repos.Signature(discovered)

			bus := state.NewBus(cfg.Root)

			// 1. Docker — по флагу --env docker (все сервисы) или --env local (только Qdrant).
			// По умолчанию (пустой envMode) — local режим.
			startDockerAll := envMode == "docker"
			startDockerQdrant := envMode == "" || envMode == "local" || startDockerAll

			if startDockerQdrant || startDockerAll {
				runner := docker.New(cfg.Root, cfg.Docker)
				// Гарантируем остановку при выходе
				defer func() {
					fmt.Fprintf(os.Stderr, "docker: stopping containers...\n")
					_ = runner.Down(context.Background())
				}()

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
						fmt.Fprintf(os.Stderr, "docker: all containers are up\n")
						go startDockerMonitor(ctx, runner, bus)
					}
				}
			}

			// 2. В режиме --env docker переключаем URL на Docker-контейнеры
			// Но для подключения с хоста используем localhost (порты проброшены)
			if startDockerAll {
				// Qdrant в Docker — подключаемся через localhost (порт проброшен)
				cfg.Qdrant.Host = "localhost"
			}

			// Общие ресурсы.
			var (
				st      *store.SQLite
				tsIdx   *index.TreeSitter
				vIdx    *index.Vector
				astIdx  *astindex.Indexer
				qd      *qdrant.Client
				emb     *embedder.Ollama
				lspMgr  *lsp.Manager
				bleveIx bm25.Index
				rer     rerank.Reranker
				grSvc   *graph.Service
				symSvc  *symbols.Service
			)
			if enableTS || enableVector || enableSymbol || doWatch {
				// OpenFresh инвалидирует БД при смене состава репо/корня.
				st, err = store.OpenFresh(cfg.SQLitePath(), repoSig)
				if err != nil {
					return fmt.Errorf("sqlite open: %w", err)
				}
				defer st.Close()
			}
			if enableTS || doWatch {
				tsIdx = index.NewTreeSitter(cfg, st, bus)
			}
			// AST units / edges — нужны для symbol-aware MCP и graph expansion.
			if enableSymbol || enableTS || doWatch {
				astIdx = astindex.New(cfg, st)
				astIdx.SetBus(bus)
				astIdx.SetRepoResolver(repoResolver)
			}
			// BM25 (Bleve) — лексический индекс для hybrid retrieval.
			if (enableVector || doWatch) && cfg.BM25.Enabled {
				b, berr := bm25.Open(cfg.BM25Path(), cfg.BM25.K1, cfg.BM25.B)
				if berr != nil {
					fmt.Fprintf(os.Stderr, "bm25: open %s failed: %v (continuing without BM25)\n", cfg.BM25Path(), berr)
				} else {
					bleveIx = b
					defer bleveIx.Close()
				}
			}
			if enableVector || doWatch {
				qd = qdrant.New(fmt.Sprintf("http://%s:%d", cfg.Qdrant.Host, cfg.Qdrant.Port))
				emb = embedder.New(cfg.Ollama.URL, cfg.Ollama.EmbedModel)
				vIdx = index.NewVector(cfg, qd, emb, st, bus)
				vIdx.SetRepoResolver(repoResolver)
				if bleveIx != nil {
					vIdx.SetBM25(bleveIx)
				}
			}
			if cfg.Rerank.Enabled {
				rer = rerank.New(rerank.Options{
					URL:      cfg.RerankURL(),
					Model:    cfg.Rerank.Model,
					Required: cfg.Rerank.Required,
					TopN:     cfg.Rerank.TopN,
				})
				if vIdx != nil {
					rer.SetSemaphore(vIdx.GetSemaphore())
				}
			}
			if enableSymbol || doWatch {
				grSvc = graph.New(cfg, st)
				if lspMgr != nil {
					grSvc = graph.NewWithLSP(cfg, st, lspMgr)
				}
				grSvc.SetBus(bus)
				symSvc = symbols.New(st, grSvc, nil)
				if vIdx != nil {
					symSvc.SetSimilarSearcher(vIdx)
				}
			}
			if enableLSP {
				var specs []lsp.ServerSpec
				if startDockerAll && cfg.Docker.LSP.Image != "" {
					// В режиме --env docker используем LSP из Docker-контейнера
					// Все языки обслуживаются одним контейнером
					languages := []string{"go", "typescript", "javascript", "python", "java"}
					for _, lang := range languages {
						specs = append(specs, lsp.ServerSpec{
							Language:  lang,
							Command:   getLSPCommand(lang),
							Args:      getLSPArgs(lang),
							LocalRoot: cfg.Root,
							IsDocker:  true,
						})
					}
				} else {
					// В режиме --env local используем локальные LSP-серверы
					for _, s := range cfg.LSP {
						specs = append(specs, lsp.ServerSpec{
							Language:  s.Language,
							Command:   s.Command,
							Args:      s.Args,
							LocalRoot: cfg.Root,
						})
					}
				}
				lspMgr = lsp.NewManager(cfg.Root, specs)
				lspMgr.SetRepoResolver(repoResolver)
				// Не закрываем менеджер здесь - он нужен для работы SSE серверов.
				// Закроется после shutdown SSE серверов в конце функции.
			}

			// 3. Готовность Qdrant/Ollama и full-scan vector.
			if doWatch && vIdx != nil {
				go waitAndScanVector(ctx, qd, emb, vIdx, bus)
			}

			// 3. Watcher + полное сканирование tree-sitter и AST units.
			var w *watcher.Watcher
			if doWatch {
				matcher := fileutil.NewMatcher(cfg.Ignore)
				w, err = watcher.New(cfg.Root, matcher, cfg.Extensions, 300*time.Millisecond)
				if err != nil {
					return err
				}
				w.SetRepoResolver(repoResolver)
				defer w.Close()
				if err := w.Start(ctx); err != nil {
					return err
				}
				if tsIdx != nil {
					time.Sleep(1 * time.Second) // Даем немного времени TUI и другим процессам
					go func() { _ = tsIdx.FullScan(ctx) }()
				}
				if astIdx != nil {
					// Не запускаем одновременно с TS чтобы не грузить SQLite
					go func() {
						time.Sleep(2 * time.Second)
						_ = astIdx.FullScan(ctx)
					}()
				}
				go fanoutWatchEvents(ctx, w, tsIdx, vIdx, astIdx)
			}

			// 4. MCP-серверы.
			var wg sync.WaitGroup
			sseServers := make([]*server.SSEServer, 0, 3)

			if enableTS {
				port := cfg.MCP.TreeSitter
				mcpSrv := mcppkg.NewTreeSitterServer(cfg, tsIdx, st, bus).Build()
				sseServers = append(sseServers, startSSE(ctx, &wg, mcpSrv, "tree-sitter", port))
			}
			if enableVector {
				port := cfg.MCP.Vector
				if !doWatch {
					if err := vIdx.Init(ctx); err != nil {
						return fmt.Errorf("vector init: %w", err)
					}
				}
				vs := mcppkg.NewVectorServer(cfg, vIdx, qd, bus)
				if bleveIx != nil {
					vs.SetBM25(bleveIx)
				}
				if rer != nil {
					vs.SetReranker(rer)
				}
				sseServers = append(sseServers, startSSE(ctx, &wg, vs.Build(), "vector", port))
			}
			if enableLSP {
				port := cfg.MCP.LSP
				mcpSrv := mcppkg.NewLSPServer(cfg, lspMgr, st, bus).Build()
				sseServers = append(sseServers, startSSE(ctx, &wg, mcpSrv, "lsp", port))
			}
			if enableSymbol {
				port := cfg.MCP.Symbol
				mcpSrv := mcppkg.NewSymbolServer(cfg, st, symSvc, grSvc, bus).Build()
				sseServers = append(sseServers, startSSE(ctx, &wg, mcpSrv, "symbol", port))
			}

			// 5. UI / блокировка.
			runErr := func() error {
				if doWatch && !noTUI {
					if err := tui.Run(ctx, bus); err != nil && !errors.Is(err, context.Canceled) {
						return err
					}
					return nil
				}
				if len(sseServers) > 0 {
					fmt.Fprintln(os.Stderr, "ai-tools run: MCP SSE servers started, Ctrl+C to stop")
				}
				<-ctx.Done()
				return nil
			}()

			// 6. Грейсфул-стоп.
			// Сначала отменяем контекст, чтобы остановить все фоновые goroutines,
			// которые его слушают (waitAndScanVector, fanoutWatchEvents, и т.д.)
			fmt.Fprintln(os.Stderr, "\nragota: shutting down...")
			cancel()

			shutdownCtx, sc := context.WithTimeout(context.Background(), 5*time.Second)
			defer sc()
			fmt.Fprintln(os.Stderr, "ragota: stopping SSE servers...")
			for _, s := range sseServers {
				_ = s.Shutdown(shutdownCtx)
			}
			fmt.Fprintln(os.Stderr, "ragota: waiting for MCP servers...")
			wg.Wait()
			// Дожидаемся завершения фоновых задач индексатора перед остановкой Docker
			if vIdx != nil {
				fmt.Fprintln(os.Stderr, "ragota: closing vector indexer...")
				vIdx.Close()
			}
			// Закрываем LSP менеджер после остановки SSE серверов
			if lspMgr != nil {
				fmt.Fprintln(os.Stderr, "ragota: closing LSP manager...")
				lspMgr.Close()
			}
			fmt.Fprintln(os.Stderr, "ragota: shutdown complete.")
			return runErr
		},
	}
	c.Flags().BoolVarP(&enableTS, "ts", "t", false, "run tree-sitter MCP server")
	c.Flags().BoolVarP(&enableVector, "vec", "v", false, "run vector (qdrant+ollama) MCP server")
	c.Flags().BoolVarP(&enableLSP, "lsp", "l", false, "run LSP-multiplexer MCP server")
	c.Flags().BoolVarP(&enableSymbol, "sym", "s", false, "run symbol-aware MCP server (AST units + code graph)")
	c.Flags().BoolVarP(&doWatch, "watch", "w", false, "start indexers and TUI dashboard for the directory")
	c.Flags().StringVar(&envMode, "env", "", "environment mode: 'local' (Qdrant in Docker, rest on host) or 'docker' (all in Docker containers)")
	c.Flags().BoolVar(&noTUI, "no-tui", false, "don't open TUI dashboard (only with -w)")
	return c
}

// startSSE поднимает SSE-обёртку над MCPServer на указанном порту.
func startSSE(_ context.Context, wg *sync.WaitGroup, mcp *server.MCPServer, name string, port int) *server.SSEServer {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	sse := server.NewSSEServer(mcp, server.WithBaseURL(baseURL))
	fmt.Fprintf(os.Stderr, "mcp[%s]: serving SSE on %s\n", name, baseURL)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := sse.Start(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "mcp[%s]: SSE server error: %v\n", name, err)
		}
	}()
	return sse
}

// waitAndScanVector ждёт готовности qdrant и ollama, затем инициализирует
// коллекцию и делает full-scan.
func waitAndScanVector(ctx context.Context, qd *qdrant.Client, emb *embedder.Ollama, vIdx *index.Vector, bus *state.Bus) {
	for range 30 {
		if ctx.Err() != nil {
			return
		}
		pCtx, c2 := context.WithTimeout(ctx, 3*time.Second)
		qErr := qd.Ping(pCtx)
		oErr := emb.Ping(pCtx)
		c2()
		if qErr == nil && oErr == nil {
			// Даем Ollama немного времени на прогрев после старта контейнера, если нужно
			time.Sleep(1 * time.Second)
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
}

// fanoutWatchEvents транслирует события watcher'а во все индексаторы.
func fanoutWatchEvents(ctx context.Context, w *watcher.Watcher, tsIdx *index.TreeSitter, vIdx *index.Vector, astIdx *astindex.Indexer) {
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
}

// startDockerMonitor периодически обновляет статус контейнеров в bus для TUI.
func startDockerMonitor(ctx context.Context, r *docker.Runner, bus *state.Bus) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ps, err := r.Ps(ctx)
			s := state.DockerStatus{Running: true}
			if err != nil {
				s.LastError = err.Error()
			} else {
				for _, p := range ps {
					s.Services = append(s.Services, fmt.Sprintf("%s(%s)", p.Name, p.State))
				}
			}
			bus.SetDocker(s)
		}
	}
}

// getLSPCommand возвращает команду для LSP-сервера по языку.
func getLSPCommand(lang string) string {
	switch lang {
	case "go":
		return "gopls"
	case "typescript", "javascript":
		return "typescript-language-server"
	case "python":
		return "pyright-langserver"
	case "java":
		return "jdtls"
	default:
		return lang + "-language-server"
	}
}

// getLSPArgs возвращает аргументы для LSP-сервера по языку.
func getLSPArgs(lang string) []string {
	switch lang {
	case "typescript", "javascript", "python":
		return []string{"--stdio"}
	case "java":
		// jdtls требует указания data directory
		return []string{
			// ВАЖНО: argparse в jdtls трактует значение, начинающееся с "--",
			// как следующий флаг. Используем форму --jvm-arg=VALUE.
			"--jvm-arg=-Xmx4G",
			"--jvm-arg=--add-opens=java.base/sun.misc=ALL-UNNAMED",
			"--jvm-arg=--add-opens=java.base/java.lang.reflect=ALL-UNNAMED",
			"--jvm-arg=--add-opens=java.base/java.util=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.api=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.util=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.code=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.main=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.tree=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.model=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.comp=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.file=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.jvm=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.parser=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.processing=ALL-UNNAMED",
			"-data", "/workspace/.ragota/jdtls-data",
		}
	default:
		return nil
	}
}
