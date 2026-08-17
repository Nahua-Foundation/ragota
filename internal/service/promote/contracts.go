package promote

import (
	"context"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/graph"
	"github.com/Nahua-Foundation/ragota/internal/indexing"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// A question that names a contract carries the answer's address in it.
// "Where does POST /admin/draft-orders/:id/convert-to-order go" contains the
// exact key the linker already built for that route; "which model maps to the
// image table" contains "db:image". Ranking cannot beat a lookup here — the
// same words are also in the generated OpenAPI page and the migration and the
// client SDK, and BM25 has no reason to prefer the handler — so a literal key
// is resolved through the graph and its answers are placed ahead of the text
// results, exactly like a callers question (see callers.go).
//
// Only literal keys count. A route named in prose ("which endpoint returns the
// list of veterinarians") carries no key and is left to retrieval; a key that
// is not in the index changes nothing. This is what keeps the mechanism
// language- and framework-independent: the keys are built by the linker the
// same way for every language, and this file only reads them back.

const (
	// maxPromotedContracts caps how many graph answers are placed in front of
	// the text results for one query.
	maxPromotedContracts = 10
	// contractEdgeLimit is how many edges are pulled per resolved key.
	contractEdgeLimit = 50
	// maxContractKeys bounds how many distinct keys one query may resolve.
	maxContractKeys = 4
	// rpcImplScanLimit bounds the implements_rpc edges read per repository for
	// the implementation lookup.
	rpcImplScanLimit = 2000
)

// contractRef is one contract key found in a query.
type contractRef struct {
	kind contract.Kind
	key  string
}

// Query-language patterns for the four contract families. They describe how a
// developer writes a key in a sentence, not how any framework declares one.
var (
	// "POST /admin/draft-orders/:id/convert-to-order", "get /api/v1/pets".
	httpKeyRe = regexp.MustCompile(`(?i)\b(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\s+(/[^\s,;)"']*)`)
	// "table named image", "table `orders`" — groups: introducer, quote, name.
	tableAfterRe = regexp.MustCompile(`(?i)\btables?\s+(named\s+|called\s+)?(["'` + "`" + `])?([A-Za-z_][A-Za-z0-9_]*)`)
	// "the login_attempt table".
	tableBeforeRe = regexp.MustCompile(`(?i)\b([A-Za-z_][A-Za-z0-9_]*)\s+tables?\b`)
	// "topic orders.created", "channel named events".
	topicAfterRe = regexp.MustCompile(`(?i)\b(?:topic|queue|channel|subject|stream)s?\s+(named\s+|called\s+)?(["'` + "`" + `])?([A-Za-z_][A-Za-z0-9_.\-]*)`)
	// "the orders queue".
	topicBeforeRe = regexp.MustCompile(`(?i)\b([A-Za-z_][A-Za-z0-9_.\-]*)\s+(?:topic|queue|channel|subject|stream)s?\b`)
	// "CustomersService/GetOwner", "cart.CartService/AddItem".
	grpcPairRe = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_.]*)/([A-Z][A-Za-z0-9_]*)\b`)
	// "the gRPC method AddItem", "rpc GetOwner".
	grpcMethodRe = regexp.MustCompile(`(?i)\b(?:grpc|rpc)\s+(?:method\s+|call\s+)?([A-Z][A-Za-z0-9_]*)\b`)
)

// contractNoise are words the "<word> table/queue/topic" patterns pick up when
// the sentence is about the concept rather than about a named one ("the
// database table", "a message queue"). They are the grammar around the key,
// never a key.
var contractNoise = map[string]bool{
	"a": true, "an": true, "the": true, "this": true, "that": true, "its": true,
	"database": true, "db": true, "sql": true, "message": true, "event": true,
	"kafka": true, "rabbitmq": true, "nats": true, "sqs": true, "pubsub": true,
	"same": true, "which": true, "what": true, "one": true, "each": true,
}

// contractKeys extracts the literal contract keys a query names.
func contractKeys(query string) []contractRef {
	var out []contractRef
	seen := map[string]bool{}
	add := func(kind contract.Kind, key string) {
		if key == "" || seen[key] || len(out) >= maxContractKeys {
			return
		}
		seen[key] = true
		out = append(out, contractRef{kind: kind, key: key})
	}

	for _, m := range httpKeyRe.FindAllStringSubmatch(query, -1) {
		add(contract.KindHTTP, contract.HTTP(m[1], m[2]))
	}
	for _, name := range namedEntities(query, tableAfterRe, tableBeforeRe) {
		add(contract.KindDB, contract.DB(strings.ToLower(name)))
	}
	for _, name := range namedEntities(query, topicAfterRe, topicBeforeRe) {
		add(contract.KindTopic, contract.Topic(name))
	}
	for _, m := range grpcPairRe.FindAllStringSubmatch(query, -1) {
		add(contract.KindGRPC, contract.GRPC(m[1], m[2]))
	}
	for _, m := range grpcMethodRe.FindAllStringSubmatch(query, -1) {
		add(contract.KindGRPC, contract.GRPC("", m[1]))
	}
	return out
}

// namedEntities collects the entity names a query gives for one contract
// family, from the "<keyword> X" pattern (after) and the "X <keyword>" pattern
// (before).
//
// The two positions are not equally trustworthy. Before the keyword, the word
// is the name by grammar: "the orders queue" can only be about a queue called
// orders. After it, the sentence usually just continues — "the message queue
// configured by" — so a name there is only believed when the sentence
// introduces it ("named", "called"), quotes it, or when it is shaped like an
// identifier rather than an English word.
func namedEntities(query string, after, before *regexp.Regexp) []string {
	var out []string
	for _, m := range after.FindAllStringSubmatch(query, -1) {
		introduced, quoted, name := m[1] != "", m[2] != "", m[3]
		if introduced || quoted || entityShaped(name) {
			out = append(out, name)
		}
	}
	for _, m := range before.FindAllStringSubmatch(query, -1) {
		out = append(out, m[1])
	}

	kept := out[:0]
	for _, name := range out {
		name = strings.Trim(name, "`'\"")
		if len(name) >= 3 && !contractNoise[strings.ToLower(name)] {
			kept = append(kept, name)
		}
	}
	return dedupStrings(kept)
}

// entityShaped reports whether a token reads as a machine name rather than an
// English word: a separator, a digit, or an internal case change.
func entityShaped(name string) bool {
	prevLower := false
	for i, r := range name {
		switch {
		case r == '_' || r == '.' || r == '-':
			return true
		case r >= '0' && r <= '9':
			return true
		case r >= 'A' && r <= 'Z':
			if i > 0 && prevLower {
				return true
			}
		}
		prevLower = r >= 'a' && r <= 'z'
	}
	return false
}

// contractEdgeRank orders the edges pointing at one contract key by how
// directly each answers "where does this contract live". The code that
// implements or serves a contract comes before the code that consumes it.
var contractEdgeRank = map[string]int{
	storage.EdgeHandledBy:     0, // route -> handler: the server side
	storage.EdgeImplementsRPC: 0,
	storage.EdgeProduces:      1,
	storage.EdgeWritesTo:      1,
	storage.EdgeConsumes:      2,
	storage.EdgeReadsFrom:     2,
}

// contractClientEdges are the far side of a contract: code that calls it
// rather than code that serves it. They are never promoted (see contractHits).
var contractClientEdges = map[string]bool{
	storage.EdgeHTTPCall: true,
	storage.EdgeRPCCall:  true,
}

// contractEdgeVerb phrases a hit's Reason per edge kind.
var contractEdgeVerb = map[string]string{
	storage.EdgeHandledBy:     "handles",
	storage.EdgeImplementsRPC: "implements",
	storage.EdgeProduces:      "publishes to",
	storage.EdgeWritesTo:      "writes",
	storage.EdgeConsumes:      "consumes",
	storage.EdgeReadsFrom:     "reads",
}

// rpcMentionRe gates the implementation lookup below: it only makes sense for
// a question that is about gRPC at all.
var rpcMentionRe = regexp.MustCompile(`(?i)\b(?:grpc|rpc)\b`)

// rpcStopWords are the words such a question is made of rather than the words
// that name the service or method it asks about.
var rpcStopWords = map[string]bool{
	"grpc": true, "rpc": true, "method": true, "service": true, "server": true,
	"where": true, "is": true, "are": true, "the": true, "a": true, "an": true,
	"implemented": true, "implementation": true, "implements": true, "handler": true,
	"in": true, "of": true, "for": true, "which": true, "what": true, "does": true,
	"api": true, "client": true, "call": true, "calls": true,
}

// PromoteRPCImplementations answers "where is <gRPC thing> implemented" from
// the implements_rpc edges. A question can name the contract in ways no key
// pattern catches — "the ApplicationService Sync grpc method", "the grpc
// basket service" — but the edges carry the key, so the match runs the other
// way round: read the repository's implementation edges and keep the ones
// whose key the question describes.
//
// It deliberately does not fire for a callers question. "What calls the
// shipping service ShipOrder rpc" names the same contract and wants the
// opposite side of it, and promoting implementations there would undo the
// callers answer.
func (p *Promoter) PromoteRPCImplementations(ctx context.Context, q *indexing.SearchQuery,
	hits []*indexing.Hit, meta map[string]interface{}) []*indexing.Hit {

	if p.storage == nil || !rpcMentionRe.MatchString(q.Query) {
		return hits
	}
	words := map[string]bool{}
	for _, w := range graph.WordComponents(q.Query) {
		if !rpcStopWords[w] {
			words[w] = true
		}
	}
	if len(words) == 0 {
		return hits
	}

	repos := q.Repos
	if len(repos) == 0 {
		repos = []string{""}
	}
	var matched []*storage.Edge
	for _, repoID := range repos {
		edges, err := p.storage.GetEdges(ctx, storage.QueryOpts{
			RepoID: repoID, Kind: storage.EdgeImplementsRPC, Limit: rpcImplScanLimit,
		})
		if err != nil {
			slog.Debug("rpc intent: implements lookup failed", "error", err)
			continue
		}
		for _, e := range edges {
			if e != nil && !testishPath(e.FilePath) && rpcKeyDescribed(e.DstName, words) {
				matched = append(matched, e)
			}
		}
	}
	if len(matched) == 0 {
		return hits
	}
	sortContractEdges(matched)
	if len(matched) > maxPromotedContracts {
		matched = matched[:maxPromotedContracts]
	}

	srcIDs := make([]string, 0, len(matched))
	for _, e := range matched {
		srcIDs = append(srcIDs, e.SrcID)
	}
	srcUnits := p.unitsByID(ctx, srcIDs)

	promoted := make([]*indexing.Hit, 0, len(matched))
	seen := map[string]bool{}
	for _, e := range matched {
		key := e.RepoID + "\x00" + e.FilePath + "\x00" + strconv.Itoa(e.Line)
		if seen[key] {
			continue
		}
		seen[key] = true
		promoted = append(promoted, contractEdgeHit(e, srcUnits[e.SrcID], e.DstName, "implements"))
	}
	meta["rpc_implementations"] = len(promoted)
	return prependPromoted(promoted, hits)
}

// rpcKeyDescribed reports whether the question names the service or the method
// of a gRPC key. Either side is enough: "the grpc basket service" names only
// the service, "the ApplicationService Sync grpc method" only resolves by its
// method, because the key the extractor stored has no service at all.
func rpcKeyDescribed(key string, words map[string]bool) bool {
	service, method, ok := contract.ParseGRPC(key)
	if !ok {
		return false
	}
	return componentsDescribed(method, words) || componentsDescribed(service, words)
}

// componentsDescribed reports whether every content word of name appears in
// the question. The question's scaffolding is dropped from both sides, or the
// comparison is asymmetric: "the grpc shipping service" leaves the word
// "shipping", while the key it names, ShippingService, still carries the
// component "service" and would never match it.
func componentsDescribed(name string, words map[string]bool) bool {
	var content int
	for _, c := range graph.WordComponents(name) {
		if rpcStopWords[c] {
			continue
		}
		if !words[c] {
			return false
		}
		content++
	}
	return content > 0
}

// PromoteContracts places the graph's answers for every contract key named in
// the query ahead of the ranked text hits. It returns hits unchanged when the
// query names no key, or when no key resolves to anything indexed.
func (p *Promoter) PromoteContracts(ctx context.Context, q *indexing.SearchQuery,
	hits []*indexing.Hit, meta map[string]interface{}) []*indexing.Hit {

	if p.storage == nil {
		return hits
	}
	refs := contractKeys(q.Query)
	if len(refs) == 0 {
		return hits
	}

	var promoted []*indexing.Hit
	var resolved []string
	for _, ref := range refs {
		found := p.contractHits(ctx, q, ref)
		if len(found) == 0 {
			continue
		}
		resolved = append(resolved, ref.key)
		promoted = append(promoted, found...)
		if len(promoted) >= maxPromotedContracts {
			promoted = promoted[:maxPromotedContracts]
			break
		}
	}
	if len(promoted) == 0 {
		return hits
	}

	meta["contract_keys"] = resolved
	meta["contract_promoted"] = len(promoted)
	return prependPromoted(promoted, hits)
}

// contractHits answers one key: the declaration itself (a route, a table, a
// proto method), whatever serves it (a route's handler), and the code that
// touches it, in that order.
func (p *Promoter) contractHits(ctx context.Context, q *indexing.SearchQuery, ref contractRef) []*indexing.Hit {
	units := p.unitsByKey(ctx, q, ref.key)

	var out []*indexing.Hit
	seen := map[string]bool{}
	push := func(h *indexing.Hit) {
		key := h.RepoID + "\x00" + h.FilePath + "\x00" + strconv.Itoa(h.Line)
		if seen[key] || len(out) >= maxPromotedContracts {
			return
		}
		seen[key] = true
		out = append(out, h)
	}

	// A route's answer is its handler, not the line that registers it.
	for _, u := range units {
		if u.Kind != storage.KindHTTPRoute {
			continue
		}
		for _, h := range p.handlersOf(ctx, u) {
			push(h)
		}
	}
	for _, u := range units {
		push(unitHit(u, contractDeclares(u.Kind)))
	}

	edges, err := p.storage.GetEdges(ctx, storage.QueryOpts{Name: ref.key, Limit: contractEdgeLimit})
	if err != nil {
		slog.Debug("contract intent: edge lookup failed", "key", ref.key, "error", err)
		return out
	}
	kept := edges[:0]
	for _, e := range edges {
		switch {
		case e == nil || e.Kind == storage.EdgeCall:
		case !repoAllowed(q.Repos, e.RepoID):
		// The far side of a contract is not where the contract lives. A load
		// generator that posts to /cart/checkout and a functional test that
		// calls PUT /api/orders/cancel both match the key perfectly and
		// answer a different question; promoting them ahead of ranking put
		// locustfile.py and OrderingApiTests.cs above the handler. "Who calls
		// this endpoint" is the callers intent's question, and it is still
		// answered — by the callers path, from the same edges.
		case contractClientEdges[e.Kind]:
		// Nothing is promoted from test scaffolding: placing a hit ahead of
		// the ranked results asserts it is the answer, and a test exercising
		// a contract is at best context.
		case testishPath(e.FilePath):
		default:
			kept = append(kept, e)
		}
	}
	sortContractEdges(kept)

	srcIDs := make([]string, 0, len(kept))
	for _, e := range kept {
		srcIDs = append(srcIDs, e.SrcID)
	}
	srcUnits := p.unitsByID(ctx, srcIDs)
	for _, e := range kept {
		verb := contractEdgeVerb[e.Kind]
		if verb == "" {
			verb = "uses"
		}
		push(contractEdgeHit(e, srcUnits[e.SrcID], ref.key, verb))
	}

	filters := indexing.ParseFilters(q.Filter)
	if filters.Empty() {
		return out
	}
	kept2 := out[:0]
	for _, h := range out {
		if filters.Match(h.Language, h.Kind, h.FilePath) {
			kept2 = append(kept2, h)
		}
	}
	return kept2
}

// unitsByKey resolves a contract key to the units that declare it: an exact
// qualified match first, then a suffix match, which is how a key survives the
// package or schema prefix a declaration may carry ("grpc:pkg.Svc/M" for a
// query that only named "Svc/M").
func (p *Promoter) unitsByKey(ctx context.Context, q *indexing.SearchQuery, key string) []*storage.ASTUnit {
	opts := storage.QueryOpts{Qualified: key, Limit: 5}
	if len(q.Repos) == 1 {
		opts.RepoID = q.Repos[0]
	}
	units, err := p.storage.GetASTUnits(ctx, opts)
	if err != nil {
		slog.Debug("contract intent: unit lookup failed", "key", key, "error", err)
		return nil
	}
	if len(units) == 0 {
		opts.Qualified, opts.QualifiedSuffix = "", key
		if units, err = p.storage.GetASTUnits(ctx, opts); err != nil {
			return nil
		}
	}
	out := units[:0]
	for _, u := range units {
		if repoAllowed(q.Repos, u.RepoID) {
			out = append(out, u)
		}
	}
	return out
}

// handlersOf follows a route unit's handled_by edges to the functions that
// serve it.
func (p *Promoter) handlersOf(ctx context.Context, route *storage.ASTUnit) []*indexing.Hit {
	edges, err := p.storage.GetEdges(ctx, storage.QueryOpts{
		SrcID: route.ID, Kind: storage.EdgeHandledBy, Limit: 10,
	})
	if err != nil || len(edges) == 0 {
		return nil
	}
	ids := make([]string, 0, len(edges))
	for _, e := range edges {
		ids = append(ids, e.DstID)
	}
	byID := p.unitsByID(ctx, ids)

	var out []*indexing.Hit
	for _, e := range edges {
		if u := byID[e.DstID]; u != nil {
			out = append(out, unitHit(u, "handles "+route.Name))
		}
	}
	return out
}

// sortContractEdges ranks a key's edges: the serving side first, production
// code before test scaffolding, higher confidence first, then location.
func sortContractEdges(edges []*storage.Edge) {
	sortEdgesBy(edges, func(kind string) int {
		if r, ok := contractEdgeRank[kind]; ok {
			return r
		}
		return len(contractEdgeRank)
	})
}

// contractDeclares names what a declaration unit is, for the hit's Reason.
func contractDeclares(kind string) string {
	switch kind {
	case storage.KindHTTPRoute:
		return "declares the route"
	case storage.KindDBTable:
		return "declares the table"
	case storage.KindRPCMethod:
		return "declares the rpc method"
	case storage.KindTopicChannel:
		return "declares the channel"
	}
	return "declares the contract"
}

// unitHit turns a unit into a hit at its own definition.
func unitHit(u *storage.ASTUnit, reason string) *indexing.Hit {
	return &indexing.Hit{
		RepoID:   u.RepoID,
		FilePath: u.FilePath,
		Path:     u.FilePath,
		Line:     u.StartLine,
		EndLine:  u.EndLine,
		Symbol:   u.Name,
		Kind:     u.Kind,
		Language: u.Language,
		Reason:   reason,
		Snippet:  unitSnippet(u, reason),
	}
}

// contractEdgeHit turns one edge into a hit at the line that touches the
// contract.
func contractEdgeHit(e *storage.Edge, src *storage.ASTUnit, key, verb string) *indexing.Hit {
	hit := &indexing.Hit{
		RepoID:   e.RepoID,
		FilePath: e.FilePath,
		Path:     e.FilePath,
		Line:     e.Line,
		EndLine:  e.Line,
		Reason:   verb + " " + key,
	}
	if src != nil {
		hit.Symbol = src.Name
		hit.Kind = src.Kind
		hit.Language = src.Language
	}
	var b strings.Builder
	if src != nil {
		writeUnitHead(&b, src)
		b.WriteByte('\n')
	}
	b.WriteString(verb)
	b.WriteByte(' ')
	b.WriteString(key)
	b.WriteString(" at ")
	b.WriteString(e.FilePath)
	b.WriteByte(':')
	b.WriteString(strconv.Itoa(e.Line))
	hit.Snippet = b.String()
	return hit
}

// unitSnippet describes a unit for a reranker or an LLM, from what the graph
// already knows.
func unitSnippet(u *storage.ASTUnit, reason string) string {
	var b strings.Builder
	writeUnitHead(&b, u)
	if reason != "" {
		b.WriteByte('\n')
		b.WriteString(reason)
	}
	return b.String()
}

// writeUnitHead writes "<kind> <qualified><signature>" plus the first doc line.
func writeUnitHead(b *strings.Builder, u *storage.ASTUnit) {
	if u.Kind != "" {
		b.WriteString(u.Kind)
		b.WriteByte(' ')
	}
	b.WriteString(firstNonEmpty(u.Qualified, u.Name))
	if u.Signature != "" {
		b.WriteString(u.Signature)
	}
	if doc := firstDocLine(u.Doc); doc != "" {
		b.WriteByte('\n')
		b.WriteString(doc)
	}
}
