package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"ragota/internal/indexing/ast"
	"ragota/internal/search/bm25"
	"ragota/pkg/config"
	"ragota/internal/indexing/embedder"
	"ragota/pkg/fileutil"
	"ragota/internal/search/graph"
	"ragota/internal/indexing/vector"
	"ragota/pkg/lsp"
	"ragota/pkg/lsp/manager"
	mcppkg "ragota/internal/transport/mcp"
	"ragota/pkg/qdrant"
	"ragota/pkg/repos"
	"ragota/internal/search/rerank"
	"ragota/pkg/state"
	"ragota/internal/store"
	"ragota/internal/transport/tui"
	"ragota/pkg/watcher"

	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
)

// newRunCmd: ai-tools run [-t] [-v] [-l] [-s] [-w] [--env local|docker] [directory]
func newRunCmd() *cobra.Command {
	var (
		enableVector bool
		enableLSP    bool
		enableSymbol bool
		doWatch      bool
		envMode      string
		noTUI        bool
	)
	c := &cobra.Command{
		Use:   "run [directory]",
		Short: "Run unified code MCP server and/or -w indexing",
		Long: "Run the unified ragota-code MCP server and/or watcher.\n" +
			"Flags: -v (vector), -l (LSP), -s (symbol), -w (watch+TUI).\n" +
			"All enabled features are combined into a single unified server.\n" +
			"Environment: --env local (default) or --env docker.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !enableVector && !enableLSP && !enableSymbol && !doWatch {
				return errors.New("nothing to run: pass at least one of -v, -l, -s, -w")
			}
			if envMode != "" && envMode != "local" && envMode != "docker" {
				return fmt.Errorf("invalid --env: %s", envMode)
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
			boot, err := bootstrap(ctx, cfg, bus, envMode)
			if err != nil {
				return err
			}
			defer func() {
				if boot.runner != nil {
					fmt.Fprintf(os.Stderr, "docker: stopping containers...\n")
					_ = boot.runner.Down(context.Background())
				}
			}()

			startDockerAll := envMode == "docker"

			return runCore(ctx, cancel, cfg, bus, boot.resolver, boot.repoSig,
				enableVector, enableLSP, enableSymbol, doWatch, noTUI, startDockerAll)
		},
	}
	c.Flags().BoolVarP(&enableVector, "vec", "v", false, "enable vector search")
	c.Flags().BoolVarP(&enableLSP, "lsp", "l", false, "enable LSP features")
	c.Flags().BoolVarP(&enableSymbol, "sym", "s", false, "enable symbol-aware search")
	c.Flags().BoolVarP(&doWatch, "watch", "w", false, "start indexers and TUI")
	c.Flags().StringVar(&envMode, "env", "", "environment: 'local' or 'docker'")
	c.Flags().BoolVar(&noTUI, "no-tui", false, "don't open TUI (only with -w)")
	return c
}

func runCore(ctx context.Context, cancel context.CancelFunc, cfg *config.Config, bus *state.Bus, repoResolver *repos.Resolver, repoSig string,
	enableVector, enableLSP, enableSymbol, doWatch, noTUI, startDockerAll bool) error {

	var (
		st      *store.SQLite
		vIdx    *vector.Vector
		astIdx  *astindex.Indexer
		qd      *qdrant.Client
		emb     *embedder.Ollama
		lspMgr  *manager.Manager
		bleveIx bm25.Index
		rer     rerank.Reranker
		grSvc   *graph.Service
		err     error
	)
	if enableVector || enableSymbol || doWatch {
		st, err = store.OpenFresh(cfg.SQLitePath(), repoSig)
		if err != nil {
			return fmt.Errorf("sqlite open: %w", err)
		}
		defer st.Close()
	}
	if enableSymbol || doWatch {
		astIdx = astindex.New(cfg, st)
		astIdx.SetBus(bus)
		astIdx.SetRepoResolver(repoResolver)
	}
	if (enableVector || doWatch) && cfg.BM25.Enabled {
		b, berr := bm25.Open(cfg.BM25Path(), cfg.BM25.K1, cfg.BM25.B)
		if berr != nil {
			fmt.Fprintf(os.Stderr, "bm25: open failed: %v\n", berr)
		} else {
			bleveIx = b
			defer bleveIx.Close()
		}
	}
	if enableVector || doWatch {
		qd = qdrant.New(fmt.Sprintf("http://%s:%d", cfg.Qdrant.Host, cfg.Qdrant.Port))
		emb = embedder.New(cfg.Ollama.URL, cfg.Ollama.EmbedModel)
		vIdx = vector.NewVector(cfg, qd, emb, st, bus)
		vIdx.SetRepoResolver(repoResolver)
		if bleveIx != nil {
			vIdx.SetBM25(bm25.AsWriteSink(bleveIx))
		}
	}
	if cfg.Rerank.Enabled {
		rer = rerank.New(rerank.Options{
			URL: cfg.RerankURL(), Model: cfg.Rerank.Model,
			Required: cfg.Rerank.Required, TopN: cfg.Rerank.TopN,
		})
		if vIdx != nil {
			rer.SetSemaphore(vIdx.GetSemaphore())
		}
	}
	if enableLSP {
		specs := buildLSPSpecs(cfg, startDockerAll)
		lspMgr = manager.NewManager(cfg.Root, specs)
		lspMgr.SetRepoResolver(repoResolver)
	}
	if enableSymbol || doWatch {
		grSvc = graph.New(cfg, st)
		if lspMgr != nil {
			grSvc = graph.NewWithLSP(cfg, st, lspMgr)
		}
		grSvc.SetBus(bus)
	}

	if doWatch && vIdx != nil {
		go waitAndScanVector(ctx, qd, emb, vIdx, bus)
	}
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
		if astIdx != nil {
			go func() {
				time.Sleep(2 * time.Second)
				_ = astIdx.FullScan(ctx)
			}()
		}
		go fanoutWatchEvents(ctx, w, vIdx, astIdx)
	}

	var wg sync.WaitGroup
	sseServers := make([]*server.SSEServer, 0, 1)

	// Build unified code server with all enabled features
	codeSrv := mcppkg.NewCodeServer(cfg, st, bus, repoResolver)
	if astIdx != nil {
		codeSrv.SetASTIndex(astIdx)
	}
	if vIdx != nil {
		codeSrv.SetVector(vIdx, qd)
		if bleveIx != nil {
			codeSrv.SetBM25(bm25.AsWriteSink(bleveIx))
		}
		if rer != nil {
			codeSrv.SetReranker(rer)
		}
	}
	if lspMgr != nil {
		codeSrv.SetLSPManager(lspMgr)
	}
	if grSvc != nil {
		codeSrv.SetGraphService(grSvc)
	}

	if !doWatch {
		if vIdx != nil {
			if err := vIdx.Init(ctx); err != nil {
				return fmt.Errorf("vector init: %w", err)
			}
		}
	}
	sseServers = append(sseServers, startSSE(ctx, &wg, codeSrv.Build(), "code", cfg.MCP.Vector))

	runErr := func() error {
		if doWatch && !noTUI {
			if err := tui.Run(ctx, bus); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		}
		<-ctx.Done()
		return nil
	}()

	fmt.Fprintln(os.Stderr, "\nragota: shutting down...")
	cancel()
	shutdownCtx, sc := context.WithTimeout(context.Background(), 5*time.Second)
	defer sc()
	for _, s := range sseServers {
		_ = s.Shutdown(shutdownCtx)
	}
	wg.Wait()
	if vIdx != nil {
		vIdx.Close()
	}
	if lspMgr != nil {
		lspMgr.Close()
	}
	return runErr
}

// buildLSPSpecs строит список LSP-спецификаций.
func buildLSPSpecs(cfg *config.Config, dockerMode bool) []lsp.ServerSpec {
	if dockerMode && cfg.Docker.LSP.Image != "" {
		specs := make([]lsp.ServerSpec, 0, 5)
		for _, lang := range []string{"go", "typescript", "javascript", "python", "java"} {
			specs = append(specs, lsp.ServerSpec{
				Language: lang, Command: getLSPCommand(lang),
				Args: getLSPArgs(lang), LocalRoot: cfg.Root, IsDocker: true,
			})
		}
		return specs
	}
	return cfgToLSPSpecs(cfg)
}
