package promote

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/graph"
	"github.com/Nahua-Foundation/ragota/internal/index"
	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// Answering "what calls X" from text retrieval alone fails by construction: a
// call site is one line inside a function whose name and documentation are
// about something else, so no BM25 or embedding document for the caller talks
// about X. The graph already holds every call site — the AST pass stores call
// and contract edges — so the service resolves X (by exact symbol name and
// through the retrieval hits) and promotes the incoming edges' call sites to
// the front of the result list. When the graph has nothing, the text results
// are returned untouched.

const (
	// calleeFromHits is how many leading retrieval hits are examined as
	// candidate definitions of the thing the question is about.
	calleeFromHits = 6
	// calleeRankGate: a hit-derived callee whose unit name is not described by
	// the query words is trusted only at the very top of the ranking.
	calleeRankGate = 2
	// calleeMaxUnits caps how many distinct callees are expanded to their
	// callers, and calleeNameLookup how many units one name may return.
	//
	// A name is not a callee: generated clients, vendored protos and copied
	// interfaces put the same name on many units — boutique holds 17 units
	// named ShipOrder, one per service's genproto copy. The edges resolve to
	// exactly one of them, so looking at "the first five units with this name"
	// asked the wrong five and found no callers at all. Units sharing a
	// qualified name are therefore one callee with several ids.
	calleeMaxUnits   = 8
	calleeNameLookup = 60
	// calleeIDsPerGroup bounds the edge lookups one callee may cost.
	calleeIDsPerGroup = 25
	// callerEdgeLimit is how many incoming edges are pulled per callee unit.
	callerEdgeLimit = 50
	// maxPromotedCallers caps how many call sites are placed ahead of the text
	// results, so a popular symbol cannot evict every text hit.
	maxPromotedCallers = 10
	// promotedScoreStep separates the promoted hits' synthetic scores, keeping
	// the served order recoverable from the scores alone.
	promotedScoreStep = 0.001
)

// structuralEdgeKinds are incoming edges that do not represent "code using
// the unit": imports are file plumbing, implements_rpc points from the
// implementation to the contract (that answers "where is it implemented",
// the opposite question), and runtime_call carries service-level tracing
// aggregates rather than a code location.
var structuralEdgeKinds = map[string]bool{
	store.EdgeImport:        true,
	store.EdgeImplementsRPC: true,
	store.EdgeRuntimeCall:   true,
}

// callerKindRank orders caller edges by how directly they answer "what calls
// X". Unlisted kinds sort after the listed ones.
var callerKindRank = map[string]int{
	store.EdgeCall:        0,
	store.EdgeRPCCall:     1,
	store.EdgeHTTPCall:    2,
	store.EdgeHandledBy:   3, // src is the route unit: the registration site
	store.EdgeProduces:    4,
	store.EdgeConsumes:    5,
	store.EdgeWritesTo:    6,
	store.EdgeReadsFrom:   7,
	store.EdgeRPCRequest:  8,
	store.EdgeRPCResponse: 9,
	store.EdgeKafkaFlow:   10,
}

// callerKindVerb phrases a hit's Reason for each edge kind.
var callerKindVerb = map[string]string{
	store.EdgeCall:        "calls",
	store.EdgeRPCCall:     "rpc call to",
	store.EdgeHTTPCall:    "http call to",
	store.EdgeHandledBy:   "routes to",
	store.EdgeProduces:    "publishes to",
	store.EdgeConsumes:    "consumes",
	store.EdgeWritesTo:    "writes",
	store.EdgeReadsFrom:   "reads",
	store.EdgeRPCRequest:  "uses message",
	store.EdgeRPCResponse: "uses message",
	store.EdgeKafkaFlow:   "feeds",
}

// PromoteCallers returns the hit list for a callers-intent query: the call
// sites of the resolved callee units first, the ranked text hits after, with
// metadata recording what was resolved so a client can tell a graph answer
// from a plain one. It runs after the rank stage — passing call sites through
// a reranker was measured and lost (see the call site in Search) — and the
// caller truncates.
//
// The hits are returned unchanged when no callee or no caller is found.
func (p *Promoter) PromoteCallers(ctx context.Context, q *index.SearchQuery, callee string,
	hits []*index.Hit, meta map[string]interface{}) []*index.Hit {

	if p.store == nil || p.graph == nil {
		return hits
	}
	meta["intent"] = IntentCallers

	callees := p.calleeUnits(ctx, q, callee, hits)
	if len(callees) == 0 {
		return hits
	}
	names := make([]string, 0, len(callees))
	for _, g := range callees {
		names = append(names, g.repr.Name)
	}
	meta["intent_callees"] = names

	promoted := p.callerHits(ctx, q, callees)
	if len(promoted) == 0 {
		return hits
	}
	meta["intent_promoted"] = len(promoted)
	return prependPromoted(promoted, hits)
}

// prependPromoted puts what the graph knows in front of the ranked text hits.
//
// The promoted hits carry synthetic scores above the best text score, so that
// they lead the list a reranker is given (and, with no reranker, the list that
// is served) and a client sorting by score reproduces that order. A text hit
// that points at the same lines as a promoted one is dropped rather than
// repeated.
func prependPromoted(promoted, hits []*index.Hit) []*index.Hit {
	top := float32(1.0)
	if len(hits) > 0 && hits[0].Score > 0 {
		top = hits[0].Score
	}
	for i, h := range promoted {
		h.Score = top + promotedScoreStep*float32(len(promoted)-i)
	}

	merged := make([]*index.Hit, 0, len(promoted)+len(hits))
	merged = append(merged, promoted...)
	for _, h := range hits {
		if overlapsAny(h, promoted) {
			continue
		}
		merged = append(merged, h)
	}
	return merged
}

// calleeGroup is one callee: every unit that declares it, under one
// representative. A gRPC method declared in a .proto vendored into ten
// services is ten units and one callee, and its callers resolve to whichever
// copy the linker picked.
type calleeGroup struct {
	repr *domain.ASTUnit
	ids  []string
}

// calleeUnits resolves the callee description to callee groups, two ways:
// exact symbol names present in the question ("who uses AugmentSyncMsg"), and
// the units behind the leading retrieval hits — a described callee retrieves
// its own definition well even when its name never appears in the query. A
// hit-derived unit is kept when the query words describe its name, or when
// retrieval put it at the very top.
func (p *Promoter) calleeUnits(ctx context.Context, q *index.SearchQuery, callee string, hits []*index.Hit) []*calleeGroup {
	var out []*calleeGroup
	byKey := map[string]*calleeGroup{}
	add := func(u *domain.ASTUnit) {
		if u == nil || u.ID == "" {
			return
		}
		// Units that declare the same thing are one callee. Qualified names
		// are the identity when present; a bare name plus kind otherwise.
		key := u.Qualified
		if key == "" {
			key = u.Kind + "\x00" + u.Name
		}
		if g := byKey[key]; g != nil {
			if len(g.ids) < calleeIDsPerGroup {
				g.ids = append(g.ids, u.ID)
			}
			return
		}
		if len(out) >= calleeMaxUnits {
			return
		}
		g := &calleeGroup{repr: u, ids: []string{u.ID}}
		byKey[key] = g
		out = append(out, g)
	}

	for _, tok := range identifierTokens(callee) {
		opts := domain.QueryOpts{Name: tok, Limit: calleeNameLookup}
		if len(q.Repos) == 1 {
			opts.RepoID = q.Repos[0]
		}
		units, err := p.store.GetASTUnits(ctx, opts)
		if err != nil {
			slog.Debug("callers intent: symbol lookup failed", "name", tok, "error", err)
			continue
		}
		for _, u := range units {
			if repoAllowed(q.Repos, u.RepoID) {
				add(u)
			}
		}
	}

	words := wordSet(graph.WordComponents(callee))
	for i, hit := range hits {
		if i >= calleeFromHits || len(out) >= calleeMaxUnits {
			break
		}
		if hit == nil {
			continue
		}
		unit, err := p.graph.UnitInRange(ctx, hit.RepoID, hit.FilePath, hit.Line, hit.EndLine)
		if err != nil {
			continue
		}
		// The rank gate means "retrieval is confident this is the code being
		// asked about" — a hit in test scaffolding or a config file is a
		// vocabulary coincidence, not that code, so those qualify only by
		// having their name actually described in the query.
		gated := i < calleeRankGate && !testishPath(hit.FilePath) && rankGateKind(unit.Kind)
		if gated || nameDescribed(unit.Name, words) {
			add(unit)
		}
	}
	return out
}

// rankGateKind reports whether a unit kind may be accepted as the callee on
// retrieval rank alone. Config entries and generated summaries repeat the
// question's vocabulary without being the code it asks about.
func rankGateKind(kind string) bool {
	return kind != store.KindConfigKey && kind != store.KindSummary
}

// callerHits turns the callees' incoming edges into ranked hits at the call
// sites. Callees are visited round-robin so one popular unit cannot starve
// the others.
func (p *Promoter) callerHits(ctx context.Context, q *index.SearchQuery, callees []*calleeGroup) []*index.Hit {
	filters := index.ParseFilters(q.Filter)

	perCallee := make([][]*domain.Edge, len(callees))
	var srcIDs []string
	for i, g := range callees {
		edges := p.incomingEdges(ctx, g)
		sortCallerEdges(edges)
		perCallee[i] = edges
		for _, e := range edges {
			if e.SrcID != "" && e.SrcID != "0" {
				srcIDs = append(srcIDs, e.SrcID)
			}
		}
	}
	srcUnits := p.unitsByID(ctx, srcIDs)

	var out []*index.Hit
	seen := map[string]bool{}
	for round := 0; len(out) < maxPromotedCallers; round++ {
		advanced := false
		for i, edges := range perCallee {
			if round >= len(edges) || len(out) >= maxPromotedCallers {
				continue
			}
			advanced = true
			e := edges[round]
			if !repoAllowed(q.Repos, e.RepoID) {
				continue
			}
			key := e.RepoID + "\x00" + e.FilePath + "\x00" + strconv.Itoa(e.Line)
			if seen[key] {
				continue
			}
			hit := callerHit(e, srcUnits[e.SrcID], callees[i].repr)
			if !filters.Empty() && !filters.Match(hit.Language, hit.Kind, hit.FilePath) {
				continue
			}
			seen[key] = true
			out = append(out, hit)
		}
		if !advanced {
			break
		}
	}
	return out
}

// incomingEdges returns the edges that represent code using the callee:
// resolved edges against every unit that declares it, then unresolved ones by
// name, minus structural kinds and self-references. Every declaring unit is
// queried because the linker resolves an edge to one of them and nothing says
// which.
func (p *Promoter) incomingEdges(ctx context.Context, g *calleeGroup) []*domain.Edge {
	var edges []*domain.Edge
	own := make(map[string]bool, len(g.ids))
	for _, id := range g.ids {
		own[id] = true
	}
	for _, id := range g.ids {
		if len(edges) >= callerEdgeLimit {
			break
		}
		found, err := p.store.GetEdges(ctx, domain.QueryOpts{
			DstID: id, Limit: callerEdgeLimit - len(edges),
		})
		if err != nil {
			slog.Debug("callers intent: edge lookup failed", "unit", g.repr.Name, "error", err)
			continue
		}
		edges = append(edges, found...)
	}
	if len(edges) < callerEdgeLimit {
		unresolved, uerr := p.store.GetEdges(ctx, domain.QueryOpts{
			RepoID: g.repr.RepoID, Name: g.repr.Name, Unresolved: true,
			Limit: callerEdgeLimit - len(edges),
		})
		if uerr == nil {
			edges = append(edges, unresolved...)
		}
	}

	kept := edges[:0]
	for _, e := range edges {
		if e == nil || structuralEdgeKinds[e.Kind] {
			continue
		}
		if own[e.SrcID] { // recursion is not an answer to "what calls X"
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

// sortCallerEdges ranks a callee's incoming edges: direct kinds first, then
// production code before test scaffolding — an unresolved production call
// site still beats a confirmed call from a test, because tests are almost
// never the answer to "what calls X" — then resolved destinations before
// name-match guesses, higher confidence first, and location for determinism.
func sortCallerEdges(edges []*domain.Edge) {
	sortEdgesBy(edges, kindRank)
}

// sortEdgesBy is the shared edge ordering: by the caller's notion of which
// edge kinds answer the question best, then production code before test
// scaffolding, resolved destinations before name-match guesses, higher
// confidence first, and location so that equal edges have a fixed order.
func sortEdgesBy(edges []*domain.Edge, rank func(kind string) int) {
	sort.SliceStable(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if ka, kb := rank(a.Kind), rank(b.Kind); ka != kb {
			return ka < kb
		}
		if ta, tb := testishPath(a.FilePath), testishPath(b.FilePath); ta != tb {
			return !ta
		}
		if ra, rb := resolvedEdge(a), resolvedEdge(b); ra != rb {
			return ra
		}
		if a.Confidence != b.Confidence {
			return a.Confidence > b.Confidence
		}
		if a.FilePath != b.FilePath {
			return a.FilePath < b.FilePath
		}
		return a.Line < b.Line
	})
}

func kindRank(kind string) int {
	if r, ok := callerKindRank[kind]; ok {
		return r
	}
	return len(callerKindRank)
}

// resolvedEdge reports whether the edge's destination was resolved to a unit
// rather than matched by name.
func resolvedEdge(e *domain.Edge) bool {
	return e.DstID != "" && e.DstID != "0"
}

// callerHit builds the hit for one call site. The hit points at the edge's
// line — the line the answer lives on — not at the caller's whole body.
func callerHit(e *domain.Edge, src *domain.ASTUnit, callee *domain.ASTUnit) *index.Hit {
	verb := callerKindVerb[e.Kind]
	if verb == "" {
		verb = "uses"
	}
	hit := &index.Hit{
		RepoID:   e.RepoID,
		FilePath: e.FilePath,
		Path:     e.FilePath,
		Line:     e.Line,
		EndLine:  e.Line,
		Reason:   verb + " " + callee.Name,
	}
	if src != nil {
		hit.Symbol = src.Name
		hit.Kind = src.Kind
		hit.Language = src.Language
	}
	hit.Snippet = callerSnippet(e, src, callee, verb)
	return hit
}

// callerSnippet describes a call site in words, because a hit with no snippet
// is judged on its file path alone by everything downstream — the reranker
// scores it against a query, and an LLM is handed it as context. The text is
// built from what the graph already knows (the caller's signature and doc,
// the relationship, the location) rather than by reading the file, so it
// costs nothing per hit.
func callerSnippet(e *domain.Edge, src, callee *domain.ASTUnit, verb string) string {
	var b strings.Builder
	if src != nil {
		writeUnitHead(&b, src)
		b.WriteByte('\n')
	}
	b.WriteString(verb)
	b.WriteByte(' ')
	b.WriteString(firstNonEmpty(callee.Qualified, callee.Name))
	fmt.Fprintf(&b, " at %s:%d", e.FilePath, e.Line)
	return b.String()
}

// firstDocLine returns the first non-empty line of a doc comment: the summary
// sentence is the part worth putting in front of a reranker.
func firstDocLine(doc string) string {
	for _, line := range strings.Split(doc, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// unitsByID loads the callers' units in one batch, for their names and kinds.
func (p *Promoter) unitsByID(ctx context.Context, ids []string) map[string]*domain.ASTUnit {
	out := map[string]*domain.ASTUnit{}
	if len(ids) == 0 {
		return out
	}
	units, err := p.store.GetASTUnitsByIDs(ctx, dedupStrings(ids))
	if err != nil {
		slog.Debug("callers intent: caller unit lookup failed", "error", err)
		return out
	}
	for _, u := range units {
		out[u.ID] = u
	}
	return out
}

// --- small helpers -----------------------------------------------------------

// identifierTokens picks the tokens of s that are shaped like code
// identifiers rather than prose: an internal case change ("checkSKU"), an
// underscore ("_render_filename"), or a digit stuck to letters. Plain
// lowercase words are prose and never qualify.
func identifierTokens(s string) []string {
	var out []string
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool {
		return !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	}) {
		if len(tok) >= 3 && identifierShaped(tok) {
			out = append(out, tok)
		}
	}
	return dedupStrings(out)
}

func identifierShaped(tok string) bool {
	if strings.Contains(tok, "_") {
		return true
	}
	hasLower, hasUpper, hasDigit := false, false, false
	caseChange := false
	prevLower := false
	for i, r := range tok {
		switch {
		case r >= 'a' && r <= 'z':
			hasLower = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
			if i > 0 && prevLower {
				caseChange = true
			}
		case r >= '0' && r <= '9':
			hasDigit = true
		}
		prevLower = r >= 'a' && r <= 'z'
	}
	if caseChange {
		return true
	}
	// "SKU42", "sha256": letters glued to digits read as identifiers.
	return hasDigit && (hasLower || hasUpper)
}

// nameDescribed reports whether every word component of a unit name appears
// in the query words — the query is actually talking about this unit, not
// merely landing in its file. Trivially short names prove nothing and are
// rejected.
func nameDescribed(name string, queryWords map[string]bool) bool {
	comps := graph.WordComponents(name)
	if len(comps) == 0 {
		return false
	}
	total := 0
	for _, c := range comps {
		if !queryWords[c] {
			return false
		}
		total += len(c)
	}
	return total >= 4
}

func wordSet(words []string) map[string]bool {
	out := make(map[string]bool, len(words))
	for _, w := range words {
		out[w] = true
	}
	return out
}

func repoAllowed(repos []string, repoID string) bool {
	if len(repos) == 0 {
		return true
	}
	for _, r := range repos {
		if r == repoID {
			return true
		}
	}
	return false
}

func overlapsAny(h *index.Hit, list []*index.Hit) bool {
	for _, p := range list {
		if p.Overlaps(h) {
			return true
		}
	}
	return false
}

func dedupStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// testishPath reports whether a path looks like test, mock or vendored code.
// The judgment itself lives in repos.IsTestPath, shared with the symbol-summary
// pass; this is the local spelling, kept because several functions here take a
// `repos []string` parameter that would shadow the package name.
func testishPath(path string) bool { return repos.IsTestPath(path) }
