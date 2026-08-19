package store

import "encoding/json"

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

// UnitMeta is the decoded form of ASTUnit.Meta.
type UnitMeta struct {
	Root       string `json:"root,omitempty"`        // service root directory in the repo
	DetectedBy string `json:"detected_by,omitempty"` // detector that produced the unit
	Scope      string `json:"scope,omitempty"`       // config scope / visibility
	Path       string `json:"path,omitempty"`        // source path the unit was derived from
	// Summary is one line saying what the symbol does, written by the
	// assistant LLM for code that carries no doc comment. It is indexed with
	// the symbol (see app.summarizeSymbols) so a question asked in domain
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
