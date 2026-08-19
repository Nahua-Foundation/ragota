package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/graph"
	"github.com/Nahua-Foundation/ragota/internal/index"
)

// This file implements /context: graph-expanded retrieval. It is the search
// pipeline (app.go) followed by a graph expansion around each hit — the
// ready-to-use context package the HTTP layer serves to an LLM.

// ContextItem is one search hit enriched with its unit and graph neighborhood.
type ContextItem struct {
	Hit     *index.Hit           `json:"hit"`
	Unit    *domain.ASTUnit      `json:"unit,omitempty"`
	Service string               `json:"service,omitempty"`
	Related []*graph.RelatedUnit `json:"related,omitempty"`
}

// ContextResult is a ready-to-use retrieval package for an LLM:
// relevant snippets plus the code graph around them.
type ContextResult struct {
	Query string `json:"query"`
	Mode  string `json:"mode"`
	// RewrittenQuery is set when the assistant LLM rewrote the query into a
	// keyword-style search query before retrieval.
	RewrittenQuery string         `json:"rewritten_query,omitempty"`
	Items          []*ContextItem `json:"items"`
}

// contextOverFetch is how many hits are retrieved per requested context item.
// Hits are deduplicated by unit, and several chunks of one function are the
// normal case, so retrieving exactly limit hits would return fewer items.
const contextOverFetch = 4

// BuildContext runs a search and expands each hit through the code graph:
// callers/callees, contracts (gRPC/HTTP/Kafka) and their far sides. intent is
// the request's query intent ("", "auto", "callers", "none" — see intent.go);
// with the assistant rewrite enabled, auto-detection sees the rewritten query,
// so a client that wants the callers answer regardless should pass it
// explicitly.
func (s *Service) BuildContext(ctx context.Context, query string, repos []string, mode string, limit, hops int, intent string) (*ContextResult, error) {
	if limit <= 0 {
		limit = 5
	}
	if limit > 20 {
		limit = 20
	}
	if mode == "" {
		mode = "keyword"
	}

	// Intent is resolved on the user's own words, before any rewrite, and a
	// question the detector recognises skips the rewrite entirely. The
	// rewrite is phrased as keywords, which is exactly what the detector
	// cannot read: measured on the eval set, rewriting turned rank-1 graph
	// answers into misses and cost the callers shape recall@10 0.400 -> 0.200.
	// A question the graph can answer has nothing to gain from it anyway.
	resolved, _, err := s.promote.ResolveIntent(&index.SearchQuery{Query: query, Intent: intent})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}

	// Optional assistant hook: rewrite the natural-language query into a
	// keyword-style search query before retrieval.
	searchQuery, rewritten := query, false
	if resolved != "" {
		intent = resolved // pin it: the searcher must not re-detect
	} else {
		searchQuery, rewritten = s.enrich.RewriteQuery(ctx, query)
	}

	searchRes, err := s.searchForContext(ctx, searchQuery, repos, mode, limit, intent)
	if err != nil {
		return nil, err
	}

	// A rewrite that retrieves nothing is worse than no rewrite: fall back to
	// the user's own wording rather than returning an empty context.
	if rewritten && len(searchRes.Hits) == 0 {
		original, oerr := s.searchForContext(ctx, query, repos, mode, limit, intent)
		if oerr == nil && len(original.Hits) > 0 {
			slog.Warn("rewritten query returned no hits; falling back to the original",
				"query", query, "rewritten", searchQuery)
			searchRes, searchQuery, rewritten = original, query, false
		}
	}

	result := &ContextResult{Query: query, Mode: mode, Items: []*ContextItem{}}
	if rewritten {
		result.RewrittenQuery = searchQuery
	}

	seen := make(map[string]bool, limit)
	for _, hit := range searchRes.Hits {
		if len(result.Items) >= limit {
			break
		}

		item := &ContextItem{Hit: hit}
		key := hit.RepoID + "\x00" + hit.FilePath
		unit, uerr := s.graph.UnitInRange(ctx, hit.RepoID, hit.FilePath, hit.Line, hit.EndLine)
		if uerr == nil {
			key = unit.ID
			item.Unit = unit
			item.Service = s.graph.ServiceOfUnit(ctx, unit)
			related, rerr := s.graph.Expand(ctx, unit.ID, hops)
			if rerr == nil {
				item.Related = related
			}
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		result.Items = append(result.Items, item)
	}
	return result, nil
}

// searchForContext retrieves enough hits to fill limit distinct units.
func (s *Service) searchForContext(ctx context.Context, query string, repos []string, mode string, limit int, intent string) (*index.SearchResult, error) {
	return s.Search(ctx, &index.SearchQuery{
		Query:  query,
		Repos:  repos,
		Limit:  limit * contextOverFetch,
		Intent: intent,
	}, mode)
}
