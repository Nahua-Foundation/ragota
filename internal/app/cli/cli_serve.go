package cli

import (
	"context"
	"fmt"
	"os"

	"ragota/internal/search/bm25"
	"ragota/pkg/config"
	"ragota/internal/indexing/embedder"
	"ragota/internal/indexing/ast"
	"ragota/internal/search/graph"
	"ragota/internal/indexing/vector"
	"ragota/pkg/lsp/manager"
	mcppkg "ragota/internal/transport/mcp"
	"ragota/pkg/qdrant"
	"ragota/pkg/repos"
	"ragota/internal/search/rerank"
	"ragota/pkg/state"
	"ragota/internal/store"
	"ragota/internal/search/symbols"

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

func newServeTreesitterCmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "serve-treesitter",
		Short: "Run AST/tree-sitter MCP server over stdio",
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := serveBootstrap(root)
			if err != nil {
				return err
			}
			defer env.st.Close()
			idx := astindex.New(env.cfg, env.st)
			idx.SetBus(env.bus)
			idx.SetRepoResolver(env.resolver)
			srv := mcppkg.NewTreeSitterServer(env.cfg, idx, env.st, env.bus).Build()
			return server.ServeStdio(srv)
		},
	}
	addRootFlag(c, &root)
	return c
}

func newServeVectorCmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "serve-vector",
		Short: "Run vector (qdrant+ollama) + BM25 + reranker MCP server over stdio",
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := serveBootstrap(root)
			if err != nil {
				return err
			}
			defer env.st.Close()
			qd := qdrant.New(fmt.Sprintf("http://%s:%d", env.cfg.Qdrant.Host, env.cfg.Qdrant.Port))
			emb := embedder.New(env.cfg.Ollama.URL, env.cfg.Ollama.EmbedModel)
			idx := vector.NewVector(env.cfg, qd, emb, env.st, env.bus)
			idx.SetRepoResolver(env.resolver)

			var bleveIx bm25.Index
			if env.cfg.BM25.Enabled {
				if b, berr := bm25.Open(env.cfg.BM25Path(), env.cfg.BM25.K1, env.cfg.BM25.B); berr == nil {
					bleveIx = b
					defer bleveIx.Close()
					idx.SetBM25(bm25.AsWriteSink(bleveIx))
				} else {
					fmt.Fprintf(os.Stderr, "bm25 open failed: %v (continuing)\n", berr)
				}
			}

			if err := idx.Init(context.Background()); err != nil {
				return fmt.Errorf("init qdrant: %w", err)
			}
			vs := mcppkg.NewVectorServer(env.cfg, idx, qd, env.bus)
			if bleveIx != nil {
				vs.SetBM25(bm25.AsWriteSink(bleveIx))
			}
			if env.cfg.Rerank.Enabled {
				vs.SetReranker(rerank.New(rerank.Options{
					URL:      env.cfg.RerankURL(),
					Model:    env.cfg.Rerank.Model,
					Required: env.cfg.Rerank.Required,
					TopN:     env.cfg.Rerank.TopN,
				}))
			}
			return server.ServeStdio(vs.Build())
		},
	}
	addRootFlag(c, &root)
	return c
}

func newServeLSPCmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "serve-lsp",
		Short: "Run LSP-multiplexer MCP server over stdio (go/typescript/python/java)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := serveBootstrap(root)
			if err != nil {
				return err
			}
			defer env.st.Close()

			mgr := manager.NewManager(env.cfg.Root, cfgToLSPSpecs(env.cfg))
			mgr.SetRepoResolver(env.resolver)
			defer mgr.Close()
			srv := mcppkg.NewLSPServer(env.cfg, mgr, env.st, env.bus).Build()
			return server.ServeStdio(srv)
		},
	}
	addRootFlag(c, &root)
	return c
}

func newServeSymbolCmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "serve-symbol",
		Short: "Run symbol-aware MCP server over stdio (AST units + code graph)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := serveBootstrap(root)
			if err != nil {
				return err
			}
			defer env.st.Close()
			// LSP-manager для ленивого обогащения графа (calls/implements).
			// При недоступности LSP graph.Service всегда падает обратно на tree-sitter.
			lspMgr := manager.NewManager(env.cfg.Root, cfgToLSPSpecs(env.cfg))
			lspMgr.SetRepoResolver(env.resolver)
			defer lspMgr.Close()
			gr := graph.NewWithLSP(env.cfg, env.st, lspMgr)
			gr.SetBus(env.bus)
			syms := symbols.New(env.st, gr, nil)
			syms.SetLSPManager(lspMgr)
			// Опционально подключаем similar-search через Vector, если qdrant доступен.
			qd := qdrant.New(fmt.Sprintf("http://%s:%d", env.cfg.Qdrant.Host, env.cfg.Qdrant.Port))
			emb := embedder.New(env.cfg.Ollama.URL, env.cfg.Ollama.EmbedModel)
			vIdx := vector.NewVector(env.cfg, qd, emb, env.st, env.bus)
			vIdx.SetRepoResolver(env.resolver)
			syms.SetSimilarSearcher(vIdx)

			srv := mcppkg.NewSymbolServer(env.cfg, env.st, syms, gr, env.bus).Build()
			return server.ServeStdio(srv)
		},
	}
	addRootFlag(c, &root)
	return c
}
