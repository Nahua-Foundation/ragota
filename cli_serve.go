package main

import (
	"context"
	"fmt"

	"aitools/internal/config"
	"aitools/internal/embedder"
	"aitools/internal/index"
	"aitools/internal/lsp"
	mcppkg "aitools/internal/mcp"
	"aitools/internal/qdrant"
	"aitools/internal/state"
	"aitools/internal/store"

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
		Short: "Run vector (qdrant+ollama) MCP server over stdio",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadCfg(root)
			if err != nil {
				return err
			}
			bus := state.NewBus(cfg.Root)
			qd := qdrant.New(fmt.Sprintf("http://%s:%d", cfg.Qdrant.Host, cfg.Qdrant.Port))
			emb := embedder.New(cfg.Ollama.URL, cfg.Ollama.EmbedModel)
			st, _ := store.Open(cfg.SQLitePath()) // опционально для кэша
			if st != nil {
				defer st.Close()
			}
			idx := index.NewVector(cfg, qd, emb, st, bus)
			if err := idx.Init(context.Background()); err != nil {
				return fmt.Errorf("init qdrant: %w", err)
			}
			srv := mcppkg.NewVectorServer(cfg, idx, qd, bus).Build()
			return server.ServeStdio(srv)
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
			mgr := lsp.NewManager(cfg.Root, nil)
			defer mgr.Close()
			srv := mcppkg.NewLSPServer(cfg, mgr, bus).Build()
			return server.ServeStdio(srv)
		},
	}
	addRootFlag(c, &root)
	return c
}
