package contract

// Named confidence tiers for graph edges.
//
// Confidence expresses how certain the indexer/linker is that an edge points
// at the right destination. Values are multiplied along resolution steps
// (parser confidence x linker match confidence), so a tier is a factor, not a
// probability. The numeric values are frozen: they are persisted in storage
// and asserted by existing tests — introduce a new named constant instead of
// changing one.
const (
	// ConfExact marks an exact match: a same-file symbol resolution or a
	// full contract match (complete gRPC key, exact route, same-repo table,
	// producer/consumer joined on an identical topic).
	ConfExact float32 = 0.95

	// ConfHigh marks framework detection with a literal key: the parser saw
	// the framework API call and a string-literal route/topic/table, or the
	// linker matched a table cross-repo by its exact key.
	ConfHigh float32 = 0.9

	// ConfCrossFile marks name-based resolution across files within a repo,
	// and HTTP client calls whose target route lives elsewhere: the name/key
	// matches, but there was no same-file or full-contract evidence.
	ConfCrossFile float32 = 0.8

	// ConfHeuristic marks positional or name-only heuristics: a gRPC method
	// matched without its service, or a traced value that matched an
	// argument position with the payload field unknown.
	ConfHeuristic float32 = 0.7

	// ConfWeak marks liberal guesses: implements_rpc without a detected
	// service, or passthrough assumptions where payload fields are unknown
	// and the tracked identifiers are simply carried across the hop.
	ConfWeak float32 = 0.6
)
