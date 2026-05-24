// Package mcp реализует четыре MCP-сервера: tree-sitter, vector, lsp, symbol.
// Все они общаются по stdio (стандарт MCP) и используют mark3labs/mcp-go.
package mcp

import (
	"context"
	"fmt"

	"aitools/internal/config"
	"aitools/internal/fileutil"
	"aitools/internal/index"
	"aitools/internal/state"
	"aitools/internal/store"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// TreeSitterServer — MCP-сервер для tree-sitter индекса.
// Tools:
//   - ts.search_symbols(query, kind?, language?, limit?, repo?)
//   - ts.list_symbols(file)
//   - ts.reindex(path?)
//   - ts.stats()
type TreeSitterServer struct {
	cfg *config.Config
	idx *index.TreeSitter
	st  *store.SQLite
	bus *state.Bus
}

// NewTreeSitterServer создаёт сервер.
func NewTreeSitterServer(cfg *config.Config, idx *index.TreeSitter, st *store.SQLite, bus *state.Bus) *TreeSitterServer {
	return &TreeSitterServer{cfg: cfg, idx: idx, st: st, bus: bus}
}

// Build регистрирует все tools и возвращает готовый MCP server.
func (s *TreeSitterServer) Build() *server.MCPServer {
	srv := server.NewMCPServer("ai-tools-treesitter", "0.1.0",
		server.WithToolCapabilities(false),
	)

	srv.AddTool(
		mcp.NewTool("ts.search_symbols",
			mcp.WithDescription("Search code symbols (functions/methods/classes) by substring of their name."),
			mcp.WithString("query", mcp.Required(), mcp.Description("Substring to search in symbol name.")),
			mcp.WithString("kind", mcp.Description("Filter by kind: function, method, class, interface, type, enum, var, const. Go-specific: 'function' finds only functions, 'method' finds only methods.")),
			mcp.WithString("language", mcp.Description("Filter by language: go, typescript, javascript, python, java.")),
			mcp.WithString("repo", mcp.Description("Repo filter: name | JSON-array | CSV | '*'/empty (all repos).")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 50)."), mcp.DefaultNumber(50)),
		),
		s.toolWrap("ts.search_symbols", s.handleSearch),
	)

	srv.AddTool(
		mcp.NewTool("ts.list_symbols",
			mcp.WithDescription("List all symbols in a given file (absolute or relative to project root)."),
			mcp.WithString("file", mcp.Required(), mcp.Description("File path.")),
		),
		s.toolWrap("ts.list_symbols", s.handleListSymbols),
	)

	srv.AddTool(
		mcp.NewTool("ts.reindex",
			mcp.WithDescription("Force re-index of one file, or full scan if path is empty."),
			mcp.WithString("path", mcp.Description("Path to file. Empty = full scan.")),
		),
		s.toolWrap("ts.reindex", s.handleReindex),
	)

	srv.AddTool(
		mcp.NewTool("ts.stats",
			mcp.WithDescription("Return index stats: total files and symbols."),
		),
		s.toolWrap("ts.stats", s.handleStats),
	)

	return srv
}

// toolWrap обёртка, считающая вызовы и ошибки в state.Bus.
func (s *TreeSitterServer) toolWrap(name string, fn func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res, err := fn(ctx, req)
		if err != nil {
			return errorToResult(name, err)
		}
		if s.bus != nil {
			s.bus.IncMCPCall("treesitter", name, false)
		}
		return res, nil
	}
}

func (s *TreeSitterServer) handleSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := req.GetString("query", "")
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}
	kind := req.GetString("kind", "")
	language := req.GetString("language", "")
	repo := req.GetString("repo", "")
	limit := int(req.GetFloat("limit", 50))

	syms, err := s.st.FindASTUnits(ctx, query, kind, language, repo, limit)
	if err != nil {
		return nil, err
	}
	return jsonResult(syms)
}

func (s *TreeSitterServer) handleListSymbols(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	file := req.GetString("file", "")
	if file == "" {
		return mcp.NewToolResultError("file is required"), nil
	}
	abs, err := fileutil.SecureJoin(s.cfg.Root, file)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	syms, err := s.st.ListASTUnitsByFile(ctx, abs)
	if err != nil {
		return nil, err
	}
	return jsonResult(syms)
}

func (s *TreeSitterServer) handleReindex(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := req.GetString("path", "")
	if path == "" {
		if err := s.idx.FullScan(ctx); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText("full scan completed"), nil
	}
	abs, err := fileutil.SecureJoin(s.cfg.Root, path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := s.idx.IndexFile(ctx, abs); err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(fmt.Sprintf("reindexed %s", abs)), nil
}

func (s *TreeSitterServer) handleStats(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	st, err := s.st.Stats(ctx)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]any{"files": st.Files, "symbols": st.Symbols})
}
