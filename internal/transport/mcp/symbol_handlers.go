package mcp

import (
	"context"

	"ragota/pkg/fileutil"
	"ragota/internal/store"

	"github.com/mark3labs/mcp-go/mcp"
)

func (s *SymbolServer) handleFindDefinition(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sym := req.GetString("symbol", "")
	if sym == "" {
		return mcp.NewToolResultError("symbol is required"), nil
	}
	units, err := s.syms.FindDefinition(ctx, sym)
	if err != nil {
		return nil, err
	}
	return jsonResult(units)
}

func (s *SymbolServer) handleFindReferences(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sym := req.GetString("symbol", "")
	if sym == "" {
		return mcp.NewToolResultError("symbol is required"), nil
	}
	edges, err := s.syms.FindReferences(ctx, sym)
	if err != nil {
		return nil, err
	}
	edges = filterEdgesByRepo(edges, parseRepoParam(req.GetString("repo", "")))
	return jsonResult(edges)
}

func (s *SymbolServer) handleFindImplementations(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	iface := req.GetString("interface", "")
	if iface == "" {
		return mcp.NewToolResultError("interface is required"), nil
	}
	units, err := s.syms.FindImplementations(ctx, iface)
	if err != nil {
		return nil, err
	}
	units = filterUnitsByRepo(units, parseRepoParam(req.GetString("repo", "")))
	return jsonResult(units)
}

func (s *SymbolServer) handleFindCallers(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	fn := req.GetString("function", "")
	if fn == "" {
		return mcp.NewToolResultError("function is required"), nil
	}
	units, err := s.syms.FindCallers(ctx, fn)
	if err != nil {
		return nil, err
	}
	units = filterUnitsByRepo(units, parseRepoParam(req.GetString("repo", "")))
	return jsonResult(units)
}

func (s *SymbolServer) handleFindCallees(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	fn := req.GetString("function", "")
	if fn == "" {
		return mcp.NewToolResultError("function is required"), nil
	}
	units, err := s.syms.FindCallees(ctx, fn)
	if err != nil {
		return nil, err
	}
	units = filterUnitsByRepo(units, parseRepoParam(req.GetString("repo", "")))
	return jsonResult(units)
}

func (s *SymbolServer) handleGetFileSymbols(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := req.GetString("path", "")
	if path == "" {
		return mcp.NewToolResultError("path is required"), nil
	}
	abs, err := fileutil.SecureJoin(s.cfg.Root, path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	units, err := s.syms.FileSymbols(ctx, abs)
	if err != nil {
		return nil, err
	}
	return jsonResult(units)
}

func (s *SymbolServer) handleGetSymbol(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := req.GetInt("symbol_id", 0)
	if id <= 0 {
		return mcp.NewToolResultError("symbol_id is required"), nil
	}
	u, err := s.syms.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return jsonResult(u)
}

func (s *SymbolServer) handleGetParent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := req.GetInt("symbol_id", 0)
	if id <= 0 {
		return mcp.NewToolResultError("symbol_id is required"), nil
	}
	u, err := s.syms.Parent(ctx, id)
	if err != nil {
		return nil, err
	}
	return jsonResult(u)
}

func (s *SymbolServer) handleGetChildren(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := req.GetInt("symbol_id", 0)
	if id <= 0 {
		return mcp.NewToolResultError("symbol_id is required"), nil
	}
	us, err := s.syms.Children(ctx, id)
	if err != nil {
		return nil, err
	}
	return jsonResult(us)
}

func (s *SymbolServer) handleSurroundingContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := req.GetInt("symbol_id", 0)
	if id <= 0 {
		return mcp.NewToolResultError("symbol_id is required"), nil
	}
	before := req.GetInt("before_lines", 0)
	after := req.GetInt("after_lines", 0)
	txt, err := s.syms.SurroundingContext(ctx, id, before, after)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(txt), nil
}

func (s *SymbolServer) handleRelatedFiles(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := req.GetInt("symbol_id", 0)
	if id <= 0 {
		return mcp.NewToolResultError("symbol_id is required"), nil
	}
	files, err := s.syms.RelatedFiles(ctx, id)
	if err != nil {
		return nil, err
	}
	return jsonResult(files)
}

func (s *SymbolServer) handleSimilarCode(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := req.GetInt("symbol_id", 0)
	if id <= 0 {
		return mcp.NewToolResultError("symbol_id is required"), nil
	}
	limit := req.GetInt("limit", 10)
	us, err := s.syms.SimilarCode(ctx, id, limit)
	if err != nil {
		return nil, err
	}
	if us == nil {
		us = []store.ASTUnit{}
	}
	return jsonResult(us)
}

func (s *SymbolServer) handleGetExecutionContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := req.GetInt("symbol_id", 0)
	if id <= 0 {
		return mcp.NewToolResultError("symbol_id is required"), nil
	}
	ectx, err := s.gr.GetExecutionContext(ctx, id)
	if err != nil {
		return nil, err
	}
	return jsonResult(ectx)
}

func (s *SymbolServer) handleGetSymbolSummary(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := req.GetInt("symbol_id", 0)
	if id <= 0 {
		return mcp.NewToolResultError("symbol_id is required"), nil
	}
	res, err := s.gr.GetSymbolSummary(ctx, id)
	if err != nil {
		return nil, err
	}
	return jsonResult(res)
}

func (s *SymbolServer) handleGetFileIntent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := req.GetString("path", "")
	if path == "" {
		return mcp.NewToolResultError("path is required"), nil
	}
	abs, err := fileutil.SecureJoin(s.cfg.Root, path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	res, err := s.gr.GetFileIntent(ctx, abs)
	if err != nil {
		return nil, err
	}
	return jsonResult(res)
}

func (s *SymbolServer) handleGetSemanticNeighborhood(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := req.GetInt("symbol_id", 0)
	if id <= 0 {
		return mcp.NewToolResultError("symbol_id is required"), nil
	}
	res, err := s.gr.GetSemanticNeighborhood(ctx, id)
	if err != nil {
		return nil, err
	}
	return jsonResult(res)
}
