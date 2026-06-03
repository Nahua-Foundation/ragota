package mcp

import (
	"context"
	"strings"

	"ragota/internal/search/graph"

	"github.com/mark3labs/mcp-go/mcp"
)

func (s *SymbolServer) handleExpandNeighbors(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := req.GetInt("node_id", 0)
	if id <= 0 {
		return mcp.NewToolResultError("node_id is required"), nil
	}
	depth := req.GetInt("depth", 1)
	kinds := parseCSVParam(req.GetString("kinds", ""))
	n, err := s.gr.ExpandNeighbors(ctx, id, depth, kinds)
	if err != nil {
		return nil, err
	}
	repoRaw := req.GetString("repo", "")
	var repoFilter any
	if strings.TrimSpace(repoRaw) == "" {
		if u, err := s.syms.Get(ctx, id); err == nil && u != nil && u.Repo != "" {
			repoFilter = u.Repo
		}
	} else {
		repoFilter = parseRepoParam(repoRaw)
	}
	if n != nil {
		n.Nodes = filterUnitsByRepo(n.Nodes, repoFilter)
		n.Edges = filterEdgesByRepo(n.Edges, repoFilter)
	}
	return jsonResult(n)
}

func (s *SymbolServer) handleDependencyGraph(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	module := req.GetString("module", "")
	if module == "" {
		return mcp.NewToolResultError("module is required"), nil
	}
	depth := req.GetInt("depth", 2)
	n, err := s.gr.DependencyGraph(ctx, module, depth)
	if err != nil {
		return nil, err
	}
	return jsonResult(n)
}

func (s *SymbolServer) handleCallGraph(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	depth := req.GetInt("depth", 2)

	repoRaw := req.GetString("repo", "")
	explicitRepo := strings.TrimSpace(repoRaw) != ""
	repoFilter := parseRepoParam(repoRaw)

	if id := req.GetInt("symbol_id", 0); id > 0 {
		n, err := s.gr.CallGraph(ctx, id, depth)
		if err != nil {
			return nil, err
		}
		if !explicitRepo {
			if u, err := s.syms.Get(ctx, id); err == nil && u != nil && u.Repo != "" {
				repoFilter = u.Repo
			}
		}
		if n != nil {
			n.Nodes = filterUnitsByRepo(n.Nodes, repoFilter)
			n.Edges = filterEdgesByRepo(n.Edges, repoFilter)
		}
		return jsonResult(n)
	}

	fn := strings.TrimSpace(req.GetString("function", ""))
	if fn == "" {
		return mcp.NewToolResultError("either `function` (name) or `symbol_id` is required"), nil
	}
	defs, err := s.syms.FindDefinition(ctx, fn)
	if err != nil {
		return nil, err
	}
	if !explicitRepo {
		reposSet := map[string]struct{}{}
		for _, d := range defs {
			if d.Repo != "" {
				reposSet[d.Repo] = struct{}{}
			}
		}
		if len(reposSet) == 1 {
			for r := range reposSet {
				repoFilter = r
			}
		}
	}
	seenNodes := map[int]bool{}
	seenEdges := map[int]bool{}
	merged := &graph.Neighborhood{}
	for _, d := range defs {
		if d.Kind != "function" && d.Kind != "method" {
			continue
		}
		n, err := s.gr.CallGraph(ctx, d.ID, depth)
		if err != nil {
			return nil, err
		}
		if n == nil {
			continue
		}
		for _, u := range n.Nodes {
			if !seenNodes[u.ID] {
				seenNodes[u.ID] = true
				merged.Nodes = append(merged.Nodes, u)
			}
		}
		for _, e := range n.Edges {
			if !seenEdges[e.ID] {
				seenEdges[e.ID] = true
				merged.Edges = append(merged.Edges, e)
			}
		}
	}
	merged.Nodes = filterUnitsByRepo(merged.Nodes, repoFilter)
	merged.Edges = filterEdgesByRepo(merged.Edges, repoFilter)
	return jsonResult(merged)
}

func (s *SymbolServer) handleTraverseGraph(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := req.GetInt("symbol_id", 0)
	if id <= 0 {
		return mcp.NewToolResultError("symbol_id is required"), nil
	}
	depth := req.GetInt("depth", 1)
	kinds := parseCSVParam(req.GetString("edge_types", ""))
	res, err := s.gr.TraverseGraph(ctx, id, depth, kinds)
	if err != nil {
		return nil, err
	}
	return jsonResult(res)
}
