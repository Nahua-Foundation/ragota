package search

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/indexing"
	"github.com/Nahua-Foundation/ragota/internal/llm"
	"github.com/Nahua-Foundation/ragota/internal/obs"
)

// FusionMethod is the method for fusing results from multiple indexers.
type FusionMethod string

const (
	// FusionRRF uses Reciprocal Rank Fusion.
	FusionRRF FusionMethod = "rrf"
)

// Config is the configuration for the search service.
type Config struct {
	// RRF constant k (default 60).
	RRFK float64
	// Weights for each indexer type.
	Weights map[indexing.IndexType]float32
}

// DefaultConfig returns default search configuration.
//
// The weights are deliberately equal. RRF only interleaves sources while
// their per-rank contributions overlap; with k=60 and 20 candidates per
// source, a source weighted w contributes between w/61 and w/80, so any
// weight below 80/61 ≈ 0.76 of the other's puts its ENTIRE range under the
// other source's last rank — every hit of the favoured source then outranks
// every unshared hit of the other, and after truncation the weaker source
// can only confirm results, never contribute them. Measured on the retrieval
// eval, a 1.0/0.7 split turned "hybrid" into vector-only: answers keyword
// ranked first fell out of the top twenty entirely.
func DefaultConfig() *Config {
	return &Config{
		RRFK: 60.0,
		Weights: map[indexing.IndexType]float32{
			indexing.IndexTypeVector: 1.0,
			indexing.IndexTypeBM25:   1.0,
		},
	}
}

// getWeight returns the weight for an indexer type; defaults to 1.0 if unset.
func (s *Service) getWeight(typ indexing.IndexType) float32 {
	if w, ok := s.config.Weights[typ]; ok {
		return w
	}
	return 1.0
}

// defaultRerankTopN is how many leading candidates are fed to the reranker
// when no explicit top_n is configured.
const defaultRerankTopN = 50

// searchOrder fixes the order in which searchers are queried and fused, so
// that fusion (which merges overlapping regions on a first-seen basis) is
// deterministic regardless of goroutine scheduling.
var searchOrder = []indexing.IndexType{indexing.IndexTypeVector, indexing.IndexTypeBM25}

// Service is the search service.
type Service struct {
	searchers  map[indexing.IndexType]indexing.Searcher
	config     *Config
	reranker   llm.Reranker // optional rerank stage over top results
	rerankTopN int
}

// SetReranker enables a rerank stage over the leading search results. topN
// caps how many candidates are fed to the reranker (<=0 means default 50).
func (s *Service) SetReranker(r llm.Reranker, topN int) {
	s.reranker = r
	if topN <= 0 {
		topN = defaultRerankTopN
	}
	s.rerankTopN = topN
}

// New creates a new search service.
func New(searchers map[indexing.IndexType]indexing.Searcher, cfg *Config) *Service {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	return &Service{
		searchers: searchers,
		config:    cfg,
	}
}

// topN returns the configured rerank window size.
func (s *Service) topN() int {
	if s.reranker == nil {
		return 0
	}
	if s.rerankTopN <= 0 {
		return defaultRerankTopN
	}
	return s.rerankTopN
}

// candidateQuery returns a copy of the query asking each searcher for enough
// candidates to fill the rerank window. Retrieving only the caller's limit
// would make top_n meaningless: the reranker could then merely reorder the
// documents that were going to be returned anyway.
func (s *Service) candidateQuery(query *indexing.SearchQuery) *indexing.SearchQuery {
	n := s.topN()
	if n <= query.Limit {
		return query
	}
	candidates := *query
	candidates.Limit = n
	return &candidates
}

// Search performs a search query with optional fusion.
func (s *Service) Search(ctx context.Context, query *indexing.SearchQuery, fusion bool) (*indexing.SearchResult, error) {
	start := time.Now()

	// If fusion is disabled, use vector only
	if !fusion {
		return s.singleSearch(ctx, indexing.IndexTypeVector, query)
	}

	hits, meta, err := s.Candidates(ctx, query)
	if err != nil {
		return nil, err
	}
	hits = s.Rank(ctx, query.Query, hits, meta, query.Limit)

	return &indexing.SearchResult{
		Hits:     hits,
		Total:    len(hits),
		Query:    query.Query,
		Duration: time.Since(start),
		Metadata: meta,
	}, nil
}

// Candidates is the retrieval half of Search: it queries every searcher and
// fuses the results, without ranking or truncating. A caller that has its own
// candidates to contribute — the code graph knows call sites that no text
// document describes — merges them into this list and then calls Rank, so
// that its candidates are judged by the same rank stage as everything else
// instead of being stapled to the front of a finished result.
func (s *Service) Candidates(ctx context.Context, query *indexing.SearchQuery) ([]*indexing.Hit, map[string]interface{}, error) {
	results, failures := s.collectResults(ctx, query)
	meta := map[string]interface{}{}
	if len(results) == 0 {
		if len(failures) > 0 {
			return nil, meta, fmt.Errorf("all searchers failed: %w", primaryFailure(failures))
		}
		return []*indexing.Hit{}, meta, nil
	}
	hits := s.fuseRRF(results)
	addFailureMetadata(meta, results, failures)
	return hits, meta, nil
}

// CandidatesFrom is Candidates for a single searcher (the keyword-only and
// semantic-only modes). An unknown or unconfigured searcher yields no
// candidates rather than an error, matching singleSearch.
func (s *Service) CandidatesFrom(ctx context.Context, typ indexing.IndexType, query *indexing.SearchQuery) ([]*indexing.Hit, map[string]interface{}, error) {
	srch, ok := s.searchers[typ]
	if !ok {
		return []*indexing.Hit{}, map[string]interface{}{}, nil
	}
	result, err := srch.Search(ctx, s.candidateQuery(query))
	if err != nil {
		return nil, nil, err
	}
	if result == nil {
		return nil, nil, fmt.Errorf("searcher %s returned no result", typ)
	}
	settleSearcherOrder(result)
	meta := result.Metadata
	if meta == nil {
		meta = map[string]interface{}{}
	}
	return result.Hits, meta, nil
}

// Rank is the ranking half of Search: rerank the leading candidates and cut
// the list down to limit.
func (s *Service) Rank(ctx context.Context, query string, hits []*indexing.Hit, meta map[string]interface{}, limit int) []*indexing.Hit {
	return truncate(s.applyRerank(ctx, query, hits, meta), limit)
}

// singleSearch runs one searcher, reranks the leading candidates and cuts the
// result down to the caller's limit.
func (s *Service) singleSearch(ctx context.Context, typ indexing.IndexType, query *indexing.SearchQuery) (*indexing.SearchResult, error) {
	start := time.Now()

	srch, ok := s.searchers[typ]
	if !ok {
		return &indexing.SearchResult{
			Hits:     []*indexing.Hit{},
			Total:    0,
			Query:    query.Query,
			Duration: time.Since(start),
		}, nil
	}

	result, err := srch.Search(ctx, s.candidateQuery(query))
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("searcher %s returned no result", typ)
	}
	settleSearcherOrder(result)

	meta := result.Metadata
	if meta == nil {
		meta = map[string]interface{}{}
	}
	result.Hits = truncate(s.applyRerank(ctx, query.Query, result.Hits, meta), query.Limit)
	result.Total = len(result.Hits)
	result.Metadata = meta
	return result, nil
}

// truncate cuts hits down to limit (limit <= 0 means "no limit").
func truncate(hits []*indexing.Hit, limit int) []*indexing.Hit {
	if limit > 0 && len(hits) > limit {
		return hits[:limit]
	}
	return hits
}

// searcherFailure records a searcher that could not answer the query.
type searcherFailure struct {
	source indexing.IndexType
	err    error
}

// primaryFailure picks the failure worth reporting when every searcher failed.
// A damaged index outranks the rest: it names a condition the caller can act on
// and no retry can clear, and it must stay visible in the error chain even when
// a searcher that failed for some ordinary reason was collected first.
func primaryFailure(failures []searcherFailure) error {
	for _, f := range failures {
		if errors.Is(f.err, indexing.ErrIndexDamaged) {
			return f.err
		}
	}
	return failures[0].err
}

// collectResults queries all available searchers concurrently. A searcher that
// fails is reported separately instead of being silently dropped; the caller
// decides whether the remaining ones are enough.
func (s *Service) collectResults(ctx context.Context, query *indexing.SearchQuery) ([]indexedResult, []searcherFailure) {
	candidates := s.candidateQuery(query)

	type outcome struct {
		result *indexing.SearchResult
		err    error
	}
	outcomes := make([]outcome, len(searchOrder))

	var wg sync.WaitGroup
	for i, typ := range searchOrder {
		srch, ok := s.searchers[typ]
		if !ok {
			continue
		}
		wg.Add(1)
		go func(i int, srch indexing.Searcher) {
			defer wg.Done()
			res, err := srch.Search(ctx, candidates)
			outcomes[i] = outcome{result: res, err: err}
		}(i, srch)
	}
	wg.Wait()

	var results []indexedResult
	var failures []searcherFailure
	for i, typ := range searchOrder {
		if _, ok := s.searchers[typ]; !ok {
			continue
		}
		out := outcomes[i]
		switch {
		case out.err != nil:
			slog.Warn("searcher failed; hybrid search degraded", "searcher", typ, "error", out.err)
			obs.Inc("ragota_search_searcher_failures_total", 1)
			failures = append(failures, searcherFailure{source: typ, err: out.err})
		case out.result == nil:
			err := fmt.Errorf("searcher returned nil result")
			slog.Warn("searcher failed; hybrid search degraded", "searcher", typ, "error", err)
			obs.Inc("ragota_search_searcher_failures_total", 1)
			failures = append(failures, searcherFailure{source: typ, err: err})
		default:
			settleSearcherOrder(out.result)
			results = append(results, indexedResult{result: out.result, source: typ})
		}
	}
	return results, failures
}

// addFailureMetadata records which searchers contributed and which failed, so
// a caller can tell a genuinely empty result from a degraded one.
func addFailureMetadata(meta map[string]interface{}, results []indexedResult, failures []searcherFailure) {
	used := make([]string, 0, len(results))
	for _, r := range results {
		used = append(used, string(r.source))
	}
	meta["searchers"] = used
	if len(failures) == 0 {
		return
	}
	failed := make([]string, 0, len(failures))
	errsBySearcher := make(map[string]string, len(failures))
	for _, f := range failures {
		failed = append(failed, string(f.source))
		errsBySearcher[string(f.source)] = f.err.Error()
	}
	meta["degraded"] = true
	meta["failed_searchers"] = failed
	meta["searcher_errors"] = errsBySearcher
}

// cluster is one fused result: a representative hit plus the accumulated RRF
// score and the reasons of every hit merged into it.
type cluster struct {
	hit      *indexing.Hit
	score    float32
	bestRank int
	sources  map[indexing.IndexType]bool
	reasons  []string
}

// fuseRRF fuses results using Reciprocal Rank Fusion. Hits covering the same
// file region are merged even when the indexes chunk differently (BM25 uses
// line windows while the vector index may use symbol cards), otherwise the
// same code competes with itself and never earns cross-retriever agreement.
// Only hits from *different* searchers are merged: overlapping windows from
// one searcher are separate candidates by construction.
func (s *Service) fuseRRF(results []indexedResult) []*indexing.Hit {
	k := s.config.RRFK
	if k == 0 {
		k = 60.0
	}

	byKey := map[string]*cluster{}
	byFile := map[string][]*cluster{}
	var order []*cluster

	for _, indexed := range results {
		weight := s.getWeight(indexed.source)
		for rank, hit := range indexed.result.Hits {
			if hit == nil {
				continue
			}
			contribution := float32(1.0/(k+float64(rank+1))) * weight

			key := hit.Key()
			fileKey := hit.RepoID + "\x00" + hit.FilePath

			c := byKey[key]
			if c == nil {
				for _, candidate := range byFile[fileKey] {
					if !candidate.sources[indexed.source] && candidate.hit.Overlaps(hit) {
						c = candidate
						break
					}
				}
			}

			if c == nil {
				c = &cluster{
					hit:      hit,
					bestRank: rank,
					sources:  map[indexing.IndexType]bool{},
				}
				byKey[key] = c
				byFile[fileKey] = append(byFile[fileKey], c)
				order = append(order, c)
			} else if rank < c.bestRank {
				// Keep the higher-ranked hit as the cluster's representative.
				c.hit = hit
				c.bestRank = rank
			}

			c.score += contribution
			c.sources[indexed.source] = true
			c.reasons = appendReason(c.reasons, hit.Reason, indexed.source)
		}
	}

	hits := make([]*indexing.Hit, 0, len(order))
	for _, c := range order {
		hitCopy := *c.hit
		hitCopy.Score = c.score
		hitCopy.Reason = strings.Join(c.reasons, "+")
		hits = append(hits, &hitCopy)
	}

	sortHits(hits)
	return hits
}

// appendReason records a contributing source's reason, keeping the list unique
// and in first-seen order so a hit found by two indexes reports both.
func appendReason(reasons []string, reason string, source indexing.IndexType) []string {
	if reason == "" {
		reason = string(source)
	}
	for _, part := range strings.Split(reason, "+") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !contains(reasons, part) {
			reasons = append(reasons, part)
		}
	}
	return reasons
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// settleSearcherOrder puts one searcher's hits into the order the rest of the
// pipeline is entitled to assume before anything reads their positions.
//
// A searcher returns hits in score order and says nothing about how it arranges
// the ones that tie. The BM25 leg hands back bleve's internal document order,
// which records how the indexing goroutines happened to write the documents and
// not anything the corpus says — the same defect as ordering SQL rows by their
// autoincrement id. It would be harmless if a tie stayed a tie, but fuseRRF
// scores a hit by its *position* in this list, so a tie is converted into a
// difference in the fused score and the sortHits below, which runs afterwards,
// has nothing left to settle.
//
// Measured on the boutique corpus: "which services ask the shipping service for
// a shipping quote" returned src/frontend/rpc.go and src/frontend/handlers.go —
// identical BM25 scores — at ranks 12 and 13 in either order between two builds
// of the same sources, moving the question's span rank and the run's nDCG.
//
// It is a no-op for a searcher whose hits already carry distinct scores.
func settleSearcherOrder(result *indexing.SearchResult) {
	if result != nil {
		sortHits(result.Hits)
	}
}

// sortHits sorts hits by score descending with deterministic tie-breaking.
func sortHits(hits []*indexing.Hit) {
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		if hits[i].RepoID != hits[j].RepoID {
			return hits[i].RepoID < hits[j].RepoID
		}
		if hits[i].FilePath != hits[j].FilePath {
			return hits[i].FilePath < hits[j].FilePath
		}
		return hits[i].Line < hits[j].Line
	})
}

// indexedResult holds a search result with its source.
type indexedResult struct {
	result *indexing.SearchResult
	source indexing.IndexType
}

// SemanticSearch performs semantic-only search.
func (s *Service) SemanticSearch(ctx context.Context, query *indexing.SearchQuery) (*indexing.SearchResult, error) {
	return s.singleSearch(ctx, indexing.IndexTypeVector, query)
}

// KeywordSearch performs keyword-only search (BM25).
func (s *Service) KeywordSearch(ctx context.Context, query *indexing.SearchQuery) (*indexing.SearchResult, error) {
	return s.singleSearch(ctx, indexing.IndexTypeBM25, query)
}

// applyRerank reorders the first min(rerankTopN, len(hits)) hits by reranker
// scores; the tail keeps its original order. Reranked hits keep scores in the
// fusion range (see blendRerankScores) and get "+rerank" appended to Reason.
// Any reranker failure is logged and the original order is returned — search
// must never fail because of the reranker.
func (s *Service) applyRerank(ctx context.Context, query string, hits []*indexing.Hit, meta map[string]interface{}) []*indexing.Hit {
	if s.reranker == nil || len(hits) <= 1 {
		return hits
	}

	n := s.topN()
	if n > len(hits) {
		n = len(hits)
	}

	docs := make([]string, n)
	for i, h := range hits[:n] {
		doc := h.Snippet
		if doc == "" {
			doc = strings.TrimSpace(strings.Join(nonEmpty(h.Kind, h.Symbol, h.Path), " "))
		}
		if doc == "" {
			doc = h.FilePath
		}
		docs[i] = doc
	}

	start := time.Now()
	scores, err := s.reranker.Rerank(ctx, query, docs)
	obs.RecordDuration("ragota_rerank_seconds", time.Since(start).Seconds())
	if err == nil && len(scores) != n {
		err = fmt.Errorf("got %d scores for %d documents", len(scores), n)
	}
	if err != nil {
		obs.Inc("ragota_rerank_failures_total", 1)
		slog.Warn("rerank failed; keeping original order",
			"reranker", s.reranker.Name(), "error", err)
		if meta != nil {
			meta["reranked"] = false
			meta["rerank_error"] = err.Error()
		}
		return hits
	}

	blended := blendRerankScores(hits[:n], hits[n:], scores)

	head := make([]*indexing.Hit, n)
	for i, h := range hits[:n] {
		hitCopy := *h
		hitCopy.Score = blended[i]
		if hitCopy.Reason != "" {
			hitCopy.Reason += "+rerank"
		} else {
			hitCopy.Reason = "rerank"
		}
		head[i] = &hitCopy
	}
	sort.SliceStable(head, func(i, j int) bool {
		return head[i].Score > head[j].Score
	})

	if meta != nil {
		meta["reranked"] = true
		meta["rerank_candidates"] = n
	}

	out := make([]*indexing.Hit, 0, len(hits))
	out = append(out, head...)
	out = append(out, hits[n:]...)
	return out
}

func nonEmpty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// blendRerankScores maps cross-encoder scores onto the fusion score range of
// the reranked head. Cross-encoders emit logits (often negative) that are not
// comparable with the RRF scores kept by the tail, so returning them verbatim
// would make a client that sorts by score produce a different order than the
// one served. The mapping is monotone in the rerank score and stays inside the
// head's own fusion range, never dipping below the best tail score.
func blendRerankScores(head, tail []*indexing.Hit, scores []float64) []float32 {
	out := make([]float32, len(head))

	hi, lo := head[0].Score, head[0].Score
	for _, h := range head {
		if h.Score > hi {
			hi = h.Score
		}
		if h.Score < lo {
			lo = h.Score
		}
	}
	for _, h := range tail {
		if h.Score > lo {
			lo = h.Score
		}
	}
	if hi < lo {
		hi = lo
	}

	minScore, maxScore := scores[0], scores[0]
	for _, sc := range scores {
		if sc > maxScore {
			maxScore = sc
		}
		if sc < minScore {
			minScore = sc
		}
	}

	if maxScore == minScore {
		// No ordering information from the reranker: keep the fusion scores.
		for i, h := range head {
			out[i] = h.Score
		}
		return out
	}

	if hi == lo {
		// Degenerate fusion range (all candidates tied): spread the head just
		// above the shared value so the served order stays recoverable from
		// the scores.
		step := float32(1e-6)
		if hi > 0 {
			step = hi * 1e-3
		}
		hi = lo + step*float32(len(head))
	}

	span := float64(hi - lo)
	for i, sc := range scores {
		t := (sc - minScore) / (maxScore - minScore)
		out[i] = lo + float32(t*span)
	}
	return out
}
