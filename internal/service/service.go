// Package service provides the core application service layer.
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/graph"
	"github.com/Nahua-Foundation/ragota/internal/indexing"
	"github.com/Nahua-Foundation/ragota/internal/llm"
	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/search"
	"github.com/Nahua-Foundation/ragota/internal/service/enrich"
	"github.com/Nahua-Foundation/ragota/internal/service/promote"
	"github.com/Nahua-Foundation/ragota/internal/status"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// ErrBadRequest is returned when a request contains invalid data.
var ErrBadRequest = fmt.Errorf("bad request")

// Service is the core application service.
// It coordinates all components and provides business logic.
type Service struct {
	cfg       *config.Config
	storage   storage.Storage
	indexers  map[indexing.IndexType]indexing.Indexer
	sources   map[repos.SourceType]repos.RepoSource
	searchSvc *search.Service
	graph     *graph.Graph
	linker    *graph.Linker

	// enrich runs the optional LLM passes — summaries, symbol summaries, recon,
	// query rewrite. All of them are opt-in and best-effort, and they own the
	// dozen configuration fields that used to sit here (see service/enrich).
	enrich *enrich.Enricher

	// promote answers from the code graph what retrieval cannot: query intent
	// and the caller/contract promotions it drives (see service/promote).
	promote *promote.Promoter

	// callRefiner, when set, corrects the repository's call edges with
	// language-server evidence after linking (see lsp.CallRefiner).
	callRefiner callRefiner

	// bus, when set, receives index progress for an interactive front end. It
	// is a *status.Bus rather than an interface so that the nil case stays a
	// no-op inside the publisher: a nil interface value holding a nil pointer
	// is not nil, and every publish site would need a guard.
	bus *status.Bus

	baseCtx    context.Context
	cancelBase context.CancelFunc
	wg         sync.WaitGroup
	linkMu     sync.Mutex // serializes global linker runs
}

// New creates a new Service.
func New(
	cfg *config.Config,
	st storage.Storage,
	indexers map[indexing.IndexType]indexing.Indexer,
	sources map[repos.SourceType]repos.RepoSource,
	searchSvc *search.Service,
) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	linker := graph.NewLinker(st)
	g := graph.New(st)
	intentMode := ""
	if cfg != nil && cfg.Search != nil {
		intentMode = cfg.Search.Intent
	}
	s := &Service{
		cfg:        cfg,
		storage:    st,
		indexers:   indexers,
		sources:    sources,
		searchSvc:  searchSvc,
		graph:      g,
		linker:     linker,
		enrich:     enrich.New(st, indexers, linker),
		promote:    promote.New(st, g, intentMode),
		baseCtx:    ctx,
		cancelBase: cancel,
	}
	// Distributed indexing worker. Started here (rather than from setup.Build)
	// so every construction path gets a worker; it is a strict no-op unless
	// indexes.distributed is enabled, so single-instance behaviour and unit
	// tests are unaffected. The goroutine is tracked by s.wg and stops via
	// baseCtx on Close.
	s.startJobPoller()
	return s
}

// Search limits. A caller asking for a limit outside this range is clamped
// rather than rejected; an unbounded limit would let one request pull the
// whole index into memory.
const (
	defaultSearchLimit = 10
	maxSearchLimit     = 100
)

// Search modes accepted by Search.
const (
	SearchModeSemantic = "semantic"
	SearchModeKeyword  = "keyword"
	SearchModeHybrid   = "hybrid"
)

// Search performs a search query in the given mode ("semantic" | "keyword" | "hybrid").
func (s *Service) Search(ctx context.Context, q *indexing.SearchQuery, mode string) (*indexing.SearchResult, error) {
	if q == nil {
		return nil, fmt.Errorf("%w: search query is required", ErrBadRequest)
	}
	if strings.TrimSpace(q.Query) == "" && len(q.Vector) == 0 {
		return nil, fmt.Errorf("%w: query must not be empty", ErrBadRequest)
	}

	switch {
	case q.Limit <= 0:
		q.Limit = defaultSearchLimit
	case q.Limit > maxSearchLimit:
		q.Limit = maxSearchLimit
	}

	if mode == "" {
		mode = SearchModeHybrid
	}

	if s.searchSvc == nil {
		return nil, fmt.Errorf("search service not available")
	}

	// A callers-question retrieves by the callee's description — "what calls
	// the shipping service" should look for the shipping service, not for the
	// words "what calls" — and is answered from the graph after retrieval.
	intent, callee, err := s.promote.ResolveIntent(q)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}

	// Retrieval defaults to the active set (see retrievalScope). The scope is
	// written into the query itself, before anything reads it, so that
	// everything this request answers with — the searchers' hits and the call
	// sites and contract far sides the graph promotions add below — obeys one
	// repository selector, the same one a client could have sent explicitly.
	scope, none, err := s.retrievalScope(ctx, q.Repos)
	if err != nil {
		return nil, err
	}
	if none {
		return &indexing.SearchResult{
			Hits: []*indexing.Hit{}, Query: q.Query, Mode: mode,
			Metadata: map[string]interface{}{},
		}, nil
	}
	q.Repos = scope

	exec := q
	if intent == promote.IntentCallers && callee != "" && callee != q.Query {
		stripped := *q
		stripped.Query = callee
		exec = &stripped
	}

	// Retrieval and ranking are separate halves on purpose: what this layer
	// contributes from the code graph joins the pipeline at a chosen point
	// rather than being stapled onto a finished result (see below).
	start := time.Now()
	var hits []*indexing.Hit
	var meta map[string]interface{}
	switch mode {
	case SearchModeSemantic:
		hits, meta, err = s.searchSvc.CandidatesFrom(ctx, indexing.IndexTypeVector, exec)
	case SearchModeKeyword:
		hits, meta, err = s.searchSvc.CandidatesFrom(ctx, indexing.IndexTypeBM25, exec)
	case SearchModeHybrid:
		hits, meta, err = s.searchSvc.Candidates(ctx, exec)
	default:
		return nil, fmt.Errorf("%w: unknown search mode %q (want semantic, keyword or hybrid)", ErrBadRequest, mode)
	}
	if err != nil {
		return nil, err
	}
	if meta == nil {
		meta = map[string]interface{}{}
	}

	// Ranking sees the user's own question, while retrieval saw the stripped
	// callee description: the strip exists so that the searchers look for the
	// thing being asked about instead of for the words "what calls", and a
	// reranker is the one stage that can use the rest of the sentence — it is
	// asked "does this document answer this question", and "what calls X" is
	// a different question from "X". Measured, this is a wash on the eval set
	// (one query gained rank 1, two gained a covering chunk, MRR moved
	// -0.002); it is kept because it is the defensible reading of what a
	// reranker is for, not because the numbers chose it.
	hits = s.searchSvc.Rank(ctx, q.Query, hits, meta, q.Limit)

	// The graph's call sites are added after ranking, not before. Feeding
	// them through the rerank stage was measured and is worse: a cross-encoder
	// scores a call site by how much its text resembles the question, but "X
	// is called here" is a structural fact that no wording makes more or less
	// true, and the reranker demoted correct call sites it could not verify —
	// callers recall@10 0.778 -> 0.667, MRR 0.556 -> 0.370, one rank-1 answer
	// lost entirely. What the graph knows, ranking cannot improve on.
	if intent == promote.IntentCallers {
		hits = s.promote.PromoteCallers(ctx, q, callee, hits, meta)
	}
	// A literal contract key in the query is an address, not a hint: it needs
	// no interrogative phrasing to be trusted, so it is resolved whenever
	// intent handling is on at all, callers question or not.
	if s.promote.IntentEnabled(q) {
		hits = s.promote.PromoteContracts(ctx, q, hits, meta)
		if intent != promote.IntentCallers {
			hits = s.promote.PromoteRPCImplementations(ctx, q, hits, meta)
		}
		// Last, so that it leads: this is the only lookup that reads which side
		// of a contract the question asked for, and the strictest — it fires
		// only when the question describes the part of the key that tells one
		// contract from its siblings (see contractusers.go).
		hits = s.promote.PromoteContractUses(ctx, q, intent, hits, meta)
	}
	if q.Limit > 0 && len(hits) > q.Limit {
		hits = hits[:q.Limit]
	}

	return &indexing.SearchResult{
		Hits:  hits,
		Total: len(hits),
		Query: q.Query,
		// The resolved mode, not the requested one: this is the only place that
		// knows an empty request mode meant hybrid, and a caller that reads the
		// mode back is asking what ran.
		Mode:     mode,
		Duration: time.Since(start),
		Metadata: meta,
	}, nil
}

// Stats returns statistics for all indexers.
func (s *Service) Stats(ctx context.Context) (map[string]*indexing.IndexerStats, error) {
	stats := make(map[string]*indexing.IndexerStats)
	for typ, idx := range s.indexers {
		idxStats, err := idx.Stats(ctx)
		if err != nil {
			continue
		}
		stats[string(typ)] = idxStats
	}
	return stats, nil
}

// Close closes the service and all its components.
func (s *Service) Close(ctx context.Context) error {
	s.cancelBase()
	s.wg.Wait() // wait for in-flight background indexing before closing resources

	var errs []error

	// Close indexers
	for _, idx := range s.indexers {
		if err := idx.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close indexer %s: %w", idx.Name(), err))
		}
	}

	// Close sources
	for _, src := range s.sources {
		if err := src.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close source %s: %w", src.Name(), err))
		}
	}

	// Close storage
	if err := s.storage.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close storage: %w", err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("close errors: %v", errs)
	}

	return nil
}

// The enrichment setters are delegated so that wiring (setup.Build) keeps
// configuring one object: which optional LLM passes run is the Enricher's
// business, but it is reached through the service that owns it.

// SetGenerator enables LLM summaries of files and services.
func (s *Service) SetGenerator(g llm.Generator, maxFiles int, files bool) {
	s.enrich.SetGenerator(g, maxFiles, files)
}

// SetSymbolSummaries enables one-line summaries of boundary symbols.
func (s *Service) SetSymbolSummaries(enabled bool, max int) {
	s.enrich.SetSymbolSummaries(enabled, max)
}

// SetAssistant installs the auxiliary assistant LLM and says whether it may
// rewrite a query before retrieval.
func (s *Service) SetAssistant(gen llm.Generator, queryRewrite bool) {
	s.enrich.SetAssistant(gen, queryRewrite)
}

// SetReconAssistant installs the assistant used by the pre-index recon pass and
// by edge disambiguation.
func (s *Service) SetReconAssistant(gen llm.Generator, recon, disambiguate bool) {
	s.enrich.SetReconAssistant(gen, recon, disambiguate)
}

// SetStatusBus routes index progress to an interactive front end. Passing nil
// (or never calling it, which is what the server does) turns every publish
// into a no-op; nothing in the indexing path may depend on a bus existing.
func (s *Service) SetStatusBus(b *status.Bus) { s.bus = b }

// ErrRepoBusy is returned when a repository is already being indexed. It is a
// retry condition, not a malformed request: callers map it to 409/503.
var ErrRepoBusy = errors.New("repo is already indexing")

// terminalWriteTimeout bounds the detached status/bookkeeping writes made
// after a run finishes, including during shutdown.
const terminalWriteTimeout = 15 * time.Second

// terminalCtx derives a context for the writes that record how a run ended.
// They must survive cancellation of the run's own context — Close cancels it,
// and a dropped terminal write is exactly what leaves a repo claimed forever.
func terminalCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), terminalWriteTimeout)
}
