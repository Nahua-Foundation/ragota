package domain

// ASTUnit represents a code symbol/definition from AST parsing.
type ASTUnit struct {
	ID        string `json:"id"`
	RepoID    string `json:"repo_id"`
	FilePath  string `json:"file_path"`
	Language  string `json:"language"`
	Kind      string `json:"kind"` // function, method, class, interface, type, var, const, field, etc.
	Name      string `json:"name"`
	Qualified string `json:"qualified"` // Fully qualified name
	ParentID  string `json:"parent_id,omitempty"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	StartByte int    `json:"start_byte"`
	EndByte   int    `json:"end_byte"`
	Signature string `json:"signature,omitempty"`
	Doc       string `json:"doc,omitempty"`
	Hash      string `json:"hash"`           // For cache invalidation
	Meta      string `json:"meta,omitempty"` // JSON: extra unit metadata, see store.UnitMeta
}

// QueryOpts represents options for querying AST units and edges.
type QueryOpts struct {
	RepoID string
	// Repos restricts unit queries to a set of repositories, which is what a
	// caller holding a working set has where RepoID is one repository. Empty
	// means every repository: a scope covering all of them is left empty rather
	// than spelled out, so that the common case emits the same statement it did
	// before any set existed (see app.retrievalScope).
	//
	// It narrows together with RepoID rather than widening it — naming a
	// repository outside the set matches nothing — because the two answer
	// different questions: RepoID is which repository the caller asked about,
	// Repos is which ones it may read at all.
	//
	// Unit queries only. The edge filters ignore it on purpose: the
	// cross-repository graph is the point of the system, so following an edge
	// into a dormant repository is not a leak but the feature.
	Repos           []string
	FilePath        string
	Language        string
	Kind            string
	Kinds           []string // match any of these kinds (used with empty Kind)
	Name            string
	Qualified       string // exact qualified name
	QualifiedSuffix string // qualified name suffix match (cross-repo contract keys)
	// NameOrQualified matches units whose name *or* qualified name equals it.
	// Name and Qualified are separate filters that narrow together, which is
	// what a caller knowing which of the two it holds wants; a caller holding
	// one string that may be either ("ShipOrder" or
	// "ShippingService.ShipOrder") sets this instead of guessing.
	NameOrQualified string
	// Fallback fills the page from hand-written code matching the term as
	// written, then tops it up with units whose name (or qualified name) merely
	// *contains* it — ranking generated and test paths below every hand-written
	// row, including below the ones only they match exactly. It is opt-in
	// because widening a name lookup would change what the linker resolves an
	// edge to, and because demoting a generated exact match is a claim about
	// what a question is about rather than about what a name means; for a symbol
	// lookup an empty answer is a dead end and a near miss is a next step, while
	// a dozen generated stubs are neither. It needs a Limit: without one the
	// query is a bulk read of every matching unit, not a lookup, and a substring
	// scan of the whole table is not what such a caller asked for.
	Fallback   bool
	SrcID      string // edges: filter by source unit
	DstID      string // edges: filter by destination unit
	Unresolved bool   // edges: only unresolved (dst_id empty/0)
	// Line restricts units to those whose [start_line, end_line] range
	// contains it (1-based). Containment is evaluated in SQL: a file can hold
	// far more units than any client-side page, so filtering after a LIMIT
	// silently loses symbols late in large files.
	Line  int
	Limit int
}
