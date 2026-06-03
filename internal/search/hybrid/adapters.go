package hybrid

import (
	"context"
	"fmt"
	"sort"

	"ragota/internal/indexing/vector"
)

// VectorHybridAdapter превращает vector.Vector в hybrid.VectorRetriever.
// Живёт на стороне потребителя (search/hybrid), а не продюсера.
type VectorHybridAdapter struct {
	V *vector.Vector
}

func (a *VectorHybridAdapter) HybridCandidates(ctx context.Context, query string, limit int, filter map[string]any) ([]Candidate, error) {
	if a == nil || a.V == nil {
		return nil, nil
	}
	hits, err := a.V.Search(ctx, query, limit, filter)
	if err != nil {
		return nil, err
	}
	// явная сортировка по score перед присвоением Rank
	sort.Slice(hits, func(i, j int) bool {
		return hits[i].Score > hits[j].Score
	})
	out := make([]Candidate, 0, len(hits))
	for i, h := range hits {
		path, _ := h.Payload["file"].(string)
		lang, _ := h.Payload["language"].(string)
		kind, _ := h.Payload["kind"].(string)
		sym, _ := h.Payload["symbol"].(string)
		text, _ := h.Payload["text"].(string)
		startLine, _ := h.Payload["start_line"].(float64)
		endLine, _ := h.Payload["end_line"].(float64)
		out = append(out, Candidate{
			ID:        fmt.Sprintf("%v", h.ID),
			Path:      path,
			Language:  lang,
			Kind:      kind,
			Symbol:    sym,
			StartLine: int(startLine),
			EndLine:   int(endLine),
			Content:   text,
			Score:     float64(h.Score),
			Source:    SrcVector,
			Rank:      i + 1,
		})
	}
	return out, nil
}
