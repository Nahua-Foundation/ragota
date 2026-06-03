package cli

import (
	"context"
	"fmt"
	"os"

	"ragota/internal/indexing/ast"
	"ragota/internal/indexing/crossrepo/classifier"
	"ragota/internal/indexing/crossrepoindex"
	"ragota/internal/search/bm25"
	"ragota/pkg/config"
	"ragota/internal/indexing/embedder"
	"ragota/internal/search/graph"
	"ragota/internal/indexing/vector"
	"ragota/pkg/lsp/manager"
	mcppkg "ragota/internal/transport/mcp"
	"ragota/pkg/qdrant"
	"ragota/pkg/repos"
	"ragota/internal/search/rerank"
	"ragota/pkg/state"
	"ragota/internal/store"

	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
)

// общая опция --root для serve-команд
func addRootFlag(cmd *cobra.Command, dst *string) {
	cmd.Flags().StringVar(dst, "root", ".", "project root directory")
}

// loadCfg — shortcut с глобальным --config из main.go.
func loadCfg(root string) (*config.Config, error) {
	return config.Load(root, configPath)
}

// resolveWorkspace выполняет auto-discovery репозиториев и возвращает
// резолвер + signature для invalidation БД. Стандартный путь для всех
// serve-команд: даёт согласованный multi-repo контекст.
func resolveWorkspace(cfg *config.Config) (*repos.Resolver, string, error) {
	rs, err := repos.Discover(cfg.Root)
	if err != nil {
		return nil, "", err
	}
	if len(rs) > 1 {
		fmt.Fprintf(os.Stderr, "ai-tools: multi-repo workspace, найдено %d репо:\n", len(rs))
		for _, r := range rs {
			fmt.Fprintf(os.Stderr, "  - %s -> %s\n", r.Name, r.Path)
		}
	}
	return repos.NewResolver(rs), repos.Signature(rs), nil
}

// serveEnv — ресурсы, инициализируемые при старте любой serve-команды.
type serveEnv struct {
	cfg      *config.Config
	resolver *repos.Resolver
	bus      *state.Bus
	st       *store.SQLite
}

// serveBootstrap выполняет общий для всех serve-* команд startup:
// загрузка конфига, resolve workspace, открытие store.
func serveBootstrap(root string) (*serveEnv, error) {
	cfg, err := loadCfg(root)
	if err != nil {
		return nil, err
	}
	resolver, repoSig, err := resolveWorkspace(cfg)
	if err != nil {
		return nil, err
	}
	st, err := store.OpenFresh(cfg.SQLitePath(), repoSig)
	if err != nil {
		return nil, err
	}
	return &serveEnv{
		cfg:      cfg,
		resolver: resolver,
		bus:      state.NewBus(cfg.Root),
		st:       st,
	}, nil
}

func newServeCmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "serve",
		Short: "Run unified ragota-code MCP server over stdio (AST + vector + LSP + graph)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := serveBootstrap(root)
			if err != nil {
				return err
			}
			defer env.st.Close()

			// AST indexer
			astIdx := astindex.New(env.cfg, env.st)
			astIdx.SetBus(env.bus)
			astIdx.SetRepoResolver(env.resolver)

			// LSP manager
			lspMgr := manager.NewManager(env.cfg.Root, cfgToLSPSpecs(env.cfg))
			lspMgr.SetRepoResolver(env.resolver)
			defer lspMgr.Close()

			// Graph service
			gr := graph.NewWithLSP(env.cfg, env.st, lspMgr)
			gr.SetBus(env.bus)

			// Cross-repo индексатор (отдельный, после AST)
			crIdx := crossrepoindex.New(env.resolver, env.st)
			crIdx.InitManifests()
			if env.cfg.Ollama.URL != "" {
				crIdx.SetClassifier(classifier.New(env.cfg.Ollama.URL, "qwen2.5-coder:3b"))
			}
			crIdx.SetBus(env.bus)
			// Запускаем после AST
			if err := crIdx.FullScan(context.Background()); err != nil {
				fmt.Fprintf(os.Stderr, "crossrepo: fullscan failed: %v\n", err)
			}
			gr.SetCrossRepoIndex(crIdx)

			// Vector index
			qd := qdrant.New(fmt.Sprintf("http://%s:%d", env.cfg.Qdrant.Host, env.cfg.Qdrant.Port))
			emb := embedder.New(env.cfg.Ollama.URL, env.cfg.Ollama.EmbedModel)
			vIdx := vector.NewVector(env.cfg, qd, emb, env.st, env.bus)
			vIdx.SetRepoResolver(env.resolver)

			// BM25
			var bleveIx bm25.Index
			if env.cfg.BM25.Enabled {
				if b, berr := bm25.Open(env.cfg.BM25Path(), env.cfg.BM25.K1, env.cfg.BM25.B); berr == nil {
					bleveIx = b
					defer bleveIx.Close()
					vIdx.SetBM25(bm25.AsWriteSink(bleveIx))
				} else {
					fmt.Fprintf(os.Stderr, "bm25 open failed: %v (continuing)\n", berr)
				}
			}

			// Init vector collections
			if err := vIdx.Init(context.Background()); err != nil {
				return fmt.Errorf("init qdrant: %w", err)
			}

			// Reranker
			var rer rerank.Reranker
			if env.cfg.Rerank.Enabled {
				rer = rerank.New(rerank.Options{
					URL:      env.cfg.RerankURL(),
					Model:    env.cfg.Rerank.Model,
					Required: env.cfg.Rerank.Required,
					TopN:     env.cfg.Rerank.TopN,
				})
			}

			// Build unified code server
			srv := mcppkg.NewCodeServer(env.cfg, env.st, env.bus, env.resolver)
			srv.SetASTIndex(astIdx)
			srv.SetVector(vIdx, qd)
			srv.SetLSPManager(lspMgr)
			srv.SetGraphService(gr)
			if bleveIx != nil {
				srv.SetBM25(bm25.AsWriteSink(bleveIx))
			}
			if rer != nil {
				srv.SetReranker(rer)
			}

			return server.ServeStdio(srv.Build())
		},
	}
	addRootFlag(c, &root)
	return c
}
