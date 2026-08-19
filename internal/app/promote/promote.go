// Package promote answers a question from the code graph when retrieval alone
// cannot.
//
// Text retrieval ranks documents by how much they read like the question. Some
// questions are not about wording at all: "what calls the shipping service"
// asks for edges, "who serves POST /orders" asks for a contract key, "which
// service subscribes to price changes" asks for the far side of one. The passes
// here answer those from the graph and put the answers in front of the ranked
// hits.
//
// They run after ranking, never before. Feeding a call site through a
// cross-encoder was measured and is worse: "X is called here" is a structural
// fact that no wording makes more or less true, and the reranker demoted
// correct call sites it could not verify (callers recall@10 0.778 -> 0.667).
// What the graph knows, ranking cannot improve on.
package promote

import (
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/graph"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// Promoter runs the graph-backed promotion passes over a ranked result.
type Promoter struct {
	store store.Storage
	graph *graph.Graph
	// intentOff mirrors search.intent: off — detection is disabled server-wide,
	// but an intent the client names explicitly still runs (see IntentEnabled).
	intentOff bool
}

// New returns a Promoter reading from the given stores. intentMode is the
// configured search.intent value ("" or "auto" to detect, "off" to stop
// detecting).
func New(stor store.Storage, g *graph.Graph, intentMode string) *Promoter {
	return &Promoter{
		store:     stor,
		graph:     g,
		intentOff: strings.EqualFold(strings.TrimSpace(intentMode), "off"),
	}
}
