package storage

import "encoding/json"

// Index job statuses.
const (
	JobStatusPending = "pending"
	JobStatusRunning = "running"
	JobStatusDone    = "done"
	JobStatusError   = "error"
)

// Index job kinds. They share one queue (and one worker loop) but differ in
// what a job carries and how many of them may be queued per repo: an index job
// is a repeatable "reindex this repo" request, of which one pending entry per
// repo is enough, while a commit job carries a specific batch of commits that
// must be applied exactly once and in order.
const (
	JobKindIndex   = "index"
	JobKindCommits = "commits"
)

// IndexJob is a queued repository task. In distributed mode several
// ragota-core instances share one queue over a common database and claim jobs
// atomically.
type IndexJob struct {
	ID          string `json:"id"`
	RepoID      string `json:"repo_id"`
	Kind        string `json:"kind"` // index | commits
	Force       bool   `json:"force"`
	Status      string `json:"status"` // pending | running | done | error
	Error       string `json:"error,omitempty"`
	CreatedAt   int64  `json:"created_at"`   // unix seconds
	ClaimedAt   int64  `json:"claimed_at"`   // unix seconds, 0 if never claimed
	HeartbeatAt int64  `json:"heartbeat_at"` // unix seconds, 0 if never claimed
	ClaimedBy   string `json:"claimed_by,omitempty"`
	// Payload is the opaque body of a commit job (the encoded commit batch).
	// It is only populated by ClaimNextIndexJob: it can be tens of megabytes,
	// so the read paths that merely report queue state never select it, and it
	// is never serialized to API clients.
	Payload string `json:"-"`
}

// Contract kinds reported by coverage. They are the families of outbound
// contract an indexer can recognize at a call site; every coverage counter is
// keyed by one of them.
const (
	ContractKindHTTP      = "http"      // outbound HTTP request
	ContractKindRPC       = "rpc"       // gRPC / RPC client call
	ContractKindMessaging = "messaging" // publish/subscribe on a broker
	ContractKindDB        = "db"        // query against a table
)

// ContractKinds lists the coverage kinds in report order.
var ContractKinds = []string{ContractKindHTTP, ContractKindRPC, ContractKindMessaging, ContractKindDB}

// CoverageCounts is one contract kind's tally for a repository: how many call
// sites looked like an outbound contract, and how many of them produced an
// edge.
//
// The gap between the two is the only thing that separates "there is nothing
// to find here" from "we did not find it": a CMS with 42 HTTP calls and 42
// candidates is fully covered, while a project with 104 edges against
// thousands of candidates is not — the counts are equal in the first case and
// nowhere near it in the second.
type CoverageCounts struct {
	Candidates int `json:"candidates"`
	Edges      int `json:"edges"`
}

// RepoCoverage is the per-repo contract coverage summary written by the last
// full index pass.
type RepoCoverage struct {
	RepoID    string                    `json:"repo_id"`
	UpdatedAt int64                     `json:"updated_at"` // unix seconds
	Kinds     map[string]CoverageCounts `json:"kinds"`
}

// EdgeResolution is one edge's resolved destination, as applied by
// EdgeResolutionBatcher. An empty DstID clears the resolution.
type EdgeResolution struct {
	EdgeID     string
	DstID      string
	DstRepoID  string
	Confidence float32
}

// EdgeResolutionFailure attributes one failed write inside a batch. Index is
// the position in the slice handed to BatchUpdateEdgeResolutions, so a caller
// that buffers resolutions can still report which edge it lost.
type EdgeResolutionFailure struct {
	Index  int
	EdgeID string
	Err    error
}

// File represents a source code file.
type File struct {
	RepoID   string `json:"repo_id"`
	Path     string `json:"path"` // Relative path in repo
	Hash     string `json:"hash"` // SHA256 of content
	Language string `json:"language"`
	Size     int64  `json:"size"`
	ModTime  int64  `json:"mod_time"` // Unix timestamp
	Indexed  bool   `json:"indexed"`
}

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
	Meta      string `json:"meta,omitempty"` // JSON: extra unit metadata, see UnitMeta
}

// UnitMeta is the decoded form of ASTUnit.Meta.
type UnitMeta struct {
	Root       string `json:"root,omitempty"`        // service root directory in the repo
	DetectedBy string `json:"detected_by,omitempty"` // detector that produced the unit
	Scope      string `json:"scope,omitempty"`       // config scope / visibility
	Path       string `json:"path,omitempty"`        // source path the unit was derived from
	// Summary is one line saying what the symbol does, written by the
	// assistant LLM for code that carries no doc comment. It is indexed with
	// the symbol (see service.summarizeSymbols) so a question asked in domain
	// language can meet the symbol in the same language.
	Summary string `json:"summary,omitempty"`
}

// EncodeUnitMeta encodes m as JSON for ASTUnit.Meta; nil -> "".
func EncodeUnitMeta(m *UnitMeta) string {
	if m == nil {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

// DecodeUnitMeta decodes ASTUnit.Meta; returns an empty UnitMeta on failure.
func DecodeUnitMeta(s string) *UnitMeta {
	m := &UnitMeta{}
	if s == "" {
		return m
	}
	_ = json.Unmarshal([]byte(s), m)
	return m
}

// EncodeEdgeMeta encodes m as JSON for Edge.Meta; nil -> "".
func EncodeEdgeMeta(m *EdgeMeta) string {
	if m == nil {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

// DecodeEdgeMeta decodes Edge.Meta; returns an empty EdgeMeta on failure.
// It lives here with the type it decodes, so the indexer that writes the meta
// and the graph that reads it share one codec without the reader depending on
// the parser.
func DecodeEdgeMeta(s string) *EdgeMeta {
	m := &EdgeMeta{}
	if s == "" {
		return m
	}
	_ = json.Unmarshal([]byte(s), m)
	return m
}

// Unit kinds for contract/graph nodes (in addition to language symbols).
const (
	KindService      = "service"       // deployable unit detected in a repo
	KindHTTPRoute    = "http_route"    // server-side HTTP route declaration
	KindRPCMethod    = "rpc_method"    // gRPC method declared in a .proto file
	KindProtoService = "proto_service" // service declared in a .proto file
	KindProtoMessage = "proto_message" // message declared in a .proto file
	KindProtoField   = "proto_field"   // field of a proto message
	KindDBTable      = "db_table"      // table from SQL migrations (qualified "db:<table>")
	KindDBColumn     = "db_column"     // column (qualified "db:<table>.<column>")
	KindConfigKey    = "config_key"    // config entry (qualified "config:<dot.path>")
	KindTopicChannel = "topic_channel" // channel declared in an AsyncAPI spec (qualified "topic:<name>")
	KindSummary      = "summary"       // LLM-generated summary of a file or service
)

// Edge kinds. Contract edges use DstName as a join key until the linker
// resolves them: "grpc:<Service>/<Method>", "http:<METHOD> <path>", "topic:<name>".
const (
	EdgeCall          = "call"           // function/method call
	EdgeImport        = "import"         // file/module import
	EdgeHTTPCall      = "http_call"      // outgoing HTTP request -> http_route unit
	EdgeRPCCall       = "rpc_call"       // gRPC client call -> rpc_method unit
	EdgeImplementsRPC = "implements_rpc" // server method -> rpc_method unit
	EdgeHandledBy     = "handled_by"     // http_route unit -> handler function
	EdgeProduces      = "produces"       // function -> kafka topic (DstName = topic:<name>)
	EdgeConsumes      = "consumes"       // function/handler -> kafka topic (DstName = topic:<name>)
	EdgeKafkaFlow     = "kafka_flow"     // derived: producer fn -> consumer fn (linker)
	EdgeRPCRequest    = "rpc_request"    // rpc_method -> proto_message (request type)
	EdgeRPCResponse   = "rpc_response"   // rpc_method -> proto_message (response type)
	EdgeWritesTo      = "writes_to"      // function -> db_table (INSERT/UPDATE/DELETE)
	EdgeReadsFrom     = "reads_from"     // function -> db_table (SELECT/JOIN)
	EdgeRuntimeCall   = "runtime_call"   // service -> service, observed in tracing data
)

// Edge represents a relationship between AST units.
type Edge struct {
	ID         string  `json:"id"`
	RepoID     string  `json:"repo_id"`
	SrcID      string  `json:"src_id"`             // Source ASTUnit ID
	DstID      string  `json:"dst_id,omitempty"`   // Destination ASTUnit ID (empty if unresolved)
	Kind       string  `json:"kind"`               // import, call, reference, implements, inherits
	DstName    string  `json:"dst_name,omitempty"` // Destination name (if unresolved)
	FilePath   string  `json:"file_path"`
	Line       int     `json:"line"`
	DstRepoID  string  `json:"dst_repo_id,omitempty"` // For cross-repo edges
	Confidence float32 `json:"confidence"`            // 0-1, for ML-based edges
	Meta       string  `json:"meta,omitempty"`        // JSON: call args, wire field mappings, topic, path
}

// EdgeMeta is the decoded form of Edge.Meta.
type EdgeMeta struct {
	Args    []string          `json:"args,omitempty"`    // argument expressions at the call site, in order
	Fields  map[string]string `json:"fields,omitempty"`  // wire field -> source expression (request/message construction)
	Aliases map[string]string `json:"aliases,omitempty"` // local alias -> source expression, live at the call site
	Topic   string            `json:"topic,omitempty"`   // kafka topic name
	Path    string            `json:"path,omitempty"`    // http path template
	Method  string            `json:"method,omitempty"`  // http method
	Host    string            `json:"host,omitempty"`    // http target host, if literal
	// Receiver and RecvType disambiguate a call: several definitions can share
	// a method name, and without the call site's receiver the linker has to
	// guess which one the edge points at.
	Receiver string `json:"receiver,omitempty"`  // receiver expression at the call site
	RecvType string `json:"recv_type,omitempty"` // declared type of that receiver, when known
	// TopicRef and Source are written by the linker: TopicRef keeps the
	// original ${KEY} reference after a topic is resolved so a later config
	// change can re-resolve it, and Source marks how the edge was resolved.
	TopicRef string `json:"topic_ref,omitempty"`
	Source   string `json:"source,omitempty"`
	// BaseConf is the parser's base confidence for a resolvable edge, recorded
	// at index time so the linker can recompute base*factor idempotently. It
	// survives the unresolve that reindexing performs (which clears dst_id but
	// not confidence or meta), so re-resolution does not compound the factor.
	// Set only for edge kinds the linker resolves; see graph.confidenceAfterResolve.
	BaseConf float32 `json:"base_conf,omitempty"`
}

// VectorPoint represents a vector with its payload.
type VectorPoint struct {
	ID        string            `json:"id"`
	Vector    []float32         `json:"vector"`
	RepoID    string            `json:"repo_id"`
	FilePath  string            `json:"file_path"`
	Language  string            `json:"language"`
	StartLine int               `json:"start_line"`
	EndLine   int               `json:"end_line"`
	Kind      string            `json:"kind,omitempty"`   // function, class, etc.
	Symbol    string            `json:"symbol,omitempty"` // Symbol name
	Text      string            `json:"text"`             // Chunk content
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// VectorResult represents a vector search result.
type VectorResult struct {
	ID       string   `json:"id"`
	Score    float32  `json:"score"`
	RepoID   string   `json:"repo_id"`
	FilePath string   `json:"file_path"`
	EndLine  int      `json:"end_line"`
	Line     int      `json:"line"`
	Text     string   `json:"text"`
	Metadata Metadata `json:"metadata"`
}

// Metadata is a convenience type for result metadata.
type Metadata map[string]string

// QueryOpts represents options for querying AST units and edges.
type QueryOpts struct {
	RepoID string
	// Repos restricts unit queries to a set of repositories, which is what a
	// caller holding a working set has where RepoID is one repository. Empty
	// means every repository: a scope covering all of them is left empty rather
	// than spelled out, so that the common case emits the same statement it did
	// before any set existed (see service.retrievalScope).
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

// VectorSearchOpts represents options for vector search.
type VectorSearchOpts struct {
	Query     []float32
	Repos     []string // filter to these repos; empty = all
	Languages []string
	Limit     int
	Filter    map[string]string // Additional filters
}

// VectorStats holds vector storage statistics.
type VectorStats struct {
	Documents  int64  // Total number of vectors
	SizeBytes  int64  // Total size of stored data
	Repos      int    // Number of repositories
	Collection string // Collection name (for Qdrant)
}
