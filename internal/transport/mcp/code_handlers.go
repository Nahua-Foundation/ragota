// Хендлеры для code.* инструментов.
// Каждый хендлер соответствует одному инструменту из code.go.

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ragota/internal/indexing/chunker"
	"ragota/pkg/fileutil"
	"ragota/internal/search/bm25"
	"ragota/internal/search/graph"
	"ragota/internal/search/hybrid"
	"ragota/internal/indexing/vector"
	"ragota/internal/store"

	"github.com/mark3labs/mcp-go/mcp"
)

// ============================================================
// 1. code.search
// ============================================================

func (s *CodeServer) handleSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := req.GetString("query", "")
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}
	mode := strings.ToLower(req.GetString("mode", "hybrid"))
	limit := req.GetInt("limit", 0)
	if limit <= 0 {
		limit = 10
	}
	lang := req.GetString("language", "")
	repoRaw := req.GetString("repo", "")
	repoFilter := parseRepoParam(repoRaw)
	symType := strings.ToLower(req.GetString("symbol_type", ""))

	filter := map[string]any{}
	if lang != "" {
		filter["language"] = lang
	}
	if repoFilter != nil {
		filter["repo"] = repoFilter
	}

	switch mode {
	case "semantic":
		return s.searchSemantic(ctx, query, limit, filter)
	case "keyword":
		return s.searchKeyword(ctx, query, limit, symType, repoRaw)
	case "hybrid", "":
		return s.searchHybrid(ctx, query, limit, filter)
	default:
		return mcp.NewToolResultError(fmt.Sprintf("unknown mode %q, expected: semantic | keyword | hybrid", mode)), nil
	}
}

func (s *CodeServer) searchSemantic(ctx context.Context, query string, limit int, filter map[string]any) (*mcp.CallToolResult, error) {
	if s.vecIdx == nil {
		return mcp.NewToolResultError("vector index is not initialized"), nil
	}
	hits, err := s.vecIdx.Search(ctx, query, limit, filter)
	if err != nil {
		return nil, err
	}
	return jsonResult(hits)
}

func (s *CodeServer) searchKeyword(ctx context.Context, query string, limit int, kind, repoRaw string) (*mcp.CallToolResult, error) {
	if s.bm25 == nil {
		return mcp.NewToolResultError("bm25 index is not configured"), nil
	}
	hits, err := s.bm25.Search(ctx, vector.SearchQuery{
		Text:     query,
		Kind:     kind,
		Repos:    parseRepoListParam(repoRaw),
		Limit:    limit,
	})
	if err != nil {
		return nil, err
	}
	return jsonResult(hits)
}

func (s *CodeServer) searchHybrid(ctx context.Context, query string, limit int, filter map[string]any) (*mcp.CallToolResult, error) {
	if s.vecIdx == nil {
		return mcp.NewToolResultError("vector index is not initialized"), nil
	}

	vecRet := &hybrid.VectorHybridAdapter{V: s.vecIdx}
	var lexRet hybrid.BM25Retriever
	if s.bm25 != nil {
		lexRet = &bm25.BM25HybridAdapter{Idx: s.bm25}
	}

	candsPerSource := 50
	if s.cfg.Hybrid.CandidatesPerSource > 0 {
		candsPerSource = s.cfg.Hybrid.CandidatesPerSource
	}

	eng := hybrid.New(vecRet, lexRet, hybrid.Options{
		VectorWeight: s.cfg.Hybrid.VectorWeight,
		BM25Weight:   s.cfg.Hybrid.BM25Weight,
		RRFK:         s.cfg.Hybrid.RRFK,
	})
	res, err := eng.Search(ctx, query, candsPerSource, limit, filter)
	if err != nil {
		return nil, err
	}
	return jsonResult(res)
}

// ============================================================
// 2. code.find_symbol
// ============================================================

func (s *CodeServer) handleFindSymbol(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := req.GetString("name", "")
	if name == "" {
		return mcp.NewToolResultError("name is required"), nil
	}
	repo := req.GetString("repo", "")
	exact := req.GetBool("exact", false)
	limit := req.GetInt("limit", 0)
	if limit <= 0 {
		limit = 50
	}

	units, err := s.st.FindASTUnits(ctx, name, "", "", repo, limit)
	if err != nil {
		return nil, err
	}

	if exact {
		var filtered []store.ASTUnit
		for _, u := range units {
			if strings.EqualFold(u.Name, name) || strings.EqualFold(u.Qualified, name) {
				filtered = append(filtered, u)
			}
		}
		return jsonResult(filtered)
	}

	return jsonResult(units)
}

// ============================================================
// 3. code.get_definition
// ============================================================

func (s *CodeServer) handleGetDefinition(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	symbol := req.GetString("symbol", "")
	if symbol == "" {
		return mcp.NewToolResultError("symbol is required"), nil
	}
	repo := req.GetString("repo", "")
	file := req.GetString("file", "")
	contextLines := req.GetInt("context_lines", 5)

	// Пытаемся найти через AST
	u, err := s.resolveSymbolID(ctx, symbol, file, repo)
	if err != nil {
		return nil, err
	}

	// Читаем исходный код с контекстом
	code, err := u.ReadSource(store.SourceOptions{
		BeforeLines: contextLines,
		AfterLines:  contextLines,
	})
	if err != nil {
		return nil, err
	}

	result := map[string]any{
		"symbol":        u.Name,
		"qualified":     u.Qualified,
		"kind":          u.Kind,
		"language":      u.Language,
		"file":          u.FilePath,
		"repo":          u.Repo,
		"start_line":    u.StartLine,
		"end_line":      u.EndLine,
		"signature":     u.Signature,
		"doc":           u.Doc,
		"source":        code,
		"context_lines": contextLines,
	}
	return jsonResult(result)
}

// ============================================================
// 4. code.find_references
// ============================================================

func (s *CodeServer) handleFindReferences(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	symbol := req.GetString("symbol", "")
	if symbol == "" {
		return mcp.NewToolResultError("symbol is required"), nil
	}
	repo := req.GetString("repo", "")
	excludeTests := req.GetBool("exclude_tests", false)
	limit := req.GetInt("limit", 50)

	u, err := s.resolveSymbolID(ctx, symbol, "", repo)
	if err != nil {
		return nil, err
	}

	edges, err := s.gr.References(ctx, u.ID)
	if err != nil {
		return nil, err
	}

	edges = filterEdgesByRepo(edges, parseRepoParam(repo))

	if excludeTests {
		var filtered []store.Edge
		for _, e := range edges {
			if !strings.Contains(e.FilePath, "_test.") && !strings.Contains(e.FilePath, "test/") {
				filtered = append(filtered, e)
			}
		}
		edges = filtered
	}

	if limit > 0 && len(edges) > limit {
		edges = edges[:limit]
	}

	return jsonResult(edges)
}

// ============================================================
// 5. code.find_implementations
// ============================================================

func (s *CodeServer) handleFindImplementations(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	symbol := req.GetString("symbol", "")
	if symbol == "" {
		return mcp.NewToolResultError("symbol is required"), nil
	}
	repo := req.GetString("repo", "")
	recursive := req.GetBool("recursive", false)

	u, err := s.resolveSymbolID(ctx, symbol, "", repo)
	if err != nil {
		return nil, err
	}

	units, err := s.gr.Implementations(ctx, u.ID)
	if err != nil {
		return nil, err
	}

	units = filterUnitsByRepo(units, parseRepoParam(repo))

	if recursive {
		// Транзитивные реализации: для каждой найденной реализации ищем её подтипы
		seen := map[int]bool{u.ID: true}
		for _, imp := range units {
			seen[imp.ID] = true
		}
		for _, imp := range units {
			sub, err := s.gr.Implementations(ctx, imp.ID)
			if err == nil {
				for _, s := range sub {
					if !seen[s.ID] {
						seen[s.ID] = true
						units = append(units, s)
					}
				}
			}
		}
	}

	return jsonResult(units)
}

// ============================================================
// 6. code.get_context
// ============================================================

func (s *CodeServer) handleGetContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	symbol := req.GetString("symbol", "")
	if symbol == "" {
		return mcp.NewToolResultError("symbol is required"), nil
	}
	repo := req.GetString("repo", "")
	file := req.GetString("file", "")
	depth := req.GetInt("depth", 1)
	includeRaw := req.GetString("include", "")
	maxTokens := req.GetInt("max_tokens", 0)

	u, err := s.resolveSymbolID(ctx, symbol, file, repo)
	if err != nil {
		return nil, err
	}

	// Парсим include
	includeSet := map[string]bool{}
	if includeRaw != "" {
		for _, part := range strings.Split(includeRaw, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				includeSet[part] = true
			}
		}
	}

	result := map[string]any{
		"symbol":    u.Name,
		"qualified": u.Qualified,
		"kind":      u.Kind,
		"language":  u.Language,
		"file":      u.FilePath,
		"repo":      u.Repo,
	}

	// Читаем исходный код
	if srcCode, err := u.ReadSource(store.SourceOptions{}); err == nil && srcCode != "" {
		result["source"] = srcCode
	}

	// Callers
	if includeSet["callers"] || includeRaw == "" {
		callers, err := s.gr.Callers(ctx, u.ID)
		if err == nil {
			callers = filterUnitsByRepo(callers, parseRepoParam(repo))
			result["callers"] = callers
		}
	}

	// Callees
	if includeSet["callees"] || includeRaw == "" {
		callees, err := s.gr.Callees(ctx, u.ID)
		if err == nil {
			callees = filterUnitsByRepo(callees, parseRepoParam(repo))
			result["callees"] = callees
		}
	}

	// References
	if includeSet["references"] || includeRaw == "" {
		refs, err := s.gr.References(ctx, u.ID)
		if err == nil {
			refs = filterEdgesByRepo(refs, parseRepoParam(repo))
			result["references"] = refs
		}
	}

	// Imports
	if includeSet["imports"] || includeRaw == "" {
		edges, err := s.st.EdgesFrom(ctx, u.ID, graph.EdgeImport)
		if err == nil {
			var imports []store.ASTUnit
			for _, e := range edges {
				if e.DstID != 0 {
					if iu, err := s.st.GetASTUnit(ctx, e.DstID); err == nil && iu != nil {
						imports = append(imports, *iu)
					}
				}
			}
			result["imports"] = imports
		}
	}

	// Related types
	if includeSet["related_types"] || includeRaw == "" {
		impls, err := s.gr.Implementations(ctx, u.ID)
		if err == nil {
			impls = filterUnitsByRepo(impls, parseRepoParam(repo))
			result["related_types"] = impls
		}
	}

	// Parent
	if includeSet["parent"] {
		if u.ParentID.Valid {
			parent, err := s.st.GetASTUnit(ctx, int(u.ParentID.Int64))
			if err == nil && parent != nil {
				result["parent"] = parent
			}
		}
	}

	// Graph expansion по глубине
	if depth > 1 {
		neighborhood, err := s.gr.ExpandNeighbors(ctx, u.ID, depth-1, nil)
		if err == nil && neighborhood != nil {
			neighborhood.Nodes = filterUnitsByRepo(neighborhood.Nodes, parseRepoParam(repo))
			neighborhood.Edges = filterEdgesByRepo(neighborhood.Edges, parseRepoParam(repo))
			result["neighborhood"] = neighborhood
		}
	}

	// Ограничение по токенам (приблизительное — считаем по символям)
	if maxTokens > 0 {
		result = truncateByTokens(result, maxTokens)
	}

	return jsonResult(result)
}

// truncateByTokens приближённо обрезает результат по количеству токенов.
// 1 токен ≈ 4 символа для английского текста.
func truncateByTokens(result map[string]any, maxTokens int) map[string]any {
	maxBytes := maxTokens * 4
	estimated := estimateBytes(result)
	if estimated <= maxBytes {
		return result
	}

	// Обрезаем source и neighborhood
	if src, ok := result["source"].(string); ok {
		if len(src) > maxBytes/3 {
			result["source"] = src[:maxBytes/3] + "\n... (truncated)"
		}
	}
	return result
}

func estimateBytes(v any) int {
	switch x := v.(type) {
	case string:
		return len(x)
	case []byte:
		return len(x)
	case map[string]any:
		total := 0
		for _, val := range x {
			total += estimateBytes(val)
		}
		return total
	case []any:
		total := 0
		for _, val := range x {
			total += estimateBytes(val)
		}
		return total
	default:
		return 100 // fallback
	}
}

// ============================================================
// 7. code.get_call_graph
// ============================================================

func (s *CodeServer) handleGetCallGraph(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	symbol := req.GetString("symbol", "")
	if symbol == "" {
		return mcp.NewToolResultError("symbol is required"), nil
	}
	repo := req.GetString("repo", "")
	direction := strings.ToLower(req.GetString("direction", "both"))
	depth := req.GetInt("depth", 2)
	maxNodes := req.GetInt("max_nodes", 100)

	u, err := s.resolveSymbolID(ctx, symbol, "", repo)
	if err != nil {
		return nil, err
	}

	var result *graph.Neighborhood

	switch direction {
	case "up":
		// Callers: идём от callers к root
		callers, err := s.gr.Callers(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		callers = filterUnitsByRepo(callers, parseRepoParam(repo))
		result = &graph.Neighborhood{Nodes: callers, Edges: []store.Edge{}}
	case "down":
		// Callees: идём от function к leaves
		result, err = s.gr.CallGraph(ctx, u.ID, depth)
		if err != nil {
			return nil, err
		}
	case "both", "":
		// Полный call graph
		result, err = s.gr.CallGraph(ctx, u.ID, depth)
		if err != nil {
			return nil, err
		}
		// Добавляем callers
		callers, err := s.gr.Callers(ctx, u.ID)
		if err == nil {
			callers = filterUnitsByRepo(callers, parseRepoParam(repo))
			seenNodes := map[int]bool{}
			for _, n := range result.Nodes {
				seenNodes[n.ID] = true
			}
			for _, c := range callers {
				if !seenNodes[c.ID] {
					result.Nodes = append(result.Nodes, c)
					seenNodes[c.ID] = true
				}
			}
		}
	default:
		return mcp.NewToolResultError(fmt.Sprintf("unknown direction %q, expected: up | down | both", direction)), nil
	}

	if maxNodes > 0 && len(result.Nodes) > maxNodes {
		result.Nodes = result.Nodes[:maxNodes]
	}

	return jsonResult(result)
}

// ============================================================
// 8. code.reindex
// ============================================================

func (s *CodeServer) handleReindex(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	mode := strings.ToLower(req.GetString("mode", "full"))
	force := req.GetBool("force", false)
	pathsRaw := req.GetString("paths", "")

	if mode == "full" || force {
		return s.reindexFull(ctx)
	}

	// Incremental: по конкретным путям
	if pathsRaw != "" {
		var paths []string
		if err := json.Unmarshal([]byte(pathsRaw), &paths); err != nil {
			// Пробуем как одиночный путь
			paths = []string{pathsRaw}
		}
		return s.reindexPaths(ctx, paths)
	}

	// Если paths не указан — full scan
	return s.reindexFull(ctx)
}

func (s *CodeServer) reindexFull(ctx context.Context) (*mcp.CallToolResult, error) {
	var results []string

	// AST reindex
	if s.astIdx != nil {
		if err := s.astIdx.FullScan(ctx); err != nil {
			return nil, fmt.Errorf("ast full scan: %w", err)
		}
		results = append(results, "ast: full scan completed")
	}

	// Vector reindex
	if s.vecIdx != nil {
		if err := s.vecIdx.FullScan(ctx); err != nil {
			return nil, fmt.Errorf("vector full scan: %w", err)
		}
		results = append(results, "vector: full scan completed")
	}

	return mcp.NewToolResultText(strings.Join(results, "; ")), nil
}

func (s *CodeServer) reindexPaths(ctx context.Context, paths []string) (*mcp.CallToolResult, error) {
	var results []string

	for _, path := range paths {
		abs, err := s.resolveAbs(path)
		if err != nil {
			results = append(results, fmt.Sprintf("%s: invalid path (%v)", path, err))
			continue
		}

		if s.astIdx != nil {
			if err := s.astIdx.IndexFile(ctx, abs); err != nil {
				results = append(results, fmt.Sprintf("%s: ast error (%v)", path, err))
			} else {
				results = append(results, fmt.Sprintf("%s: ast ok", path))
			}
		}

		if s.vecIdx != nil {
			if err := s.vecIdx.IndexFile(ctx, abs); err != nil {
				results = append(results, fmt.Sprintf("%s: vector error (%v)", path, err))
			} else {
				results = append(results, fmt.Sprintf("%s: vector ok", path))
			}
		}
	}

	return mcp.NewToolResultText(strings.Join(results, "; ")), nil
}

// ============================================================
// 9. code.get_chunks
// ============================================================

func (s *CodeServer) handleGetChunks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := req.GetString("path", "")
	if path == "" {
		return mcp.NewToolResultError("path is required"), nil
	}
	symbol := req.GetString("symbol", "")
	_ = req.GetInt("max_tokens", 0)     // for future: max tokens per chunk
	_ = req.GetInt("overlap_tokens", 0) // for future: overlap between chunks

	abs, err := s.resolveAbs(path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	src, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}

	// Определяем язык по расширению файла
	lang := fileutil.LanguageByExt(filepath.Ext(path))

	// Чанкаем через window chunker
	ch := chunker.New(60, 10) // default window/overlap
	chunks := ch.Chunk(abs, lang, src, nil)

	// Если указан symbol — фильтруем чанки
	if symbol != "" {
		var filtered []chunker.Chunk
		for _, c := range chunks {
			if c.Symbol == symbol || strings.Contains(c.Path, symbol) {
				filtered = append(filtered, c)
			}
		}
		chunks = filtered
	}

	// Формируем результат
	type ChunkResult struct {
		Path      string `json:"path"`
		Text      string `json:"text"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
		Kind      string `json:"kind"`
		Symbol    string `json:"symbol,omitempty"`
		Language  string `json:"language"`
		Comments  string `json:"comments,omitempty"`
	}

	var out []ChunkResult
	for _, c := range chunks {
		out = append(out, ChunkResult{
			Path:      c.Path,
			Text:      c.Text,
			StartLine: c.StartLine,
			EndLine:   c.EndLine,
			Kind:      c.Kind,
			Symbol:    c.Symbol,
			Language:  c.Language,
			Comments:  c.Comments,
		})
	}

	return jsonResult(out)
}

// ============================================================
// 10. code.batch_get_context
// ============================================================

func (s *CodeServer) handleBatchGetContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	symbolsRaw := req.GetString("symbols", "")
	if symbolsRaw == "" {
		return mcp.NewToolResultError("symbols JSON array is required"), nil
	}

	type symbolReq struct {
		Symbol string `json:"symbol"`
		File   string `json:"file,omitempty"`
		Repo   string `json:"repo,omitempty"`
	}

	var symbols []symbolReq
	if err := json.Unmarshal([]byte(symbolsRaw), &symbols); err != nil {
		return mcp.NewToolResultError("invalid symbols JSON: " + err.Error()), nil
	}

	includeRaw := req.GetString("include", "")
	maxTokensPerSymbol := req.GetInt("max_tokens_per_symbol", 0)

	type batchResult struct {
		Symbol  string `json:"symbol"`
		File    string `json:"file"`
		Repo    string `json:"repo"`
		Found   bool   `json:"found"`
		Error   string `json:"error,omitempty"`
		Context any    `json:"context,omitempty"`
	}

	var results []batchResult

	for _, sr := range symbols {
		br := batchResult{Symbol: sr.Symbol, File: sr.File, Repo: sr.Repo}

		u, err := s.resolveSymbolID(ctx, sr.Symbol, sr.File, sr.Repo)
		if err != nil {
			br.Error = err.Error()
			results = append(results, br)
			continue
		}

		br.Found = true
		br.File = u.FilePath
		br.Repo = u.Repo

		// Собираем контекст
		ctxData := map[string]any{
			"name":     u.Name,
			"qualified": u.Qualified,
			"kind":     u.Kind,
			"language": u.Language,
			"file":     u.FilePath,
		}

		// Source
		if srcCode, err := u.ReadSource(store.SourceOptions{}); err == nil && srcCode != "" {
			ctxData["source"] = srcCode
		}

		// Callers
		if strings.Contains(includeRaw, "callers") || includeRaw == "" {
			callers, err := s.gr.Callers(ctx, u.ID)
			if err == nil {
				ctxData["callers"] = len(callers)
				ctxData["caller_names"] = callerNames(callers)
			}
		}

		// Callees
		if strings.Contains(includeRaw, "callees") || includeRaw == "" {
			callees, err := s.gr.Callees(ctx, u.ID)
			if err == nil {
				ctxData["callees"] = len(callees)
				ctxData["callee_names"] = callerNames(callees)
			}
		}

		// References
		if strings.Contains(includeRaw, "references") || includeRaw == "" {
			refs, err := s.gr.References(ctx, u.ID)
			if err == nil {
				ctxData["references"] = len(refs)
			}
		}

		// Imports
		if strings.Contains(includeRaw, "imports") || includeRaw == "" {
			edges, err := s.st.EdgesFrom(ctx, u.ID, graph.EdgeImport)
			if err == nil {
				var importNames []string
				for _, e := range edges {
					importNames = append(importNames, e.DstName)
				}
				ctxData["imports"] = importNames
			}
		}

		if maxTokensPerSymbol > 0 {
			ctxData = truncateByTokens(ctxData, maxTokensPerSymbol)
		}

		br.Context = ctxData
		results = append(results, br)
	}

	return jsonResult(results)
}

func callerNames(units []store.ASTUnit) []string {
	names := make([]string, 0, len(units))
	for _, u := range units {
		names = append(names, u.Qualified)
	}
	return names
}

// ============================================================
// 11. code.get_file_intent
// ============================================================

func (s *CodeServer) handleGetFileIntent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := req.GetString("path", "")
	if path == "" {
		return mcp.NewToolResultError("path is required"), nil
	}
	useLLM := req.GetBool("use_llm", true)

	abs, err := s.resolveAbs(path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Базовая информация из AST
	units, err := s.st.ListASTUnitsByFile(ctx, abs)
	if err != nil {
		return nil, err
	}

	var symbols []string
	importSet := make(map[string]struct{})
	language := ""
	for _, u := range units {
		if u.Kind != "file" && u.Kind != "module" && u.Kind != "package" {
			symbols = append(symbols, u.Name)
		}
		edges, _ := s.st.EdgesFrom(ctx, u.ID, graph.EdgeImport)
		for _, e := range edges {
			importSet[e.DstName] = struct{}{}
		}
		if u.Language != "" {
			language = u.Language
		}
	}

	var imports []string
	for imp := range importSet {
		imports = append(imports, imp)
	}

	if !useLLM {
		// Без LLM — только tree-sitter summary
		repoName := ""
		if len(units) > 0 {
			repoName = units[0].Repo
		}
		return jsonResult(map[string]any{
			"path":       abs,
			"repo":       repoName,
			"language":   language,
			"symbols":    symbols,
			"imports":    imports,
			"top_symbols": topN(symbols, 10),
		})
	}

	// С LLM — через graph.Service
	if s.gr == nil {
		return mcp.NewToolResultError("graph service is not initialized"), nil
	}

	intent, err := s.gr.GetFileIntent(ctx, abs)
	if err != nil {
		return nil, err
	}

	return jsonResult(intent)
}

// ============================================================
// 12. code.stats (внутренний)
// ============================================================

func (s *CodeServer) handleStats(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	out := map[string]any{}

	if s.st != nil {
		st, err := s.st.GraphStats(ctx)
		if err == nil {
			out["ast_units"] = st.Units
			out["edges"] = st.Edges
		}
	}

	if s.qd != nil {
		codeName := s.cfg.CodeCollection().Name
		textName := s.cfg.TextCollection().Name
		if n, err := s.qd.Count(ctx, codeName); err == nil {
			out["code_chunks"] = n
		}
		if n, err := s.qd.Count(ctx, textName); err == nil {
			out["text_chunks"] = n
		}
	}

	if s.bm25 != nil {
		if n, err := s.bm25.Count(ctx); err == nil {
			out["bm25_docs"] = n
		}
	}

	return jsonResult(out)
}

// topN возвращает первые n элементов слайса.
func topN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
