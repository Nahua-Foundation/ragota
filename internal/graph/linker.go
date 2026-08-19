// Package graph builds and queries the cross-repository code graph:
// edge resolution (linking), traversal, service aggregation and
// parameter-flow tracing.
package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// Linker resolves edge destinations after index.
//
// It runs in two scopes:
//   - local: call / handled_by edges are resolved to units of the same repo;
//   - global: contract edges (rpc_call, implements_rpc, http_call) are matched
//     against contract units of ALL repos, and produces/consumes pairs are
//     joined into derived kafka_flow edges.
//
// Contract linking is recomputed on every run so that reindexing one repo
// re-points edges from all other repos. Already-valid resolutions (dst_id
// pointing at a still-existing contract unit) are kept as-is to avoid
// rewrite churn.
type Linker struct {
	store store.Storage

	// disambig, when set, is consulted for ambiguous contract matches: it
	// receives a prompt listing the candidates and returns the zero-based
	// index of the chosen one (ok=false keeps the heuristic result).
	disambig func(ctx context.Context, prompt string) (choice int, ok bool)
}

// SetDisambiguator installs an optional LLM-backed chooser for ambiguous
// contract edges (see Linker.disambig). Passing nil disables it.
func (l *Linker) SetDisambiguator(fn func(ctx context.Context, prompt string) (int, bool)) {
	l.disambig = fn
}

// NewLinker creates a Linker.
func NewLinker(store store.Storage) *Linker {
	return &Linker{store: store}
}

// RunStats aggregates counters from a single linking run.
type RunStats struct {
	ResolvedLocal     int // call/handled_by edges resolved within the repo
	ResolvedContracts int // contract edges (re)pointed at contract units
	SkippedValid      int // contract edges left alone: already validly resolved
	KafkaFlows        int // derived kafka_flow edges stored
	Errors            int // per-edge store/update failures (do not fail the run)
}

// Run links edges after repoID has been (re)indexed. It is a compatibility
// wrapper around RunWithStats that discards the statistics.
func (l *Linker) Run(ctx context.Context, repoID string) error {
	_, err := l.RunWithStats(ctx, repoID)
	return err
}

// RunWithStats links edges after repoID has been (re)indexed and returns
// run counters. Per-edge update failures are logged and counted in
// stats.Errors but do not fail the run.
func (l *Linker) RunWithStats(ctx context.Context, repoID string) (*RunStats, error) {
	stats := &RunStats{}
	if err := l.resolveLocal(ctx, repoID, stats); err != nil {
		return stats, fmt.Errorf("resolve local edges: %w", err)
	}
	rewritten, err := l.resolveConfigRefs(ctx, stats)
	if err != nil {
		return stats, fmt.Errorf("resolve config refs: %w", err)
	}
	if err := l.resolveContracts(ctx, stats); err != nil {
		return stats, fmt.Errorf("resolve contract edges: %w", err)
	}
	if err := l.deriveKafkaFlows(ctx, repoID, rewritten, stats); err != nil {
		return stats, fmt.Errorf("derive kafka flows: %w", err)
	}
	slog.Info("linker run",
		"repo_id", repoID,
		"resolved_local", stats.ResolvedLocal,
		"resolved_contracts", stats.ResolvedContracts,
		"skipped_valid", stats.SkippedValid,
		"kafka_flows", stats.KafkaFlows,
		"errors", stats.Errors)
	return stats, nil
}

// resolveConfigRefs rewrites topic references like "topic:${ORDERS_TOPIC}"
// using config_key units indexed from yaml/properties/env files, falling back
// to the default of a "${KEY:default}" placeholder that the parser recorded in
// the edge meta.
//
// The reference itself is preserved in the edge meta (metaKeyTopicRef), so an
// already-rewritten edge is re-checked on every run and follows a later config
// value change. It returns the set of topic keys it touched — both the
// previous and the new key of every rewritten edge — which deriveKafkaFlows
// must rebuild even when the config value lives in another repository.
func (l *Linker) resolveConfigRefs(ctx context.Context, stats *RunStats) (map[string]struct{}, error) {
	edges, err := l.store.GetEdges(ctx, domain.QueryOpts{
		Kinds: []string{store.EdgeProduces, store.EdgeConsumes},
	})
	if err != nil {
		return nil, err
	}
	type refEdge struct {
		edge *domain.Edge
		key  edgeKey // group the edge currently belongs to
		ref  string
	}
	var refs []refEdge
	groups := map[edgeKey][]*domain.Edge{}
	for _, e := range edges {
		key := edgeKey{e.Kind, e.DstName}
		groups[key] = append(groups[key], e)
		ref, ok := contract.ParseTopicRef(e.DstName)
		if !ok {
			ref = metaField(e.Meta, metaKeyTopicRef) // resolved earlier
		}
		if ref != "" {
			refs = append(refs, refEdge{edge: e, key: key, ref: ref})
		}
	}
	if len(refs) == 0 {
		return nil, nil
	}

	keys, err := l.store.GetASTUnits(ctx, domain.QueryOpts{Kind: store.KindConfigKey})
	if err != nil {
		return nil, err
	}
	idx := buildConfigIndex(keys)

	touched := map[string]struct{}{}
	dirty := map[edgeKey]bool{}
	for _, r := range refs {
		value := matchConfigKeyIndexed(idx, r.ref, r.edge.RepoID)
		if value == "" {
			value = metaDefaultTopic(r.edge.Meta)
		}
		if value == "" {
			continue
		}
		name := contract.Topic(value)
		if name == r.edge.DstName && metaField(r.edge.Meta, metaKeyTopicRef) == r.ref {
			continue
		}
		// The topic_ref annotation only ever accompanies a new join key: it
		// records the reference the key was resolved from, and an edge whose
		// key does not move is skipped above. So this path always needs the
		// group rewrite, never an in-place meta update.
		touched[r.key.dstName] = struct{}{}
		touched[name] = struct{}{}
		dirty[r.key] = true
		r.edge.DstName = name
		r.edge.Meta = metaWithField(r.edge.Meta, metaKeyTopicRef, r.ref)
	}
	rewrite := make(map[edgeKey][]*domain.Edge, len(dirty))
	for key := range dirty {
		rewrite[key] = groups[key]
	}
	l.rewriteEdgeGroups(ctx, rewrite, stats)
	return touched, nil
}

// Config key match tiers, weakest first. A full-path match always outranks a
// leaf-only one, whatever repository the key comes from.
const (
	configNoMatch   = 0
	configLeafMatch = 1
	configPathMatch = 2
)

// minLeafComponents is the shortest leaf name that may answer a reference on
// its own: a one-word key (kafka.topic) is too generic to claim ORDERS_TOPIC.
const minLeafComponents = 2

// indexedConfigKey is one config_key unit with its path and leaf split into
// components once, rather than once per reference matched against it.
type indexedConfigKey struct {
	unit *domain.ASTUnit
	path []string // components of the key path (qualified minus the kind prefix)
	leaf []string // components of the leaf name
}

// configIndex buckets config_key units by their trailing component.
//
// Both match tiers align at that component: a path match needs the key path to
// end with the reference, a leaf match needs the reference to end with the leaf,
// so either way the last component of the reference is the last component of
// the key path or of the key leaf. Bucketing on it is therefore exact, not a
// prefilter — a reference can only ever match inside its own bucket.
//
// One repository's config alone can hold hundreds of thousands of keys, so the
// index is built once per run and shared by every reference; a per-reference
// scan is what made a single "topic:${…}" reference cost seconds.
type configIndex struct {
	byLast map[string][]indexedConfigKey
}

func buildConfigIndex(units []*domain.ASTUnit) *configIndex {
	idx := &configIndex{byLast: make(map[string][]indexedConfigKey, len(units))}
	for _, u := range units {
		k := indexedConfigKey{
			unit: u,
			path: wordComponents(contract.TrimKind(u.Qualified, contract.KindConfig)),
			leaf: wordComponents(u.Name),
		}
		pathLast := ""
		if len(k.path) > 0 {
			pathLast = k.path[len(k.path)-1]
			idx.byLast[pathLast] = append(idx.byLast[pathLast], k)
		}
		if len(k.leaf) >= minLeafComponents {
			if leafLast := k.leaf[len(k.leaf)-1]; leafLast != pathLast {
				idx.byLast[leafLast] = append(idx.byLast[leafLast], k)
			}
		}
	}
	return idx
}

// matchConfigKey finds the value of a config key matching the reference
// (ORDERS_TOPIC ~ kafka.orders-topic). Matching is component-wise — the
// reference must align with whole components of the key path — so a key whose
// leaf merely ends the reference cannot win. Same-repo keys are preferred only
// within one tier.
//
// It indexes the keys for this one lookup; callers resolving several references
// build the index once and call matchConfigKeyIndexed.
func matchConfigKey(keys []*domain.ASTUnit, ref, repoID string) string {
	return matchConfigKeyIndexed(buildConfigIndex(keys), ref, repoID)
}

// matchConfigKeyIndexed is matchConfigKey over a prebuilt config index. The
// winner is the highest-scoring key and, among equals, the lexicographically
// smallest qualified name, so the result does not depend on the order keys were
// loaded or bucketed in.
func matchConfigKeyIndexed(idx *configIndex, ref, repoID string) string {
	refComps := wordComponents(ref)
	if len(refComps) == 0 {
		return ""
	}
	var best *domain.ASTUnit
	bestScore := 0
	for _, k := range idx.byLast[refComps[len(refComps)-1]] {
		tier := configMatchTier(k, refComps)
		if tier == configNoMatch {
			continue
		}
		score := tier * 2
		if k.unit.RepoID == repoID {
			score++
		}
		if score > bestScore || (score == bestScore && best != nil && k.unit.Qualified < best.Qualified) {
			best, bestScore = k.unit, score
		}
	}
	if best == nil {
		return ""
	}
	return best.Signature
}

// configMatchTier scores one config key against the components of a reference.
func configMatchTier(k indexedConfigKey, refComps []string) int {
	if hasComponentSuffix(k.path, refComps) {
		return configPathMatch
	}
	if len(k.leaf) >= minLeafComponents && hasComponentSuffix(refComps, k.leaf) {
		return configLeafMatch
	}
	return configNoMatch
}

// metaDefaultTopic returns the topic recorded in the edge meta when it is a
// usable literal: the parser stores the default of a "${KEY:default}"
// placeholder there, which is the best answer left when no config key matches.
func metaDefaultTopic(meta string) string {
	if meta == "" {
		return ""
	}
	m := &store.EdgeMeta{}
	if err := json.Unmarshal([]byte(meta), m); err != nil {
		return ""
	}
	if m.Topic == "" || strings.Contains(m.Topic, "${") {
		return ""
	}
	return m.Topic
}

// resolveLocal links call and handled_by edges within one repository by
// symbol name, preferring the definition owned by the call's receiver and then
// same-file definitions.
func (l *Linker) resolveLocal(ctx context.Context, repoID string, stats *RunStats) error {
	units, err := l.store.GetASTUnits(ctx, domain.QueryOpts{
		RepoID: repoID,
		Kinds:  []string{"function", "method"},
	})
	if err != nil {
		return err
	}
	byName := map[string][]*domain.ASTUnit{}
	for _, u := range units {
		byName[u.Name] = append(byName[u.Name], u)
	}
	for _, cands := range byName {
		sortUnits(cands)
	}
	comps := newUnitComponents()

	edges, err := l.store.GetEdges(ctx, domain.QueryOpts{
		RepoID: repoID,
		Kinds:  []string{store.EdgeCall, store.EdgeHandledBy},
	})
	if err != nil {
		return err
	}

	w := newResolutionWriter(l.store, stats, "link local edge")
	for _, e := range edges {
		name := contract.LastComponent(e.DstName)
		candidates := byName[name]
		if len(candidates) == 0 {
			continue
		}
		best, conf := resolveLocalTarget(name, e, candidates, comps)
		if e.DstID == best.ID && e.DstRepoID == repoID {
			continue
		}
		w.add(ctx, store.EdgeResolution{
			EdgeID:     e.ID,
			DstID:      best.ID,
			DstRepoID:  repoID,
			Confidence: confidenceAfterResolve(e, conf),
		})
	}
	w.flush(ctx)
	stats.ResolvedLocal += w.applied
	return nil
}

// resolutionBuffer is how many resolutions the linker queues before handing
// them to the store. It bounds the memory a link pass adds — a repository the
// size of Elasticsearch resolves millions of edges, which is far too many to
// hold — and is the granularity a failed write is retried at.
const resolutionBuffer = 1000

// resolutionWriter buffers edge resolutions and writes them in batches.
//
// One autocommit UPDATE per resolved edge is what made linking a large
// repository as expensive as indexing it: 2.6M transactions for Elasticsearch,
// all on one connection. Batching them is only possible when the store
// implements store.EdgeResolutionBatcher; any other implementation (a test
// double, a future backend) keeps the single-row path.
//
// Failures stay per-edge whichever path runs: each one is logged with its edge
// id and counted in stats.Errors, and the rest of the batch is still applied.
type resolutionWriter struct {
	store   store.Storage
	batcher store.EdgeResolutionBatcher // nil: write one edge at a time
	stats   *RunStats
	msg     string // log message for a failed write
	buf     []store.EdgeResolution
	applied int // resolutions written successfully
}

func newResolutionWriter(st store.Storage, stats *RunStats, msg string) *resolutionWriter {
	w := &resolutionWriter{store: st, stats: stats, msg: msg}
	w.batcher, _ = st.(store.EdgeResolutionBatcher)
	return w
}

// add queues one resolution, writing the buffer once it is full. Every edge
// must be queued at most once per run: the Postgres batch joins on edge id and
// would apply only one of two rows claiming the same edge.
func (w *resolutionWriter) add(ctx context.Context, r store.EdgeResolution) {
	w.buf = append(w.buf, r)
	if len(w.buf) >= resolutionBuffer {
		w.flush(ctx)
	}
}

// flush writes everything queued. Errors are logged and counted rather than
// returned: a link pass reports them and carries on.
func (w *resolutionWriter) flush(ctx context.Context) {
	if len(w.buf) == 0 {
		return
	}
	failures, err := w.write(ctx)
	applied := len(w.buf) - len(failures)
	if err != nil {
		// The backend gave up on the batch as a whole instead of naming the
		// rows that failed, so nothing it did not report on is known to have
		// landed.
		slog.Warn(w.msg, "edges", len(w.buf), "err", err)
		w.stats.Errors += applied
		applied = 0
	}
	for _, f := range failures {
		w.stats.Errors++
		slog.Warn(w.msg, "edge", f.EdgeID, "err", f.Err)
	}
	w.applied += applied
	w.buf = w.buf[:0]
}

// write applies the buffer and returns the resolutions that failed.
func (w *resolutionWriter) write(ctx context.Context) ([]store.EdgeResolutionFailure, error) {
	if w.batcher != nil {
		return w.batcher.BatchUpdateEdgeResolutions(ctx, w.buf)
	}
	var failures []store.EdgeResolutionFailure
	for i, r := range w.buf {
		if err := w.store.UpdateEdgeResolution(ctx, r.EdgeID, r.DstID, r.DstRepoID, r.Confidence); err != nil {
			failures = append(failures, store.EdgeResolutionFailure{Index: i, EdgeID: r.EdgeID, Err: err})
		}
	}
	return failures, nil
}

// resolveLocalTarget picks the definition a call/handled_by edge refers to,
// preferring, in order: a receiver-owned definition in the same file, a
// receiver-owned definition anywhere, a same-file definition, the only
// definition with that name.
//
// Choosing between several same-named definitions with nothing to tell them
// apart is a guess (repo.Save may be (*UserRepo).Save or (*OrderRepo).Save)
// and scores ConfHeuristic, so that Trace does not map call arguments onto an
// arbitrary parameter list at cross-file confidence.
func resolveLocalTarget(name string, e *domain.Edge, cands []*domain.ASTUnit, comps *unitComponents) (*domain.ASTUnit, float32) {
	if recv := edgeReceiver(e); recv != "" {
		owned := comps.ownedBy(name, cands, recv)
		if len(owned) > 0 {
			if u := firstInFile(owned, e.FilePath); u != nil {
				return u, contract.ConfExact
			}
			if len(owned) == 1 {
				return owned[0], contract.ConfCrossFile
			}
			return owned[0], contract.ConfHeuristic
		}
	}
	if u := firstInFile(cands, e.FilePath); u != nil {
		return u, contract.ConfExact
	}
	if len(cands) == 1 {
		return cands[0], contract.ConfCrossFile
	}
	return cands[0], contract.ConfHeuristic
}

// edgeReceiver returns the receiver recorded for a call edge: the "receiver"
// (or "recv_type") meta field, else the qualifier of a dotted destination name
// ("repo.Save" -> "repo"). Language extractors currently emit the bare method
// name without a receiver, so this is best-effort by design.
func edgeReceiver(e *domain.Edge) string {
	// One parse for both keys: this runs once per call edge, and a repository
	// the size of Kafka has hundreds of thousands of them.
	if e.Meta != "" {
		m := map[string]any{}
		if err := json.Unmarshal([]byte(e.Meta), &m); err == nil {
			for _, key := range []string{metaKeyReceiver, metaKeyRecvType} {
				if v, _ := m[key].(string); v != "" {
					return v
				}
			}
		}
	}
	if i := strings.LastIndex(e.DstName, "."); i > 0 {
		return e.DstName[:i]
	}
	return ""
}

// unitComponents memoizes the word decomposition of unit names. Without it the
// receiver check re-splits the same qualified names for every candidate of
// every call edge, which dominates linking on a large repository.
type unitComponents struct {
	qualified map[string][]string // unit id -> owner path components
	byTail    map[string]map[string][]*domain.ASTUnit
}

func newUnitComponents() *unitComponents {
	return &unitComponents{
		qualified: map[string][]string{},
		byTail:    map[string]map[string][]*domain.ASTUnit{},
	}
}

// owner returns a unit's owner path: its qualified name minus its own name
// components ((*UserRepo).Save -> ["user","repo"]).
func (c *unitComponents) owner(u *domain.ASTUnit) []string {
	if comps, ok := c.qualified[u.ID]; ok {
		return comps
	}
	qual := wordComponents(u.Qualified)
	own := len(wordComponents(u.Name))
	var comps []string
	if own < len(qual) {
		comps = qual[:len(qual)-own]
	}
	c.qualified[u.ID] = comps
	return comps
}

// ownedBy returns the candidates a receiver expression owns. Candidates are
// indexed by the last component of their owner path, because a suffix match
// requires that component to be the receiver's last one — scanning the whole
// bucket instead is what made linking a repository the size of Kafka
// effectively not finish: one name there has 443 definitions and the repo has
// ~870k call edges.
func (c *unitComponents) ownedBy(name string, cands []*domain.ASTUnit, recv string) []*domain.ASTUnit {
	rc := wordComponents(recv)
	if len(rc) == 0 {
		return nil
	}
	idx, ok := c.byTail[name]
	if !ok {
		idx = map[string][]*domain.ASTUnit{}
		for _, u := range cands {
			if o := c.owner(u); len(o) > 0 {
				idx[o[len(o)-1]] = append(idx[o[len(o)-1]], u)
			}
		}
		c.byTail[name] = idx
	}
	var owned []*domain.ASTUnit
	for _, u := range idx[rc[len(rc)-1]] {
		if hasComponentSuffix(c.owner(u), rc) {
			owned = append(owned, u)
		}
	}
	return owned
}

// receiverMatches reports whether a unit is owned by the given receiver: the
// qualified name minus its own name components must end with the receiver's
// components, so "repo" and "userRepo" both match (*UserRepo).Save while
// "orderRepo" does not.
func receiverMatches(u *domain.ASTUnit, recv string) bool {
	rc := wordComponents(recv)
	if len(rc) == 0 {
		return false
	}
	qual := wordComponents(u.Qualified)
	own := len(wordComponents(u.Name))
	if own >= len(qual) {
		return false
	}
	return hasComponentSuffix(qual[:len(qual)-own], rc)
}

// firstInFile returns the first candidate defined in the given file, or nil.
func firstInFile(cands []*domain.ASTUnit, filePath string) *domain.ASTUnit {
	if filePath == "" {
		return nil
	}
	for _, c := range cands {
		if c.FilePath == filePath {
			return c
		}
	}
	return nil
}

// sortUnits orders candidates deterministically: storage order must never
// decide which of several equally plausible destinations wins.
//
// Which is why the last resort is where a unit sits in its file rather than its
// id, though the id reads like the stable key here. An id is the order the
// indexing goroutines committed the row in — indexes.workers of them — so two
// passes over identical sources handed cands[0] to a different overload of the
// same qualified name. That is worse than an unstable ranking: the winner is
// written back to edges.dst_id, so it outlives the pass that chose it.
func sortUnits(units []*domain.ASTUnit) {
	sort.Slice(units, func(i, j int) bool {
		a, b := units[i], units[j]
		switch {
		case a.RepoID != b.RepoID:
			return a.RepoID < b.RepoID
		case a.Qualified != b.Qualified:
			return a.Qualified < b.Qualified
		case a.FilePath != b.FilePath:
			return a.FilePath < b.FilePath
		default:
			return beforeInFile(a, b)
		}
	})
}

// beforeInFile orders two units of one file by where in it they are. It ends
// every candidate ordering in this file, so it has to be a function of the
// source and not of the pass that read it; two units it cannot separate are at
// the same position under the same name and resolve an edge to the same answer.
func beforeInFile(a, b *domain.ASTUnit) bool {
	switch {
	case a.StartLine != b.StartLine:
		return a.StartLine < b.StartLine
	case a.StartByte != b.StartByte:
		return a.StartByte < b.StartByte
	case a.Kind != b.Kind:
		return a.Kind < b.Kind
	default:
		return a.Name < b.Name
	}
}

// contractIndex holds prebuilt lookup structures over the contract units of
// all repositories, so that per-edge matching is a hash lookup plus a scan
// over a small candidate bucket instead of a scan over all units.
type contractIndex struct {
	// rpcByMethod buckets rpc_method units by the method component of their
	// qualified key (part after the last '/'), preserving input order.
	rpcByMethod map[string][]indexedRPC
	// routesBySegCount buckets pre-parsed http_route units by path segment
	// count (routeMatchScore rejects any segment-count mismatch).
	routesBySegCount map[int][]indexedRoute
	// tables indexes db_table units by key and by bare table name.
	tables *tableIndex
	// unitIDs is the set of IDs of all loaded contract units; used to skip
	// rewriting edges whose resolution is still valid.
	unitIDs map[string]struct{}
}

type indexedRPC struct {
	unit *domain.ASTUnit
	svc  string
}

type indexedRoute struct {
	unit   *domain.ASTUnit
	method string
	segs   []string
}

func buildRPCIndex(units []*domain.ASTUnit) map[string][]indexedRPC {
	idx := make(map[string][]indexedRPC, len(units))
	for _, u := range units {
		svc, method, ok := splitGrpcKey(u.Qualified)
		if !ok {
			continue
		}
		idx[method] = append(idx[method], indexedRPC{unit: u, svc: svc})
	}
	return idx
}

func buildRouteIndex(units []*domain.ASTUnit) map[int][]indexedRoute {
	idx := make(map[int][]indexedRoute, len(units))
	for _, u := range units {
		method, path, ok := splitRouteKey(u.Qualified)
		if !ok {
			continue
		}
		segs := splitPath(path)
		idx[len(segs)] = append(idx[len(segs)], indexedRoute{unit: u, method: method, segs: segs})
	}
	return idx
}

// tableIndex indexes db_table units by their full key (schema qualifier
// included), by the bare table name for schema-qualified tables, and by the key
// their entity name would have produced. The latter two let a key that cannot
// be matched exactly still find them — at a weaker tier.
type tableIndex struct {
	byKey  map[string][]*domain.ASTUnit // "db:analytics.users"
	byName map[string][]*domain.ASTUnit // "db:users" -> schema-qualified units
	// byEntity maps the key an ORM detector derives from an entity name to the
	// tables that entity actually declares under another name: an EF Core
	// ToTable("Catalog") on CatalogItem publishes db:catalog, while the write
	// it records is keyed db:catalog_items.
	byEntity map[string][]*domain.ASTUnit
}

func buildTableIndex(units []*domain.ASTUnit) *tableIndex {
	idx := &tableIndex{
		byKey:    make(map[string][]*domain.ASTUnit, len(units)),
		byName:   map[string][]*domain.ASTUnit{},
		byEntity: map[string][]*domain.ASTUnit{},
	}
	for _, u := range units {
		idx.byKey[u.Qualified] = append(idx.byKey[u.Qualified], u)
		if entity := signatureEntity(u.Signature); entity != "" {
			// A table declared under its derived name is already reachable by
			// key; only an explicit name (ToTable, @Table, @Entity("…")) needs
			// the entity detour.
			if key := contract.DB(entityTableName(entity)); key != u.Qualified {
				idx.byEntity[key] = append(idx.byEntity[key], u)
			}
		}
		table, ok := contract.ParseDB(u.Qualified)
		if !ok {
			continue
		}
		if i := strings.LastIndex(table, "."); i >= 0 {
			key := contract.DB(table[i+1:])
			idx.byName[key] = append(idx.byName[key], u)
		}
	}
	return idx
}

// entitySigPrefix is how every parser records the entity a db_table unit was
// published for (see the ORM paths in internal/index/ast).
const entitySigPrefix = "entity:"

// signatureEntity returns the entity name a db_table unit names in its
// signature, or "" for tables that name none (a SQL migration, say).
func signatureEntity(sig string) string {
	if !strings.HasPrefix(sig, entitySigPrefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(sig, entitySigPrefix))
}

// entityTableName derives the table an ORM maps an entity to. It is the same
// function the indexer used to produce the key on the edge side — see
// contract.TableName, which is where it lives so the two cannot drift.
func entityTableName(entity string) string { return contract.TableName(entity) }

func newContractIndex(rpcUnits, routeUnits, tableUnits []*domain.ASTUnit) *contractIndex {
	ids := make(map[string]struct{}, len(rpcUnits)+len(routeUnits)+len(tableUnits))
	for _, units := range [][]*domain.ASTUnit{rpcUnits, routeUnits, tableUnits} {
		for _, u := range units {
			ids[u.ID] = struct{}{}
		}
	}
	return &contractIndex{
		rpcByMethod:      buildRPCIndex(rpcUnits),
		routesBySegCount: buildRouteIndex(routeUnits),
		tables:           buildTableIndex(tableUnits),
		unitIDs:          ids,
	}
}

// resolveContracts links rpc_call/implements_rpc edges to rpc_method units and
// http_call edges to http_route units across all repositories. Contract units
// are loaded and indexed once per run; each edge is then matched against a
// small candidate bucket. Edges already resolved to a still-existing contract
// unit are skipped entirely.
// confidenceAfterResolve returns the confidence to store when the linker points
// an edge at a destination: the parser-base confidence times the linker's match
// factor (see contract/confidence.go).
//
// The base is read from the edge meta, where the parser records it once, rather
// than from e.Confidence — because e.Confidence is not a reliable base. Every
// time a destination repo is reindexed, DeleteASTUnitsByFile unresolves the
// edges pointing into it: dst_id is cleared but the confidence is left as it
// was, already carrying the previous linker factor. Reading base from that
// value and multiplying the factor in again would decay the score geometrically
// toward ConfWeak over reindex cycles, mis-tiering edges and spuriously tripping
// LLM disambiguation. Reading it from meta keeps re-resolution idempotent: the
// same edge resolves to the same confidence however many times its destination
// is reindexed.
//
// Edges indexed before this change carry no recorded base; for them the stored
// confidence is the best estimate, so a forced reindex is what fully settles
// their scores.
func confidenceAfterResolve(e *domain.Edge, conf float32) float32 {
	base := e.Confidence
	if e.Meta != "" {
		m := &store.EdgeMeta{}
		if json.Unmarshal([]byte(e.Meta), m) == nil && m.BaseConf > 0 {
			base = m.BaseConf
		}
	}
	return base * conf
}

func (l *Linker) resolveContracts(ctx context.Context, stats *RunStats) error {
	rpcUnits, err := l.store.GetASTUnits(ctx, domain.QueryOpts{Kinds: []string{store.KindRPCMethod}})
	if err != nil {
		return err
	}
	routeUnits, err := l.store.GetASTUnits(ctx, domain.QueryOpts{Kinds: []string{store.KindHTTPRoute}})
	if err != nil {
		return err
	}
	tableUnits, err := l.store.GetASTUnits(ctx, domain.QueryOpts{Kinds: []string{store.KindDBTable}})
	if err != nil {
		return err
	}
	idx := newContractIndex(rpcUnits, routeUnits, tableUnits)

	edges, err := l.store.GetEdges(ctx, domain.QueryOpts{
		Kinds: []string{
			store.EdgeRPCCall, store.EdgeImplementsRPC, store.EdgeHTTPCall,
			store.EdgeWritesTo, store.EdgeReadsFrom,
		},
	})
	if err != nil {
		return err
	}

	ds := newDisambigRun()
	linkW := newResolutionWriter(l.store, stats, "link contract edge")
	clearW := newResolutionWriter(l.store, stats, "clear stale contract edge")

	for _, e := range edges {
		if e.DstID != "" {
			if _, ok := idx.unitIDs[e.DstID]; ok {
				stats.SkippedValid++
				continue
			}
		}
		var dst *domain.ASTUnit
		var conf float32
		var llm bool
		switch e.Kind {
		case store.EdgeRPCCall, store.EdgeImplementsRPC:
			var cands []*domain.ASTUnit
			cands, conf = rpcCandidates(idx.rpcByMethod, e.DstName)
			if len(cands) > 0 {
				dst = cands[0]
			}
			// Ambiguous: several services expose the method and the client key
			// does not say which one (no package, or none at all).
			if l.disambig != nil && dst != nil && conf <= contract.ConfHeuristic && len(cands) > 1 {
				if pick, ok := l.disambiguate(ctx, ds, e, cands); ok {
					dst, conf, llm = pick, contract.ConfHigh, true
				}
			}
		case store.EdgeHTTPCall:
			cands := routeCandidates(idx.routesBySegCount, e.DstName, e.RepoID)
			if len(cands) > 0 {
				dst, conf = cands[0].unit, cands[0].score
			}
			// Ambiguous: the two best route candidates are equally good.
			if l.disambig != nil && dst != nil && routesAmbiguous(cands) {
				units := make([]*domain.ASTUnit, len(cands))
				for i := range cands {
					units[i] = cands[i].unit
				}
				if pick, ok := l.disambiguate(ctx, ds, e, units); ok {
					dst, conf, llm = pick, contract.ConfHigh, true
				}
			}
		case store.EdgeWritesTo, store.EdgeReadsFrom:
			var cands []*domain.ASTUnit
			cands, conf = tableCandidates(idx.tables, e.DstName, e.RepoID)
			if len(cands) > 0 {
				dst = cands[0]
			}
			// Ambiguous: the same table name is declared by several other
			// repositories (or by several schemas).
			if l.disambig != nil && dst != nil && conf <= contract.ConfCrossFile && len(cands) > 1 {
				if pick, ok := l.disambiguate(ctx, ds, e, cands); ok {
					dst, conf, llm = pick, contract.ConfHigh, true
				}
			}
		}
		if dst == nil {
			// The edge kept a resolution whose target no longer exists (the
			// check above only skips still-valid ones): drop it rather than
			// leave a dangling dst_id behind.
			if e.DstID != "" {
				e.DstID, e.DstRepoID = "", ""
				clearW.add(ctx, store.EdgeResolution{EdgeID: e.ID, Confidence: e.Confidence})
			}
			continue
		}
		if e.DstID == dst.ID && e.DstRepoID == dst.RepoID {
			continue
		}
		e.Confidence = confidenceAfterResolve(e, conf)
		e.DstID, e.DstRepoID = dst.ID, dst.RepoID
		if !llm {
			linkW.add(ctx, store.EdgeResolution{
				EdgeID: e.ID, DstID: dst.ID, DstRepoID: dst.RepoID, Confidence: e.Confidence,
			})
			continue
		}
		// An LLM-disambiguated edge also records how it was resolved. The
		// annotation only makes sense once the resolution it describes is
		// stored, so such an edge is written on its own rather than queued.
		// Only its meta changes — the join key stays where it was — so it is
		// updated in place instead of rewriting its whole (kind, dst_name)
		// group.
		if err := l.store.UpdateEdgeResolution(ctx, e.ID, dst.ID, dst.RepoID, e.Confidence); err != nil {
			stats.Errors++
			slog.Warn("link contract edge", "edge", e.ID, "err", err)
			continue
		}
		e.Meta = metaWithLLMSource(e.Meta)
		if err := l.store.UpdateEdgeMeta(ctx, e.ID, e.Meta); err != nil {
			stats.Errors++
			slog.Warn("mark llm-resolved edge", "edge", e.ID, "err", err)
		}
		stats.ResolvedContracts++
	}
	linkW.flush(ctx)
	clearW.flush(ctx)
	stats.ResolvedContracts += linkW.applied
	return nil
}

// edgeKey identifies a group of contract edges sharing kind and join key.
type edgeKey struct{ kind, dstName string }

// rewriteEdgeGroups replaces the stored edges of whole (kind, dst_name) groups
// with the given in-memory edges, which carry a new dst_name.
//
// This is only for edges whose join key moves: an annotation that leaves the
// key alone is written with UpdateEdgeMeta instead. Deletion is by (kind,
// dst_name) and there is no cross-call transaction, so a group is deleted and
// re-inserted. Two properties keep that safe: every group is deleted before
// anything is re-inserted, because a rewrite may move edges onto the key of
// another group in the batch, and each group is re-inserted with one batched
// (transactional) call, so a failure cannot leave half a group behind. A group
// whose delete failed is left as stored rather than duplicated.
//
// Complete groups must be passed in: deletion is by (kind, dst_name) and takes
// every edge sharing that key, including ones the caller did not change.
func (l *Linker) rewriteEdgeGroups(ctx context.Context, groups map[edgeKey][]*domain.Edge, stats *RunStats) {
	keys := make([]edgeKey, 0, len(groups))
	for key, edges := range groups {
		if len(edges) == 0 {
			continue
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].kind != keys[j].kind {
			return keys[i].kind < keys[j].kind
		}
		return keys[i].dstName < keys[j].dstName
	})

	pending := make([]edgeKey, 0, len(keys))
	for _, key := range keys {
		if err := l.store.DeleteEdgesByKindAndDst(ctx, key.kind, key.dstName); err != nil {
			stats.Errors++
			slog.Warn("rewrite edge group", "kind", key.kind, "dst", key.dstName, "err", err)
			continue
		}
		pending = append(pending, key)
	}
	for _, key := range pending {
		edges := groups[key]
		for _, e := range edges {
			e.ID = "" // re-insert under a fresh id
		}
		if err := l.store.BatchStoreEdges(ctx, edges); err != nil {
			stats.Errors++
			slog.Warn("restore edge group", "kind", key.kind, "dst", key.dstName, "err", err)
		}
	}
}

// maxDisambigCalls caps LLM disambiguation calls per linker run.
const maxDisambigCalls = 20

// routeAmbiguityDelta: two top route candidates within this score delta are
// considered ambiguous.
const routeAmbiguityDelta float32 = 0.05

// disambigRun is per-run disambiguation state: answers cached by edge
// DstName and the LLM call budget.
type disambigRun struct {
	cache map[string]*domain.ASTUnit // DstName -> chosen unit (nil = declined)
	calls int
}

func newDisambigRun() *disambigRun {
	return &disambigRun{cache: map[string]*domain.ASTUnit{}}
}

// disambiguate asks the configured disambiguator to pick one of cands for
// edge e. Answers are cached by e.DstName; at most maxDisambigCalls LLM calls
// are made per run.
func (l *Linker) disambiguate(ctx context.Context, ds *disambigRun, e *domain.Edge, cands []*domain.ASTUnit) (*domain.ASTUnit, bool) {
	if pick, seen := ds.cache[e.DstName]; seen {
		if pick == nil {
			return nil, false
		}
		for _, c := range cands {
			if c.ID == pick.ID {
				return pick, true
			}
		}
		return nil, false
	}
	if ds.calls >= maxDisambigCalls {
		return nil, false
	}
	ds.calls++
	disambigTotal.Inc()
	choice, ok := l.disambig(ctx, disambigPrompt(e, cands))
	if !ok || choice < 0 || choice >= len(cands) {
		ds.cache[e.DstName] = nil
		return nil, false
	}
	ds.cache[e.DstName] = cands[choice]
	return cands[choice], true
}

// disambigPrompt lists the edge context and its candidates and asks for the
// index of the correct one ("number or -1").
func disambigPrompt(e *domain.Edge, cands []*domain.ASTUnit) string {
	var b strings.Builder
	b.WriteString("A cross-service code graph edge has several possible destinations.\n")
	fmt.Fprintf(&b, "Call: %s (edge kind %s) at %s:%d in repo %s\n", e.DstName, e.Kind, e.FilePath, e.Line, e.RepoID)
	b.WriteString("Candidates:\n")
	for i, c := range cands {
		fmt.Fprintf(&b, "%d) %s (repo=%s, file=%s)\n", i, c.Qualified, c.RepoID, c.FilePath)
	}
	b.WriteString("Which candidate is the actual destination of this call? Reply with the number only, or -1 if unsure.")
	return b.String()
}

// Linker annotations carried as extra keys in the edge meta JSON. They have no
// field in store.EdgeMeta, which decodes them away harmlessly.
const (
	metaKeySource   = "source"    // "llm" for a disambiguated resolution
	metaKeyTopicRef = "topic_ref" // the config reference a topic came from
	metaKeyReceiver = "receiver"  // call-site receiver expression
	metaKeyRecvType = "recv_type" // static type of the call-site receiver
)

// metaWithLLMSource returns meta marked as LLM-resolved.
func metaWithLLMSource(meta string) string {
	return metaWithField(meta, metaKeySource, "llm")
}

// metaWithField merges one string key into the JSON object of an edge meta
// (a minimal map-level merge; the existing keys are preserved).
func metaWithField(meta, key, value string) string {
	m := map[string]any{}
	if meta != "" {
		if err := json.Unmarshal([]byte(meta), &m); err != nil {
			m = map[string]any{}
		}
	}
	m[key] = value
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

// scoredRoute is one route candidate with its match score.
type scoredRoute struct {
	unit     *domain.ASTUnit
	score    float32
	sameRepo bool
}

// routeCandidates returns all matching route candidates for an http_call key,
// sorted best-first. Ranking is by match quality alone; the repo relationship
// only breaks ties, so an exact same-repo route is never displaced by a
// fuzzier cross-repo one.
func routeCandidates(idx map[int][]indexedRoute, key, srcRepo string) []scoredRoute {
	method, path, ok := splitRouteKey(key)
	if !ok {
		return nil
	}
	callSegs := splitPath(path)
	var out []scoredRoute
	for _, r := range idx[len(callSegs)] {
		score := routeMatchScoreSegs(method, callSegs, r.method, r.segs)
		if score <= 0 {
			continue
		}
		out = append(out, scoredRoute{unit: r.unit, score: score, sameRepo: r.unit.RepoID == srcRepo})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.score != b.score:
			return a.score > b.score
		case a.sameRepo != b.sameRepo:
			return !a.sameRepo // an HTTP client normally calls another service
		case a.unit.RepoID != b.unit.RepoID:
			return a.unit.RepoID < b.unit.RepoID
		case a.unit.FilePath != b.unit.FilePath:
			return a.unit.FilePath < b.unit.FilePath
		default:
			// Position, not id, for sortUnits' reason — and here the id decided
			// more than the winner: routesAmbiguous compares the two best
			// candidates, so it also decided whether an LLM call was spent.
			return beforeInFile(a.unit, b.unit)
		}
	})
	return out
}

// routesAmbiguous reports whether the two best route candidates are close
// enough, and alike enough in repo relationship, that the heuristic has no
// real reason to prefer one.
func routesAmbiguous(cands []scoredRoute) bool {
	return len(cands) > 1 &&
		cands[0].score-cands[1].score <= routeAmbiguityDelta &&
		cands[0].sameRepo == cands[1].sameRepo
}

// rpcCandidates returns the rpc_method units an rpc key may refer to, best
// tier first, together with the confidence of that tier.
//
// Tiers: units whose service equals the key's service, then units whose
// service ends in ".<service>" (client keys rarely carry the proto package),
// then every unit sharing the method name. A tier holding more than one
// candidate cannot be told apart — orders.v1.OrderService and
// orders.v2.OrderService both end in ".OrderService" — so it scores
// ConfHeuristic and the caller routes it through disambiguation instead of
// silently taking the first entry at ConfExact.
func rpcCandidates(idx map[string][]indexedRPC, key string) ([]*domain.ASTUnit, float32) {
	svc, method, ok := splitGrpcKey(key)
	if !ok {
		return nil, 0
	}
	var exact, suffix, byMethod []*domain.ASTUnit
	for _, c := range idx[method] {
		byMethod = append(byMethod, c.unit)
		switch {
		case svc == "":
		case c.svc == svc:
			exact = append(exact, c.unit)
		case strings.HasSuffix(c.svc, "."+svc):
			suffix = append(suffix, c.unit)
		}
	}
	for _, tier := range [][]*domain.ASTUnit{exact, suffix} {
		if len(tier) == 0 {
			continue
		}
		sortUnits(tier)
		if len(tier) == 1 {
			return tier, contract.ConfExact
		}
		return tier, contract.ConfHeuristic
	}
	if len(byMethod) == 0 {
		return nil, 0
	}
	sortUnits(byMethod)
	return byMethod, contract.ConfHeuristic
}

// matchRPC matches "grpc:Svc/Method" (possibly without service or package)
// against rpc_method units qualified "grpc:pkg.Svc/Method".
func matchRPC(units []*domain.ASTUnit, key string) (*domain.ASTUnit, float32) {
	return matchRPCIndexed(buildRPCIndex(units), key)
}

// matchRPCIndexed is matchRPC over a prebuilt method -> units index.
func matchRPCIndexed(idx map[string][]indexedRPC, key string) (*domain.ASTUnit, float32) {
	cands, conf := rpcCandidates(idx, key)
	if len(cands) == 0 {
		return nil, 0
	}
	return cands[0], conf
}

// splitGrpcKey is a thin wrapper over contract.ParseGRPC, kept for callers
// (and tests) inside this package.
func splitGrpcKey(key string) (svc, method string, ok bool) {
	return contract.ParseGRPC(key)
}

// matchRoute matches an "http:METHOD /path" key against http_route units.
// Between equally good matches a cross-repo route wins — an HTTP client
// normally calls another service — but never against a better same-repo one.
func matchRoute(units []*domain.ASTUnit, key, srcRepo string) (*domain.ASTUnit, float32) {
	return matchRouteIndexed(buildRouteIndex(units), key, srcRepo)
}

// matchRouteIndexed is matchRoute over a prebuilt segment-count -> parsed
// routes index. Only routes with the same number of path segments as the call
// can match, so a single bucket is scanned.
func matchRouteIndexed(idx map[int][]indexedRoute, key, srcRepo string) (*domain.ASTUnit, float32) {
	cands := routeCandidates(idx, key, srcRepo)
	if len(cands) == 0 {
		return nil, 0
	}
	return cands[0].unit, cands[0].score
}

// matchTable matches a "db:[schema.]table" key against db_table units,
// preferring same-repo tables.
func matchTable(units []*domain.ASTUnit, key, srcRepo string) (*domain.ASTUnit, float32) {
	return matchTableIndexed(buildTableIndex(units), key, srcRepo)
}

// matchTableIndexed is matchTable over a prebuilt table index.
func matchTableIndexed(idx *tableIndex, key, srcRepo string) (*domain.ASTUnit, float32) {
	cands, conf := tableCandidates(idx, key, srcRepo)
	if len(cands) == 0 {
		return nil, 0
	}
	return cands[0], conf
}

// tableCandidates returns the db_table units a "db:[schema.]table" key may
// refer to, best tier first, with the confidence of that tier.
//
// The schema qualifier is part of the key: db:analytics.users and
// db:public.users are different tables. A key without a schema still reaches
// schema-qualified tables, but only at a weaker tier, since it cannot say
// which schema it meant, and weaker still it reaches tables whose entity — not
// whose name — the key was derived from. Same-repo tables win over tables of
// other repos, and a tier holding several candidates is ambiguous: it scores
// one tier lower so the caller routes it through disambiguation instead of
// taking the first cross-repo table at ConfHigh.
func tableCandidates(idx *tableIndex, key, srcRepo string) ([]*domain.ASTUnit, float32) {
	if !contract.IsKind(key, contract.KindDB) {
		return nil, 0
	}
	tiers := [][]*domain.ASTUnit{idx.byKey[key]}
	if table, ok := contract.ParseDB(key); ok && !strings.Contains(table, ".") {
		tiers = append(tiers, idx.byName[key], idx.byEntity[key])
	}
	// Confidence per tier, by [exact key | table name | entity][same repo | other].
	confs := [3][2]float32{
		{contract.ConfExact, contract.ConfHigh},
		{contract.ConfHigh, contract.ConfCrossFile},
		{contract.ConfCrossFile, contract.ConfHeuristic},
	}
	for tier, units := range tiers {
		var same, cross []*domain.ASTUnit
		for _, u := range units {
			if u.RepoID == srcRepo {
				same = append(same, u)
			} else {
				cross = append(cross, u)
			}
		}
		for repo, group := range [][]*domain.ASTUnit{same, cross} {
			if len(group) == 0 {
				continue
			}
			sortUnits(group)
			conf := confs[tier][repo]
			if len(group) > 1 {
				conf = weakerTier(conf)
			}
			return group, conf
		}
	}
	return nil, 0
}

// weakerTier returns the named tier one step below conf.
func weakerTier(conf float32) float32 {
	switch {
	case conf > contract.ConfHigh:
		return contract.ConfHigh
	case conf > contract.ConfCrossFile:
		return contract.ConfCrossFile
	case conf > contract.ConfHeuristic:
		return contract.ConfHeuristic
	default:
		return contract.ConfWeak
	}
}

// splitRouteKey is a thin wrapper over contract.ParseHTTP, kept for callers
// (and tests) inside this package.
func splitRouteKey(key string) (method, path string, ok bool) {
	return contract.ParseHTTP(key)
}

// routeMatchScore scores an HTTP call key against a route template.
// Returns 0 for no match.
func routeMatchScore(callMethod, callPath, routeMethod, routePath string) float32 {
	return routeMatchScoreSegs(callMethod, splitPath(callPath), routeMethod, splitPath(routePath))
}

// Route scoring factors. The named contract tiers stay authoritative for what
// a match is worth; these say how much an inexact route degrades it.
const (
	// routeBaseScore is the value of a fully literal route match.
	routeBaseScore = contract.ConfExact
	// routeMethodAnyFactor applies when one side declares no method.
	routeMethodAnyFactor float32 = 0.8
	// routePathParamFactor applies per segment matched against a template
	// parameter rather than literally.
	routePathParamFactor float32 = 0.95
)

// routeMatchScoreSegs is routeMatchScore over pre-split path segments.
func routeMatchScoreSegs(callMethod string, callSegs []string, routeMethod string, routeSegs []string) float32 {
	methodScore := float32(0)
	switch {
	case callMethod == routeMethod:
		methodScore = 1.0
	case callMethod == "ANY" || routeMethod == "ANY":
		methodScore = routeMethodAnyFactor
	default:
		return 0
	}

	if len(callSegs) != len(routeSegs) {
		return 0
	}
	pathScore := float32(1.0)
	for i := range callSegs {
		if strings.EqualFold(callSegs[i], routeSegs[i]) {
			continue
		}
		if isPathParam(routeSegs[i]) || isPathParam(callSegs[i]) {
			pathScore *= routePathParamFactor
			continue
		}
		return 0
	}
	return routeBaseScore * methodScore * pathScore
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// isPathParam reports whether a path segment is a template parameter. The
// definition lives with the key builder, so that what a key reduces to and what
// route matching tolerates can never drift apart.
func isPathParam(seg string) bool { return contract.IsPathParam(seg) }

// deriveKafkaFlows joins produces and consumes edges on topic into derived
// kafka_flow edges (producer function -> consumer function).
//
// When repoID is non-empty the rebuild is incremental: only topics touched by
// that repo are affected — topics of its produces/consumes edges (after
// resolveConfigRefs has rewritten config references) plus topics of existing
// kafka_flow edges whose producer or consumer side belongs to the repo (to
// clean up flows whose producers/consumers were removed). The rewritten set
// adds the topics whose config reference resolved during this run, whichever
// repo they belong to: resolveConfigRefs works globally, so a value indexed
// here can re-point a producer of another repo. Flows for the affected topics
// are deleted and re-derived from the global produces×consumes join, since
// flow pairs are cross-repo. With an empty repoID all flows are rebuilt from
// scratch.
func (l *Linker) deriveKafkaFlows(ctx context.Context, repoID string, rewritten map[string]struct{}, stats *RunStats) error {
	produces, err := l.store.GetEdges(ctx, domain.QueryOpts{Kind: store.EdgeProduces})
	if err != nil {
		return err
	}
	consumes, err := l.store.GetEdges(ctx, domain.QueryOpts{Kind: store.EdgeConsumes})
	if err != nil {
		return err
	}

	// affected == nil means "all topics" (full rebuild).
	var affected map[string]struct{}
	if repoID != "" {
		affected = map[string]struct{}{}
		for topic := range rewritten {
			affected[topic] = struct{}{}
		}
		for _, e := range produces {
			if e.RepoID == repoID {
				affected[e.DstName] = struct{}{}
			}
		}
		for _, e := range consumes {
			if e.RepoID == repoID {
				affected[e.DstName] = struct{}{}
			}
		}
		flows, err := l.store.GetEdges(ctx, domain.QueryOpts{Kind: store.EdgeKafkaFlow})
		if err != nil {
			return err
		}
		for _, f := range flows {
			if f.RepoID == repoID || f.DstRepoID == repoID {
				affected[f.DstName] = struct{}{}
			}
		}
		if len(affected) == 0 {
			return nil
		}
		for topic := range affected {
			if err := l.store.DeleteEdgesByKindAndDst(ctx, store.EdgeKafkaFlow, topic); err != nil {
				return err
			}
		}
	} else {
		if err := l.store.DeleteEdgesByKind(ctx, "", store.EdgeKafkaFlow); err != nil {
			return err
		}
	}

	consumersByTopic := map[string][]*domain.Edge{}
	for _, c := range consumes {
		consumersByTopic[c.DstName] = append(consumersByTopic[c.DstName], c)
	}

	for _, p := range produces {
		if affected != nil {
			if _, ok := affected[p.DstName]; !ok {
				continue
			}
		}
		for _, c := range consumersByTopic[p.DstName] {
			conf := p.Confidence
			if c.Confidence < conf {
				conf = c.Confidence
			}
			flow := &domain.Edge{
				RepoID:     p.RepoID,
				SrcID:      p.SrcID,
				DstID:      c.SrcID,
				DstRepoID:  c.RepoID,
				Kind:       store.EdgeKafkaFlow,
				DstName:    p.DstName,
				FilePath:   p.FilePath,
				Line:       p.Line,
				Confidence: conf * contract.ConfExact,
				Meta:       p.Meta, // carry producer payload fields for tracing
			}
			if err := l.store.StoreEdge(ctx, flow); err != nil {
				stats.Errors++
				slog.Warn("store kafka_flow edge", "topic", p.DstName, "err", err)
				continue
			}
			stats.KafkaFlows++
		}
	}
	return nil
}
