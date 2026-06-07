package cli

// cli_up.go — ragota up [dir]: Docker + индексация + TUI + MCP SSE сервер.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	astindex "ragota/internal/indexing/ast"
	"ragota/internal/indexing/crossrepo/classifier"
	"ragota/internal/indexing/crossrepoindex"
	"ragota/internal/search/bm25"
	"ragota/pkg/config"
	"ragota/internal/search/graph"
	"ragota/internal/indexing/vector"
	"ragota/pkg/logger"
	"ragota/pkg/lsp"
	"ragota/pkg/lsp/manager"
	mcppkg "ragota/internal/transport/mcp"
	"ragota/pkg/qdrant"
	"ragota/internal/search/rerank"
	"ragota/pkg/state"
	"ragota/internal/transport/tui"
	"ragota/pkg/watcher"

	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
)

func newUpCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "up [directory]",
		Short: "Start docker, indexing, TUI dashboard and MCP SSE server",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			// Преобразуем в абсолютный путь ДО загрузки конфига
			// чтобы .ragota/ создавалась в целевой директории, а не в CWD
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			cfg, err := config.Load(absDir)
			if err != nil {
				return err
			}
			if err := cfg.EnsureDataDir(); err != nil {
				return err
			}

			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			bus := state.NewBus(cfg.Root)

			// Подключаем TUI-хук к логгеру: warn/error логи отображаются в TUI
			logger.SetHook(logger.NewTUIHook(bus))

			// Docker — всегда запускается
			boot, err := bootstrap(ctx, cfg, bus)
			if err != nil {
				cancel()
				return err
			}
			defer boot.cleanup()

			// AST indexer
			astIdx := astindex.New(cfg, boot.st)
			astIdx.SetBus(bus)
			astIdx.SetRepoResolver(boot.resolver)

			// Cross-repo indexer
			crIdx := crossrepoindex.New(boot.resolver, boot.st)
			crIdx.InitManifests()
			if cfg.Ollama.URL != "" {
				crIdx.SetClassifier(classifier.New(cfg.Ollama.URL, "qwen2.5-coder:3b"))
			}
			crIdx.SetBus(bus)

			// BM25 — создаём ДО vector indexer чтобы избежать race condition
			// (FullScan запускается асинхронно в createVectorIndexer)
			var bleveIx bm25.Index
			if cfg.BM25.Enabled {
				if b, berr := bm25.Open(cfg.BM25Path(), cfg.BM25.K1, cfg.BM25.B); berr == nil {
					bleveIx = b
					defer bleveIx.Close()
				}
			}

			// Vector indexer
			vIdx := boot.createVectorIndexer(ctx, cfg, bus, bleveIx)

			// LSP manager
			lspMgr := manager.NewManager(cfg.Root, cfgToLSPSpecs(cfg))
			lspMgr.SetRepoResolver(boot.resolver)
			defer lspMgr.Close()

			// Graph service
			grSvc := graph.NewWithLSP(cfg, boot.st, lspMgr)
			grSvc.SetBus(bus)
			grSvc.SetCrossRepoIndex(crIdx)

			// Reranker
			var rer rerank.Reranker
			if cfg.Rerank.Enabled {
				rer = rerank.New(rerank.Options{
					URL: cfg.RerankURL(), Model: cfg.Rerank.Model,
					Required: cfg.Rerank.Required, TopN: cfg.Rerank.TopN,
				})
				if vIdx != nil {
					rer.SetSemaphore(vIdx.GetSemaphore())
				}
			}

			// Watcher
			w, err := boot.createWatcher(ctx, cfg)
			if err != nil {
				cancel()
				return err
			}
			defer w.Close()

			// FullScan после старта watcher
			go func() {
				_ = astIdx.FullScan(ctx)
				_ = crIdx.FullScan(ctx)
			}()
			go fanoutWatchEvents(ctx, w, vIdx, astIdx, crIdx)

			// Build unified code server
			codeSrv := mcppkg.NewCodeServer(cfg, boot.st, bus, boot.resolver)
			codeSrv.SetASTIndex(astIdx)
			if vIdx != nil {
				codeSrv.SetVector(vIdx, qdrant.New(cfg.QdrantURL))
				if bleveIx != nil {
					codeSrv.SetBM25(bm25.AsWriteSink(bleveIx))
				}
				if rer != nil {
					codeSrv.SetReranker(rer)
				}
			}
			codeSrv.SetLSPManager(lspMgr)
			codeSrv.SetGraphService(grSvc)

			// Start MCP SSE server
			var wg sync.WaitGroup
			sseServers := make([]*server.SSEServer, 0, 1)
			sseServers = append(sseServers, startSSE(ctx, &wg, codeSrv.Build(), "code", cfg.MCPPort))

			// Run TUI
			var runErr error
			if err := tui.Run(ctx, bus); err != nil && !errors.Is(err, context.Canceled) {
				runErr = err
			}

			// Shutdown
			fmt.Fprintln(os.Stderr, "\nragota: shutting down...")
			cancel()
			shutdownCtx, sc := context.WithTimeout(context.Background(), 30*time.Second)
			defer sc()
			for _, s := range sseServers {
				_ = s.Shutdown(shutdownCtx)
			}
			wg.Wait()

			boot.stopDocker(shutdownCtx)
			return runErr
		},
	}
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

// fanoutWatchEvents транслирует события watcher'а во все индексаторы.
func fanoutWatchEvents(ctx context.Context, w *watcher.Watcher, vIdx *vector.Vector, astIdx *astindex.Indexer, crIdx *crossrepoindex.Indexer) {
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
				if vIdx != nil {
					_ = vIdx.RemoveFile(ctx, ev.AbsPath)
				}
				if astIdx != nil {
					_ = astIdx.RemoveFile(ctx, ev.AbsPath)
				}
				if crIdx != nil {
					_ = crIdx.RemoveFile(ctx, ev.AbsPath)
				}
			default:
				if vIdx != nil {
					_ = vIdx.IndexFile(ctx, ev.AbsPath)
				}
				if astIdx != nil {
					_ = astIdx.IndexFile(ctx, ev.AbsPath)
				}
				if crIdx != nil {
					_ = crIdx.IndexFile(ctx, ev.AbsPath)
				}
			}
		}
	}
}

// cfgToLSPSpecs конвертирует cfg.LSP в []lsp.ServerSpec.
func cfgToLSPSpecs(cfg *config.Config) []lsp.ServerSpec {
	specs := make([]lsp.ServerSpec, 0, len(cfg.LSP))
	for _, s := range cfg.LSP {
		specs = append(specs, lsp.ServerSpec{
			Language:  s.Language,
			Command:   s.Command,
			Args:      s.Args,
			LocalRoot: cfg.Root,
			IsDocker:  true, // LSP серверы работают внутри контейнера ragota-lsp
		})
	}
	return specs
}
