// Package hybrid — слияние результатов векторного и BM25 поиска
// (с опциональным реранкингом сверху).
//
// Алгоритм по умолчанию: Reciprocal Rank Fusion (RRF), стабильный и не
// требующий калибровки скор разных источников. При заданных весах
// VectorWeight/BM25Weight используется weighted-sum нормализованных скор.
//
// СКЕЛЕТ: основные структуры и сигнатуры. Реальная склейка с index.Vector
// и bm25.Index будет добавлена в следующих итерациях.
package hybrid

import (
	"context"
	"errors"
	"sort"
)

// ErrNotImplemented — заглушка для нереализованных путей.
var ErrNotImplemented = errors.New("hybrid: not implemented yet (skeleton)")

// Source указывает, откуда пришёл кандидат.
type Source string

const (
	SrcVector Source = "vector"
	SrcBM25   Source = "bm25"
)

// Candidate — общая модель кандидата от любого ретривера.
type Candidate struct {
	ID        string
	Path      string
	Language  string
	Kind      string
	Symbol    string
	AstUnitID int64
	StartLine int
	EndLine   int
	Content   string
	Score     float64
	Source    Source
	Rank      int // позиция в исходной выдаче ретривера (1-based)
}

// Result — итоговый кандидат после слияния (и опционально реранкинга).
type Result struct {
	Candidate
	HybridScore float64
}

// Options — параметры слияния.
type Options struct {
	VectorWeight float64
	BM25Weight   float64
	RRFK         int // обычно 60
}

// VectorRetriever — абстракция над index.Vector, чтобы пакет не зависел
// от конкретной реализации.
type VectorRetriever interface {
	HybridCandidates(ctx context.Context, query string, limit int, filter map[string]any) ([]Candidate, error)
}

// BM25Retriever — абстракция над bm25.Index.
type BM25Retriever interface {
	HybridCandidates(ctx context.Context, query string, limit int) ([]Candidate, error)
}

// Engine — гибридный поисковый движок.
type Engine struct {
	Vec  VectorRetriever
	Lex  BM25Retriever
	Opts Options
}

// New создаёт Engine с указанными ретриверами.
func New(vec VectorRetriever, lex BM25Retriever, opts Options) *Engine {
	if opts.RRFK <= 0 {
		opts.RRFK = 60
	}
	return &Engine{Vec: vec, Lex: lex, Opts: opts}
}

// Search выполняет vector + BM25 поиск параллельно и сливает результаты.
// candidatesPerSource — сколько брать из каждого источника до слияния.
// limit — сколько вернуть в итоге.
//
// TODO: параллелизация через errgroup, фильтры, нормализация скор.
func (e *Engine) Search(ctx context.Context, query string, candidatesPerSource, limit int, filter map[string]any) ([]Result, error) {
	if e == nil || (e.Vec == nil && e.Lex == nil) {
		return nil, ErrNotImplemented
	}
	if candidatesPerSource <= 0 {
		candidatesPerSource = 50
	}

	var vecCands, lexCands []Candidate
	if e.Vec != nil {
		c, err := e.Vec.HybridCandidates(ctx, query, candidatesPerSource, filter)
		if err != nil {
			return nil, err
		}
		vecCands = c
	}
	if e.Lex != nil {
		c, err := e.Lex.HybridCandidates(ctx, query, candidatesPerSource)
		if err != nil {
			return nil, err
		}
		lexCands = c
	}

	merged := Merge(vecCands, lexCands, e.Opts)
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

// Merge сливает две выдачи. Если оба веса заданы (> 0) — weighted sum
// (требует нормализации скор), иначе — RRF.
// require BOTH weights > 0 to avoid silently dropping one source.
func Merge(vec, lex []Candidate, opts Options) []Result {
	useWeights := opts.VectorWeight > 0 && opts.BM25Weight > 0

	type acc struct {
		cand  Candidate
		score float64
	}
	m := make(map[string]*acc, len(vec)+len(lex))

	add := func(cands []Candidate, weight float64, normMax float64) {
		for i, c := range cands {
			a, ok := m[c.ID]
			if !ok {
				a = &acc{cand: c}
				m[c.ID] = a
			} else if a.cand.Content == "" {
				a.cand = c
			}
			if useWeights {
				if normMax > 0 {
					a.score += weight * (c.Score / normMax)
				}
			} else {
				// RRF
				rank := c.Rank
				if rank <= 0 {
					rank = i + 1
				}
				a.score += 1.0 / float64(opts.RRFK+rank)
			}
		}
	}

	if useWeights {
		add(vec, opts.VectorWeight, maxScore(vec))
		add(lex, opts.BM25Weight, maxScore(lex))
	} else {
		add(vec, 0, 0)
		add(lex, 0, 0)
	}

	out := make([]Result, 0, len(m))
	for _, a := range m {
		out = append(out, Result{Candidate: a.cand, HybridScore: a.score})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].HybridScore > out[j].HybridScore })
	return out
}

func maxScore(cs []Candidate) float64 {
	mx := 0.0
	for _, c := range cs {
		if c.Score > mx {
			mx = c.Score
		}
	}
	return mx
}
