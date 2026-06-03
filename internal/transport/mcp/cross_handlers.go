// Cross-repo MCP tool handlers.
// Добавляют 5 новых инструментов к code.* серверу:
//   - code.find_dependencies
//   - code.get_service_graph
//   - code.resolve_call
//   - code.find_callers_across_repos
//   - code.search_across_repos

package mcp

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"ragota/internal/indexing/crossrepo"
	"ragota/internal/search/graph"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// registerCrossRepoTools регистрирует 5 cross-repo инструментов.
func (s *CodeServer) registerCrossRepoTools(srv *server.MCPServer) {
	if s.gr == nil || s.gr.CRI == nil {
		return
	}

	// 1. code.find_dependencies — найти зависимости репозитория
	srv.AddTool(
		mcp.NewTool("code.find_dependencies",
			mcp.WithDescription("Find cross-repo dependencies of a repository. Returns imports, HTTP/gRPC/Kafka calls to other repos."),
			mcp.WithString("repo", mcp.Required(), mcp.Description("Repository name.")),
			mcp.WithString("protocol", mcp.Description("Filter by protocol: import, http, grpc, kafka, or empty for all.")),
		),
		s.wrap("code.find_dependencies", s.handleFindDependencies),
	)

	// 2. code.get_service_graph — граф всех сервисов и связей
	srv.AddTool(
		mcp.NewTool("code.get_service_graph",
			mcp.WithDescription("Return the full service dependency graph across all repositories. Shows which repos depend on which."),
			mcp.WithString("repo", mcp.Description("Optional: limit graph to specific repo and its direct dependencies.")),
		),
		s.wrap("code.get_service_graph", s.handleGetServiceGraph),
	)

	// 3. code.resolve_call — разрешить вызов куда он ведёт
	srv.AddTool(
		mcp.NewTool("code.resolve_call",
			mcp.WithDescription("Resolve a cross-repo call: where does a call at file:line lead? Returns target repo, endpoint, and symbol if found."),
			mcp.WithString("file", mcp.Required(), mcp.Description("File path (relative to root).")),
			mcp.WithString("line", mcp.Required(), mcp.Description("Line number.")),
			mcp.WithString("repo", mcp.Description("Repo scope.")),
		),
		s.wrap("code.resolve_call", s.handleResolveCall),
	)

	// 4. code.find_callers_across_repos — кто вызывает символ во всех репах
	srv.AddTool(
		mcp.NewTool("code.find_callers_across_repos",
			mcp.WithDescription("Find all callers of a symbol across all repositories, not just within one repo."),
			mcp.WithString("symbol", mcp.Required(), mcp.Description("Symbol name.")),
			mcp.WithString("limit", mcp.Description("Max results (default 50).")),
		),
		s.wrap("code.find_callers_across_repos", s.handleFindCallersAcrossRepos),
	)

	// 5. code.search_across_repos — поиск с cross-repo контекстом
	srv.AddTool(
		mcp.NewTool("code.search_across_repos",
			mcp.WithDescription("Search code with cross-repo context. Results include dependency information and cross-repo call targets."),
			mcp.WithString("query", mcp.Required(), mcp.Description("Search query.")),
			mcp.WithString("repo", mcp.Description("Repo scope: empty = all repos.")),
			mcp.WithString("include_dependencies", mcp.Description("Include cross-repo dependency context (default true).")),
			mcp.WithString("limit", mcp.Description("Max results (default 10).")),
		),
		s.wrap("code.search_across_repos", s.handleSearchAcrossRepos),
	)
}

// handleFindDependencies — handler для code.find_dependencies.
func (s *CodeServer) handleFindDependencies(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	repo := req.GetString("repo", "")
	if repo == "" {
		return mcp.NewToolResultError("repo is required"), nil
	}
	protocol := req.GetString("protocol", "")

	edges, err := s.gr.CRI.GetEdgesByRepo(repo)
	if err != nil {
		return nil, err
	}

	// Filter by protocol if specified
	if protocol != "" {
		filtered := make([]crossrepo.CrossEdge, 0)
		for _, e := range edges {
			if e.Protocol == protocol {
				filtered = append(filtered, e)
			}
		}
		edges = filtered
	}

	data, _ := json.MarshalIndent(map[string]any{
		"repo":  repo,
		"edges": edges,
		"count": len(edges),
	}, "", "  ")

	return mcp.NewToolResultText(string(data)), nil
}

// handleGetServiceGraph — handler для code.get_service_graph.
func (s *CodeServer) handleGetServiceGraph(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	repo := req.GetString("repo", "")

	graphData := s.gr.ServiceGraph(ctx)

	// Filter to specific repo if requested
	if repo != "" {
		if node, ok := graphData[repo]; ok {
			filtered := map[string]*graph.ServiceNode{repo: node}
			for _, dep := range node.Dependencies {
				if n, ok := graphData[dep.Target]; ok {
					filtered[dep.Target] = n
				}
			}
			graphData = filtered
		}
	}

	data, _ := json.MarshalIndent(graphData, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}

// handleResolveCall — handler для code.resolve_call.
func (s *CodeServer) handleResolveCall(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	file := req.GetString("file", "")
	lineStr := req.GetString("line", "")

	if file == "" || lineStr == "" {
		return mcp.NewToolResultError("file and line are required"), nil
	}

	line, err := strconv.Atoi(lineStr)
	if err != nil || line <= 0 {
		return mcp.NewToolResultError("line must be a positive integer"), nil
	}

	abs, err := s.resolveAbs(file)
	if err != nil {
		return nil, err
	}

	res, err := s.gr.ResolveCrossCall(ctx, abs, line)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return mcp.NewToolResultText("No cross-repo call found at this location"), nil
	}

	data, _ := json.MarshalIndent(res, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}

// handleFindCallersAcrossRepos — handler для code.find_callers_across_repos.
func (s *CodeServer) handleFindCallersAcrossRepos(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	symbol := req.GetString("symbol", "")
	limitStr := req.GetString("limit", "50")

	if symbol == "" {
		return mcp.NewToolResultError("symbol is required"), nil
	}

	limit, _ := strconv.Atoi(limitStr)
	if limit <= 0 {
		limit = 50
	}

	edges, err := s.gr.CrossRepoCallers(ctx, symbol)
	if err != nil {
		return nil, err
	}

	if len(edges) > limit {
		edges = edges[:limit]
	}

	data, _ := json.MarshalIndent(map[string]any{
		"symbol":  symbol,
		"callers": edges,
		"count":   len(edges),
	}, "", "  ")

	return mcp.NewToolResultText(string(data)), nil
}

// handleSearchAcrossRepos — handler для code.search_across_repos.
func (s *CodeServer) handleSearchAcrossRepos(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := req.GetString("query", "")
	_ = req.GetString("repo", "") // repo передаётся в handleSearch через req
	includeDepsStr := req.GetString("include_dependencies", "true")
	limitStr := req.GetString("limit", "10")

	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}

	includeDeps := strings.ToLower(includeDepsStr) != "false"
	limit, _ := strconv.Atoi(limitStr)
	if limit <= 0 {
		limit = 10
	}

	// Делегируем стандартному search с теми же параметрами
	result, err := s.handleSearch(ctx, req)
	if err != nil {
		return nil, err
	}

	if !includeDeps || s.gr.CRI == nil {
		return result, nil
	}

	// Добавляем cross-repo dependency context
	graphData := s.gr.ServiceGraph(ctx)
	data, _ := json.MarshalIndent(map[string]any{
		"search_result": result,
		"service_graph": graphData,
		"query":         query,
	}, "", "  ")

	return mcp.NewToolResultText(string(data)), nil
}
