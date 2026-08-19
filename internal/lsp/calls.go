package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// The call graph the tree-sitter pass produces records a callee's NAME, and
// the linker resolves that name to a unit. This is language-independent and
// approximate in exactly one way: when several definitions share a name the
// linker has to guess, and it says so by resolving at contract.ConfHeuristic.
// A language server does not guess — it answers from the same analysis the
// compiler does.
//
// CallRefiner spends that answer on the call edges that already exist rather
// than on a parallel edge kind: a reference site the server reports either
// confirms the edge at that line, moves it to the definition the server
// actually resolves, or — when the parser recorded no call there at all —
// becomes a new call edge. Symmetrically, an edge resolved to a definition the
// server does not reference from that line is a name-match that the evidence
// contradicts, and it stops claiming to be a call of it.
//
// What makes this affordable is the bound: not "every symbol" (a references
// request per function over a 40k-file repository is tens of thousands of
// requests) but the symbols where name resolution is demonstrably weak or
// where the answer is worth most — see selectCandidates.

// callEdgeSource marks the edges this pass resolved, so a reader (and a later
// diagnosis) can tell compiler-grade evidence from a name match.
const callEdgeSource = "lsp"

// metaKeySource duplicates the linker's annotation key: both packages record
// how an edge was resolved, and importing the linker here would be a cycle.
const metaKeySource = "source"

// Defaults for LSPCallsConfig.
const (
	defaultMaxSymbols       = 4000
	defaultMaxRefsPerSymbol = 200
)

// lineTolerance is how far a reference position may sit from the line the
// parser recorded for a call and still be the same call site. A chained or
// wrapped expression puts the callee identifier on a later line than the call
// node the parser attributed the edge to:
//
//	return customersServiceClient
//	    .getOwner(ownerId)      // the identifier the server points at
const lineTolerance = 2

// CallRefiner corrects a repository's call edges with language-server
// evidence. Unlike Refiner it is repository-scoped, not file-scoped: it runs
// once per repository after linking, on one language-server session per
// language, because a session costs a full workspace load (60-90 s for a large
// Go module) and a per-file session would pay that for every batch of files.
type CallRefiner struct {
	store   store.Storage
	cfg     *config.LSPConfig
	calls   *config.LSPCallsConfig
	mapper  Mapper
	timeout time.Duration
}

// NewCallRefiner creates a CallRefiner, or nil when the pass is not enabled.
func NewCallRefiner(st store.Storage, cfg *config.LSPConfig) *CallRefiner {
	if cfg == nil || !cfg.Enabled || cfg.Calls == nil || !cfg.Calls.Enabled {
		return nil
	}
	timeout := DefaultTimeout
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	return &CallRefiner{
		store:   st,
		cfg:     cfg,
		calls:   cfg.Calls,
		mapper:  NewMapper(cfg.HostRoot, cfg.MountRoot),
		timeout: timeout,
	}
}

// CallStats reports what one repository's pass cost and what it changed. The
// cost half is the point of the pass being bounded at all, so it is returned
// rather than only logged.
type CallStats struct {
	Candidates   int // definitions selected by the bound
	Asked        int // definitions a references request was actually issued for
	Requests     int // references requests issued (Asked plus retries)
	FilesOpened  int
	References   int // reference sites inside the repository
	Confirmed    int // edges already pointing at the definition the server names
	Repointed    int // edges moved to a different definition
	Added        int // call sites the parser had no edge for
	Contradicted int // resolutions dropped: the server does not reference them
	Truncated    bool
	Languages    []string
	Failed       []string // languages skipped because the server was unusable
	Duration     time.Duration
	LangSeconds  map[string]float64
}

// RefineRepo runs the pass over one repository. It is best-effort: an
// unreachable server, a language the workspace cannot load, a file that
// vanished — each is recorded and skipped rather than failing the index.
func (c *CallRefiner) RefineRepo(ctx context.Context, repoID, repoPath string) (*CallStats, error) {
	start := time.Now()
	stats := &CallStats{LangSeconds: map[string]float64{}}
	defer func() {
		stats.Duration = time.Since(start)
		lspCallPassSeconds.Observe(stats.Duration.Seconds())
	}()

	repo, err := c.loadRepo(ctx, repoID)
	if err != nil {
		return stats, err
	}
	candidates := c.selectCandidates(repo)
	stats.Candidates = len(candidates)
	if len(candidates) == 0 {
		return stats, nil
	}
	if max := c.maxSymbols(); len(candidates) > max {
		candidates = candidates[:max]
		stats.Truncated = true
		lspCallTruncated.Inc()
		slog.Warn("lsp: call pass hit its symbol budget; some ambiguous callees keep their name-matched resolution",
			"repo", repoID, "budget", max, "candidates", stats.Candidates)
	}

	byLang := map[string][]*domain.ASTUnit{}
	for _, u := range candidates {
		byLang[u.Language] = append(byLang[u.Language], u)
	}
	langs := sortedKeys(byLang)

	plan := newCallPlan()
	root := absPath(repoPath)
	for _, lang := range langs {
		langStart := time.Now()
		if err := c.refineLang(ctx, repo, root, lang, byLang[lang], plan, stats); err != nil {
			lspServerFailures.Inc()
			slog.Warn("lsp: call pass skipped a language", "repo", repoID, "language", lang, "error", err)
			stats.Failed = append(stats.Failed, lang)
			continue
		}
		stats.Languages = append(stats.Languages, lang)
		stats.LangSeconds[lang] = time.Since(langStart).Seconds()
	}
	if len(stats.Languages) == 0 {
		return stats, fmt.Errorf("lsp: no language server usable for %s (%s)", repoID, strings.Join(stats.Failed, ", "))
	}

	c.apply(ctx, repo, plan, stats)
	return stats, nil
}

// --- repository snapshot ------------------------------------------------------

// repoGraph is the slice of the stored graph this pass reasons over, read once
// per repository. The linker already loads the same two lists for the same
// repository, so this is a known-affordable read.
type repoGraph struct {
	id    string
	units []*domain.ASTUnit // function/method units
	byID  map[string]*domain.ASTUnit
	// byFile holds each file's function/method units, sorted by start line, so
	// the unit enclosing a reference site can be found without a query.
	byFile map[string][]*domain.ASTUnit
	// nameCount says how many definitions share a name: > 1 is exactly the
	// case the linker cannot resolve on evidence.
	nameCount map[string]int
	// called is the set of names some call edge names, so an ambiguous
	// definition nothing calls is not worth a request.
	called map[string]bool
	// boundary is the set of unit ids sitting on a contract boundary.
	boundary map[string]bool
	// calls indexes the repository's call edges by callee name.
	calls map[string][]*domain.Edge
	// byDst indexes them by resolved destination, for the contradiction check.
	byDst map[string][]*domain.Edge
	// langByFile is the language of a file, taken from its units.
	langByFile map[string]string
}

// boundaryKinds are the edge kinds whose endpoints are contract boundaries:
// the code that answers a route or an RPC, and the code that calls out to
// another service, publishes, consumes or touches a table. These are the
// symbols a cross-service question is most often about, and the ones whose
// callers are worth knowing exactly.
var boundaryKinds = []string{
	store.EdgeHandledBy, store.EdgeImplementsRPC,
	store.EdgeHTTPCall, store.EdgeRPCCall,
	store.EdgeProduces, store.EdgeConsumes,
	store.EdgeWritesTo, store.EdgeReadsFrom,
}

func (c *CallRefiner) loadRepo(ctx context.Context, repoID string) (*repoGraph, error) {
	units, err := c.store.GetASTUnits(ctx, domain.QueryOpts{
		RepoID: repoID, Kinds: []string{"function", "method"},
	})
	if err != nil {
		return nil, fmt.Errorf("lsp: load units of %s: %w", repoID, err)
	}
	callEdges, err := c.store.GetEdges(ctx, domain.QueryOpts{RepoID: repoID, Kind: store.EdgeCall})
	if err != nil {
		return nil, fmt.Errorf("lsp: load call edges of %s: %w", repoID, err)
	}
	boundaryEdges, err := c.store.GetEdges(ctx, domain.QueryOpts{RepoID: repoID, Kinds: boundaryKinds})
	if err != nil {
		return nil, fmt.Errorf("lsp: load contract edges of %s: %w", repoID, err)
	}

	g := &repoGraph{
		id:         repoID,
		units:      units,
		byID:       make(map[string]*domain.ASTUnit, len(units)),
		byFile:     map[string][]*domain.ASTUnit{},
		nameCount:  map[string]int{},
		called:     map[string]bool{},
		boundary:   map[string]bool{},
		calls:      map[string][]*domain.Edge{},
		byDst:      map[string][]*domain.Edge{},
		langByFile: map[string]string{},
	}
	for _, u := range units {
		g.byID[u.ID] = u
		g.byFile[u.FilePath] = append(g.byFile[u.FilePath], u)
		g.nameCount[u.Name]++
		if u.Language != "" {
			g.langByFile[u.FilePath] = u.Language
		}
	}
	for _, list := range g.byFile {
		sort.Slice(list, func(i, j int) bool {
			if list[i].StartLine != list[j].StartLine {
				return list[i].StartLine < list[j].StartLine
			}
			return beforeAtLine(list[i], list[j])
		})
	}
	for _, e := range callEdges {
		g.called[e.DstName] = true
		g.calls[e.DstName] = append(g.calls[e.DstName], e)
		if resolved(e.DstID) {
			g.byDst[e.DstID] = append(g.byDst[e.DstID], e)
		}
	}
	// handled_by points route -> handler, the outbound kinds point caller ->
	// contract, so the boundary symbol is the destination of the first and the
	// source of the rest.
	for _, e := range boundaryEdges {
		if e.Kind == store.EdgeHandledBy {
			if resolved(e.DstID) {
				g.boundary[e.DstID] = true
			}
			continue
		}
		if resolved(e.SrcID) {
			g.boundary[e.SrcID] = true
		}
	}
	return g, nil
}

func resolved(id string) bool { return id != "" && id != "0" }

// --- the bound ----------------------------------------------------------------

// selectCandidates picks the definitions worth a references request.
//
// Two disjoint reasons qualify a definition, and both are things the stored
// graph already knows:
//
//   - it sits on a contract boundary — a handler, an RPC implementation, an
//     outbound call/publish/consume/table access. These are few (4 007 over the
//     nine-repository evaluation corpus) and they are what cross-service
//     questions are about.
//   - its name is shared by another definition in the same repository *and*
//     some call edge names it. That is precisely where the linker guessed, and
//     an ambiguous name nothing calls costs a request for nothing.
//
// Candidates are ordered boundary-first and then by qualified name, so a
// truncated pass truncates the same way twice and spends its budget on the
// stronger reason first. Test scaffolding and vendored trees are excluded:
// they are never the answer to "what calls X" and they are a large share of a
// repository's symbols.
func (c *CallRefiner) selectCandidates(g *repoGraph) []*domain.ASTUnit {
	scope := c.scope()
	wantBoundary := scope == "boundary" || scope == "both"
	wantAmbiguous := scope == "ambiguous" || scope == "both"

	type scored struct {
		unit     *domain.ASTUnit
		boundary bool
	}
	var out []scored
	for _, u := range g.units {
		if _, ok := c.cfg.Servers[u.Language]; !ok {
			continue
		}
		if LanguageID(u.Language) == "" || excludedPath(u.FilePath) {
			continue
		}
		isBoundary := g.boundary[u.ID]
		isAmbiguous := g.nameCount[u.Name] > 1 && g.called[u.Name]
		switch {
		case wantBoundary && isBoundary:
		case wantAmbiguous && isAmbiguous:
		default:
			continue
		}
		out = append(out, scored{unit: u, boundary: isBoundary})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.boundary != b.boundary {
			return a.boundary
		}
		if a.unit.Qualified != b.unit.Qualified {
			return a.unit.Qualified < b.unit.Qualified
		}
		if a.unit.FilePath != b.unit.FilePath {
			return a.unit.FilePath < b.unit.FilePath
		}
		if a.unit.StartLine != b.unit.StartLine {
			return a.unit.StartLine < b.unit.StartLine
		}
		return beforeAtLine(a.unit, b.unit)
	})
	units := make([]*domain.ASTUnit, len(out))
	for i := range out {
		units[i] = out[i].unit
	}
	return units
}

// excludedMarkers are the path fragments that mark code no "what calls X"
// answer should be spent on. Kept in sync in spirit with the callers intent's
// own test-path check, which ranks such hits last rather than dropping them.
var excludedMarkers = []string{
	"_test.", ".test.", ".spec.", "/test/", "/tests/", "/testing/",
	"/testdata/", "/__tests__/", "/mocks/", "/mock/", "/fixtures/",
	"/vendor/", "/node_modules/",
}

func excludedPath(path string) bool {
	p := strings.ToLower(path)
	if strings.HasPrefix(p, "test/") || strings.HasPrefix(p, "tests/") {
		return true
	}
	for _, m := range excludedMarkers {
		if strings.Contains(p, m) {
			return true
		}
	}
	return false
}

func (c *CallRefiner) scope() string {
	if c.calls.Scope == "" {
		return "both"
	}
	return c.calls.Scope
}

func (c *CallRefiner) maxSymbols() int {
	if c.calls.MaxSymbols > 0 {
		return c.calls.MaxSymbols
	}
	return defaultMaxSymbols
}

func (c *CallRefiner) maxRefs() int {
	if c.calls.MaxRefsPerSymbol > 0 {
		return c.calls.MaxRefsPerSymbol
	}
	return defaultMaxRefsPerSymbol
}

// --- asking the server --------------------------------------------------------

// callPlan accumulates what the pass decided, so that every decision is taken
// before any of it is written. Two definitions can name the same call site
// (an overload pair matched within lineTolerance); deciding first and writing
// second keeps "confirmed by some definition" from being contradicted by
// another one purely because of the order they were asked in.
type callPlan struct {
	claim map[string]*domain.ASTUnit // edge id -> definition the server names
	add   []*domain.Edge             // call sites with no edge at all
	// complete is the set of definitions whose reference list came back whole
	// and non-empty; only those may contradict an existing resolution.
	complete map[string]*domain.ASTUnit
	// analysed is the set of files the server demonstrably resolved something
	// in. Nothing outside it may be contradicted (see apply).
	analysed map[string]bool
	seenAdd  map[string]bool // dedup key for added edges
}

func newCallPlan() *callPlan {
	return &callPlan{
		claim:    map[string]*domain.ASTUnit{},
		complete: map[string]*domain.ASTUnit{},
		analysed: map[string]bool{},
		seenAdd:  map[string]bool{},
	}
}

func (c *CallRefiner) refineLang(ctx context.Context, g *repoGraph, root, lang string,
	defs []*domain.ASTUnit, plan *callPlan, stats *CallStats) error {

	srv := c.cfg.Servers[lang]
	client, err := Dial(srv.Addr, c.timeout)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.Initialize(ctx, c.mapper.ToURI(root), srv.InitOptions); err != nil {
		return err
	}

	byFile := map[string][]*domain.ASTUnit{}
	var files []string
	for _, d := range defs {
		if _, ok := byFile[d.FilePath]; !ok {
			files = append(files, d.FilePath)
		}
		byFile[d.FilePath] = append(byFile[d.FilePath], d)
	}
	sort.Strings(files)

	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		content, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			slog.Debug("lsp: call pass cannot read a file", "repo", g.id, "file", rel, "error", err)
			continue
		}
		uri := c.mapper.ToURI(filepath.Join(root, rel))
		if err := client.DidOpen(uri, LanguageID(lang), string(content)); err != nil {
			return fmt.Errorf("didOpen %s: %w", rel, err)
		}
		stats.FilesOpened++
		lines := strings.Split(string(content), "\n")
		for _, def := range byFile[rel] {
			c.askDefinition(ctx, client, g, root, uri, lines, def, plan, stats)
		}
	}
	return nil
}

// askDefinition asks the server who references one definition and records the
// consequences in the plan.
func (c *CallRefiner) askDefinition(ctx context.Context, client *Client, g *repoGraph,
	root, uri string, lines []string, def *domain.ASTUnit, plan *callPlan, stats *CallStats) {

	line0, col := namePosition(lines, def)
	if col < 0 {
		return
	}
	stats.Asked++
	stats.Requests++
	lspCallRequests.Inc()
	locs, err := client.References(ctx, uri, line0, col)
	if err != nil {
		lspReferenceErrors.Inc()
		slog.Debug("lsp: references failed", "repo", g.id, "symbol", def.Qualified, "error", err)
		return
	}
	// A symbol with more call sites than the budget is not an answer to "what
	// calls X" — and rewriting every one of its edges costs more than the
	// answer is worth. Its resolutions are left exactly as the linker made
	// them, which is why it may not contradict anything either.
	if len(locs) > c.maxRefs() {
		slog.Debug("lsp: skipping a symbol with more references than the budget",
			"repo", g.id, "symbol", def.Qualified, "references", len(locs), "budget", c.maxRefs())
		return
	}
	// An empty answer is not the statement "nothing calls this". A server that
	// cannot resolve the symbol — an unloaded project, a generated declaration,
	// a position that landed on a type rather than a name — answers exactly the
	// same way, and reading that as "every recorded caller is wrong" deletes
	// correct resolutions wholesale. Measured on petclinic before this guard:
	// 44 of 144 resolved call edges dropped, including two correct ones for a
	// getter jdtls simply did not resolve.
	if len(locs) == 0 {
		return
	}
	plan.complete[def.ID] = def

	for _, loc := range locs {
		rel, ok := repoRelative(c.mapper, root, loc.URI)
		if !ok {
			continue
		}
		site := loc.Line + 1 // storage lines are 1-based
		stats.References++
		plan.analysed[rel] = true
		if e := matchCallEdge(g.calls[def.Name], rel, site); e != nil {
			if _, taken := plan.claim[e.ID]; !taken {
				plan.claim[e.ID] = def
			}
			continue
		}
		// No parsed call at this line: the extractor missed it (a call inside a
		// chained expression, a lambda, a generated wrapper). The server says
		// the enclosing code uses this definition, which is the fact the
		// question asks for.
		encl := enclosingIn(g.byFile[rel], site)
		if encl == nil || encl.ID == def.ID {
			continue
		}
		key := rel + "\x00" + strconv.Itoa(site) + "\x00" + def.ID
		if plan.seenAdd[key] {
			continue
		}
		plan.seenAdd[key] = true
		plan.add = append(plan.add, &domain.Edge{
			RepoID:     g.id,
			SrcID:      encl.ID,
			DstID:      def.ID,
			DstRepoID:  g.id,
			Kind:       store.EdgeCall,
			DstName:    def.Name,
			FilePath:   rel,
			Line:       site,
			Confidence: contract.ConfExact,
			Meta:       metaWithSource(""),
		})
	}
}

// repoRelative converts a reference URI to a repo-relative path, rejecting
// anything outside the repository: a language server answers over its whole
// workspace, which holds the module cache and — when several checkouts share
// one mount — other repositories.
func repoRelative(mapper Mapper, root, uri string) (string, bool) {
	rel, err := filepath.Rel(root, mapper.FromURI(uri))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// matchCallEdge finds the parsed call edge for a reference site: same file,
// and the line the parser recorded within lineTolerance of the identifier the
// server points at. The closest line wins, so two calls of the same name a
// line apart keep their own edges.
func matchCallEdge(edges []*domain.Edge, file string, line int) *domain.Edge {
	var best *domain.Edge
	bestDelta := lineTolerance + 1
	for _, e := range edges {
		if e.FilePath != file {
			continue
		}
		delta := e.Line - line
		if delta < 0 {
			delta = -delta
		}
		if delta > lineTolerance {
			continue
		}
		// Equidistant candidates are settled by where they are, not by which
		// row was written first: an edge id is the order the indexing
		// goroutines committed it in, and the winner here has its dst_id
		// rewritten, so an id tie-break puts that difference in the database.
		if delta < bestDelta || (delta == bestDelta && best != nil && beforeAtEdge(e, best)) {
			best, bestDelta = e, delta
		}
	}
	return best
}

// beforeAtLine and beforeAtEdge end the orderings above on something the corpus
// decides. Both used to end on the row's autoincrement id, which is the order
// indexes.workers goroutines happened to commit it in, so two passes over
// identical sources ordered the same rows differently — and this package
// truncates (defaultMaxSymbols) and rewrites (dst_id) on the strength of those
// orderings, which puts the difference in the database rather than in one
// response. Rows they still cannot separate are at the same place under the
// same name and refine to the same answer.
func beforeAtLine(a, b *domain.ASTUnit) bool {
	switch {
	case a.StartByte != b.StartByte:
		return a.StartByte < b.StartByte
	case a.Kind != b.Kind:
		return a.Kind < b.Kind
	default:
		return a.Name < b.Name
	}
}

func beforeAtEdge(a, b *domain.Edge) bool {
	switch {
	case a.Line != b.Line:
		return a.Line < b.Line
	case a.DstName != b.DstName:
		return a.DstName < b.DstName
	default:
		return a.Meta < b.Meta
	}
}

// enclosingIn returns the smallest function/method containing a 1-based line.
func enclosingIn(units []*domain.ASTUnit, line int) *domain.ASTUnit {
	var best *domain.ASTUnit
	for _, u := range units {
		if line < u.StartLine || line > u.EndLine {
			continue
		}
		if best == nil || u.EndLine-u.StartLine < best.EndLine-best.StartLine {
			best = u
		}
	}
	return best
}

// --- writing the result -------------------------------------------------------

// apply writes the plan: claimed edges are pointed at the definition the
// server named, missing call sites are inserted, and resolutions the evidence
// contradicts stop claiming to be calls of their target.
//
// Resolutions go through the store's batch API when it has one. One autocommit
// UPDATE per edge is what made a link pass as expensive as indexing (see
// graph.resolutionWriter); this pass rewrites the same order of magnitude of
// edges and would reintroduce it.
func (c *CallRefiner) apply(ctx context.Context, g *repoGraph, plan *callPlan, stats *CallStats) {
	w := newEdgeWriter(c.store)

	for _, id := range sortedKeys(plan.claim) {
		def := plan.claim[id]
		e := findEdge(g.calls[def.Name], id)
		if e == nil {
			continue
		}
		moved := e.DstID != def.ID
		if !moved && e.Confidence >= contract.ConfExact && metaField(e.Meta, metaKeySource) == callEdgeSource {
			stats.Confirmed++ // already ours from an earlier pass: nothing to write
			continue
		}
		w.resolve(ctx, e, def.ID, g.id, contract.ConfExact)
		if moved {
			stats.Repointed++
			lspCallRepointed.Inc()
		} else {
			stats.Confirmed++
			lspCallConfirmed.Inc()
		}
	}

	// Contradictions. A definition whose reference list came back whole knows
	// every place it is used; an edge resolved to it from anywhere else is a
	// name match the server denies.
	//
	// Two guards keep "the server said nothing about it" from being read as
	// "the server denies it". The definition's language must match the file's,
	// so a call recorded in a language this server never analysed is outside
	// its knowledge rather than contradicted by it; and the file must be one
	// the server demonstrably resolved something in during this pass, so a
	// module it failed to load cannot silently unresolve the whole graph.
	for _, defID := range sortedKeys(plan.complete) {
		def := plan.complete[defID]
		for _, e := range g.byDst[defID] {
			if _, claimed := plan.claim[e.ID]; claimed {
				continue
			}
			if g.langByFile[e.FilePath] != def.Language || !plan.analysed[e.FilePath] {
				continue
			}
			w.resolve(ctx, e, "", "", contract.ConfWeak)
			stats.Contradicted++
			lspCallContradicted.Inc()
		}
	}
	w.flush(ctx)

	if len(plan.add) > 0 {
		if err := c.store.BatchStoreEdges(ctx, plan.add); err != nil {
			slog.Warn("lsp: store call edges the parser missed", "repo", g.id, "edges", len(plan.add), "error", err)
		} else {
			stats.Added = len(plan.add)
			lspCallAdded.Add(float64(len(plan.add)))
		}
	}
}

// edgeWriteBuffer is how many resolutions are queued before they are handed to
// the store, matching the linker's own batch size.
const edgeWriteBuffer = 1000

// edgeWriter applies resolution+annotation pairs, batching the resolutions
// when the backend supports it. The annotation is written per edge — there is
// no batch API for meta — but only for edges that actually change.
type edgeWriter struct {
	store   store.Storage
	batcher store.EdgeResolutionBatcher
	buf     []store.EdgeResolution
	metas   []*domain.Edge
}

func newEdgeWriter(st store.Storage) *edgeWriter {
	w := &edgeWriter{store: st}
	w.batcher, _ = st.(store.EdgeResolutionBatcher)
	return w
}

func (w *edgeWriter) resolve(ctx context.Context, e *domain.Edge, dstID, dstRepoID string, conf float32) {
	w.buf = append(w.buf, store.EdgeResolution{
		EdgeID: e.ID, DstID: dstID, DstRepoID: dstRepoID, Confidence: conf,
	})
	w.metas = append(w.metas, e)
	if len(w.buf) >= edgeWriteBuffer {
		w.flush(ctx)
	}
}

func (w *edgeWriter) flush(ctx context.Context) {
	if len(w.buf) == 0 {
		return
	}
	failed := map[int]bool{}
	if w.batcher != nil {
		failures, err := w.batcher.BatchUpdateEdgeResolutions(ctx, w.buf)
		if err != nil {
			slog.Warn("lsp: apply call-edge resolutions", "edges", len(w.buf), "error", err)
			w.buf, w.metas = w.buf[:0], w.metas[:0]
			return
		}
		for _, f := range failures {
			failed[f.Index] = true
			slog.Warn("lsp: apply call-edge resolution", "edge", f.EdgeID, "error", f.Err)
		}
	} else {
		for i, r := range w.buf {
			if err := w.store.UpdateEdgeResolution(ctx, r.EdgeID, r.DstID, r.DstRepoID, r.Confidence); err != nil {
				failed[i] = true
				slog.Warn("lsp: apply call-edge resolution", "edge", r.EdgeID, "error", err)
			}
		}
	}
	// The annotation only makes sense once the resolution it describes landed.
	for i, e := range w.metas {
		if failed[i] {
			continue
		}
		if err := w.store.UpdateEdgeMeta(ctx, e.ID, metaWithSource(e.Meta)); err != nil {
			slog.Warn("lsp: mark language-server resolved call edge", "edge", e.ID, "error", err)
		}
	}
	w.buf, w.metas = w.buf[:0], w.metas[:0]
}

func findEdge(edges []*domain.Edge, id string) *domain.Edge {
	for _, e := range edges {
		if e.ID == id {
			return e
		}
	}
	return nil
}

// metaWithSource records that this edge's destination came from a language
// server rather than from a name match. Like the linker's own annotation it is
// a map-level merge: the parser's call arguments and field mappings stay.
func metaWithSource(meta string) string {
	m := map[string]any{}
	if meta != "" {
		if err := json.Unmarshal([]byte(meta), &m); err != nil {
			m = map[string]any{}
		}
	}
	m[metaKeySource] = callEdgeSource
	b, err := json.Marshal(m)
	if err != nil {
		return meta
	}
	return string(b)
}

// metaField reads one string key out of an edge meta, "" when absent.
func metaField(meta, key string) string {
	if meta == "" {
		return ""
	}
	m := map[string]any{}
	if err := json.Unmarshal([]byte(meta), &m); err != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// Log renders the pass's cost and effect as one line's worth of key/values.
func (s *CallStats) Log() []any {
	return []any{
		"candidates", s.Candidates, "asked", s.Asked, "files", s.FilesOpened,
		"references", s.References, "confirmed", s.Confirmed, "repointed", s.Repointed,
		"added", s.Added, "contradicted", s.Contradicted, "truncated", s.Truncated,
		"languages", strings.Join(s.Languages, ","), "failed", strings.Join(s.Failed, ","),
		"seconds", int(s.Duration.Seconds()),
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
