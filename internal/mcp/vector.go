package mcp

import (
	"context"
	"fmt"

	"aitools/internal/config"
	"aitools/internal/fileutil"
	"aitools/internal/index"
	"aitools/internal/qdrant"
	"aitools/internal/state"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// VectorServer — MCP-сервер семантического поиска.
// Tools:
//   - vec.search(query, limit?, language?)
//   - vec.reindex(path?)
//   - vec.count()
type VectorServer struct {
	cfg *config.Config
	idx *index.Vector
	qd  *qdrant.Client
	bus *state.Bus
}

// NewVectorServer создаёт сервер.
func NewVectorServer(cfg *config.Config, idx *index.Vector, qd *qdrant.Client, bus *state.Bus) *VectorServer {
	return &VectorServer{cfg: cfg, idx: idx, qd: qd, bus: bus}
}

// Build регистрирует tools.
func (s *VectorServer) Build() *server.MCPServer {
	srv := server.NewMCPServer("ai-tools-vector", "0.1.0",
		server.WithToolCapabilities(false),
	)

	srv.AddTool(
		mcp.NewTool("vec.search",
			mcp.WithDescription("Semantic search over project code using vector index."),
			mcp.WithString("query", mcp.Required(), mcp.Description("Natural-language or code query.")),
			mcp.WithNumber("limit", mcp.Description("Top-K results (default 10)."), mcp.DefaultNumber(10)),
			mcp.WithString("language", mcp.Description("Filter by language: go, typescript, javascript, python, java.")),
		),
		s.wrap("vec.search", s.handleSearch),
	)

	srv.AddTool(
		mcp.NewTool("vec.reindex",
			mcp.WithDescription("Re-index a file (or full scan when path is empty)."),
			mcp.WithString("path", mcp.Description("Path to file or empty for full scan.")),
		),
		s.wrap("vec.reindex", s.handleReindex),
	)

	srv.AddTool(
		mcp.NewTool("vec.count",
			mcp.WithDescription("Return number of indexed chunks in Qdrant."),
		),
		s.wrap("vec.count", s.handleCount),
	)

	return srv
}

func (s *VectorServer) wrap(name string, fn func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res, err := fn(ctx, req)
		if s.bus != nil {
			s.bus.IncMCPCall("vector", name, err != nil)
		}
		return res, err
	}
}

func (s *VectorServer) handleSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := req.GetString("query", "")
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}
	limit := int(req.GetFloat("limit", 10))
	if limit <= 0 {
		limit = 10
	}
	filter := map[string]any{}
	if lang := req.GetString("language", ""); lang != "" {
		filter["language"] = lang
	}
	hits, err := s.idx.Search(ctx, query, limit, filter)
	if err != nil {
		return nil, err
	}
	return jsonResult(hits)
}

func (s *VectorServer) handleReindex(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := req.GetString("path", "")
	if path == "" {
		if err := s.idx.FullScan(ctx); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText("full vector scan completed"), nil
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

func (s *VectorServer) handleCount(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	n, err := s.qd.Count(ctx, s.cfg.Collection)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]any{"chunks": n, "collection": s.cfg.Collection})
}
