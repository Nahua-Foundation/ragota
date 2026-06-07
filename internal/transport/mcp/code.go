// Package mcp — единый MCP-сервер ragota-code.
//
// Все инструменты объединены под префиксом code.* (37 → 12).
// Сервер объединяет AST, vector, LSP и graph-поиск в единый API.
package mcp

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"ragota/internal/indexing/ast"
	"ragota/pkg/config"
	"ragota/pkg/fileutil"
	"ragota/internal/search/graph"
	"ragota/internal/indexing/vector"
	"ragota/pkg/lsp/manager"
	"ragota/pkg/qdrant"
	"ragota/internal/search/rerank"
	"ragota/pkg/repos"
	"ragota/pkg/state"
	"ragota/internal/store"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// CodeServer — единый MCP-сервер для RAG-агента.
//
// Tools:
//   - code.search(query, options) — гибридный поиск (semantic/keyword/hybrid)
//   - code.find_symbol(name, options) — поиск символов по имени
//   - code.get_definition(symbol, options) — go-to-definition (LSP → tree-sitter)
//   - code.find_references(symbol, options) — все ссылки на символ
//   - code.find_implementations(symbol, options) — реализации интерфейса
//   - code.get_context(symbol, options) — комплексный контекст символа
//   - code.get_call_graph(symbol, options) — граф вызовов
//   - code.reindex(options) — переиндексация AST + vector + BM25
//   - code.get_chunks(path, options) — нарезанные чанки для RAG
//   - code.batch_get_context(symbols, options) — пакетный контекст
//   - code.get_file_intent(path, options) — LLM-анализ назначения файла
type CodeServer struct {
	cfg      *config.Config
	st       *store.SQLite
	bus      *state.Bus
	resolver *repos.Resolver

	// AST / tree-sitter
	astIdx *astindex.Indexer

	// Vector / BM25 / hybrid
	vecIdx  *vector.Vector
	qd      *qdrant.Client
	bm25    vector.WriteSink
	rer     rerank.Reranker

	// LSP
	lspMgr *manager.Manager

	// Graph
	gr *graph.Service
}

// NewCodeServer создаёт единый сервер.
func NewCodeServer(cfg *config.Config, st *store.SQLite, bus *state.Bus, resolver *repos.Resolver) *CodeServer {
	return &CodeServer{
		cfg:      cfg,
		st:       st,
		bus:      bus,
		resolver: resolver,
	}
}

// SetASTIndex подключает AST-индексатор.
func (s *CodeServer) SetASTIndex(idx *astindex.Indexer) { s.astIdx = idx }

// SetVector подключает векторный индекс.
func (s *CodeServer) SetVector(v *vector.Vector, qd *qdrant.Client) {
	s.vecIdx = v
	s.qd = qd
}

// SetBM25 подключает BM25 индекс.
func (s *CodeServer) SetBM25(b vector.WriteSink) { s.bm25 = b }

// SetReranker подключает реранкер.
func (s *CodeServer) SetReranker(r rerank.Reranker) { s.rer = r }

// SetLSPManager подключает LSP менеджер.
func (s *CodeServer) SetLSPManager(mgr *manager.Manager) {
	s.lspMgr = mgr
}

// SetGraphService подключает graph-сервис.
func (s *CodeServer) SetGraphService(gr *graph.Service) { s.gr = gr }

// Build регистрирует все tools и возвращает готовый MCP server.
func (s *CodeServer) Build() *server.MCPServer {
	srv := server.NewMCPServer("ragota-code", "0.2.0",
		server.WithToolCapabilities(false),
	)

	// === 1. code.search ===
	srv.AddTool(
		mcp.NewTool("code.search",
			mcp.WithDescription("Unified code search with automatic mode selection. Default: hybrid (RRF). Supports semantic, keyword, and hybrid modes. Returns chunks with metadata: file, symbol, scores, code fragment."),
			mcp.WithString("query", mcp.Required(), mcp.Description("Search query.")),
			mcp.WithString("mode", mcp.Description("Search mode: semantic | keyword | hybrid (default).")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 10).")),
			mcp.WithString("language", mcp.Description("Language filter.")),
			mcp.WithString("repo", mcp.Description("Repo scope: empty/'*' = all repos; 'name' or JSON array to scope.")),
			mcp.WithString("symbol_type", mcp.Description("Filter by AST kind: function | class | method | any (default).")),
		),
		s.wrap("code.search", s.handleSearch),
	)

	// === 2. code.find_symbol ===
	srv.AddTool(
		mcp.NewTool("code.find_symbol",
			mcp.WithDescription("Search symbols by name with substring matching. Returns AST units with file path. If single result, returns definition immediately."),
			mcp.WithString("name", mcp.Required(), mcp.Description("Symbol name or substring.")),
			mcp.WithString("repo", mcp.Description("Repo scope.")),
			mcp.WithBoolean("exact", mcp.Description("Exact match only (default false).")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 50).")),
		),
		s.wrap("code.find_symbol", s.handleFindSymbol),
	)

	// === 3. code.get_definition ===
	srv.AddTool(
		mcp.NewTool("code.get_definition",
			mcp.WithDescription("Go-to-definition with fallback: LSP → tree-sitter. Returns full source code of definition + surrounding context."),
			mcp.WithString("symbol", mcp.Required(), mcp.Description("Symbol name.")),
			mcp.WithString("repo", mcp.Description("Repo scope.")),
			mcp.WithString("file", mcp.Description("Optional file path to disambiguate.")),
			mcp.WithNumber("context_lines", mcp.Description("Lines of surrounding context (default 5).")),
		),
		s.wrap("code.get_definition", s.handleGetDefinition),
	)

	// === 4. code.find_references ===
	srv.AddTool(
		mcp.NewTool("code.find_references",
			mcp.WithDescription("Find all references to a symbol. Fallback: LSP → tree-sitter. Returns locations with brief context."),
			mcp.WithString("symbol", mcp.Required(), mcp.Description("Symbol name.")),
			mcp.WithString("repo", mcp.Description("Repo scope.")),
			mcp.WithBoolean("exclude_tests", mcp.Description("Exclude test files (default false).")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 50).")),
		),
		s.wrap("code.find_references", s.handleFindReferences),
	)

	// === 5. code.find_implementations ===
	srv.AddTool(
		mcp.NewTool("code.find_implementations",
			mcp.WithDescription("Find implementations of an interface or abstract class."),
			mcp.WithString("symbol", mcp.Required(), mcp.Description("Interface or abstract class name.")),
			mcp.WithString("repo", mcp.Description("Repo scope.")),
			mcp.WithBoolean("recursive", mcp.Description("Include transitive implementations (default false).")),
		),
		s.wrap("code.find_implementations", s.handleFindImplementations),
	)

	// === 6. code.get_context ===
	srv.AddTool(
		mcp.NewTool("code.get_context",
			mcp.WithDescription("Comprehensive symbol context. Parameters control depth and relation types. Returns graph around symbol with code, callers/callees, imports. Main method for understanding code."),
			mcp.WithString("symbol", mcp.Required(), mcp.Description("Symbol name.")),
			mcp.WithString("repo", mcp.Description("Repo scope.")),
			mcp.WithString("file", mcp.Description("Optional file path to disambiguate.")),
			mcp.WithNumber("depth", mcp.Description("Graph traversal depth (default 1).")),
			mcp.WithString("include", mcp.Description("Comma-separated relation types: callers,callees,references,imports,related_types,parent.")),
			mcp.WithNumber("max_tokens", mcp.Description("Max tokens in response (approximate limit).")),
		),
		s.wrap("code.get_context", s.handleGetContext),
	)

	// === 7. code.get_call_graph ===
	srv.AddTool(
		mcp.NewTool("code.get_call_graph",
			mcp.WithDescription("Call graph with direction. Returns hierarchical structure."),
			mcp.WithString("symbol", mcp.Required(), mcp.Description("Function/method name.")),
			mcp.WithString("repo", mcp.Description("Repo scope.")),
			mcp.WithString("direction", mcp.Description("Direction: up (callers) | down (callees) | both (default).")),
			mcp.WithNumber("depth", mcp.Description("Depth (default 2).")),
			mcp.WithNumber("max_nodes", mcp.Description("Max nodes in result (default 100).")),
		),
		s.wrap("code.get_call_graph", s.handleGetCallGraph),
	)

	// === 8. code.reindex ===
	srv.AddTool(
		mcp.NewTool("code.reindex",
			mcp.WithDescription("Unified reindexing: AST + vectors + BM25. Incremental or full scan."),
			mcp.WithString("repo", mcp.Description("Repo to reindex (empty = all).")),
			mcp.WithString("paths", mcp.Description("JSON array of file paths to reindex. Empty = full scan.")),
			mcp.WithString("mode", mcp.Description("Mode: incremental | full (default).")),
			mcp.WithBoolean("force", mcp.Description("Force full reindex even if incremental (default false).")),
		),
		s.wrap("code.reindex", s.handleReindex),
	)

	// === 9. code.get_chunks ===
	srv.AddTool(
		mcp.NewTool("code.get_chunks",
			mcp.WithDescription("Returns pre-chunked code for RAG with metadata. Ready for insertion into prompt. Critical for RAG — no need to chunk yourself."),
			mcp.WithString("path", mcp.Required(), mcp.Description("File path.")),
			mcp.WithString("repo", mcp.Description("Repo scope.")),
			mcp.WithString("symbol", mcp.Description("Optional: return chunks for specific symbol only.")),
			mcp.WithNumber("max_tokens", mcp.Description("Max tokens per chunk (approximate).")),
			mcp.WithNumber("overlap_tokens", mcp.Description("Overlap between chunks (approximate).")),
		),
		s.wrap("code.get_chunks", s.handleGetChunks),
	)

	// === 10. code.batch_get_context ===
	srv.AddTool(
		mcp.NewTool("code.batch_get_context",
			mcp.WithDescription("Batch context request for multiple symbols. Fewer RPC calls, lower latency."),
			mcp.WithString("symbols", mcp.Required(), mcp.Description(`JSON array: [{"symbol": "Foo", "file?: "...", "repo?: "..."}]`)),
			mcp.WithNumber("depth", mcp.Description("Graph depth (default 1).")),
			mcp.WithString("include", mcp.Description("Comma-separated: callers,callees,references,imports.")),
			mcp.WithNumber("max_tokens_per_symbol", mcp.Description("Max tokens per symbol context (approximate).")),
		),
		s.wrap("code.batch_get_context", s.handleBatchGetContext),
	)

	// === 11. code.get_file_intent ===
	srv.AddTool(
		mcp.NewTool("code.get_file_intent",
			mcp.WithDescription("LLM analysis of file purpose. Returns brief description of file responsibility. Helps agent understand why a file exists without reading all code."),
			mcp.WithString("path", mcp.Required(), mcp.Description("File path.")),
			mcp.WithString("repo", mcp.Description("Repo scope.")),
			mcp.WithBoolean("use_llm", mcp.Description("Use LLM analysis (default true). If false, returns tree-sitter summary only.")),
		),
		s.wrap("code.get_file_intent", s.handleGetFileIntent),
	)

	// === Stats (внутренний, для отладки) ===
	srv.AddTool(
		mcp.NewTool("code.stats",
			mcp.WithDescription("Return index stats: total files, symbols, vector chunks, BM25 docs."),
		),
		s.wrap("code.stats", s.handleStats),
	)

	// === Cross-repo tools (если crIdx подключён) ===
	s.registerCrossRepoTools(srv)

	return srv
}

func (s *CodeServer) wrap(name string, fn func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Инициализация логгера при первом вызове
		InitMCPLog(s.cfg.Root)

		start := time.Now()
		args := req.GetArguments()

		// Логируем вход
		mcpLog.Debug().
			Str("tool", name).
			Interface("args", args).
			Msg("mcp call")

		res, err := fn(ctx, req)

		// Логируем результат
		elapsed := time.Since(start)
		if err != nil {
			mcpLog.Error().
				Str("tool", name).
				Err(err).
				Dur("elapsed", elapsed).
				Msg("mcp error")
			return errorToResult(name, err)
		}

		// Определяем успешность по isError в результате
		isError := false
		if res != nil && res.IsError {
			isError = true
		}

		lvl := mcpLog.Debug()
		if isError {
			lvl = mcpLog.Warn()
		}
		lvl.Str("tool", name).
			Dur("elapsed", elapsed).
			Bool("is_error", isError).
			Msg("mcp done")

		if s.bus != nil {
			s.bus.IncMCPCall("code", name, false)
		}
		return res, nil
	}
}

// resolveAbs преобразует относительный путь в абсолютный через cfg.Root.
func (s *CodeServer) resolveAbs(path string) (string, error) {
	return fileutil.SecureJoin(s.cfg.Root, path)
}

// wordAt извлекает слово на позиции (line, char) из файла.
func (s *CodeServer) wordAt(file string, line, char int) (string, error) {
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
	for start > 0 && isIdentChar(rune(l[start-1])) {
		start--
	}
	end := char
	for end < len(l) && isIdentChar(rune(l[end])) {
		end++
	}
	return l[start:end], nil
}

func isIdentChar(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
}

// resolveSymbolID находит AST unit по имени символа.
// Если file указан — приоритет в этот файл.
// Если repo указан — фильтрует по репо.
func (s *CodeServer) resolveSymbolID(ctx context.Context, symbol, file, repo string) (*store.ASTUnit, error) {
	units, err := s.st.FindASTUnits(ctx, symbol, "", "", repo, 100)
	if err != nil {
		return nil, err
	}
	if file != "" {
		abs, err := s.resolveAbs(file)
		if err == nil {
			for _, u := range units {
				if u.FilePath == abs {
					return &u, nil
				}
			}
		}
	}
	if len(units) == 0 {
		return nil, fmt.Errorf("symbol %q not found", symbol)
	}
	return &units[0], nil
}
