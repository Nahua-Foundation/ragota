// fileCtx is the per-file extraction state every extractor writes into:
// units, edges, coverage counters, pending table bindings and source marks.
package ast

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/storage"

	sitter "github.com/smacker/go-tree-sitter"
)

// srcMarkPrefix marks Edge.SrcID/DstID values that reference a unit by its
// index in the parsed file's unit slice instead of a storage ID.
const srcMarkPrefix = "#"

// srcMark returns the positional marker for unit index i.
func srcMark(i int) string { return fmt.Sprintf("%s%d", srcMarkPrefix, i) }

// resolveMark returns the unit index for a marker, or -1.
func resolveMark(s string) int {
	if !strings.HasPrefix(s, srcMarkPrefix) {
		return -1
	}
	var i int
	if _, err := fmt.Sscanf(s[len(srcMarkPrefix):], "%d", &i); err != nil {
		return -1
	}
	return i
}

type fileCtx struct {
	path  string
	lang  string
	src   []byte
	units []*storage.ASTUnit
	edges []*storage.Edge

	grpcChecked bool // hasGRPC scanned the source already
	grpcSeen    bool

	memdbChecked bool // hasMemdb scanned the source already
	memdbSeen    bool

	alchemyChecked bool // hasSQLAlchemy scanned the source already
	alchemySeen    bool

	brokerChecked bool // hasBroker scanned the source already
	brokerSeen    bool

	// cov tallies the call sites this file's extractor recognized as outbound
	// contracts, per storage.ContractKind*.
	cov map[string]storage.CoverageCounts

	// https records the outbound HTTP call sites seen in this file, resolved
	// or not, so the functions containing them can be published as wrappers.
	https []httpSite

	// tables records the data-access sites whose table is named by a constant
	// this file does not declare, and consts the table names this file declares
	// for the rest of its package. The two halves are joined per directory once
	// the batch is parsed (see linkPackageTables).
	tables []pendingTable
	consts map[string]string
}

// newCoverage returns a zeroed counter for every contract kind. The tree-sitter
// extractors look for all four, so a kind that stays at {0,0} is the answer
// "this file has none", which is what the report distinguishes from silence.
func newCoverage() map[string]storage.CoverageCounts {
	cov := make(map[string]storage.CoverageCounts, len(storage.ContractKinds))
	for _, kind := range storage.ContractKinds {
		cov[kind] = storage.CoverageCounts{}
	}
	return cov
}

// contractSite records one call site that the extractor recognized as an
// outbound contract of the given kind, and whether it produced an edge.
//
// Counting is per call site, not per edge: a subscription that names three
// topics is one candidate that resolved, not three, and Edges must never
// exceed Candidates.
func (fc *fileCtx) contractSite(kind string, emitted bool) {
	if fc.cov == nil {
		fc.cov = newCoverage()
	}
	c := fc.cov[kind]
	c.Candidates++
	if emitted {
		c.Edges++
	}
	fc.cov[kind] = c
}

// httpSite is one outbound HTTP call site. A resolved site carries the route
// it targets; an unresolved one carries the expressions the URL and method
// were built from, which is what tells the wrapper builder that the enclosing
// function forwards its own arguments to the client.
type httpSite struct {
	unit       int
	method     string
	path       string
	host       string
	urlExpr    string
	methodExpr string
}

// httpCandidate records a call site that a client rule claimed but whose URL
// did not resolve to a route.
func (fc *fileCtx) httpCandidate(unit int, urlExpr, methodExpr string) {
	if unit < 0 {
		return
	}
	fc.https = append(fc.https, httpSite{unit: unit, urlExpr: urlExpr, methodExpr: methodExpr})
}

// tableCandidate records a data-access site whose table is named by a constant
// declared elsewhere in the package. The site has already been counted as a
// candidate; resolving it later adds the edge and nothing else.
func (fc *fileCtx) tableCandidate(unit int, ident, kind string, line int, args []string) {
	if unit < 0 || len(fc.tables) >= maxPendingTables {
		return
	}
	fc.tables = append(fc.tables, pendingTable{
		Src: srcMark(unit), Ident: ident, Kind: kind, Line: line, Args: args,
	})
}

// publishConst offers one identifier -> table name binding to the rest of the
// package.
func (fc *fileCtx) publishConst(name, value string) {
	if !isTableNameLike(value) || len(fc.consts) >= maxPackageConsts {
		return
	}
	if fc.consts == nil {
		fc.consts = map[string]string{}
	}
	fc.consts[name] = value
}

// Limits on what one file contributes to its package: enough for a schema file
// that declares every table of a store, and bounded for a generated file that
// declares thousands of strings.
const (
	maxPendingTables = 512
	maxPackageConsts = 256
)

// isTableNameLike reports whether a constant's value could be the name of a
// table: one short word of name characters. It is what keeps the published
// bindings to the handful a schema file declares instead of every string
// constant in it.
func isTableNameLike(v string) bool {
	if len(v) < 2 || len(v) > 64 {
		return false
	}
	letters := 0
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			letters++
		case c >= '0' && c <= '9', c == '_', c == '-', c == '.':
		default:
			return false
		}
	}
	return letters >= 2
}

// hasGRPC reports whether this file names a gRPC dependency of its own. It is
// the corroboration the "receiver typed XxxClient" stub heuristic needs before
// it may emit an rpc_call (see grpcStubService). Scanned once per file.
func (fc *fileCtx) hasGRPC() bool {
	if !fc.grpcChecked {
		fc.grpcChecked, fc.grpcSeen = true, hasGRPCImport(fc.src)
	}
	return fc.grpcSeen
}

// memdbMarker is the import path of hashicorp/go-memdb, the in-memory table
// store whose transactions name their table at the call site.
const memdbMarker = "go-memdb"

// hasMemdb reports whether this file imports go-memdb. It is the corroboration
// the transaction-receiver heuristic needs before a tx.Get is read as a table
// read (see memdbAccess). Scanned once per file.
func (fc *fileCtx) hasMemdb() bool {
	if !fc.memdbChecked {
		fc.memdbChecked, fc.memdbSeen = true, bytes.Contains(fc.src, []byte(memdbMarker))
	}
	return fc.memdbSeen
}

// alchemyMarker is the import name of SQLAlchemy, whose Session shares every
// method name that matters with requests.Session.
const alchemyMarker = "sqlalchemy"

// hasSQLAlchemy reports whether this file imports SQLAlchemy. It is what tells
// a `session.get(...)` that reads a row from one that makes a request (see
// pyHTTPClientNoSession). Scanned once per file.
func (fc *fileCtx) hasSQLAlchemy() bool {
	if !fc.alchemyChecked {
		fc.alchemyChecked, fc.alchemySeen = true, bytes.Contains(fc.src, []byte(alchemyMarker))
	}
	return fc.alchemySeen
}

// hasBroker reports whether this file names a message-broker client library
// (see brokerSourceMarkers). It is the corroboration an ambiguous messaging
// call name needs before the site is read as a contract. Scanned once per file.
func (fc *fileCtx) hasBroker() bool {
	if !fc.brokerChecked {
		fc.brokerChecked, fc.brokerSeen = true, hasBrokerImport(fc.src)
	}
	return fc.brokerSeen
}

func (fc *fileCtx) text(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	return n.Content(fc.src)
}

// addUnit appends a unit built from the node and returns its index.
func (fc *fileCtx) addUnit(n *sitter.Node, kind, name, qualified, signature, doc string) int {
	u := &storage.ASTUnit{
		Kind:      kind,
		Name:      name,
		Qualified: qualified,
		Signature: signature,
		Doc:       doc,
		StartLine: int(n.StartPoint().Row) + 1,
		EndLine:   int(n.EndPoint().Row) + 1,
		StartByte: int(n.StartByte()),
		EndByte:   int(n.EndByte()),
	}
	fc.units = append(fc.units, u)
	return len(fc.units) - 1
}

// addEdge appends an edge whose source is the unit at index srcIdx.
//
// conf is one of the named tiers in internal/contract/confidence.go
// (contract.ConfExact ... contract.ConfWeak); extractors pass the constants
// directly.
func (fc *fileCtx) addEdge(srcIdx int, kind, dstName string, line int, conf float32, meta *storage.EdgeMeta) {
	if srcIdx < 0 {
		return
	}
	if kind == storage.EdgeHTTPCall && meta != nil {
		fc.https = append(fc.https, httpSite{unit: srcIdx, method: meta.Method, path: meta.Path, host: meta.Host})
	}
	if edgeNeedsBaseConf(kind) {
		if meta == nil {
			meta = &storage.EdgeMeta{}
		}
		meta.BaseConf = conf
	}
	fc.edges = append(fc.edges, &storage.Edge{
		SrcID:      srcMark(srcIdx),
		Kind:       kind,
		DstName:    dstName,
		Line:       line,
		Confidence: conf,
		Meta:       storage.EncodeEdgeMeta(meta),
	})
}

// edgeNeedsBaseConf reports whether the linker later re-resolves this edge kind
// and therefore needs the parser's base confidence recorded in meta so the
// linker can recompute base*factor without compounding the factor across
// reindexes (see graph.confidenceAfterResolve).
func edgeNeedsBaseConf(kind string) bool {
	switch kind {
	case storage.EdgeCall, storage.EdgeHandledBy, storage.EdgeRPCCall,
		storage.EdgeImplementsRPC, storage.EdgeHTTPCall,
		storage.EdgeWritesTo, storage.EdgeReadsFrom:
		return true
	}
	return false
}

// addRoute publishes an http_route unit for a registration and links it to the
// handler it names. detectedBy records which detector produced it, so a route
// found by the generic registry rule stays distinguishable from one a
// framework rule recognized exactly.
func (fc *fileCtx) addRoute(n *sitter.Node, method, path, handler string, line int, conf float32, detectedBy string) int {
	idx := fc.addUnit(n, storage.KindHTTPRoute, method+" "+path, routeKey(method, path), "path:"+path, "")
	if detectedBy != "" {
		fc.units[idx].Meta = storage.EncodeUnitMeta(&storage.UnitMeta{DetectedBy: detectedBy})
	}
	if handler != "" {
		fc.addEdge(idx, storage.EdgeHandledBy, lastComponent(handler), line, conf, nil)
	}
	return idx
}

// enclosingUnit returns the index of the smallest unit containing byte pos,
// optionally restricted to the given kinds. Returns -1 if none.
func (fc *fileCtx) enclosingUnit(pos int, kinds ...string) int {
	best := -1
	bestSpan := 1 << 62
	for i, u := range fc.units {
		if u.StartByte <= pos && pos < u.EndByte {
			if len(kinds) > 0 && !slices.Contains(kinds, u.Kind) {
				continue
			}
			span := u.EndByte - u.StartByte
			if span < bestSpan {
				best, bestSpan = i, span
			}
		}
	}
	return best
}

// callableKinds are unit kinds that can be the source of call-like edges.
var callableKinds = []string{"function", "method", storage.KindHTTPRoute}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// namedChildTexts returns the source texts of a node's named children.
// Returns nil for a nil node.
func namedChildTexts(fc *fileCtx, n *sitter.Node) []string {
	if n == nil {
		return nil
	}
	var out []string
	for i := 0; i < int(n.NamedChildCount()); i++ {
		out = append(out, fc.text(n.NamedChild(i)))
	}
	return out
}

// argNodes returns the named children of a call's child field (usually
// "arguments"), i.e. the argument expression nodes.
func argNodes(call *sitter.Node, field string) []*sitter.Node {
	argsNode := call.ChildByFieldName(field)
	if argsNode == nil {
		return nil
	}
	var nodes []*sitter.Node
	for i := 0; i < int(argsNode.NamedChildCount()); i++ {
		nodes = append(nodes, argsNode.NamedChild(i))
	}
	return nodes
}
