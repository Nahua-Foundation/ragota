package client

// The wire types of the ragota-core HTTP API.
//
// They live here rather than beside the handlers because this is the package
// another repository imports: an MCP server, a CLI, anything outside this
// module. internal/api aliases every one of them, so there is a single
// definition of each and a field cannot be added on one side only.
//
// What a caller gets is chosen here, not inherited from storage. The
// alternative was serializing the database rows themselves, which made the
// schema the HTTP contract: a client saw start_byte, end_byte, the content
// hash and a raw JSON meta string, and a field added for the indexer's own use
// appeared in responses without anyone deciding it should.

// SchemaVersion is the version of the HTTP contract this package speaks. It is
// the `info.version` of the served openapi.yaml and what GET /health reports as
// `api_version`; the binary's build version says which process answered, but
// not what it speaks. See CompatibleWith for what a caller should do with it.
const SchemaVersion = "0.2.0"

// --- system ---

// HealthResponse is the liveness answer.
//
// It carries the versions because an external client has no other way to learn
// what it is talking to: it cannot read the server's build flags, and a
// retrieval client that hits a server older than the fields it needs would
// otherwise see them as absent rather than as unsupported.
type HealthResponse struct {
	Status string `json:"status"`
	// Version is the binary's build version, stamped at link time; "dev" in an
	// unstamped build.
	Version string `json:"version"`
	// APIVersion is SchemaVersion — the wire contract, not the build.
	APIVersion string `json:"api_version"`
}

// StatsResponse reports per-indexer statistics, keyed by indexer type
// ("bm25", "ast", "vector", ...).
type StatsResponse struct {
	Indexers map[string]IndexerStats `json:"indexers,omitempty"`
}

// IndexerStats holds statistics for a single indexer.
type IndexerStats struct {
	Documents int64 `json:"documents"`
	SizeBytes int64 `json:"size_bytes"`
	Repos     int   `json:"repos"`
}

// --- repositories ---

// Repo is a registered repository as the API presents it.
type Repo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Source string `json:"source"` // local, git
	URL    string `json:"url,omitempty"`
	Path   string `json:"path"`
	Branch string `json:"branch,omitempty"`
	// IndexedAt is the unix time of the last successful index (0 if never).
	IndexedAt int64  `json:"indexed_at"`
	Status    string `json:"status"` // idle, indexing, error
	CreatedAt int64  `json:"created_at"`
	LastError string `json:"last_error,omitempty"`
	// LastCommit is the SHA of the last commit applied through the commit
	// ingestion API. Empty until an external client starts pushing commits.
	LastCommit string `json:"last_commit,omitempty"`
	// PendingCommit is the SHA a commit batch the system is holding will
	// advance the cursor to. It tells "in flight" from "lost".
	PendingCommit string `json:"pending_commit,omitempty"`
	// Active reports that the repository is in the working set the deployment
	// is currently about. Retrieval defaults to that set: Search and Context
	// with no Repos selector, and Symbol with no RepoID, answer from the active
	// repositories only, while naming a repository reads it whether it is
	// active or not. The graph — neighbors, path, trace, services, topics —
	// ignores the flag, and so do Definition and References, which are given a
	// repository and a file path and so never fall back to a default scope.
	//
	// It is a view, not a lifecycle state: an inactive repository keeps its
	// index, its edges, its coverage and its commit cursor, so a caller reading
	// false here has found a repository that is out of the way, not one that is
	// gone. Not omitempty: false is the informative value.
	Active bool `json:"active"`
}

// AddRepoRequest is a request to add a repository.
type AddRepoRequest struct {
	Name   string `json:"name"`
	Source string `json:"source"` // local, git
	URL    string `json:"url,omitempty"`
	Path   string `json:"path,omitempty"`
	Branch string `json:"branch,omitempty"`
}

// IndexRequest is a request to index a repository.
type IndexRequest struct {
	// Force reindexes files whose content hash is unchanged.
	Force bool `json:"force"`
}

// IndexAck reports what an index request actually did. It is not "started":
// with distributed indexing the request is only durably queued, and the
// difference decides whether the caller watches the repo or the job.
type IndexAck struct {
	// Status is "indexing" (this instance began the pass) or "queued" (a
	// worker will claim it).
	Status string `json:"status"`
	Queued bool   `json:"queued"`
	JobID  string `json:"job_id,omitempty"`
	// JobStatus is the queue entry's state: pending or running.
	JobStatus string `json:"job_status,omitempty"`
	// Force is the effective flag, merged with an already queued job's.
	Force      bool   `json:"force"`
	QueuedAt   int64  `json:"queued_at,omitempty"`
	ClaimedBy  string `json:"claimed_by,omitempty"`
	RepoStatus string `json:"repo_status,omitempty"`
}

// Job is a queued indexing task. The claim fields stay: an operator watching a
// distributed queue needs to know which worker holds a job and when it last
// reported in. Only the payload is withheld — it is the commit batch itself,
// and can run to tens of megabytes.
type Job struct {
	ID     string `json:"id"`
	RepoID string `json:"repo_id"`
	// Kind is "index" (a full pass) or "commits" (a pushed batch).
	Kind   string `json:"kind"`
	Force  bool   `json:"force"`
	Status string `json:"status"` // pending, running, done, error
	Error  string `json:"error,omitempty"`
	// CreatedAt, ClaimedAt and HeartbeatAt are unix seconds; the last two are
	// 0 until a worker claims the job.
	CreatedAt   int64  `json:"created_at"`
	ClaimedAt   int64  `json:"claimed_at"`
	HeartbeatAt int64  `json:"heartbeat_at"`
	ClaimedBy   string `json:"claimed_by,omitempty"`
}

// JobsResponse lists a repository's queue entries, newest first.
type JobsResponse struct {
	Jobs  []*Job `json:"jobs"`
	Total int    `json:"total"`
}

// SyncStateResponse reports everything a pushing client needs to decide what
// to do next: the cursor, whether a batch is currently being applied, when the
// index last succeeded and why the last attempt failed.
type SyncStateResponse struct {
	RepoID     string `json:"repo_id"`
	LastCommit string `json:"last_commit"`
	Status     string `json:"status"`
	// PendingCommit is the SHA a running batch is applying; empty when idle.
	// With status=indexing it distinguishes "in flight" from "lost".
	PendingCommit string `json:"pending_commit,omitempty"`
	// IndexedAt is the unix time of the last successful index (0 if never).
	IndexedAt int64 `json:"indexed_at"`
	// LastError is the failure recorded by the last attempt, if any.
	LastError string `json:"last_error,omitempty"`
}

// Coverage reports how much of a repository's outbound contract surface the
// last full index pass resolved.
//
// It answers what the graph alone cannot: a call site that produced no edge
// leaves nothing behind to count, so a project the indexer half-understands
// and one with little to find both look complete.
type Coverage struct {
	RepoID string `json:"repo_id"`
	// Reported is false when no coverage-reporting index pass has run for this
	// repo; the counters are then meaningless rather than zero.
	Reported bool `json:"reported"`
	// UpdatedAt is when the summary was written, IndexedAt when the repo last
	// finished indexing. A summary older than the index is stale.
	UpdatedAt int64          `json:"updated_at,omitempty"`
	IndexedAt int64          `json:"indexed_at"`
	Kinds     []CoverageKind `json:"kinds"`
	Totals    CoverageKind   `json:"totals"`
}

// CoverageKind is the coverage of one contract kind ("http", "rpc",
// "messaging", ...); Kind is "all" on the totals entry.
type CoverageKind struct {
	Kind string `json:"kind"`
	// Candidates counts call sites that look like an outbound contract of this
	// kind; Edges counts those that produced one.
	Candidates int `json:"candidates"`
	Edges      int `json:"edges"`
	// Ratio is Edges/Candidates, and 1 when there were no candidates: a
	// repository with nothing of this kind to find is fully covered, not
	// uncovered.
	Ratio float64 `json:"ratio"`
}

// --- search ---

// SearchRequest is a request to search.
//
// Reach for this when the question is prose. An identifier the caller already
// holds belongs in SymbolRequest.Symbol instead — measured over the same
// corpus, /search answers a natural-language question at recall@1 0.524 and
// /nav/symbol answers a known identifier at 0.667.
type SearchRequest struct {
	Query string `json:"query"`
	// Repos limits the answer to these repository ids. Left empty, the answer
	// comes from the active repositories only (see Repo.Active), so an empty
	// result may mean the repository holding the answer is out of the way
	// rather than that the index lacks it — ListRepos tells the two apart.
	// Naming a repository here reads it whether it is active or not.
	Repos []string `json:"repos,omitempty"`
	// Mode is "semantic", "keyword" or "hybrid".
	Mode  string `json:"mode"`
	Limit int    `json:"limit"`
	// Filter narrows results by attribute: "language"/"languages",
	// "kind"/"kinds" and "path"/"path_prefix", each a string or a list of
	// strings.
	Filter map[string]interface{} `json:"filter,omitempty"`
	// Intent selects what the query asks for: "auto" (default) detects it
	// from the phrasing, "callers" answers with the code that calls/uses the
	// described symbol (resolved through the code graph), "none" is plain
	// text retrieval.
	Intent string `json:"intent,omitempty"`
	// MaxBytes caps the response body; 0 (the default) does not cap it. See
	// SearchResponse.Truncated.
	MaxBytes int `json:"max_bytes,omitempty"`
	// Snippet is how much code body a hit carries: SnippetChunk (default),
	// SnippetLine or SnippetNone.
	Snippet string `json:"snippet,omitempty"`
	// Diagnostics asks for SearchResponse.Diagnostics. It is off by default
	// because it costs bytes on every response that carries it, and only a
	// caller that will act on the answer should pay for it.
	Diagnostics bool `json:"diagnostics,omitempty"`
}

// SearchResponse is a response to a search query.
type SearchResponse struct {
	Hits  []*SearchHit `json:"hits"`
	Total int          `json:"total"`
	Query string       `json:"query"`
	// Mode is the mode that ran, not the one that was asked for: a request
	// omitting the mode gets hybrid and reads "hybrid" back.
	Mode string `json:"mode"`
	// Truncated reports that max_bytes dropped hits the query did retrieve —
	// `total` counts them, `hits` no longer holds them. A caller that reads
	// only `hits` would otherwise take a budgeted answer for a complete one.
	Truncated bool `json:"truncated"`
	// Diagnostics is present only when SearchRequest.Diagnostics asked for it.
	Diagnostics *SearchDiagnostics `json:"diagnostics,omitempty"`
}

// SearchDiagnostics reports how the answer was produced: which retrieval
// channels contributed, whether any of them was unavailable, and what the
// rerank stage did.
//
// It exists for the one question a hit list cannot answer — whether an empty or
// thin result is the corpus's answer or a broken backend's. Nothing here
// changes the ranking; it describes the run that produced it.
type SearchDiagnostics struct {
	// Degraded reports that a configured searcher could not answer, so the hits
	// came from fewer channels than the deployment has. This is the field to
	// branch on: a zero-hit result with Degraded true is not evidence that the
	// corpus lacks the answer. A rerank failure is not degradation — retrieval
	// was whole, only the ordering stage was skipped; see RerankError.
	Degraded bool `json:"degraded"`
	// Searchers are the indexes that contributed candidates ("vector",
	// "bm25"), in fusion order. Only the hybrid mode fuses, so a single-index
	// mode reports none.
	Searchers []string `json:"searchers,omitempty"`
	// FailedSearchers names the indexes that were configured and did not
	// answer, and SearcherErrors maps each of them to what it reported.
	FailedSearchers []string          `json:"failed_searchers,omitempty"`
	SearcherErrors  map[string]string `json:"searcher_errors,omitempty"`
	// Reranked reports that the rerank stage scored the leading candidates and
	// its order is the one served. False with no RerankError means the stage did
	// not run — no reranker configured, or too few hits to reorder; false with
	// one means it ran, failed, and retrieval order stands. Search never fails
	// because of the reranker.
	Reranked bool `json:"reranked"`
	// RerankCandidates is how many leading hits the reranker was shown.
	RerankCandidates int    `json:"rerank_candidates,omitempty"`
	RerankError      string `json:"rerank_error,omitempty"`
}

// SearchHit is one retrieval result as the API presents it.
type SearchHit struct {
	RepoID   string  `json:"repo_id"`
	FilePath string  `json:"file_path"`
	Line     int     `json:"line"`
	EndLine  int     `json:"end_line"`
	Symbol   string  `json:"symbol,omitempty"`
	Kind     string  `json:"kind,omitempty"`
	Language string  `json:"language,omitempty"`
	Score    float32 `json:"score"`
	// Snippet is the code body, rendered at the size the request asked for
	// (see the Snippet* modes). Empty under SnippetNone.
	Snippet string `json:"snippet,omitempty"`
	// Reason is why this result matched: the contributing indexes joined with
	// "+", or what the code graph knows about it.
	Reason string `json:"reason,omitempty"`
}

// How much code body a hit carries. The choice is a caller's, not the
// server's: an agent that will open the file itself needs the location and
// nothing else, and the chunk it would otherwise be sent is the single largest
// thing in a response — one measured at 2,420 bytes, against ~120 for the hit
// around it.
const (
	// SnippetChunk returns the indexed chunk. The default: it is what /search
	// has always returned.
	SnippetChunk = "chunk"
	// SnippetLine returns the snippet's first line — the line the hit is
	// anchored at for a chunk, and the heading for a snippet the graph wrote.
	SnippetLine = "line"
	// SnippetNone returns no code body at all, only locations.
	SnippetNone = "none"
)

// --- retrieval context ---

// ContextRequest asks for a graph-expanded retrieval package.
//
// It is Search plus the code graph around every hit, and it costs accordingly:
// a default call has measured over ten thousand tokens. Send MaxBytes, or
// SnippetNone when the caller will open the files itself.
type ContextRequest struct {
	Query string `json:"query"`
	// Repos scopes the retrieval half, exactly as SearchRequest.Repos does.
	// The graph expansion around each hit is not scoped, so an item's Related
	// may reach units in repositories the retrieval half would not return.
	Repos  []string `json:"repos,omitempty"`
	Mode   string   `json:"mode,omitempty"`   // semantic | keyword | hybrid
	Limit  int      `json:"limit,omitempty"`  // search hits
	Hops   int      `json:"hops,omitempty"`   // graph expansion depth
	Intent string   `json:"intent,omitempty"` // auto | callers | none (see SearchRequest.Intent)
	// MaxBytes caps the response body; 0 (the default) does not cap it. See
	// ContextResponse.Truncated.
	MaxBytes int `json:"max_bytes,omitempty"`
	// Snippet is how much code body each item's hit carries: SnippetChunk
	// (default), SnippetLine or SnippetNone.
	Snippet string `json:"snippet,omitempty"`
}

// ContextResponse is the ready-to-use retrieval package for an LLM.
type ContextResponse struct {
	Query string `json:"query"`
	Mode  string `json:"mode"`
	// RewrittenQuery is what was actually searched, when an assistant model
	// rewrote the question into a keyword query first.
	RewrittenQuery string         `json:"rewritten_query,omitempty"`
	Items          []*ContextItem `json:"items"`
	// Truncated reports that max_bytes dropped items the query did retrieve.
	Truncated bool `json:"truncated"`
}

// ContextItem is one retrieval hit with the graph around it.
type ContextItem struct {
	Hit     *SearchHit     `json:"hit"`
	Unit    *Unit          `json:"unit,omitempty"`
	Service string         `json:"service,omitempty"`
	Related []*RelatedUnit `json:"related,omitempty"`
}

// RelatedUnit is a unit the graph reached from a hit.
type RelatedUnit struct {
	Unit    *Unit  `json:"unit"`
	Service string `json:"service,omitempty"`
	// Via is the edge kind of the first hop and Direction is "in" (something
	// reaches the hit) or "out" (the hit reaches something).
	Via       string `json:"via"`
	Direction string `json:"direction"`
	Distance  int    `json:"distance"`
}

// --- code graph ---

// Unit is a code symbol as the API presents it.
type Unit struct {
	ID        string `json:"id"`
	RepoID    string `json:"repo_id"`
	FilePath  string `json:"file_path"`
	Language  string `json:"language,omitempty"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Qualified string `json:"qualified,omitempty"`
	ParentID  string `json:"parent_id,omitempty"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Signature string `json:"signature,omitempty"`
	Doc       string `json:"doc,omitempty"`
	// Summary is the assistant's one-line description, when one was generated.
	// It comes out of the unit's meta so that a client never has to parse it.
	Summary string `json:"summary,omitempty"`
	// Service names the deployable this symbol belongs to, when the caller
	// asked for something that resolves it.
	Service string `json:"service,omitempty"`
}

// GraphEdge is a relationship between two units, as the API presents it.
type GraphEdge struct {
	ID        string `json:"id"`
	RepoID    string `json:"repo_id"`
	SrcID     string `json:"src_id"`
	DstID     string `json:"dst_id,omitempty"`
	DstRepoID string `json:"dst_repo_id,omitempty"`
	Kind      string `json:"kind"`
	DstName   string `json:"dst_name,omitempty"`
	FilePath  string `json:"file_path"`
	Line      int    `json:"line"`
	// Confidence is how sure the linker is that this edge is real, 0..1.
	Confidence float32 `json:"confidence"`
	// The contract this edge names, decoded from the edge meta. Everything
	// else in that meta describes how the edge was derived and is of no use
	// outside the linker.
	Topic  string `json:"topic,omitempty"`
	Path   string `json:"path,omitempty"`
	Method string `json:"method,omitempty"`
}

// NeighborsRequest asks for the edges around a unit. The unit id comes from a
// Unit returned by another call — a ContextItem, a PathStep, a TraceStep.
type NeighborsRequest struct {
	UnitID string `json:"unit_id"`
}

// NeighborsResponse is the graph around one unit.
type NeighborsResponse struct {
	Center *Unit      `json:"center"`
	Out    []*EdgeHop `json:"out"`
	In     []*EdgeHop `json:"in"`
}

// EdgeHop is one edge with the unit on its far side, when it resolved. Unit is
// nil for an edge that names something not indexed here.
type EdgeHop struct {
	Edge *GraphEdge `json:"edge"`
	Unit *Unit      `json:"unit,omitempty"`
}

// GraphPathRequest asks for a directed path between two units.
type GraphPathRequest struct {
	FromUnitID string `json:"from_unit_id"`
	ToUnitID   string `json:"to_unit_id"`
	MaxDepth   int    `json:"max_depth,omitempty"`
}

// GraphPathResponse is the path between two units. No path is an answer, not
// an error: Steps is then empty and Length is 0.
type GraphPathResponse struct {
	Steps  []*PathStep `json:"steps"`
	Length int         `json:"length"`
}

// PathStep is one hop of a path between two units.
type PathStep struct {
	Edge *GraphEdge `json:"edge,omitempty"`
	Unit *Unit      `json:"unit"`
	Via  string     `json:"via,omitempty"`
}

// TraceRequest asks where a parameter of a function flows.
type TraceRequest struct {
	RepoID   string `json:"repo_id"`
	Symbol   string `json:"symbol"`
	Param    string `json:"param"`
	MaxDepth int    `json:"max_depth,omitempty"`
}

// TraceResponse is where a parameter flows, best chain first.
type TraceResponse struct {
	Param        string         `json:"param"`
	Steps        []*TraceStep   `json:"steps"`
	Alternatives [][]*TraceStep `json:"alternatives,omitempty"`
	// Chains is how many chains were found in total, including the ones
	// neither Steps nor Alternatives carries.
	Chains int `json:"chains"`
}

// TraceStep is one hop of a parameter flow.
type TraceStep struct {
	Unit    *Unit  `json:"unit"`
	Service string `json:"service,omitempty"`
	// Tracked are the identifiers the value is carried in at this step.
	Tracked    []string `json:"tracked"`
	Via        string   `json:"via,omitempty"`
	Note       string   `json:"note,omitempty"`
	Line       int      `json:"line,omitempty"`
	Confidence float32  `json:"confidence"`
}

// --- navigation ---

// Position represents a position in a file.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// DefinitionRequest is a request to find the definition at a file position.
type DefinitionRequest struct {
	RepoID   string   `json:"repo_id"`
	FilePath string   `json:"file_path"`
	Position Position `json:"position"`
}

// DefinitionResponse is a response with definition location. Definition is nil
// when nothing is defined at that position — an answer, not an error.
type DefinitionResponse struct {
	Definition *ASTSymbol `json:"definition,omitempty"`
}

// ReferencesRequest is a request to find references.
type ReferencesRequest struct {
	RepoID   string   `json:"repo_id"`
	FilePath string   `json:"file_path"`
	Position Position `json:"position"`
	// Limit is how many references to return in total. 0 or absent means 50,
	// and anything above 500 is clamped rather than rejected. The endpoint
	// answers from two lookups — edges resolved to the symbol, then edges that
	// only name it — and the resolved ones take the budget first.
	Limit int `json:"limit"`
}

// ReferencesResponse is a response with references.
type ReferencesResponse struct {
	References []*ASTReference `json:"references"`
	Total      int             `json:"total"`
}

// SymbolRequest is a request to find symbols by identifier.
//
// This is the call for an identifier the caller already holds — a name lifted
// out of a stack trace, a diff or an earlier answer. A question phrased in
// prose belongs in SearchRequest.
type SymbolRequest struct {
	// RepoID limits the answer to one repository. Left empty, the answer comes
	// from the active repositories only (see Repo.Active) — this call is
	// retrieval and is scoped exactly as Search is, because the two are one
	// question asked by a caller holding different things. Naming a repository
	// reads it whether it is active or not.
	RepoID string `json:"repo_id"`
	Name   string `json:"name"`
	Kind   string `json:"kind,omitempty"` // function, class, etc.
	// Qualified narrows alongside Name: both given, both must match. Symbol
	// carries one term to be matched against either, which is what a caller
	// holding a single string out of a question has.
	Qualified string `json:"qualified,omitempty"`
	Symbol    string `json:"symbol,omitempty"`
	Limit     int    `json:"limit"`
}

// SymbolResponse is a response with symbols.
type SymbolResponse struct {
	Symbols []*ASTSymbol `json:"symbols"`
	Total   int          `json:"total"`
}

// ASTSymbol represents an AST symbol (for API responses).
type ASTSymbol struct {
	ID        string `json:"id"`
	RepoID    string `json:"repo_id"`
	FilePath  string `json:"file_path"`
	Language  string `json:"language"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Qualified string `json:"qualified"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Doc       string `json:"doc,omitempty"`
}

// ASTReference represents a reference to a symbol.
type ASTReference struct {
	RepoID   string `json:"repo_id"`
	FilePath string `json:"file_path"`
	Line     int    `json:"line"`
	Kind     string `json:"kind"`             // call, reference, import, etc.
	Word     string `json:"word"`             // the actual token/word under cursor
	Target   string `json:"target,omitempty"` // optional target (e.g. exact symbol name)
}

// --- services & topics ---

// ServicesResponse is the service graph: every detected service and the
// aggregated links between them.
type ServicesResponse struct {
	Services []*ServiceInfo `json:"services"`
	Links    []*ServiceLink `json:"links"`
	// Truncated reports that `limit` cut one of the two lists short.
	Truncated bool `json:"truncated"`
}

// ServiceInfo is one detected deployable.
type ServiceInfo struct {
	RepoID string `json:"repo_id"`
	Name   string `json:"name"`
	// Root is the directory in the repository the service was detected at, and
	// DetectedBy names the evidence (a Dockerfile, a manifest, ...).
	Root       string `json:"root"`
	DetectedBy string `json:"detected_by,omitempty"`
	// UnitID identifies the service in the code graph; pass it to Neighbors.
	UnitID string `json:"unit_id"`
}

// ServiceLink is an aggregated connection between two services.
type ServiceLink struct {
	SrcRepo    string `json:"src_repo"`
	SrcService string `json:"src_service"`
	DstRepo    string `json:"dst_repo"`
	DstService string `json:"dst_service"`
	Kind       string `json:"kind"` // rpc_call | http_call | kafka_flow | runtime_call
	Via        string `json:"via"`  // contract key: grpc:..., http:..., topic:...
	// Count is how many call sites were aggregated into this link and
	// Confidence is the highest confidence among them.
	Count      int     `json:"count"`
	Confidence float32 `json:"confidence"`
}

// TopicsResponse lists messaging topics with the code on both ends.
type TopicsResponse struct {
	Topics []*TopicInfo `json:"topics"`
}

// TopicInfo is one messaging topic with its producers and consumers.
type TopicInfo struct {
	Topic     string       `json:"topic"`
	Producers []*TopicNode `json:"producers"`
	Consumers []*TopicNode `json:"consumers"`
	// Description and Declared come from an AsyncAPI channel declaration;
	// Declared false means the topic was only observed in code.
	Description string `json:"description,omitempty"`
	Declared    bool   `json:"declared,omitempty"`
}

// TopicNode is one end of a topic: the code that produces or consumes it, and
// the service it belongs to.
type TopicNode struct {
	Unit    *Unit  `json:"unit"`
	Service string `json:"service,omitempty"`
}
