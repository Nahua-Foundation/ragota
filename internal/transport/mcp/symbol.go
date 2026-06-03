package mcp

import (
	"context"

	"ragota/pkg/config"
	"ragota/internal/search/graph"
	"ragota/pkg/state"
	"ragota/internal/store"
	"ragota/internal/search/symbols"

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
			mcp.WithString("repo", mcp.Description("Repo filter: name | JSON-array | CSV | '*'/empty.")),
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
