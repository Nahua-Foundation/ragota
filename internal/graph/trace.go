package graph

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// TraceRequest asks where a parameter of a function/method flows.
type TraceRequest struct {
	RepoID   string `json:"repo_id"`
	Symbol   string `json:"symbol"` // function/method name or qualified name
	Param    string `json:"param"`  // parameter (or field) to follow
	MaxDepth int    `json:"max_depth,omitempty"`
}

// TraceStep is one hop of a parameter flow chain.
type TraceStep struct {
	Unit       *storage.ASTUnit `json:"unit"`
	Service    string           `json:"service,omitempty"`
	Tracked    []string         `json:"tracked"`        // identifiers/fields followed at this step
	Via        string           `json:"via,omitempty"`  // edge kind that led here
	Note       string           `json:"note,omitempty"` // human-readable explanation
	Line       int              `json:"line,omitempty"` // call/edge site line in the previous unit
	Confidence float32          `json:"confidence"`
}

// TraceResult is the outcome of a parameter trace.
type TraceResult struct {
	Param        string         `json:"param"`
	Steps        []*TraceStep   `json:"steps"`                  // the best chain (most boundary crossings, then longest)
	Alternatives [][]*TraceStep `json:"alternatives,omitempty"` // other complete chains (branches: Kafka + DB, etc.)
	Chains       int            `json:"chains"`                 // total complete chains discovered
}

// maxAlternatives caps the number of alternative chains returned.
const maxAlternatives = 4

// Trace follows a parameter through call edges and service contracts
// (gRPC, HTTP, Kafka). Matching is heuristic — identifiers are compared
// normalized (case- and underscore-insensitive), and each hop multiplies
// the edge confidence into the step confidence.
func (g *Graph) Trace(ctx context.Context, req *TraceRequest) (*TraceResult, error) {
	unit, err := g.FindUnit(ctx, req.RepoID, req.Symbol)
	if err != nil {
		return nil, err
	}
	maxDepth := req.MaxDepth
	if maxDepth <= 0 || maxDepth > 24 {
		maxDepth = 16
	}

	ld := newLoader(g)
	start := &TraceStep{
		Unit:       unit,
		Service:    ld.serviceForUnit(ctx, unit),
		Tracked:    []string{req.Param},
		Note:       "start: parameter " + req.Param,
		Confidence: 1.0,
	}

	tr := &tracer{
		g: g, ctx: ctx, ld: ld, maxDepth: maxDepth,
		onPath: map[string]bool{},
		seen:   map[string]bool{},
		edges:  map[string][]*storage.Edge{},
	}
	tr.dfs(unit, []string{req.Param}, []*TraceStep{start}, 1.0)
	if tr.err != nil {
		// An edge query failed mid-walk: the chains found are missing whatever
		// lay past the failure, so report the failure instead of a partial walk
		// that reads as complete.
		return nil, fmt.Errorf("trace: %w", tr.err)
	}

	sort.SliceStable(tr.completed, func(i, j int) bool {
		return betterTraceChain(tr.completed[i], tr.completed[j])
	})
	res := &TraceResult{Param: req.Param, Chains: tr.discovered}
	if len(tr.completed) == 0 {
		res.Steps = []*TraceStep{start}
		return res, nil
	}
	res.Steps = tr.completed[0].steps
	for _, chain := range tr.completed[1:] {
		if len(res.Alternatives) >= maxAlternatives {
			break
		}
		if isPrefixChain(chain.steps, res.Steps) {
			continue // pure prefix of the best chain — no new information
		}
		res.Alternatives = append(res.Alternatives, chain.steps)
	}
	return res, nil
}

// isPrefixChain reports whether chain a is a prefix of chain b. Steps match on
// unit and tracked identifiers: two walks over the same units carrying the
// value in different parameters are different findings, not a prefix.
func isPrefixChain(a, b []*TraceStep) bool {
	if len(a) > len(b) {
		return false
	}
	for i := range a {
		if a[i].Unit.ID != b[i].Unit.ID {
			return false
		}
		if strings.Join(normAll(a[i].Tracked), ",") != strings.Join(normAll(b[i].Tracked), ",") {
			return false
		}
	}
	return true
}

// maxExpansions bounds the total number of node expansions in one trace,
// keeping highly branched graphs from exploding exponentially.
const maxExpansions = 5000

type tracer struct {
	g          *Graph
	ctx        context.Context
	ld         *loader // batches and memoizes unit and service lookups
	maxDepth   int
	expansions int                        // node expansions performed so far (global budget)
	onPath     map[string]bool            // unit+tracked keys along the CURRENT path (cycle detection)
	seen       map[string]bool            // keys of recorded chains (dedup)
	edges      map[string][]*storage.Edge // memoized edge queries, by direction+kind+unit
	discovered int                        // distinct chains found, including evicted ones
	completed  []traceChain
	err        error // first storage error hit while querying edges, if any
}

// traceChain is a recorded walk and how it ended: a truncated chain stopped at
// the depth or expansion limit rather than at a real sink, so it says nothing
// about where the value ends up.
type traceChain struct {
	steps     []*TraceStep
	truncated bool
}

// hop is a candidate transition out of a unit.
type hop struct {
	edge    *storage.Edge
	unit    *storage.ASTUnit
	tracked []string
	note    string
	factor  float32 // confidence multiplier for this hop
}

func (t *tracer) dfs(unit *storage.ASTUnit, tracked []string, path []*TraceStep, conf float32) {
	if len(path) > t.maxDepth {
		t.finish(path, noteDepthLimit)
		return
	}
	key := unit.ID + "|" + strings.Join(normAll(tracked), ",")
	if t.onPath[key] {
		t.finish(path, "") // cycle along the current path: a real end
		return
	}
	if t.expansions >= maxExpansions {
		t.finish(path, noteBudgetLimit)
		return
	}
	t.expansions++

	hops := t.expand(unit, tracked)
	if len(hops) == 0 {
		t.finish(path, "")
		return
	}
	t.onPath[key] = true
	defer delete(t.onPath, key)
	for _, h := range hops {
		nextConf := conf * h.edge.Confidence * h.factor
		step := &TraceStep{
			Unit:       h.unit,
			Service:    t.ld.serviceForUnit(t.ctx, h.unit),
			Tracked:    h.tracked,
			Via:        h.edge.Kind,
			Note:       h.note,
			Line:       h.edge.Line,
			Confidence: nextConf,
		}
		t.dfs(h.unit, h.tracked, append(append([]*TraceStep{}, path...), step), nextConf)
	}
}

// maxCompletedChains bounds memory on highly branched graphs.
const maxCompletedChains = 64

// Notes marking a walk that stopped at a limit rather than at a sink.
const (
	noteDepthLimit  = "truncated: depth limit reached"
	noteBudgetLimit = "truncated: expansion budget exhausted"
)

// finish records a chain, deduplicating on its unit and tracked-identifier
// sequence: two walks over the same units following different identifiers are
// different findings. A non-empty limit marks the chain as truncated.
//
// At maxCompletedChains the best chains are kept rather than the first ones
// found, so a highly branched graph cannot bury the answer behind 64 walks
// that happened to be discovered in storage-id order.
func (t *tracer) finish(path []*TraceStep, limit string) {
	if len(path) <= 1 {
		return
	}
	key := chainKey(path)
	if t.seen[key] {
		return
	}
	t.seen[key] = true
	t.discovered++

	chain := traceChain{steps: path, truncated: limit != ""}
	if limit != "" {
		chain.steps = withNote(path, limit)
	}
	if len(t.completed) < maxCompletedChains {
		t.completed = append(t.completed, chain)
		return
	}
	worst := 0
	for i := 1; i < len(t.completed); i++ {
		if betterTraceChain(t.completed[worst], t.completed[i]) {
			worst = i
		}
	}
	if betterTraceChain(chain, t.completed[worst]) {
		t.completed[worst] = chain
	}
}

// chainKey identifies a chain by its units and the identifiers tracked at each
// of them.
func chainKey(path []*TraceStep) string {
	var b strings.Builder
	for i, s := range path {
		if i > 0 {
			b.WriteByte('>')
		}
		b.WriteString(s.Unit.ID)
		b.WriteByte('|')
		b.WriteString(strings.Join(normAll(s.Tracked), ","))
	}
	return b.String()
}

// withNote returns the chain with note appended to its last step. The step is
// copied: step values are shared with the chains that pass through them.
func withNote(path []*TraceStep, note string) []*TraceStep {
	out := append([]*TraceStep{}, path...)
	last := *out[len(out)-1]
	if last.Note == "" {
		last.Note = note
	} else {
		last.Note += " (" + note + ")"
	}
	out[len(out)-1] = &last
	return out
}

// betterTraceChain prefers a chain that reached a sink over one cut off at a
// limit — a truncated walk must not win on length alone — and otherwise ranks
// as betterChain does.
func betterTraceChain(a, b traceChain) bool {
	if a.truncated != b.truncated {
		return !a.truncated
	}
	return betterChain(a.steps, b.steps)
}

// betterChain prefers more service boundary crossings, then longer chains.
func betterChain(a, b []*TraceStep) bool {
	if b == nil {
		return true
	}
	ac, bc := crossings(a), crossings(b)
	if ac != bc {
		return ac > bc
	}
	return len(a) > len(b)
}

func crossings(path []*TraceStep) int {
	n := 0
	for _, s := range path {
		switch s.Via {
		case storage.EdgeRPCCall, storage.EdgeHTTPCall, storage.EdgeKafkaFlow:
			n++
		}
	}
	return n
}

// expand computes the hops out of a unit for the tracked identifiers.
func (t *tracer) expand(unit *storage.ASTUnit, tracked []string) []*hop {
	var hops []*hop

	// Contract nodes expand structurally, without argument matching.
	switch unit.Kind {
	case storage.KindRPCMethod:
		for _, e := range t.reverseEdges(unit.ID, storage.EdgeImplementsRPC) {
			if u := t.unitByID(e.SrcID); u != nil {
				hops = append(hops, &hop{
					edge: e, unit: u, tracked: tracked, factor: 1.0,
					note: "server implementation of " + unit.Qualified,
				})
			}
		}
		return hops
	case storage.KindHTTPRoute:
		for _, e := range t.outEdges(unit.ID, storage.EdgeHandledBy) {
			if e.DstID == "" {
				continue
			}
			if u := t.unitByID(e.DstID); u != nil {
				hops = append(hops, &hop{
					edge: e, unit: u, tracked: tracked, factor: 1.0,
					note: "handler for " + unit.Qualified,
				})
			}
		}
		return hops
	case storage.KindDBTable:
		return t.tableReaders(unit, tracked)
	}

	for _, e := range t.outEdges(unit.ID, "") {
		meta := storage.DecodeEdgeMeta(e.Meta)
		switch e.Kind {
		case storage.EdgeCall:
			if e.DstID == "" {
				continue
			}
			argIdx := matchingArgAliased(meta.Args, tracked, meta.Aliases)
			if argIdx < 0 {
				continue
			}
			callee := t.unitByID(e.DstID)
			if callee == nil {
				continue
			}
			next := tracked
			params := contract.ParamNames(callee.Language, callee.Signature)
			if argIdx < len(params) {
				next = []string{params[argIdx]}
			}
			hops = append(hops, &hop{
				edge: e, unit: callee, tracked: next, factor: 1.0,
				note: "passed as argument " + meta.Args[argIdx] + " to " + callee.Name,
			})

		case storage.EdgeRPCCall, storage.EdgeHTTPCall:
			if e.DstID == "" {
				continue
			}
			fields := matchingFieldsAliased(meta.Fields, tracked, meta.Aliases)
			factor := float32(1.0)
			if len(fields) == 0 {
				if matchingArgAliased(meta.Args, tracked, meta.Aliases) < 0 {
					continue
				}
				fields, factor = tracked, contract.ConfHeuristic // matched an argument, field unknown
			}
			dst := t.unitByID(e.DstID)
			if dst == nil {
				continue
			}
			note := "sent via gRPC field " + strings.Join(fields, ", ")
			if e.Kind == storage.EdgeHTTPCall {
				note = "sent via HTTP " + meta.Method + " " + meta.Path + " field " + strings.Join(fields, ", ")
			}
			hops = append(hops, &hop{edge: e, unit: dst, tracked: fields, factor: factor, note: note})

		case storage.EdgeKafkaFlow:
			if e.DstID == "" {
				continue
			}
			fields := matchingFieldsAliased(meta.Fields, tracked, meta.Aliases)
			factor := float32(1.0)
			if len(fields) == 0 {
				fields, factor = tracked, contract.ConfWeak // payload fields unknown; assume passthrough
			}
			dst := t.unitByID(e.DstID)
			if dst == nil {
				continue
			}
			topic := contract.TrimKind(e.DstName, contract.KindTopic)
			hops = append(hops, &hop{
				edge: e, unit: dst, tracked: fields, factor: factor,
				note: "published to Kafka topic " + topic + " as " + strings.Join(fields, ", "),
			})

		case storage.EdgeWritesTo:
			if e.DstID == "" {
				continue
			}
			// Sink: the tracked value is written into a table column.
			columns := matchingFieldsAliased(meta.Fields, tracked, meta.Aliases)
			factor := float32(1.0)
			if len(columns) == 0 {
				if matchingArgAliased(meta.Args, tracked, meta.Aliases) < 0 {
					continue
				}
				columns, factor = tracked, contract.ConfWeak
			}
			dst := t.unitByID(e.DstID)
			if dst == nil {
				continue
			}
			table := contract.TrimKind(e.DstName, contract.KindDB)
			hops = append(hops, &hop{
				edge: e, unit: dst, tracked: columns, factor: factor,
				note: "written to table " + table + " column " + strings.Join(columns, ", "),
			})
		}
	}
	return hops
}

// tableReaders continues a trace through the database: from the table a value
// was written to, back out to every unit that reads that table. Like the
// rpc/http contract nodes the hop is structural — a reads_from edge records no
// column-level mapping, only the statement it came from — so the reader is
// followed whatever it selects.
//
// The tracked identifiers are carried unchanged: they are the column names the
// writes_to hop matched, and a reader materializes the value under a name
// derived from the column (user_id -> UserID), which the normalized comparison
// already accepts. Widening them to the whole row would make every identifier
// in the reading unit a match, and narrowing them to a column named by the
// reads_from edge is not possible: the edge carries the statement text, and a
// SELECT * names no column at all.
//
// Readers are deduplicated by unit: a unit querying the same table several
// times is one hop, not one per statement, so a hot table costs one branch per
// reader. The global expansion budget and the chain dedup bound the rest.
func (t *tracer) tableReaders(unit *storage.ASTUnit, tracked []string) []*hop {
	table := contract.TrimKind(unit.Qualified, contract.KindDB)
	var hops []*hop
	seen := map[string]bool{}
	for _, e := range t.reverseEdges(unit.ID, storage.EdgeReadsFrom) {
		if e.SrcID == "" || seen[e.SrcID] {
			continue
		}
		u := t.unitByID(e.SrcID)
		if u == nil {
			continue
		}
		seen[e.SrcID] = true
		hops = append(hops, &hop{
			edge: e, unit: u, tracked: tracked, factor: contract.ConfWeak,
			note: "read from table " + table + " column " + strings.Join(tracked, ", "),
		})
	}
	return hops
}

func (t *tracer) outEdges(unitID, kind string) []*storage.Edge {
	return t.edgesOf("out|"+kind+"|"+unitID, storage.QueryOpts{SrcID: unitID, Kind: kind})
}

func (t *tracer) reverseEdges(unitID, kind string) []*storage.Edge {
	return t.edgesOf("in|"+kind+"|"+unitID, storage.QueryOpts{DstID: unitID, Kind: kind})
}

// edgesOf runs one edge query, memoized: a DFS revisits the same unit along
// every path that reaches it, and the graph does not change during a trace.
// The units on the far side are batch-loaded into the loader in one go.
func (t *tracer) edgesOf(key string, opts storage.QueryOpts) []*storage.Edge {
	if edges, ok := t.edges[key]; ok {
		return edges
	}
	edges, err := t.g.store.GetEdges(t.ctx, opts)
	if err != nil {
		// A storage failure here would otherwise look like "no edges", ending
		// the chain at a false sink. Record the first error and log it; Trace
		// surfaces it rather than returning a confidently wrong partial trace.
		if t.err == nil {
			t.err = err
		}
		slog.Warn("trace edge query", "key", key, "err", err)
		edges = nil
	}
	ids := make([]string, 0, len(edges))
	for _, e := range edges {
		if opts.SrcID != "" {
			ids = append(ids, e.DstID)
		} else {
			ids = append(ids, e.SrcID)
		}
	}
	if err := t.ld.unitsByIDs(t.ctx, ids); err != nil {
		return edges
	}
	t.edges[key] = edges
	return edges
}

func (t *tracer) unitByID(id string) *storage.ASTUnit {
	u, err := t.ld.unit(t.ctx, id)
	if err != nil {
		return nil
	}
	return u
}

// matchingArg returns the index of the first argument expression that
// references any tracked identifier, or -1.
func matchingArg(args []string, tracked []string) int {
	return matchingArgAliased(args, tracked, nil)
}

// matchingArgAliased is matchingArg with local alias resolution (edge meta).
func matchingArgAliased(args []string, tracked []string, aliases map[string]string) int {
	for i, a := range args {
		if exprMatchesAliased(a, tracked, aliases) {
			return i
		}
	}
	return -1
}

// matchingFields returns the field names whose value expressions reference a
// tracked identifier.
func matchingFields(fields map[string]string, tracked []string) []string {
	return matchingFieldsAliased(fields, tracked, nil)
}

// matchingFieldsAliased is matchingFields with local alias resolution.
func matchingFieldsAliased(fields map[string]string, tracked []string, aliases map[string]string) []string {
	if len(fields) == 0 {
		return nil
	}
	var out []string
	for k, v := range fields {
		if exprMatchesAliased(v, tracked, aliases) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// exprMatches reports whether expr references any tracked identifier.
// Comparison is normalized: case-insensitive, underscores stripped, so
// req.GetUserId() matches user_id.
func exprMatches(expr string, tracked []string) bool {
	return exprMatchesAliased(expr, tracked, nil)
}

// Alias-chain limits for exprMatchesAliased: maxAliasDepth bounds how many
// dereference levels a chain may span (x -> y -> z -> ...), maxAliasTokens
// bounds the total number of tokens considered across the whole walk.
const (
	maxAliasDepth  = 4
	maxAliasTokens = 32
)

// exprMatchesAliased is exprMatches extended with local aliases from the edge
// meta: when the expression itself does not match, identifier tokens of the
// expression are dereferenced through the alias map transitively (up to
// maxAliasDepth levels, cycle-safe) and each alias's source expression is
// matched against the tracked identifiers. So `y := userID; x := y; foo(x)`
// matches tracked "user_id" via aliases["x"]="y", aliases["y"]="userID".
//
// The walk is iterative (a per-level worklist of unvisited tokens), never
// recursive, and gives up after maxAliasTokens tokens in total.
func exprMatchesAliased(expr string, tracked []string, aliases map[string]string) bool {
	if exprMatchesDirect(expr, tracked) {
		return true
	}
	if len(aliases) == 0 {
		return false
	}
	visited := map[string]bool{}
	considered := 0
	frontier := identTokens(expr)
	for depth := 0; depth < maxAliasDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, tok := range frontier {
			if visited[tok] {
				continue // cycle (x -> y -> x) or shared token
			}
			visited[tok] = true
			considered++
			if considered > maxAliasTokens {
				return false // token budget exhausted
			}
			src, ok := aliases[tok]
			if !ok {
				continue
			}
			if exprMatchesDirect(src, tracked) {
				return true
			}
			next = append(next, identTokens(src)...)
		}
		frontier = next
	}
	return false
}

// minLooseComponent is the shortest word component allowed to match inside a
// larger identifier: "id" must fill a token of its own, or every getUserId()
// in the file would answer a trace of "id".
const minLooseComponent = 3

// exprMatchesDirect is the normalized comparison without alias dereferencing.
// The tracked identifier must align with whole word components of the
// expression and end at a token boundary: "user_id" matches userId,
// req.GetUserId() and order.UserID, while "user" matches neither "username"
// nor "user_agent".
func exprMatchesDirect(expr string, tracked []string) bool {
	toks := tokenComponents(expr)
	if len(toks) == 0 {
		return false
	}
	for _, t := range tracked {
		want := wordComponents(t)
		if len(want) == 0 {
			continue
		}
		whole := len(want) == 1 && len(want[0]) < minLooseComponent
		for _, comps := range toks {
			if whole {
				if len(comps) == 1 && comps[0] == want[0] {
					return true
				}
				continue
			}
			if hasComponentSuffix(comps, want) {
				return true
			}
		}
	}
	return false
}

func normAll(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, s := range ids {
		out = append(out, contract.NormIdent(s))
	}
	sort.Strings(out)
	return out
}
