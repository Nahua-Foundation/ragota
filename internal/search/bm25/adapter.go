package bm25

import (
	"context"

	"ragota/internal/indexing/vector"
	"ragota/internal/search/hybrid"
)

// bm25Sink адаптирует bm25.Index к интерфейсу vector.WriteSink.
// Продюсер (indexing/vector) определяет контракт, потребитель (bm25) адаптируется.
type bm25Sink struct {
	idx Index
}

// AsWriteSink оборачивает bm25.Index в vector.WriteSink.
func AsWriteSink(idx Index) vector.WriteSink {
	return bm25Sink{idx: idx}
}

func (s bm25Sink) IndexDocs(ctx context.Context, docs []vector.Doc) error {
	bm25Docs := make([]Doc, len(docs))
	for i, d := range docs {
		bm25Docs[i] = Doc{
			ID:        d.ID,
			Repo:      d.Repo,
			Path:      d.Path,
			Language:  d.Language,
			Kind:      d.Kind,
			Symbol:    d.Symbol,
			Content:   d.Content,
			AstUnitID: 0,
			StartLine: d.StartLine,
			EndLine:   d.EndLine,
		}
	}
	return s.idx.IndexDocs(ctx, bm25Docs)
}

func (s bm25Sink) DeleteByPath(ctx context.Context, path string) error {
	return s.idx.DeleteByPath(ctx, path)
}

func (s bm25Sink) Clear(ctx context.Context) error {
	return s.idx.Clear(ctx)
}

func (s bm25Sink) Count(ctx context.Context) (uint64, error) {
	return s.idx.Count(ctx)
}

func (s bm25Sink) Search(ctx context.Context, q vector.SearchQuery) ([]vector.SearchResult, error) {
	hits, err := s.idx.Search(ctx, Query{
		Text:     q.Text,
		Language: q.Language,
		Kind:     q.Kind,
		Repos:    q.Repos,
		Limit:    q.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]vector.SearchResult, len(hits))
	for i, h := range hits {
		out[i] = vector.SearchResult{
			ID:        h.ID,
			Score:     h.Score,
			Repo:      h.Repo,
			Path:      h.Path,
			Language:  h.Language,
			Kind:      h.Kind,
			Symbol:    h.Symbol,
			Snippet:   h.Snippet,
			StartLine: h.StartLine,
			EndLine:   h.EndLine,
		}
	}
	return out, nil
}

// BM25HybridAdapter превращает vector.WriteSink в hybrid.BM25Retriever.
// Переехал сюда из indexing/vector/ — адаптер должен жить на стороне потребителя.
type BM25HybridAdapter struct {
	Idx vector.WriteSink
}

func (a *BM25HybridAdapter) HybridCandidates(ctx context.Context, query string, limit int) ([]hybrid.Candidate, error) {
	if a == nil || a.Idx == nil {
		return nil, nil
	}
	hits, err := a.Idx.Search(ctx, vector.SearchQuery{Text: query, Limit: limit})
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
			AstUnitID: 0,
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

// BM25CountWrapper — простая обёртка для вызова Count без кастинга.
func BM25Count(ctx context.Context, idx Index) (uint64, error) {
	return idx.Count(ctx)
}
