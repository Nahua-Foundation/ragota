package mcp

import (
	"context"
	"path/filepath"

	"aitools/internal/config"
	"aitools/internal/fileutil"
	"aitools/internal/lsp"
	"aitools/internal/state"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// LSPServer — MCP-обёртка над пулом LSP-клиентов (go/ts/python/java).
// Tools:
//   - lsp.definition(file, line, character)
//   - lsp.references(file, line, character, include_declaration?)
//   - lsp.hover(file, line, character)
//   - lsp.languages()
type LSPServer struct {
	cfg *config.Config
	mgr *lsp.Manager
	bus *state.Bus
}

// NewLSPServer создаёт сервер.
func NewLSPServer(cfg *config.Config, mgr *lsp.Manager, bus *state.Bus) *LSPServer {
	return &LSPServer{cfg: cfg, mgr: mgr, bus: bus}
}

func (s *LSPServer) Build() *server.MCPServer {
	srv := server.NewMCPServer("ai-tools-lsp", "0.1.0",
		server.WithToolCapabilities(false),
	)

	srv.AddTool(
		mcp.NewTool("lsp.definition",
			mcp.WithDescription("Go to definition for symbol at given position."),
			mcp.WithString("file", mcp.Required(), mcp.Description("File path.")),
			mcp.WithNumber("line", mcp.Required(), mcp.Description("0-based line.")),
			mcp.WithNumber("character", mcp.Required(), mcp.Description("0-based character.")),
		),
		s.wrap("lsp.definition", s.handleDefinition),
	)

	srv.AddTool(
		mcp.NewTool("lsp.references",
			mcp.WithDescription("Find references for symbol at given position."),
			mcp.WithString("file", mcp.Required()),
			mcp.WithNumber("line", mcp.Required()),
			mcp.WithNumber("character", mcp.Required()),
			mcp.WithBoolean("include_declaration", mcp.Description("Include the declaration itself."), mcp.DefaultBool(true)),
		),
		s.wrap("lsp.references", s.handleReferences),
	)

	srv.AddTool(
		mcp.NewTool("lsp.hover",
			mcp.WithDescription("Hover information at given position."),
			mcp.WithString("file", mcp.Required()),
			mcp.WithNumber("line", mcp.Required()),
			mcp.WithNumber("character", mcp.Required()),
		),
		s.wrap("lsp.hover", s.handleHover),
	)

	srv.AddTool(
		mcp.NewTool("lsp.languages",
			mcp.WithDescription("List configured LSP languages."),
		),
		s.wrap("lsp.languages", s.handleLanguages),
	)

	return srv
}

func (s *LSPServer) wrap(name string, fn func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res, err := fn(ctx, req)
		if err != nil {
			return errorToResult(name, err)
		}
		if s.bus != nil {
			s.bus.IncMCPCall("lsp", name, false)
		}
		return res, nil
	}
}

func (s *LSPServer) resolveAbs(file string) (string, error) {
	return fileutil.SecureJoin(s.cfg.Root, file)
}

func (s *LSPServer) handleDefinition(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	file := req.GetString("file", "")
	if file == "" {
		return mcp.NewToolResultError("file is required"), nil
	}
	abs, err := s.resolveAbs(file)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	lang := fileutil.LanguageByExt(filepath.Ext(abs))
	c, err := s.mgr.EnsureOpen(ctx, lang, abs)
	if err != nil {
		return nil, err
	}
	locs, err := c.Definition(ctx, abs, int(req.GetFloat("line", 0)), int(req.GetFloat("character", 0)))
	if err != nil {
		return nil, err
	}
	return jsonResult(locs)
}

func (s *LSPServer) handleReferences(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	file := req.GetString("file", "")
	if file == "" {
		return mcp.NewToolResultError("file is required"), nil
	}
	abs, err := s.resolveAbs(file)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	lang := fileutil.LanguageByExt(filepath.Ext(abs))
	c, err := s.mgr.EnsureOpen(ctx, lang, abs)
	if err != nil {
		return nil, err
	}
	locs, err := c.References(ctx, abs,
		int(req.GetFloat("line", 0)),
		int(req.GetFloat("character", 0)),
		req.GetBool("include_declaration", true),
	)
	if err != nil {
		return nil, err
	}
	return jsonResult(locs)
}

func (s *LSPServer) handleHover(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	file := req.GetString("file", "")
	if file == "" {
		return mcp.NewToolResultError("file is required"), nil
	}
	abs, err := s.resolveAbs(file)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	lang := fileutil.LanguageByExt(filepath.Ext(abs))
	c, err := s.mgr.EnsureOpen(ctx, lang, abs)
	if err != nil {
		return nil, err
	}
	txt, err := c.Hover(ctx, abs, int(req.GetFloat("line", 0)), int(req.GetFloat("character", 0)))
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(txt), nil
}

func (s *LSPServer) handleLanguages(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return jsonResult(s.mgr.Languages())
}
