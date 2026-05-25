package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"ragota/internal/config"
	"github.com/spf13/cobra"
)

type mcpServerConfig struct {
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	URL     string   `json:"url,omitempty"`
	Trust   bool     `json:"trust"`
}

type mcpConfigResponse struct {
	MCPServers map[string]mcpServerConfig `json:"mcpServers"`
	MCP        map[string]mcpServerConfig `json:"mcp"`
}

func newMcpConfigCmd() *cobra.Command {
	var root string
	var mode string
	c := &cobra.Command{
		Use:   "mcp-config",
		Short: "Generate JSON config for MCP clients (Claude Desktop, etc.)",
		Long: "Reads the configuration and generates a JSON snippet with all three MCP servers for use in agent configurations.\n" +
			"By default, it generates SSE config (connecting to a running `ai-tools run`). Use --mode=stdio for direct execution.",
		RunE: func(cmd *cobra.Command, args []string) error {
			absRoot, err := filepath.Abs(root)
			if err != nil {
				absRoot = root
			}

			cfg, err := config.Load(absRoot, configPath)
			if err != nil {
				return err
			}

			resp := mcpConfigResponse{
				MCPServers: make(map[string]mcpServerConfig),
				MCP:        make(map[string]mcpServerConfig),
			}

			if mode == "sse" {
				mcpConfigs := map[string]mcpServerConfig{
					"ai-tools-treesitter": {
						URL:   fmt.Sprintf("http://127.0.0.1:%d/sse", cfg.MCP.TreeSitter),
						Trust: false,
					},
					"ai-tools-vector": {
						URL:   fmt.Sprintf("http://127.0.0.1:%d/sse", cfg.MCP.Vector),
						Trust: false,
					},
					"ai-tools-lsp": {
						URL:   fmt.Sprintf("http://127.0.0.1:%d/sse", cfg.MCP.LSP),
						Trust: false,
					},
					"ai-tools-symbol": {
						URL:   fmt.Sprintf("http://127.0.0.1:%d/sse", cfg.MCP.Symbol),
						Trust: false,
					},
				}
				for k, v := range mcpConfigs {
					resp.MCPServers[k] = v
					resp.MCP[k] = v
				}
			} else {
				exe, err := os.Executable()
				if err != nil {
					return fmt.Errorf("failed to get executable path: %w", err)
				}
				absExe, err := filepath.Abs(exe)
				if err != nil {
					absExe = exe
				}
				cfgPath, err := config.ResolveConfigPath(absRoot, configPath)
				if err != nil {
					return err
				}

				// Stdio mode
				servers := []struct {
					name string
					cmd  string
				}{
					{"ai-tools-treesitter", "serve-treesitter"},
					{"ai-tools-vector", "serve-vector"},
					{"ai-tools-lsp", "serve-lsp"},
					{"ai-tools-symbol", "serve-symbol"},
				}

				for _, s := range servers {
					cfg := mcpServerConfig{
						Command: absExe,
						Args:    []string{s.cmd, "--config", cfgPath, "--root", absRoot},
					}
					resp.MCPServers[s.name] = cfg
					resp.MCP[s.name] = cfg
				}
			}

			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			return encoder.Encode(resp)
		},
	}
	c.Flags().StringVar(&mode, "mode", "sse", "Config mode: sse or stdio")
	addRootFlag(c, &root)
	return c
}
