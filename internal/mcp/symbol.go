package mcp

import (
	"context"
	"strings"

	"ragota/internal/config"
	"ragota/internal/fileutil"
	"ragota/internal/graph"
	"ragota/internal/state"
	"ragota/internal/store"
	"ragota/internal/symbols"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// SymbolServer — symbol-aware MCP-сервер.
//
// Tools полностью реализованы поверх symbols.Service + graph.Service.
type SymbolServer struct {
	cfg  *config.Config
	st   *store.SQLite
	bus  *state.Bus
	syms *symbols.Service
	gr   *graph.Service
}

// NewSymbolServer создаёт сервер.
func NewSymbolServer(cfg *config.Config, st *store.SQLite, syms *symbols.Service, gr *graph.Service, bus *state.Bus) *SymbolServer {
	return &SymbolServer{cfg: cfg, st: st, syms: syms, gr: gr, bus: bus}
}

// Build регистрирует все symbol-aware tools.
func (s *SymbolServer) Build() *server.MCPServer {
	srv := server.NewMCPServer("ragota-symbol", "0.1.0",
		server.WithToolCapabilities(false),
	)

	// --- Symbol-aware ---
	srv.AddTool(
		mcp.NewTool("sym.find_definition",
			mcp.WithDescription("Find AST units that define the given symbol (by name or qualified name)."),
			mcp.WithString("symbol", mcp.Required(), mcp.Description("Symbol name, e.g. 'Foo.bar' or 'bar'.")),
		),
		s.wrap("sym.find_definition", s.handleFindDefinition),
	)

	srv.AddTool(
		mcp.NewTool("sym.find_references",
			mcp.WithDescription("Find all references (edges) to the given symbol across the project."),
			mcp.WithString("symbol", mcp.Required(), mcp.Description("Symbol name.")),
			mcp.WithString("repo", mcp.Description("Repo filter: name | JSON-array | CSV | '*'/empty (all repos).")),
		),
		s.wrap("sym.find_references", s.handleFindReferences),
	)

	srv.AddTool(
		mcp.NewTool("sym.find_implementations",
			mcp.WithDescription("Find concrete implementations of the given interface."),
			mcp.WithString("interface", mcp.Required(), mcp.Description("Interface name or qualified name.")),
			mcp.WithString("repo", mcp.Description("Repo filter: name | JSON-array | CSV | '*'/empty (all repos).")),
		),
		s.wrap("sym.find_implementations", s.handleFindImplementations),
	)

	srv.AddTool(
		mcp.NewTool("sym.find_callers",
			mcp.WithDescription("Find functions/methods that call the given function."),
			mcp.WithString("function", mcp.Required(), mcp.Description("Function name.")),
			mcp.WithString("repo", mcp.Description("Repo filter: name | JSON-array | CSV | '*'/empty (all repos).")),
		),
		s.wrap("sym.find_callers", s.handleFindCallers),
	)

	srv.AddTool(
		mcp.NewTool("sym.find_callees",
			mcp.WithDescription("Find functions/methods called by the given function."),
			mcp.WithString("function", mcp.Required(), mcp.Description("Function name.")),
			mcp.WithString("repo", mcp.Description("Repo filter: name | JSON-array | CSV | '*'/empty (all repos).")),
		),
		s.wrap("sym.find_callees", s.handleFindCallees),
	)

	srv.AddTool(
		mcp.NewTool("sym.get_execution_context",
			mcp.WithDescription("Get a comprehensive execution context for a symbol (definition, callers, callees, references, related types, imports, important files)."),
			mcp.WithNumber("symbol_id", mcp.Required(), mcp.Description("Symbol ID.")),
		),
		s.wrap("sym.get_execution_context", s.handleGetExecutionContext),
	)

	srv.AddTool(
		mcp.NewTool("sym.get_symbol_summary",
			mcp.WithDescription("Get a semantic summary of a symbol (purpose, role, importance) enriched by LLM."),
			mcp.WithNumber("symbol_id", mcp.Required(), mcp.Description("Symbol ID.")),
		),
		s.wrap("sym.get_symbol_summary", s.handleGetSymbolSummary),
	)

	srv.AddTool(
		mcp.NewTool("sym.get_file_intent",
			mcp.WithDescription("Analyze the purpose and responsibilities of a source file."),
			mcp.WithString("path", mcp.Required(), mcp.Description("File path.")),
		),
		s.wrap("sym.get_file_intent", s.handleGetFileIntent),
	)

	srv.AddTool(
		mcp.NewTool("sym.get_semantic_neighborhood",
			mcp.WithDescription("Get a clustered view of a symbol's neighborhood (deterministic + LLM clustering). Requires a valid symbol_id (class, interface, method, function), not a module/file ID."),
			mcp.WithNumber("symbol_id", mcp.Required(), mcp.Description("Symbol ID.")),
		),
		s.wrap("sym.get_semantic_neighborhood", s.handleGetSemanticNeighborhood),
	)

	// --- AST / structure retrieval ---
	srv.AddTool(
		mcp.NewTool("sym.get_file_symbols",
			mcp.WithDescription("List all AST units in a file, with parent_id for parent-child navigation."),
			mcp.WithString("path", mcp.Required(), mcp.Description("File path.")),
		),
		s.wrap("sym.get_file_symbols", s.handleGetFileSymbols),
	)

	srv.AddTool(
		mcp.NewTool("sym.get_symbol",
			mcp.WithDescription("Get a single AST unit by its id."),
			mcp.WithNumber("symbol_id", mcp.Required(), mcp.Description("Symbol ID.")),
		),
		s.wrap("sym.get_symbol", s.handleGetSymbol),
	)

	srv.AddTool(
		mcp.NewTool("sym.get_parent",
			mcp.WithDescription("Get the parent AST unit of the given symbol id."),
			mcp.WithNumber("symbol_id", mcp.Required(), mcp.Description("Symbol ID.")),
		),
		s.wrap("sym.get_parent", s.handleGetParent),
	)

	srv.AddTool(
		mcp.NewTool("sym.get_children",
			mcp.WithDescription("Get direct children AST units of the given symbol id."),
			mcp.WithNumber("symbol_id", mcp.Required(), mcp.Description("Symbol ID.")),
		),
		s.wrap("sym.get_children", s.handleGetChildren),
	)

	// --- Graph retrieval ---
	srv.AddTool(
		mcp.NewTool("sym.expand_neighbors",
			mcp.WithDescription("Expand the code graph around node_id up to the given depth."),
			mcp.WithNumber("node_id", mcp.Required(), mcp.Description("Node ID.")),
			mcp.WithNumber("depth", mcp.Description("Depth (default 1).")),
			mcp.WithString("repo", mcp.Description("Repo filter: name | JSON-array | CSV | '*'/empty. By default = repo of node_id.")),
			mcp.WithString("kinds", mcp.Description("Comma-separated edge kinds: call,import,implements,extends,reference. Empty = all.")),
		),
		s.wrap("sym.expand_neighbors", s.handleExpandNeighbors),
	)

	srv.AddTool(
		mcp.NewTool("sym.get_dependency_graph",
			mcp.WithDescription("Get the import-dependency graph around a module/file. For Go, a full or relative path is required (filenames are not enough)."),
			mcp.WithString("module", mcp.Required(), mcp.Description("Module path.")),
			mcp.WithNumber("depth", mcp.Description("Depth (default 2).")),
		),
		s.wrap("sym.get_dependency_graph", s.handleDependencyGraph),
	)

	srv.AddTool(
		mcp.NewTool("sym.get_call_graph",
			mcp.WithDescription("Get the call graph around a function/method. Accepts either `function` (name) or `symbol_id`."),
			mcp.WithString("function", mcp.Description("Function/method name (e.g. 'Foo.bar' or 'bar').")),
			mcp.WithNumber("symbol_id", mcp.Description("Symbol ID of the function/method (alternative to `function`).")),
			mcp.WithNumber("depth", mcp.Description("Depth (default 2).")),
		),
		s.wrap("sym.get_call_graph", s.handleCallGraph),
	)

	srv.AddTool(
		mcp.NewTool("sym.traverse_graph",
			mcp.WithDescription("Perform semantic navigation by walking edges from a starting symbol."),
			mcp.WithNumber("symbol_id", mcp.Required(), mcp.Description("Symbol ID.")),
			mcp.WithString("edge_types", mcp.Description("Comma-separated edge kinds: call,import,implements,extends,reference. Empty = all.")),
			mcp.WithNumber("depth", mcp.Description("Depth (default 1).")),
		),
		s.wrap("sym.traverse_graph", s.handleTraverseGraph),
	)

	// --- Context retrieval ---
	srv.AddTool(
		mcp.NewTool("sym.get_surrounding_context",
			mcp.WithDescription("Return source-code context around a symbol (its parent body + adjacent units)."),
			mcp.WithNumber("symbol_id", mcp.Required(), mcp.Description("Symbol ID.")),
			mcp.WithNumber("before_lines", mcp.Description("Lines before (default 0).")),
			mcp.WithNumber("after_lines", mcp.Description("Lines after (default 0).")),
		),
		s.wrap("sym.get_surrounding_context", s.handleSurroundingContext),
	)

	srv.AddTool(
		mcp.NewTool("sym.get_related_files",
			mcp.WithDescription("Return files related to the symbol via import/call/reference edges."),
			mcp.WithNumber("symbol_id", mcp.Required(), mcp.Description("Symbol ID.")),
		),
		s.wrap("sym.get_related_files", s.handleRelatedFiles),
	)

	srv.AddTool(
		mcp.NewTool("sym.get_similar_code",
			mcp.WithDescription("Return AST units with embeddings similar to the given symbol."),
			mcp.WithNumber("symbol_id", mcp.Required(), mcp.Description("Symbol ID.")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 10).")),
		),
		s.wrap("sym.get_similar_code", s.handleSimilarCode),
	)

	return srv
}

func (s *SymbolServer) wrap(name string, fn func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res, err := fn(ctx, req)
		if err != nil {
			return errorToResult(name, err)
		}
		if s.bus != nil {
			s.bus.IncMCPCall("symbol", name, false)
		}
		return res, nil
	}
}

// notImpl — единая точка для скелетных хендлеров (оставлена для legacy
// совместимости, не используется в текущей сборке).
func notImpl(tool string) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultError(tool + ": not implemented yet"), nil
}

// --- handlers ---

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

func (s *SymbolServer) handleExpandNeighbors(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := req.GetInt("node_id", 0)
	if id <= 0 {
		return mcp.NewToolResultError("node_id is required"), nil
	}
	depth := req.GetInt("depth", 1)
	kindsStr := req.GetString("kinds", "")
	var kinds []string
	if kindsStr != "" {
		for _, k := range strings.Split(kindsStr, ",") {
			k = strings.TrimSpace(k)
			if k != "" {
				kinds = append(kinds, k)
			}
		}
	}
	n, err := s.gr.ExpandNeighbors(ctx, id, depth, kinds)
	if err != nil {
		return nil, err
	}
	// Repo-фильтр: по умолчанию ограничиваем репой стартового узла.
	// Явный repo (включая "*") перекрывает дефолт.
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

	// Repo-фильтр: явный параметр в приоритете, иначе вычисляется из репы стартового узла.
	repoRaw := req.GetString("repo", "")
	explicitRepo := strings.TrimSpace(repoRaw) != ""
	repoFilter := parseRepoParam(repoRaw)

	// Вариант 1: явный symbol_id.
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

	// Вариант 2: имя функции (как описано в README/AGENTS.md).
	fn := strings.TrimSpace(req.GetString("function", ""))
	if fn == "" {
		return mcp.NewToolResultError("either `function` (name) or `symbol_id` is required"), nil
	}
	defs, err := s.syms.FindDefinition(ctx, fn)
	if err != nil {
		return nil, err
	}
	// Если repo явно не задан — выводим из репы найденных определений
	// (если все они в одной репе). Иначе — без фильтра.
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
	// Отфильтруем callable (function/method) и объединим окрестности.
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
	kindsStr := req.GetString("edge_types", "")
	var kinds []string
	if kindsStr != "" {
		for _, k := range strings.Split(kindsStr, ",") {
			k = strings.TrimSpace(k)
			if k != "" {
				kinds = append(kinds, k)
			}
		}
	}
	res, err := s.gr.TraverseGraph(ctx, id, depth, kinds)
	if err != nil {
		return nil, err
	}
	return jsonResult(res)
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
