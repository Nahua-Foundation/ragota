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

	"aitools/internal/astindex"
	"aitools/internal/bm25"
	"aitools/internal/config"
	"aitools/internal/docker"
	"aitools/internal/embedder"
	"aitools/internal/fileutil"
	"aitools/internal/graph"
	"aitools/internal/index"
	"aitools/internal/lsp"
	mcppkg "aitools/internal/mcp"
	"aitools/internal/qdrant"
	"aitools/internal/rerank"
	"aitools/internal/state"
	"aitools/internal/store"
	"aitools/internal/symbols"
	"aitools/internal/tui"
	"aitools/internal/watcher"

	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
)

// newRunCmd: ai-tools run [-t] [-v] [-l] [-w] [--start-docker] [directory]
//
// Запускает выбранные MCP-сервера (SSE на отдельных портах) и при -w —
// индексаторы + TUI. Docker по умолчанию НЕ поднимается; включается
// --start-docker и поднимает контейнеры из секции docker конфига
// (без docker-compose.yaml). Поддерживается слитная запись коротких
// флагов вида -tvlw, -lvw, -tw в любом порядке (см. main.go).
func newRunCmd() *cobra.Command {
	var (
		enableTS     bool
		enableVector bool
		enableLSP    bool
		enableSymbol bool
		doWatch      bool
		startDocker  bool
		noTUI        bool
	)
	c := &cobra.Command{
		Use:   "run [directory]",
		Short: "Run selected MCP servers (-t -v -l) and/or -w indexing of the directory",
		Long: "Run any combination of MCP servers and the watcher in a single process.\n" +
			"Short flags can be combined: `ai-tools run -tvlw .` is equivalent to\n" +
			"`ai-tools run -t -v -l -w .`. MCP servers are exposed over SSE on ports\n" +
			"from the YAML config (mcp.*) or defaults 7771/7772/7773.\n\n" +
			"Docker containers (qdrant) are NOT started by default. Pass\n" +
			"--start-docker to spin them up using the `docker:` section of the config.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !enableTS && !enableVector && !enableLSP && !enableSymbol && !doWatch {
				return errors.New("nothing to run: pass at least one of -t, -v, -l, -s, -w")
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

			bus := state.NewBus(cfg.Root)

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
				st, err = store.Open(cfg.SQLitePath())
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
			}
			if enableSymbol || doWatch {
				grSvc = graph.New(st)
				symSvc = symbols.New(st, grSvc, nil)
				if vIdx != nil {
					symSvc.SetSimilarSearcher(vIdx)
				}
			}
			if enableLSP {
				lspMgr = lsp.NewManager(cfg.Root, nil)
				defer lspMgr.Close()
			}

			// 1. Docker — ТОЛЬКО по явному флагу.
			if startDocker {
				runner := docker.New(cfg.Root, cfg.Docker)
				if err := docker.Available(ctx); err != nil {
					bus.SetDocker(state.DockerStatus{LastError: err.Error()})
				} else {
					go startDockerNative(ctx, runner, bus)
				}
			}

			// 2. Готовность Qdrant/Ollama и full-scan vector.
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
				mcpSrv := mcppkg.NewLSPServer(cfg, lspMgr, bus).Build()
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
			shutdownCtx, sc := context.WithTimeout(context.Background(), 3*time.Second)
			defer sc()
			for _, s := range sseServers {
				_ = s.Shutdown(shutdownCtx)
			}
			wg.Wait()
			return runErr
		},
	}
	c.Flags().BoolVarP(&enableTS, "ts", "t", false, "run tree-sitter MCP server")
	c.Flags().BoolVarP(&enableVector, "vec", "v", false, "run vector (qdrant+ollama) MCP server")
	c.Flags().BoolVarP(&enableLSP, "lsp", "l", false, "run LSP-multiplexer MCP server")
	c.Flags().BoolVarP(&enableSymbol, "sym", "s", false, "run symbol-aware MCP server (AST units + code graph)")
	c.Flags().BoolVarP(&doWatch, "watch", "w", false, "start indexers and TUI dashboard for the directory")
	c.Flags().BoolVar(&startDocker, "start-docker", false,
		"start qdrant+ollama containers using the docker: section of the config")
	c.Flags().BoolVar(&noTUI, "no-tui", false, "don't open TUI dashboard (only with -w)")
	return c
}

// startSSE поднимает SSE-обёртку над MCPServer на указанном порту.
func startSSE(_ context.Context, wg *sync.WaitGroup, mcp *server.MCPServer, name string, port int) *server.SSEServer {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	sse := server.NewSSEServer(mcp, server.WithBaseURL(baseURL))
	wg.Add(1)
	go func() {
		defer wg.Done()
		fmt.Fprintf(os.Stderr, "mcp[%s]: serving SSE on %s\n", name, baseURL)
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

// startDockerNative поднимает контейнеры через нативный runner и периодически
// обновляет статус в bus для TUI.
func startDockerNative(ctx context.Context, r *docker.Runner, bus *state.Bus) {
	bus.SetDocker(state.DockerStatus{LastError: "starting..."})
	if err := r.Up(ctx); err != nil {
		bus.SetDocker(state.DockerStatus{LastError: err.Error()})
		return
	}
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
