package main

import (
	"context"
	"fmt"
	"os"

	"ragota/internal/bm25"
	"ragota/internal/config"
	"ragota/internal/embedder"
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

func newServeTreesitterCmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "serve-treesitter",
		Short: "Run tree-sitter MCP server over stdio",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadCfg(root)
			if err != nil {
				return err
			}
			_, repoSig, err := resolveWorkspace(cfg)
			if err != nil {
				return err
			}
			bus := state.NewBus(cfg.Root)
			st, err := store.OpenFresh(cfg.SQLitePath(), repoSig)
			if err != nil {
				return err
			}
			defer st.Close()
			idx := index.NewTreeSitter(cfg, st, bus)
			srv := mcppkg.NewTreeSitterServer(cfg, idx, st, bus).Build()
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
			cfg, err := loadCfg(root)
			if err != nil {
				return err
			}
			resolver, repoSig, err := resolveWorkspace(cfg)
			if err != nil {
				return err
			}
			bus := state.NewBus(cfg.Root)
			qd := qdrant.New(fmt.Sprintf("http://%s:%d", cfg.Qdrant.Host, cfg.Qdrant.Port))
			emb := embedder.New(cfg.Ollama.URL, cfg.Ollama.EmbedModel)
			st, _ := store.OpenFresh(cfg.SQLitePath(), repoSig)
			if st != nil {
				defer st.Close()
			}
			idx := index.NewVector(cfg, qd, emb, st, bus)
			idx.SetRepoResolver(resolver)

			var bleveIx bm25.Index
			if cfg.BM25.Enabled {
				if b, berr := bm25.Open(cfg.BM25Path(), cfg.BM25.K1, cfg.BM25.B); berr == nil {
					bleveIx = b
					defer bleveIx.Close()
					idx.SetBM25(bleveIx)
				} else {
					fmt.Fprintf(os.Stderr, "bm25 open failed: %v (continuing)\n", berr)
				}
			}

			if err := idx.Init(context.Background()); err != nil {
				return fmt.Errorf("init qdrant: %w", err)
			}
			vs := mcppkg.NewVectorServer(cfg, idx, qd, bus)
			if bleveIx != nil {
				vs.SetBM25(bleveIx)
			}
			if cfg.Rerank.Enabled {
				vs.SetReranker(rerank.New(rerank.Options{
					URL:      cfg.RerankURL(),
					Model:    cfg.Rerank.Model,
					Required: cfg.Rerank.Required,
					TopN:     cfg.Rerank.TopN,
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
			cfg, err := loadCfg(root)
			if err != nil {
				return err
			}
			resolver, repoSig, err := resolveWorkspace(cfg)
			if err != nil {
				return err
			}
			bus := state.NewBus(cfg.Root)
			st, _ := store.OpenFresh(cfg.SQLitePath(), repoSig)
			if st != nil {
				defer st.Close()
			}

			var specs []lsp.ServerSpec
			for _, s := range cfg.LSP {
				specs = append(specs, lsp.ServerSpec{
					Language:  s.Language,
					Command:   s.Command,
					Args:      s.Args,
					LocalRoot: cfg.Root,
				})
			}
			mgr := lsp.NewManager(cfg.Root, specs)
			mgr.SetRepoResolver(resolver)
			defer mgr.Close()
			srv := mcppkg.NewLSPServer(cfg, mgr, st, bus).Build()
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
			cfg, err := loadCfg(root)
			if err != nil {
				return err
			}
			resolver, repoSig, err := resolveWorkspace(cfg)
			if err != nil {
				return err
			}
			bus := state.NewBus(cfg.Root)
			st, err := store.OpenFresh(cfg.SQLitePath(), repoSig)
			if err != nil {
				return err
			}
			defer st.Close()
			// LSP-manager для ленивого обогащения графа (calls/implements).
			// При недоступности LSP graph.Service всегда падает обратно на tree-sitter.
			var specs []lsp.ServerSpec
			for _, s := range cfg.LSP {
				specs = append(specs, lsp.ServerSpec{
					Language:  s.Language,
					Command:   s.Command,
					Args:      s.Args,
					LocalRoot: cfg.Root,
				})
			}
			lspMgr := lsp.NewManager(cfg.Root, specs)
			lspMgr.SetRepoResolver(resolver)
			defer lspMgr.Close()
			gr := graph.NewWithLSP(cfg, st, lspMgr)
			gr.SetBus(bus)
			syms := symbols.New(st, gr, nil)
			syms.SetLSPManager(lspMgr)
			// Опционально подключаем similar-search через Vector, если qdrant доступен.
			qd := qdrant.New(fmt.Sprintf("http://%s:%d", cfg.Qdrant.Host, cfg.Qdrant.Port))
			emb := embedder.New(cfg.Ollama.URL, cfg.Ollama.EmbedModel)
			vIdx := index.NewVector(cfg, qd, emb, st, bus)
			vIdx.SetRepoResolver(resolver)
			syms.SetSimilarSearcher(vIdx)

			srv := mcppkg.NewSymbolServer(cfg, st, syms, gr, bus).Build()
			return server.ServeStdio(srv)
		},
	}
	addRootFlag(c, &root)
	return c
}
