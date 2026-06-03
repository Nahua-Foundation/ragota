package mcp

import (
	"context"
	"fmt"
	"os"
	"strings"
	"unicode"

	"ragota/pkg/config"
	"ragota/pkg/fileutil"
	"ragota/pkg/lsp/manager"
	"ragota/pkg/state"
	"ragota/internal/store"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// LSPServer — MCP-обёртка над пулом LSP-клиентов.
type LSPServer struct {
	cfg *config.Config
	mgr *manager.Manager
	st  *store.SQLite
	bus *state.Bus
}

func NewLSPServer(cfg *config.Config, mgr *manager.Manager, st *store.SQLite, bus *state.Bus) *LSPServer {
	return &LSPServer{cfg: cfg, mgr: mgr, st: st, bus: bus}
}

func (s *LSPServer) Build() *server.MCPServer {
	srv := server.NewMCPServer("ragota-lsp", "0.1.0",
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
		mcp.NewTool("lsp.implementation",
			mcp.WithDescription("Find implementations for symbol at given position."),
			mcp.WithString("file", mcp.Required()),
			mcp.WithNumber("line", mcp.Required()),
			mcp.WithNumber("character", mcp.Required()),
		),
		s.wrap("lsp.implementation", s.handleImplementation),
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

func (s *LSPServer) handleLanguages(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.mgr == nil {
		return mcp.NewToolResultError("LSP manager is not initialized"), nil
	}
	return jsonResult(s.mgr.Languages())
}

func (s *LSPServer) wordAt(file string, line, char int) (string, error) {
	content, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(content), "\n")
	if line < 0 || line >= len(lines) {
		return "", fmt.Errorf("line out of range")
	}
	l := lines[line]
	if char < 0 || char >= len(l) {
		return "", fmt.Errorf("char out of range")
	}
	start := char
	for start > 0 && (unicode.IsLetter(rune(l[start-1])) || unicode.IsDigit(rune(l[start-1])) || l[start-1] == '_') {
		start--
	}
	end := char
	for end < len(l) && (unicode.IsLetter(rune(l[end])) || unicode.IsDigit(rune(l[end])) || l[end] == '_') {
		end++
	}
	return l[start:end], nil
}
