package ast

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/storage"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/csharp"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/kotlin"
	"github.com/smacker/go-tree-sitter/python"
	tsx "github.com/smacker/go-tree-sitter/typescript/tsx"
	typescript "github.com/smacker/go-tree-sitter/typescript/typescript"
)

// TreeSitterParser is a generic tree-sitter based parser.
type TreeSitterParser struct {
	lang string
}

// NewTreeSitterParser creates a new tree-sitter parser.
func NewTreeSitterParser(lang string) *TreeSitterParser {
	return &TreeSitterParser{lang: lang}
}

// sitterLanguage returns the tree-sitter language for this parser.
func (p *TreeSitterParser) sitterLanguage() (*sitter.Language, error) {
	switch p.lang {
	case "go":
		return golang.GetLanguage(), nil
	case "java":
		return java.GetLanguage(), nil
	case "kotlin":
		return kotlin.GetLanguage(), nil
	case "csharp":
		return csharp.GetLanguage(), nil
	case "typescript":
		return typescript.GetLanguage(), nil
	case "tsx":
		return tsx.GetLanguage(), nil
	case "javascript":
		return javascript.GetLanguage(), nil
	case "python":
		return python.GetLanguage(), nil
	default:
		return nil, fmt.Errorf("unsupported language: %s", p.lang)
	}
}

// extractor is a language-specific unit/edge extractor.
type extractor interface {
	extract(fc *fileCtx, root *sitter.Node)
}

func (p *TreeSitterParser) newExtractor() (extractor, error) {
	switch p.lang {
	case "go":
		return &goExtractor{}, nil
	case "java":
		return &javaExtractor{}, nil
	case "kotlin":
		return &ktExtractor{}, nil
	case "csharp":
		return &csharpExtractor{}, nil
	case "typescript", "tsx", "javascript":
		return &tsExtractor{}, nil
	case "python":
		return &pyExtractor{}, nil
	default:
		return nil, fmt.Errorf("unsupported language: %s", p.lang)
	}
}

// Parse parses a file and extracts AST units and edges.
//
// Edges reference their source unit via a positional marker ("#<idx>" into the
// returned units slice); the indexer rewrites markers to storage IDs after the
// units are persisted.
func (p *TreeSitterParser) Parse(filePath, content string) ([]*storage.ASTUnit, []*storage.Edge, error) {
	facts, err := p.ParseFacts(filePath, content)
	if err != nil {
		return nil, nil, err
	}
	return facts.Units, facts.Edges, nil
}

// ParseFacts parses a file and returns everything the repository-level passes
// need from it: the units and edges Parse returns, plus the facts that only
// make sense once other files are in view — the local outbound wrappers and
// this file's contract-coverage counters.
func (p *TreeSitterParser) ParseFacts(filePath, content string) (*fileFacts, error) {
	lang, err := p.sitterLanguage()
	if err != nil {
		return nil, err
	}
	ext, err := p.newExtractor()
	if err != nil {
		return nil, err
	}

	parser := sitter.NewParser()
	// The parser owns a C allocation that the garbage collector only reclaims
	// through a finalizer. A parser is a tiny Go object, so nothing pressures
	// the GC into running, and indexing thousands of files kept gigabytes of
	// tree-sitter memory alive. Release it explicitly.
	defer parser.Close()
	parser.SetLanguage(lang)

	tree := parser.Parse(nil, []byte(content))
	if tree == nil {
		return nil, fmt.Errorf("failed to parse")
	}
	defer tree.Close()

	fc := &fileCtx{path: filePath, lang: p.lang, src: []byte(content), cov: newCoverage()}
	ext.extract(fc, tree.RootNode())

	// The wrapper table is built before the same-file linking runs, so an edge
	// this pass adds can never turn its own call site into another wrapper:
	// following a helper stays one level deep.
	wrappers := fc.wrappers()
	fc.linkWrappers(wrappers)

	for _, u := range fc.units {
		if u.Hash == "" && u.StartByte >= 0 && u.EndByte <= len(content) && u.StartByte < u.EndByte {
			u.Hash = hashString(content[u.StartByte:u.EndByte])
		}
	}

	return &fileFacts{Units: fc.units, Edges: fc.edges, Wrappers: wrappers,
		Coverage: fc.cov, Tables: fc.tables, Consts: fc.consts}, nil
}

// Language returns the language name.
func (p *TreeSitterParser) Language() string {
	return p.lang
}

// GetParserForLanguage returns a parser for the given language.
func GetParserForLanguage(lang string) Parser {
	switch lang {
	case "proto":
		return newProtoParser()
	case "sql":
		return newSQLParser()
	case "yaml", "json", "properties":
		return newConfigParser(lang)
	default:
		return NewTreeSitterParser(lang)
	}
}

// RegisterDefaultParsers registers default parsers for common languages.
func RegisterDefaultParsers(indexer *Indexer) {
	for _, lang := range []string{
		"go", "java", "kotlin", "csharp", "typescript", "javascript", "python",
		"proto", "sql", "yaml", "json", "properties",
	} {
		indexer.RegisterParser(GetParserForLanguage(lang))
	}
}

// ---------------------------------------------------------------------------
// fileCtx: shared per-file extraction state
// ---------------------------------------------------------------------------

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

// constResolver maps identifiers to string literal values collected from the
// file and resolves expressions to strings: literal unquoting first, then a
// constant lookup.
type constResolver map[string]string

func (c constResolver) resolve(expr string) (string, bool) {
	if v, ok := unquote(expr); ok {
		return v, true
	}
	if v, ok := c[strings.TrimSpace(expr)]; ok {
		return v, true
	}
	return "", false
}

// Alias limits: per-file collection cap (giant generated files) and per-edge
// attachment cap (keep Edge.Meta small).
const (
	maxFileAliases = 64
	maxEdgeAliases = 8
)

// aliasLiterals are RHS texts that look identifier-like but are language
// literals/keywords, never aliases.
var aliasLiterals = map[string]bool{
	"true": true, "false": true, "nil": true, "null": true,
	"None": true, "True": true, "False": true, "undefined": true, "this": true,
}

// aliasScopeTypes are the grammar node types that delimit a local alias scope.
// The set is the union over the supported languages; type names are distinct
// enough between grammars that a shared table cannot mis-scope.
var aliasScopeTypes = map[string]bool{
	// go
	"function_declaration": true, "method_declaration": true, "func_literal": true,
	// python
	"function_definition": true, "lambda": true,
	// typescript / javascript
	"generator_function_declaration": true, "function_expression": true,
	"arrow_function": true, "function": true, "method_definition": true,
	// java / kotlin
	"constructor_declaration": true, "lambda_expression": true,
	"anonymous_function": true, "lambda_literal": true,
	// c#
	"local_function_statement": true,
}

// aliasEntry is one recorded alias plus the byte range it is visible in.
type aliasEntry struct {
	name  string
	expr  string
	start int
	end   int
}

// aliasTable collects local aliases (x := userID) together with the byte range
// of the declaration that encloses them. Aliases from different functions in
// the same file never mix, so `id := req.UserID` in one function cannot be
// attached to a call in another.
type aliasTable struct {
	entries []aliasEntry
}

// record adds name -> expr scoped to the innermost function-like ancestor of
// n, respecting the per-file cap. Self-aliases and literal keywords are
// dropped. An assignment outside any function is file-scoped.
func (t *aliasTable) record(n *sitter.Node, name, expr string) {
	name, expr = strings.TrimSpace(name), strings.TrimSpace(expr)
	if name == "" || expr == "" || name == expr || aliasLiterals[expr] {
		return
	}
	if len(t.entries) >= maxFileAliases {
		return
	}
	start, end := aliasScope(n)
	t.entries = append(t.entries, aliasEntry{name: name, expr: expr, start: start, end: end})
}

// at returns the aliases visible at byte offset pos. The innermost scope wins
// for a given name; within one scope the last recorded value wins.
func (t *aliasTable) at(pos int) map[string]string {
	if t == nil || len(t.entries) == 0 {
		return nil
	}
	out := map[string]string{}
	width := map[string]int{}
	for _, e := range t.entries {
		if pos < e.start || pos >= e.end {
			continue
		}
		w := e.end - e.start
		if prev, seen := width[e.name]; seen && prev < w {
			continue
		}
		out[e.name], width[e.name] = e.expr, w
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// relevant is at() narrowed to the aliases the call site actually references.
func (t *aliasTable) relevant(pos int, args []string, fields map[string]string) map[string]string {
	return relevantAliases(t.at(pos), args, fields)
}

// aliasScope returns the byte range of the innermost function-like ancestor of
// n, or the whole file when there is none.
func aliasScope(n *sitter.Node) (int, int) {
	for p := n; p != nil; p = p.Parent() {
		if aliasScopeTypes[p.Type()] {
			return int(p.StartByte()), int(p.EndByte())
		}
	}
	return 0, 1 << 62
}

// isAliasExpr reports whether a textual RHS looks like a plain identifier or
// dotted member access ("userId", "body.UserID"): identifier characters and
// dots only, not starting with a quote or digit. Used where the extractor has
// no AST node for the value (C#).
func isAliasExpr(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || (s[0] >= '0' && s[0] <= '9') || s[0] == '.' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !contract.IsWordByte(s[i]) && s[i] != '.' {
			return false
		}
	}
	return true
}

// relevantAliases returns the subset of aliases whose NAME occurs as an
// identifier token in any of the argument expressions or field values,
// capped at maxEdgeAliases entries. Returns nil when nothing is relevant.
//
// One transitive step is included: for each directly relevant alias, aliases
// whose names occur in its VALUE are attached too, so a chain like
// `y := userID; x := y; g(x)` fits into a single edge's meta and the trace
// can dereference it end to end.
//
// Selection is deterministic under the cap: args are scanned in order, field
// values in sorted key order, and the transitive step in sorted alias-name
// order, so re-indexing the same file always stores the same subset.
func relevantAliases(aliases map[string]string, args []string, fields map[string]string) map[string]string {
	if len(aliases) == 0 {
		return nil
	}
	out := map[string]string{}
	var direct []string
	scan := func(expr string) {
		for _, tok := range contract.IdentTokens(expr) {
			if len(out) >= maxEdgeAliases {
				return
			}
			if v, ok := aliases[tok]; ok {
				if _, dup := out[tok]; !dup {
					direct = append(direct, tok)
				}
				out[tok] = v
			}
		}
	}
	for _, a := range args {
		scan(a)
	}
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		scan(fields[k])
	}
	// One transitive step over the values of the directly relevant aliases,
	// in sorted name order so the cap always keeps the same entries.
	sort.Strings(direct)
	for _, name := range direct {
		scan(out[name])
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// unquote strips matching string delimiters and returns (value, true) if the
// text was a plain string literal. Handles ", ', ` and C#'s @" prefix.
func unquote(s string) (string, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "@") // C# verbatim
	if len(s) < 2 {
		return "", false
	}
	first, last := s[0], s[len(s)-1]
	if first != last || (first != '"' && first != '\'' && first != '`') {
		return "", false
	}
	body := s[1 : len(s)-1]
	if strings.ContainsRune(body, rune(first)) {
		return "", false
	}
	return body, true
}

// lastComponent is contract.LastComponent under its local name: the
// extractors compare the result against proto and framework tables verbatim,
// see the doc there for why surrounding whitespace is dropped.
func lastComponent(s string) string { return contract.LastComponent(s) }

// trimGenericArgs drops a type's generic argument list, keeping the type
// itself: IIntegrationEventHandler<OrderStarted> -> IIntegrationEventHandler,
// Repository<Order> -> Repository.
func trimGenericArgs(typ string) string {
	if i := strings.IndexByte(typ, '<'); i >= 0 {
		return strings.TrimSpace(typ[:i])
	}
	return strings.TrimSpace(typ)
}

// genericArgs returns the type arguments of a generic type reference, split at
// the top level so a nested generic stays one argument.
func genericArgs(typ string) []string {
	open := strings.IndexByte(typ, '<')
	shut := strings.LastIndexByte(typ, '>')
	if open < 0 || shut <= open {
		return nil
	}
	var out []string
	depth, start := 0, 0
	body := typ[open+1 : shut]
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '<', '[', '(':
			depth++
		case '>', ']', ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(body[start:i]))
				start = i + 1
			}
		}
	}
	if s := strings.TrimSpace(body[start:]); s != "" {
		out = append(out, s)
	}
	return out
}

func hasSuffixFold(s, suffix string) bool {
	return len(s) >= len(suffix) && strings.EqualFold(s[len(s)-len(suffix):], suffix)
}

// capitalizeFirst upper-cases a leading lowercase ASCII letter (createOrder ->
// CreateOrder). gRPC stubs expose lowerCamel method names in Java/TS while
// proto rpc_method units are PascalCase, and the linker compares method names
// exactly — so client extractors normalize before building the grpc key.
func capitalizeFirst(s string) string {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}

// splitURL splits a URL/path expression into (host, path). Absolute URLs keep
// the host; bare paths return an empty host.
func splitURL(u string) (host, path string) {
	rest := u
	for _, scheme := range []string{"http://", "https://", "grpc://"} {
		if strings.HasPrefix(rest, scheme) {
			rest = rest[len(scheme):]
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				return rest[:i], rest[i:]
			}
			return rest, "/"
		}
	}
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	return "", rest
}

// printfVerb matches a printf/format verb in a format string: %s, %d, %+v,
// %#v, %-10.2f. "%%" is not a verb and stays.
var printfVerb = regexp.MustCompile(`%[-+# 0]*[0-9*]*(?:\.[0-9*]+)?[a-zA-Z]`)

// braceInterp matches a "${...}" interpolation: JS template literals, Spring
// property placeholders and shell/compose substitutions.
var braceInterp = regexp.MustCompile(`\$\{[^}]*\}`)

// placeholderize rewrites the interpolations inside one string literal to the
// "{}" route-parameter form. Python f-string fields ("{order_id}") already
// have that shape and pass through.
func placeholderize(lit string) string {
	lit = braceInterp.ReplaceAllString(lit, "{}")
	lit = printfVerb.ReplaceAllString(lit, "{}")
	return strings.ReplaceAll(lit, "%%", "%")
}

// interpolatedPath reduces a URL expression built from literals and runtime
// values to a route template, with every interpolated value replaced by a
// "{}" placeholder — the shape the linker's path matcher treats as a path
// parameter. It covers fmt.Sprintf and %-formatting, Python f-strings and
// .format(), JS template literals and plain concatenation.
//
// It reports false unless the result anchors on a real path: a leading "/",
// no whitespace, and at least one literal segment. That keeps log lines, SQL
// text and fully dynamic strings from being registered as routes.
func interpolatedPath(expr string) (string, bool) { return interpolatedTemplate(expr, false) }

// interpolatedTemplate is interpolatedPath with the choice of what a template
// that does not open on "/" means. relative says the target is written against
// a client's base address, where a literal first segment ("Users/" + id +
// "/Password") is part of the path and rooting it keeps the whole route;
// otherwise the leading run is read as the base URL itself and dropped.
func interpolatedTemplate(expr string, relative bool) (string, bool) {
	var b strings.Builder
	gap := false // a runtime value was seen since the last literal
	for i := 0; i < len(expr); {
		switch c := expr[i]; {
		case c == '"' || c == '\'' || c == '`':
			j := i + 1
			for j < len(expr) && expr[j] != c {
				j++
			}
			if j >= len(expr) {
				return "", false // unterminated literal
			}
			if gap && b.Len() > 0 {
				b.WriteString("{}")
			}
			b.WriteString(placeholderize(expr[i+1 : j]))
			gap, i = false, j+1
		case isWordByte(c):
			for i < len(expr) && isWordByte(expr[i]) {
				i++
			}
			gap = true
		default:
			i++
		}
	}
	t := b.String()
	// A trailing value only forms its own segment when the literal ends on a
	// separator; otherwise it is glued into the last segment and unmatchable.
	if gap && strings.HasSuffix(t, "/") {
		t += "{}"
	}
	if i := strings.IndexAny(t, "?#"); i >= 0 {
		t = t[:i]
	}
	if !strings.Contains(t, "://") && !strings.HasPrefix(t, "/") {
		// Rooting the template asserts that its first segment is part of the
		// path, so the segment has to be spelled out — braces of any shape mean
		// a value stands there, and the value is the base address the rest is
		// written against. Python spells it f"{self._base_url}/watermark",
		// which named the base as a path segment until the test read braces
		// only in their reduced "{}" form.
		if head, _, sep := strings.Cut(t, "/"); relative && sep && !strings.ContainsAny(head, "{}") {
			t = "/" + t // a literal first segment is path, not base URL
		} else if i := strings.Index(t, "/"); i > 0 {
			t = t[i:] // drop a leading base-URL placeholder
		}
	}
	if strings.ContainsAny(t, " \t\n") {
		return "", false
	}
	if !strings.HasPrefix(t, "/") && !strings.Contains(t, "://") {
		return "", false
	}
	if !hasLiteralSegment(t) {
		return "", false // every segment is a placeholder
	}
	return t, true
}

// hasLiteralSegment reports whether a route template names at least one segment
// of its own. A template that is placeholders end to end ("/{}/{}") matches
// every route there is, which is the same as matching none.
func hasLiteralSegment(t string) bool {
	_, path := splitURL(t)
	for _, seg := range strings.Split(strings.Trim(path, "/"), "/") {
		if seg != "" && !strings.ContainsAny(seg, "{}") {
			return true
		}
	}
	return false
}

// splitPlaceholderDefault splits a "${KEY:default}" (Spring) or
// "${KEY:-default}" (shell, docker-compose) placeholder into its key and
// default value. ok is false when s is not a placeholder with a default.
func splitPlaceholderDefault(s string) (key, def string, ok bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "${") || !strings.HasSuffix(s, "}") {
		return "", "", false
	}
	body := s[2 : len(s)-1]
	i := strings.Index(body, ":")
	if i < 0 {
		return body, "", false
	}
	return body[:i], strings.TrimPrefix(body[i+1:], "-"), true
}

// applyPlaceholderDefaults replaces every "${KEY:default}" in a configuration
// value with its default, which is the value the service runs with when the
// key is not set in the environment. Plain "${KEY}" references are left for
// the linker to resolve.
func applyPlaceholderDefaults(v string) string {
	return braceInterp.ReplaceAllStringFunc(v, func(m string) string {
		if _, def, ok := splitPlaceholderDefault(m); ok {
			return def
		}
		return m
	})
}

// tableName is the derivation the linker re-runs on the other side of the
// join; it lives in internal/contract so the two cannot drift.
func tableName(typeName string) string { return contract.TableName(typeName) }

// snakeCase converts CamelCase / PascalCase to snake_case, leaving text that
// is already snake_case untouched.
func snakeCase(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if i > 0 && (isLowerOrDigit(s[i-1]) || (i+1 < len(s) && s[i+1] >= 'a' && s[i+1] <= 'z')) {
				b.WriteByte('_')
			}
			b.WriteByte(c - 'A' + 'a')
			continue
		}
		b.WriteByte(c)
	}
	return strings.Trim(b.String(), "_")
}

func isLowerOrDigit(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9'
}

// Contract join keys shared by the parsers. Construction and parsing live in
// internal/contract; these are thin local wrappers for the extractors.

// routeKey builds the join key for an HTTP route: "http:POST /a/b".
func routeKey(method, path string) string { return contract.HTTP(method, path) }

// topicKey builds the join key for a Kafka topic: "topic:orders.created".
func topicKey(name string) string { return contract.Topic(name) }

// grpcKey builds the join key for a gRPC method: "grpc:Service/Method" or
// "grpc:pkg.Service/Method". Empty service yields "grpc:/Method" which the
// linker matches by suffix.
func grpcKey(service, method string) string { return contract.GRPC(service, method) }

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// extractLineComments collects consecutive comment lines directly above a node.
func extractLineComments(content string, node *sitter.Node, prefix string) string {
	lines := strings.Split(content, "\n")
	startLine := int(node.StartPoint().Row)

	var comments []string
	for i := startLine - 1; i >= 0 && i >= startLine-10; i-- {
		if i < 0 || i >= len(lines) {
			break
		}
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, prefix) {
			comments = append([]string{strings.TrimPrefix(strings.TrimPrefix(line, prefix), " ")}, comments...)
		} else if line == "" {
			continue
		} else {
			break
		}
	}

	return strings.Join(comments, "\n")
}

// isWordByte reports whether b is an identifier character.
func isWordByte(b byte) bool {
	return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}
