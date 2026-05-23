package main

import (
	"context"
	"fmt"
	"os"

	"aitools/internal/bm25"
	"aitools/internal/config"
	"aitools/internal/embedder"
	"aitools/internal/graph"
	"aitools/internal/index"
	"aitools/internal/lsp"
	mcppkg "aitools/internal/mcp"
	"aitools/internal/qdrant"
	"aitools/internal/rerank"
	"aitools/internal/state"
	"aitools/internal/store"
	"aitools/internal/symbols"

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
			bus := state.NewBus(cfg.Root)
			st, err := store.Open(cfg.SQLitePath())
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
			bus := state.NewBus(cfg.Root)
			qd := qdrant.New(fmt.Sprintf("http://%s:%d", cfg.Qdrant.Host, cfg.Qdrant.Port))
			emb := embedder.New(cfg.Ollama.URL, cfg.Ollama.EmbedModel)
			st, _ := store.Open(cfg.SQLitePath())
			if st != nil {
				defer st.Close()
			}
			idx := index.NewVector(cfg, qd, emb, st, bus)

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
			bus := state.NewBus(cfg.Root)
			st, _ := store.Open(cfg.SQLitePath())
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
			bus := state.NewBus(cfg.Root)
			st, err := store.Open(cfg.SQLitePath())
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
			defer lspMgr.Close()
			gr := graph.NewWithLSP(st, lspMgr)
			syms := symbols.New(st, gr, nil)
			// Опционально подключаем similar-search через Vector, если qdrant доступен.
			qd := qdrant.New(fmt.Sprintf("http://%s:%d", cfg.Qdrant.Host, cfg.Qdrant.Port))
			emb := embedder.New(cfg.Ollama.URL, cfg.Ollama.EmbedModel)
			vIdx := index.NewVector(cfg, qd, emb, st, bus)
			syms.SetSimilarSearcher(vIdx)

			srv := mcppkg.NewSymbolServer(cfg, st, syms, gr, bus).Build()
			return server.ServeStdio(srv)
		},
	}
	addRootFlag(c, &root)
	return c
}
