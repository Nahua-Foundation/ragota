// Package enrich holds the optional LLM passes over an indexed repository:
// file and service summaries, one-line symbol summaries, the pre-index recon
// pass, and the assistant that rewrites a query before retrieval.
//
// They are gathered here because they share a shape the rest of the service
// does not: every one of them is opt-in, every one of them is best-effort — a
// failure is logged and the index pass continues — and three of the four are
// measured negatives kept behind a flag rather than deleted (see
// tools/eval/README.md). Keeping them in one place makes that visible in the
// tree, and keeps the service's own struct from carrying a dozen fields that
// only these passes read.
package enrich

import (
	"github.com/Nahua-Foundation/ragota/internal/graph"
	"github.com/Nahua-Foundation/ragota/internal/index"
	"github.com/Nahua-Foundation/ragota/internal/llm"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// Enricher runs the optional LLM passes. The zero value does nothing: every
// pass checks its own generator and flag, so an Enricher with no assistant
// configured is inert rather than absent.
type Enricher struct {
	store    store.Storage
	indexers map[index.IndexType]index.Indexer
	linker   *graph.Linker

	generator     llm.Generator // optional; enables LLM summaries
	maxSumm       int
	fileSummaries bool

	// Symbol summaries: one line per boundary symbol, indexed with it.
	symSummaries bool
	maxSymSumm   int

	assistGen     llm.Generator // optional auxiliary assistant LLM (see assist.go)
	assistRewrite bool          // rewrite queries before retrieval

	// Assistant LLM for recon and edge disambiguation (see recon.go).
	reconGen        llm.Generator
	reconEnabled    bool
	disambigEnabled bool
}

// New returns an Enricher over the stores an enrichment pass reads and writes.
// Which passes actually run is decided by the setters.
func New(stor store.Storage, indexers map[index.IndexType]index.Indexer, linker *graph.Linker) *Enricher {
	return &Enricher{store: stor, indexers: indexers, linker: linker}
}

// ReconEnabled reports whether the pre-index recon pass should run.
func (e *Enricher) ReconEnabled() bool {
	return e.reconEnabled && e.reconGen != nil
}

// SummaryFileBudget is how many indexed files the summary pass still wants kept
// for it, or 0 when no summary pass will run — which is what tells the index
// pass not to accumulate their contents at all.
func (e *Enricher) SummaryFileBudget() int {
	if e.generator == nil || e.maxSumm <= 0 {
		return 0
	}
	return e.maxSumm
}

// RewriteEnabled reports whether a query rewrite would be attempted.
func (e *Enricher) RewriteEnabled() bool {
	return e.assistRewrite && e.assistGen != nil
}
