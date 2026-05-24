package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"aitools/internal/bm25"
	"aitools/internal/config"
	"aitools/internal/fileutil"
	"aitools/internal/hybrid"
	"aitools/internal/index"
	"aitools/internal/qdrant"
	"aitools/internal/rerank"
	"aitools/internal/state"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// VectorServer — MCP-сервер семантического / гибридного поиска и реранкинга.
type VectorServer struct {
	cfg *config.Config
	idx *index.Vector
	qd  *qdrant.Client
	bus *state.Bus

	bm25 bm25.Index
	rer  rerank.Reranker
}

// NewVectorServer создаёт сервер.
func NewVectorServer(cfg *config.Config, idx *index.Vector, qd *qdrant.Client, bus *state.Bus) *VectorServer {
	return &VectorServer{cfg: cfg, idx: idx, qd: qd, bus: bus}
}

// SetBM25 подключает BM25-индекс (используется в search_keyword/search_hybrid).
func (s *VectorServer) SetBM25(b bm25.Index) { s.bm25 = b }

// SetReranker подключает реранкер.
func (s *VectorServer) SetReranker(r rerank.Reranker) { s.rer = r }

// Build регистрирует tools.
func (s *VectorServer) Build() *server.MCPServer {
	srv := server.NewMCPServer("ai-tools-vector", "0.1.0",
		server.WithToolCapabilities(false),
	)

	srv.AddTool(mcp.NewTool("vec.search",
		mcp.WithDescription("Hybrid search over project code (alias for vec.search_hybrid). repo: '*' or omitted = all repos; pass a name or JSON array of names to scope."),
		mcp.WithString("query", mcp.Required()),
		mcp.WithNumber("limit", mcp.DefaultNumber(10)),
		mcp.WithString("language"),
		mcp.WithString("repo", mcp.Description("Repo scope: empty/'*' = all; 'name' or JSON array ['a','b'].")),
	), s.wrap("vec.search", s.handleSearchHybrid))

	srv.AddTool(mcp.NewTool("vec.search_semantic",
		mcp.WithDescription("Vector-only semantic search (qwen3-embedding for code, nomic-embed-text for markdown). Multi-repo: default scope = all repos."),
		mcp.WithString("query", mcp.Required()),
		mcp.WithNumber("top_k", mcp.DefaultNumber(10)),
		mcp.WithString("language"),
		mcp.WithString("repo", mcp.Description("Repo scope: empty/'*' = all; 'name' or JSON array ['a','b'].")),
	), s.wrap("vec.search_semantic", s.handleSearch))

	srv.AddTool(mcp.NewTool("vec.search_keyword",
		mcp.WithDescription("BM25 lexical search (Bleve). Multi-repo: default scope = all repos."),
		mcp.WithString("query", mcp.Required()),
		mcp.WithNumber("top_k", mcp.DefaultNumber(10)),
		mcp.WithString("language"),
		mcp.WithString("kind", mcp.Description("Optional AST kind filter: function/class/...")),
		mcp.WithString("repo", mcp.Description("Repo scope: empty/'*' = all; 'name' or JSON array ['a','b'].")),
	), s.wrap("vec.search_keyword", s.handleSearchKeyword))

	srv.AddTool(mcp.NewTool("vec.search_hybrid",
		mcp.WithDescription("Hybrid retrieval: vector + BM25 merged via RRF (or weighted sum). Multi-repo: default scope = all repos."),
		mcp.WithString("query", mcp.Required()),
		mcp.WithNumber("top_k", mcp.DefaultNumber(10)),
		mcp.WithString("language"),
		mcp.WithString("repo", mcp.Description("Repo scope: empty/'*' = all; 'name' or JSON array ['a','b'].")),
	), s.wrap("vec.search_hybrid", s.handleSearchHybrid))

	srv.AddTool(mcp.NewTool("vec.rerank",
		mcp.WithDescription("Rerank candidates using BGE Reranker (Ollama). Falls back to identity ordering on unavailability."),
		mcp.WithString("query", mcp.Required()),
		mcp.WithString("candidates", mcp.Required(), mcp.Description("JSON array of candidates: [{id, content, score?, path?, language?, kind?, symbol?}]")),
		mcp.WithNumber("top_n", mcp.DefaultNumber(20)),
	), s.wrap("vec.rerank", s.handleRerank))

	srv.AddTool(mcp.NewTool("vec.reindex",
		mcp.WithDescription("Re-index a file (or full scan when path is empty)."),
		mcp.WithString("path"),
	), s.wrap("vec.reindex", s.handleReindex))

	srv.AddTool(mcp.NewTool("vec.count",
		mcp.WithDescription("Return number of indexed chunks in Qdrant + BM25."),
	), s.wrap("vec.count", s.handleCount))

	return srv
}

func (s *VectorServer) wrap(name string, fn func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res, err := fn(ctx, req)
		if err != nil {
			return errorToResult(name, err)
		}
		if s.bus != nil {
			s.bus.IncMCPCall("vector", name, false)
		}
		return res, nil
	}
}

func (s *VectorServer) handleSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := req.GetString("query", "")
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}
	limit := int(req.GetFloat("limit", 0))
	if limit <= 0 {
		limit = int(req.GetFloat("top_k", 10))
	}
	if limit <= 0 {
		limit = 10
	}
	filter := map[string]any{}
	if lang := req.GetString("language", ""); lang != "" {
		filter["language"] = lang
	}
	if rv := parseRepoParam(req.GetString("repo", "")); rv != nil {
		filter["repo"] = rv
	}
	hits, err := s.idx.Search(ctx, query, limit, filter)
	if err != nil {
		return nil, err
	}
	return jsonResult(hits)
}

func (s *VectorServer) handleSearchKeyword(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := req.GetString("query", "")
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}
	if s.bm25 == nil {
		return mcp.NewToolResultError("bm25 index is not configured"), nil
	}
	limit := int(req.GetFloat("top_k", 10))
	hits, err := s.bm25.Search(ctx, bm25.Query{
		Text:     query,
		Language: req.GetString("language", ""),
		Kind:     req.GetString("kind", ""),
		Repos:    parseRepoListParam(req.GetString("repo", "")),
		Limit:    limit,
	})
	if err != nil {
		return nil, err
	}
	return jsonResult(hits)
}

func (s *VectorServer) handleSearchHybrid(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := req.GetString("query", "")
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}
	limit := int(req.GetFloat("top_k", 0))
	if limit <= 0 {
		limit = int(req.GetFloat("limit", 10))
	}
	filter := map[string]any{}
	if lang := req.GetString("language", ""); lang != "" {
		filter["language"] = lang
	}
	if rv := parseRepoParam(req.GetString("repo", "")); rv != nil {
		filter["repo"] = rv
	}

	vecRet := &index.VectorHybridAdapter{V: s.idx}
	var lexRet hybrid.BM25Retriever
	if s.bm25 != nil {
		lexRet = &index.BM25HybridAdapter{Idx: s.bm25}
	}

	eng := hybrid.New(vecRet, lexRet, hybrid.Options{
		VectorWeight: s.cfg.Hybrid.VectorWeight,
		BM25Weight:   s.cfg.Hybrid.BM25Weight,
		RRFK:         s.cfg.Hybrid.RRFK,
	})
	cands := s.cfg.Hybrid.CandidatesPerSource
	if cands <= 0 {
		cands = 50
	}
	res, err := eng.Search(ctx, query, cands, limit, filter)
	if err != nil {
		return nil, err
	}
	return jsonResult(res)
}

func (s *VectorServer) handleRerank(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := req.GetString("query", "")
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}
	raw := req.GetString("candidates", "")
	if raw == "" {
		return mcp.NewToolResultError("candidates JSON is required"), nil
	}
	var input []rerank.Candidate
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		return mcp.NewToolResultError("invalid candidates JSON: " + err.Error()), nil
	}
	topN := int(req.GetFloat("top_n", float64(s.cfg.Rerank.TopN)))
	if s.rer == nil {
		// Без подключённого реранкера ведём себя как graceful identity.
		out := make([]rerank.Scored, 0, len(input))
		for _, c := range input {
			out = append(out, rerank.Scored{Candidate: c, RerankScore: c.Score})
		}
		if topN > 0 && len(out) > topN {
			out = out[:topN]
		}
		return jsonResult(out)
	}
	scored, err := s.rer.Rerank(ctx, query, input, topN)
	if err != nil {
		return nil, err
	}
	return jsonResult(scored)
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
	out := map[string]any{}
	codeName := s.cfg.CodeCollection().Name
	textName := s.cfg.TextCollection().Name
	if n, err := s.qd.Count(ctx, codeName); err == nil {
		out["code_chunks"] = n
		out["code_collection"] = codeName
	}
	if n, err := s.qd.Count(ctx, textName); err == nil {
		out["text_chunks"] = n
		out["text_collection"] = textName
	}
	if s.bm25 != nil {
		if n, err := s.bm25.Count(ctx); err == nil {
			out["bm25_docs"] = n
		}
	}
	return jsonResult(out)
}
