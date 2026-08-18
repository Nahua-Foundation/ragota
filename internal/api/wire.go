package api

// Imported as `wire` because this package already has a `client` — the
// rate limiter's per-caller bucket.
import wire "github.com/Nahua-Foundation/ragota/client"

// The HTTP contract is defined in ragota/client, which is the package
// an external consumer imports, and aliased here.
//
// It used to be defined in this package, where nothing outside the module
// could reach it: an MCP server or a CLI had to re-declare every response
// struct it wanted to read. Two declarations of one JSON object drift the
// first time a field is added, and the drift shows up as a field that is
// silently always zero. Aliases — not copies — are what make that impossible:
// there is one definition, and this package and its clients compile against
// the same one.
//
// Handlers keep writing SearchResponse and Unit; the types they name simply
// live somewhere a client can follow them to.

// SchemaVersion is the version of the HTTP contract this build serves. It is
// the `info.version` of the embedded openapi.yaml and is what a client with its
// own release cycle can branch on; the binary's build version says which
// process answered, but not what it speaks. TestSchemaVersionMatchesSpec keeps
// the two from drifting apart.
const SchemaVersion = wire.SchemaVersion

// Machine-readable error codes. Clients branch on these rather than on the
// human-readable message, which is free to change.
const (
	CodeRepoBusy         = wire.CodeRepoBusy
	CodeCommitGap        = wire.CodeCommitGap
	CodePayloadTooLarge  = wire.CodePayloadTooLarge
	CodeInvalidPath      = wire.CodeInvalidPath
	CodeNotFound         = wire.CodeNotFound
	CodeValidationFailed = wire.CodeValidationFailed
	CodeRateLimited      = wire.CodeRateLimited
	CodeUnauthorized     = wire.CodeUnauthorized
	CodeForbidden        = wire.CodeForbidden
	CodeInternal         = wire.CodeInternal
	CodeNotReady         = wire.CodeNotReady
	CodeIndexDamaged     = wire.CodeIndexDamaged
)

// How much code body a hit carries; see wire.SnippetChunk.
const (
	SnippetChunk = wire.SnippetChunk
	SnippetLine  = wire.SnippetLine
	SnippetNone  = wire.SnippetNone
)

type (
	// HealthResponse is the liveness answer.
	HealthResponse = wire.HealthResponse
	// ErrorResponse is the body of every non-2xx JSON response.
	ErrorResponse = wire.ErrorResponse
	// StatsResponse reports per-indexer statistics.
	StatsResponse = wire.StatsResponse
	// IndexerStats holds statistics for a single indexer.
	IndexerStats = wire.IndexerStats

	// Repo is a registered repository as the API presents it.
	Repo = wire.Repo
	// AddRepoRequest is a request to add a repository.
	AddRepoRequest = wire.AddRepoRequest
	// IndexRequest is a request to index a repository.
	IndexRequest = wire.IndexRequest
	// IndexAck reports what an index request actually did.
	IndexAck = wire.IndexAck
	// Job is a queued indexing task.
	Job = wire.Job
	// JobsResponse lists a repository's queue entries, newest first.
	JobsResponse = wire.JobsResponse
	// SyncStateResponse is a repository's commit cursor and indexing status.
	SyncStateResponse = wire.SyncStateResponse
	// Coverage reports resolved contract surface for a repository.
	Coverage = wire.Coverage
	// CoverageKind is the coverage of one contract kind.
	CoverageKind = wire.CoverageKind

	// SearchRequest is a request to search.
	SearchRequest = wire.SearchRequest
	// SearchResponse is a response to a search query.
	SearchResponse = wire.SearchResponse
	// SearchHit is one retrieval result as the API presents it.
	SearchHit = wire.SearchHit
	// SearchDiagnostics reports how a search was answered; opt-in.
	SearchDiagnostics = wire.SearchDiagnostics

	// ContextRequest asks for a graph-expanded retrieval package.
	ContextRequest = wire.ContextRequest
	// ContextResponse is the ready-to-use retrieval package for an LLM.
	ContextResponse = wire.ContextResponse
	// ContextItem is one retrieval hit with the graph around it.
	ContextItem = wire.ContextItem
	// RelatedUnit is a unit the graph reached from a hit.
	RelatedUnit = wire.RelatedUnit

	// Unit is a code symbol as the API presents it.
	Unit = wire.Unit
	// GraphEdge is a relationship between two units.
	GraphEdge = wire.GraphEdge
	// EdgeHop is one edge with the unit on its far side.
	EdgeHop = wire.EdgeHop
	// NeighborsRequest asks for the edges around a unit.
	NeighborsRequest = wire.NeighborsRequest
	// NeighborsResponse is the graph around one unit.
	NeighborsResponse = wire.NeighborsResponse
	// GraphPathRequest asks for a directed path between two units.
	GraphPathRequest = wire.GraphPathRequest
	// GraphPathResponse is the path between two units.
	GraphPathResponse = wire.GraphPathResponse
	// PathStep is one hop of a path between two units.
	PathStep = wire.PathStep
	// TraceRequest asks where a parameter of a function flows.
	TraceRequest = wire.TraceRequest
	// TraceResponse is where a parameter flows, best chain first.
	TraceResponse = wire.TraceResponse
	// TraceStep is one hop of a parameter flow.
	TraceStep = wire.TraceStep

	// Position represents a position in a file.
	Position = wire.Position
	// DefinitionRequest is a request to find a definition.
	DefinitionRequest = wire.DefinitionRequest
	// DefinitionResponse is a response with a definition location.
	DefinitionResponse = wire.DefinitionResponse
	// ReferencesRequest is a request to find references.
	ReferencesRequest = wire.ReferencesRequest
	// ReferencesResponse is a response with references.
	ReferencesResponse = wire.ReferencesResponse
	// SymbolRequest is a request to find symbols by identifier.
	SymbolRequest = wire.SymbolRequest
	// SymbolResponse is a response with symbols.
	SymbolResponse = wire.SymbolResponse
	// ASTSymbol represents an AST symbol.
	ASTSymbol = wire.ASTSymbol
	// ASTReference represents a reference to a symbol.
	ASTReference = wire.ASTReference

	// ServicesResponse is the service graph.
	ServicesResponse = wire.ServicesResponse
	// ServiceInfo is one detected deployable.
	ServiceInfo = wire.ServiceInfo
	// ServiceLink is an aggregated connection between two services.
	ServiceLink = wire.ServiceLink
	// TopicsResponse lists messaging topics with the code on both ends.
	TopicsResponse = wire.TopicsResponse
	// TopicInfo is one messaging topic with its producers and consumers.
	TopicInfo = wire.TopicInfo
	// TopicNode is one end of a topic.
	TopicNode = wire.TopicNode
)
