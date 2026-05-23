package index

import (
	"context"
	"fmt"

	"aitools/internal/bm25"
	"aitools/internal/hybrid"
)

// VectorHybridAdapter превращает Vector в hybrid.VectorRetriever.
type VectorHybridAdapter struct {
	V *Vector
}

func (a *VectorHybridAdapter) HybridCandidates(ctx context.Context, query string, limit int, filter map[string]any) ([]hybrid.Candidate, error) {
	if a == nil || a.V == nil {
		return nil, nil
	}
	hits, err := a.V.Search(ctx, query, limit, filter)
	if err != nil {
		return nil, err
	}
	out := make([]hybrid.Candidate, 0, len(hits))
	for i, h := range hits {
		path, _ := h.Payload["file"].(string)
		lang, _ := h.Payload["language"].(string)
		kind, _ := h.Payload["kind"].(string)
		sym, _ := h.Payload["symbol"].(string)
		text, _ := h.Payload["text"].(string)
		startLine, _ := h.Payload["start_line"].(float64)
		endLine, _ := h.Payload["end_line"].(float64)
		out = append(out, hybrid.Candidate{
			ID:        fmt.Sprintf("%v", h.ID),
			Path:      path,
			Language:  lang,
			Kind:      kind,
			Symbol:    sym,
			StartLine: int(startLine),
			EndLine:   int(endLine),
			Content:   text,
			Score:     float64(h.Score),
			Source:    hybrid.SrcVector,
			Rank:      i + 1,
		})
	}
	return out, nil
}

// BM25HybridAdapter превращает bm25.Index в hybrid.BM25Retriever.
type BM25HybridAdapter struct {
	Idx bm25.Index
}

func (a *BM25HybridAdapter) HybridCandidates(ctx context.Context, query string, limit int) ([]hybrid.Candidate, error) {
	if a == nil || a.Idx == nil {
		return nil, nil
	}
	hits, err := a.Idx.Search(ctx, bm25.Query{Text: query, Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]hybrid.Candidate, 0, len(hits))
	for i, h := range hits {
		out = append(out, hybrid.Candidate{
			ID:        h.ID,
			Path:      h.Path,
			Language:  h.Language,
			Kind:      h.Kind,
			Symbol:    h.Symbol,
			AstUnitID: h.AstUnitID,
			StartLine: h.StartLine,
			EndLine:   h.EndLine,
			Content:   h.Snippet,
			Score:     h.Score,
			Source:    hybrid.SrcBM25,
			Rank:      i + 1,
		})
	}
	return out, nil
}
