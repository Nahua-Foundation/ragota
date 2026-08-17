package graph

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/storage"
	"github.com/Nahua-Foundation/ragota/internal/svcdetect"
)

// Graph queries the linked code graph.
type Graph struct {
	store storage.Storage
}

// New creates a Graph over the given storage.
func New(store storage.Storage) *Graph {
	return &Graph{store: store}
}

// Node is a graph node (an AST unit plus its owning service).
type Node struct {
	Unit    *storage.ASTUnit `json:"unit"`
	Service string           `json:"service,omitempty"`
}

// NeighborsResult holds the neighborhood of a unit.
type NeighborsResult struct {
	Center *Node           `json:"center"`
	Out    []*EdgeWithUnit `json:"out"`
	In     []*EdgeWithUnit `json:"in"`
}

// EdgeWithUnit is an edge together with the unit on its far side.
type EdgeWithUnit struct {
	Edge *storage.Edge    `json:"edge"`
	Unit *storage.ASTUnit `json:"unit,omitempty"` // nil if unresolved
}

// Neighbors returns edges in and out of a unit.
func (g *Graph) Neighbors(ctx context.Context, unitID string) (*NeighborsResult, error) {
	ld := newLoader(g)
	unit, err := ld.unit(ctx, unitID)
	if err != nil {
		return nil, err
	}
	res := &NeighborsResult{Center: &Node{Unit: unit, Service: ld.serviceOf(ctx, unit)}}

	out, err := g.store.GetEdges(ctx, storage.QueryOpts{SrcID: unitID})
	if err != nil {
		return nil, err
	}
	in, err := g.store.GetEdges(ctx, storage.QueryOpts{DstID: unitID})
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(out)+len(in))
	for _, e := range out {
		ids = append(ids, e.DstID)
	}
	for _, e := range in {
		ids = append(ids, e.SrcID)
	}
	if err := ld.unitsByIDs(ctx, ids); err != nil {
		return nil, err
	}

	for _, e := range out {
		ew := &EdgeWithUnit{Edge: e}
		if e.DstID != "" {
			ew.Unit = ld.cached(e.DstID)
		}
		res.Out = append(res.Out, ew)
	}
	for _, e := range in {
		res.In = append(res.In, &EdgeWithUnit{Edge: e, Unit: ld.cached(e.SrcID)})
	}

	return res, nil
}

// PathStep is one hop of a path through the graph.
type PathStep struct {
	Edge *storage.Edge    `json:"edge,omitempty"` // nil for the starting node
	Unit *storage.ASTUnit `json:"unit"`
	Via  string           `json:"via,omitempty"` // edge kind that led here
}

// Path finds a directed path between two units using BFS over resolved edges.
// rpc_method units are additionally expanded to their server implementations
// (reverse implements_rpc), so paths cross service boundaries.
func (g *Graph) Path(ctx context.Context, fromID, toID string, maxDepth int) ([]*PathStep, error) {
	if maxDepth <= 0 {
		maxDepth = 10
	}
	ld := newLoader(g)
	start, err := ld.unit(ctx, fromID)
	if err != nil {
		return nil, err
	}

	type queueItem struct {
		unitID string
		path   []*PathStep
	}
	visited := map[string]bool{fromID: true}
	queue := []queueItem{{unitID: fromID, path: []*PathStep{{Unit: start}}}}

	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		if len(item.path) > maxDepth {
			continue
		}
		if item.unitID == toID {
			return item.path, nil
		}
		next, err := g.expand(ctx, ld, item.unitID)
		if err != nil {
			return nil, err
		}
		for _, n := range next {
			if n.Unit == nil || visited[n.Unit.ID] {
				continue
			}
			visited[n.Unit.ID] = true
			path := append(append([]*PathStep{}, item.path...), &PathStep{
				Edge: n.Edge, Unit: n.Unit, Via: n.Edge.Kind,
			})
			if n.Unit.ID == toID {
				return path, nil
			}
			queue = append(queue, queueItem{unitID: n.Unit.ID, path: path})
		}
	}
	return nil, storage.ErrNotFound
}

// expand returns the traversable successors of a unit: resolved out-edges,
// plus reverse implements_rpc for rpc_method units (contract -> server impl)
// and reverse handled_by is already directional (route -> handler).
// Unit lookups go through the loader, so repeated expansions within one
// operation are batched and cached.
func (g *Graph) expand(ctx context.Context, ld *loader, unitID string) ([]*EdgeWithUnit, error) {
	out, err := g.store.GetEdges(ctx, storage.QueryOpts{SrcID: unitID})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out)+1)
	ids = append(ids, unitID)
	for _, e := range out {
		if e.DstID == "" || e.Kind == storage.EdgeImplementsRPC {
			continue // implements_rpc is traversed in reverse
		}
		ids = append(ids, e.DstID)
	}
	if err := ld.unitsByIDs(ctx, ids); err != nil {
		return nil, err
	}

	var result []*EdgeWithUnit
	for _, e := range out {
		if e.DstID == "" || e.Kind == storage.EdgeImplementsRPC {
			continue
		}
		if u := ld.cached(e.DstID); u != nil {
			result = append(result, &EdgeWithUnit{Edge: e, Unit: u})
		}
	}

	unit := ld.cached(unitID)
	if unit == nil {
		return result, nil
	}
	if unit.Kind == storage.KindRPCMethod {
		impls, err := g.store.GetEdges(ctx, storage.QueryOpts{DstID: unitID, Kind: storage.EdgeImplementsRPC})
		if err == nil {
			srcIDs := make([]string, 0, len(impls))
			for _, e := range impls {
				srcIDs = append(srcIDs, e.SrcID)
			}
			if err := ld.unitsByIDs(ctx, srcIDs); err == nil {
				for _, e := range impls {
					if u := ld.cached(e.SrcID); u != nil {
						result = append(result, &EdgeWithUnit{Edge: e, Unit: u})
					}
				}
			}
		}
	}
	return result, nil
}

// ServiceInfo describes a detected service.
type ServiceInfo struct {
	RepoID     string `json:"repo_id"`
	Name       string `json:"name"`
	Root       string `json:"root"`
	DetectedBy string `json:"detected_by,omitempty"`
	UnitID     string `json:"unit_id"`
}

// ServiceLink is an aggregated connection between two services.
type ServiceLink struct {
	SrcRepo    string  `json:"src_repo"`
	SrcService string  `json:"src_service"`
	DstRepo    string  `json:"dst_repo"`
	DstService string  `json:"dst_service"`
	Kind       string  `json:"kind"` // rpc_call | http_call | kafka_flow
	Via        string  `json:"via"`  // contract key: grpc:..., http:..., topic:...
	Count      int     `json:"count"`
	Confidence float32 `json:"confidence"` // max confidence seen
}

// ServicesGraph lists all services and the aggregated links between them.
func (g *Graph) ServicesGraph(ctx context.Context) ([]*ServiceInfo, []*ServiceLink, error) {
	svcUnits, err := g.store.GetASTUnits(ctx, storage.QueryOpts{Kind: storage.KindService})
	if err != nil {
		return nil, nil, err
	}
	var services []*ServiceInfo
	for _, u := range svcUnits {
		root, detectedBy := serviceUnitInfo(u)
		services = append(services, &ServiceInfo{
			RepoID:     u.RepoID,
			Name:       u.Name,
			Root:       root,
			DetectedBy: detectedBy,
			UnitID:     u.ID,
		})
	}

	edges, err := g.store.GetEdges(ctx, storage.QueryOpts{
		Kinds: []string{storage.EdgeRPCCall, storage.EdgeHTTPCall, storage.EdgeKafkaFlow, storage.EdgeRuntimeCall},
	})
	if err != nil {
		return nil, nil, err
	}

	ld := newLoader(g)
	ids := make([]string, 0, 2*len(edges))
	for _, e := range edges {
		if e.DstID == "" {
			continue
		}
		ids = append(ids, e.SrcID, e.DstID)
	}
	if err := ld.unitsByIDs(ctx, ids); err != nil {
		return nil, nil, err
	}

	// For rpc_call destinations, resolve the contract to its server
	// implementation with a single global implements_rpc query.
	implOf := map[string]string{} // rpc_method unit ID -> implementing unit ID
	hasRPCDst := false
	for _, e := range edges {
		if u := ld.cached(e.DstID); u != nil && u.Kind == storage.KindRPCMethod {
			hasRPCDst = true
			break
		}
	}
	if hasRPCDst {
		implEdges, err := g.store.GetEdges(ctx, storage.QueryOpts{Kinds: []string{storage.EdgeImplementsRPC}})
		if err != nil {
			return nil, nil, err
		}
		// A contract may collect several implements_rpc edges — a registered
		// implementation and weaker guesses. Take the most confident one; ties
		// go to the first, which sqlutil.EdgeOrder makes the one earliest in the
		// corpus. It used to make it the one whose row was written first, and
		// that is the order concurrent indexing left behind rather than anything
		// the sources say.
		implConf := map[string]float32{}
		implIDs := make([]string, 0, len(implEdges))
		for _, ie := range implEdges {
			if _, ok := implOf[ie.DstID]; ok && ie.Confidence <= implConf[ie.DstID] {
				continue
			}
			implOf[ie.DstID] = ie.SrcID
			implConf[ie.DstID] = ie.Confidence
			implIDs = append(implIDs, ie.SrcID)
		}
		if err := ld.unitsByIDs(ctx, implIDs); err != nil {
			return nil, nil, err
		}
	}

	links := map[string]*ServiceLink{}
	for _, e := range edges {
		if e.DstID == "" {
			continue
		}
		srcUnit := ld.cached(e.SrcID)
		if srcUnit == nil {
			continue
		}
		dstUnit := ld.cached(e.DstID)
		if dstUnit == nil {
			continue
		}
		// For rpc_call the destination contract may live next to the server
		// implementation; prefer the implementing method's location.
		if dstUnit.Kind == storage.KindRPCMethod {
			if impl := ld.cached(implOf[dstUnit.ID]); impl != nil {
				dstUnit = impl
			}
		}
		srcSvc := ld.serviceForUnit(ctx, srcUnit)
		dstSvc := ld.serviceForUnit(ctx, dstUnit)
		if srcSvc == dstSvc && srcUnit.RepoID == dstUnit.RepoID {
			continue // intra-service call, not part of the service graph
		}
		via := canonicalVia(e.DstName)
		key := strings.Join([]string{srcUnit.RepoID, srcSvc, dstUnit.RepoID, dstSvc, e.Kind, via}, "\x00")
		l, ok := links[key]
		if !ok {
			l = &ServiceLink{
				SrcRepo: srcUnit.RepoID, SrcService: srcSvc,
				DstRepo: dstUnit.RepoID, DstService: dstSvc,
				Kind: e.Kind, Via: via,
			}
			links[key] = l
		}
		l.Count++
		if e.Confidence > l.Confidence {
			l.Confidence = e.Confidence
		}
	}

	var out []*ServiceLink
	for _, l := range links {
		out = append(out, l)
	}
	// Order by the full aggregation key, not just (SrcService, Via): links that
	// tie on those two but differ in repo/destination/kind would otherwise come
	// out in map-iteration order, making the exported graph nondeterministic.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.SrcService != b.SrcService {
			return a.SrcService < b.SrcService
		}
		if a.DstService != b.DstService {
			return a.DstService < b.DstService
		}
		if a.Via != b.Via {
			return a.Via < b.Via
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.SrcRepo != b.SrcRepo {
			return a.SrcRepo < b.SrcRepo
		}
		return a.DstRepo < b.DstRepo
	})
	return services, out, nil
}

// canonicalVia normalizes the contract key a service link is aggregated on.
// HTTP routes are matched case-insensitively, so "http:GET /API/Users" and
// "http:GET /api/users" describe one link and must not produce two rows.
func canonicalVia(dstName string) string {
	method, path, ok := contract.ParseHTTP(dstName)
	if !ok {
		return dstName
	}
	return contract.HTTP(method, strings.ToLower(path))
}

// TopicInfo describes a Kafka topic with its producers and consumers.
type TopicInfo struct {
	Topic       string  `json:"topic"`
	Producers   []*Node `json:"producers"`
	Consumers   []*Node `json:"consumers"`
	Description string  `json:"description,omitempty"` // from an AsyncAPI channel declaration
	Declared    bool    `json:"declared,omitempty"`    // topic is declared in an AsyncAPI spec
}

// Topics aggregates produces/consumes edges into a topic list. When service
// is non-empty, only topics that service produces or consumes are returned.
func (g *Graph) Topics(ctx context.Context, service string) ([]*TopicInfo, error) {
	edges, err := g.store.GetEdges(ctx, storage.QueryOpts{
		Kinds: []string{storage.EdgeProduces, storage.EdgeConsumes},
	})
	if err != nil {
		return nil, err
	}
	ld := newLoader(g)
	ids := make([]string, 0, len(edges))
	for _, e := range edges {
		ids = append(ids, e.SrcID)
	}
	if err := ld.unitsByIDs(ctx, ids); err != nil {
		return nil, err
	}

	byTopic := map[string]*TopicInfo{}
	for _, e := range edges {
		topic := contract.TrimKind(e.DstName, contract.KindTopic)
		ti, ok := byTopic[topic]
		if !ok {
			ti = &TopicInfo{Topic: topic}
			byTopic[topic] = ti
		}
		unit := ld.cached(e.SrcID)
		if unit == nil {
			continue
		}
		node := &Node{Unit: unit, Service: ld.serviceForUnit(ctx, unit)}
		if e.Kind == storage.EdgeProduces {
			ti.Producers = append(ti.Producers, node)
		} else {
			ti.Consumers = append(ti.Consumers, node)
		}
	}
	// Overlay AsyncAPI channel declarations: annotate topics found in code,
	// and surface declared channels that no code produces or consumes — a
	// useful signal that the spec and the code have drifted apart.
	declared, err := g.store.GetASTUnits(ctx, storage.QueryOpts{Kind: storage.KindTopicChannel})
	if err != nil {
		return nil, err
	}
	for _, u := range declared {
		if ti, ok := byTopic[u.Name]; ok {
			ti.Declared = true
			if ti.Description == "" {
				ti.Description = u.Doc
			}
			continue
		}
		byTopic[u.Name] = &TopicInfo{Topic: u.Name, Description: u.Doc, Declared: true}
	}

	var out []*TopicInfo
	for _, ti := range byTopic {
		if service != "" && !topicInvolves(ti, service) {
			continue
		}
		out = append(out, ti)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Topic < out[j].Topic })
	return out, nil
}

func topicInvolves(ti *TopicInfo, service string) bool {
	for _, n := range ti.Producers {
		if n.Service == service {
			return true
		}
	}
	for _, n := range ti.Consumers {
		if n.Service == service {
			return true
		}
	}
	return false
}

// FindUnit locates a unit by repo and name (optionally qualified name).
func (g *Graph) FindUnit(ctx context.Context, repoID, name string) (*storage.ASTUnit, error) {
	units, err := g.store.GetASTUnits(ctx, storage.QueryOpts{RepoID: repoID, Name: name, Limit: 10})
	if err != nil {
		return nil, err
	}
	if len(units) == 0 {
		units, err = g.store.GetASTUnits(ctx, storage.QueryOpts{RepoID: repoID, Qualified: name, Limit: 10})
		if err != nil {
			return nil, err
		}
	}
	if len(units) == 0 {
		return nil, storage.ErrNotFound
	}
	// Prefer callable units.
	for _, u := range units {
		if u.Kind == "function" || u.Kind == "method" {
			return u, nil
		}
	}
	return units[0], nil
}

// loader is a request-scoped cache that batches unit lookups and memoizes
// per-repo service lists, avoiding N+1 storage queries inside one operation.
type loader struct {
	g        *Graph
	units    map[string]*storage.ASTUnit    // unit ID -> unit; nil value = known missing
	services map[string][]svcdetect.Service // repo ID -> detected services
}

func newLoader(g *Graph) *loader {
	return &loader{
		g:        g,
		units:    map[string]*storage.ASTUnit{},
		services: map[string][]svcdetect.Service{},
	}
}

// unitsByIDs ensures all given IDs are cached, loading the missing ones with
// a single batch query. Unknown IDs are cached as missing.
func (l *loader) unitsByIDs(ctx context.Context, ids []string) error {
	var missing []string
	requested := map[string]bool{}
	for _, id := range ids {
		if id == "" || requested[id] {
			continue
		}
		requested[id] = true
		if _, ok := l.units[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	units, err := l.g.store.GetASTUnitsByIDs(ctx, missing)
	if err != nil {
		return err
	}
	for _, u := range units {
		l.units[u.ID] = u
	}
	for _, id := range missing {
		if _, ok := l.units[id]; !ok {
			l.units[id] = nil // known missing
		}
	}
	return nil
}

// unit returns a single unit, loading it on a cache miss.
func (l *loader) unit(ctx context.Context, id string) (*storage.ASTUnit, error) {
	if u, ok := l.units[id]; ok {
		if u == nil {
			return nil, storage.ErrNotFound
		}
		return u, nil
	}
	u, err := l.g.store.GetASTUnitByID(ctx, id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			l.units[id] = nil
		}
		return nil, err
	}
	l.units[id] = u
	return u, nil
}

// cached returns the already-loaded unit for id, or nil if unknown/missing.
func (l *loader) cached(id string) *storage.ASTUnit {
	return l.units[id]
}

// servicesOf returns the detected services of a repo, cached per repo.
// Errors are swallowed (mirroring Graph.serviceOf) and cached as empty.
func (l *loader) servicesOf(ctx context.Context, repoID string) []svcdetect.Service {
	if svcs, ok := l.services[repoID]; ok {
		return svcs
	}
	var svcs []svcdetect.Service
	svcUnits, err := l.g.store.GetASTUnits(ctx, storage.QueryOpts{RepoID: repoID, Kind: storage.KindService})
	if err == nil {
		for _, su := range svcUnits {
			root, _ := serviceUnitInfo(su)
			svcs = append(svcs, svcdetect.Service{
				Name: su.Name,
				Root: root,
			})
		}
	}
	l.services[repoID] = svcs
	return svcs
}

// serviceOf mirrors Graph.serviceOf through the cache: the name of the
// service owning a unit, or "" if the repo has no detected services.
func (l *loader) serviceOf(ctx context.Context, unit *storage.ASTUnit) string {
	svcs := l.servicesOf(ctx, unit.RepoID)
	if len(svcs) == 0 {
		return ""
	}
	return svcdetect.ServiceFor(svcs, unit.FilePath)
}

// serviceForUnit mirrors Graph.serviceForUnit through the cache.
func (l *loader) serviceForUnit(ctx context.Context, unit *storage.ASTUnit) string {
	if unit.Kind == storage.KindService {
		return unit.Name
	}
	if name := l.serviceOf(ctx, unit); name != "" {
		return name
	}
	return unit.RepoID
}

// serviceUnitInfo extracts Root and DetectedBy from a service unit, preferring
// structured UnitMeta and falling back to the legacy Signature "root:<dir>" /
// Doc conventions for data indexed before Meta was introduced.
func serviceUnitInfo(u *storage.ASTUnit) (root, detectedBy string) {
	meta := storage.DecodeUnitMeta(u.Meta)
	root = meta.Root
	if root == "" {
		root = strings.TrimPrefix(u.Signature, "root:")
	}
	detectedBy = meta.DetectedBy
	if detectedBy == "" {
		detectedBy = u.Doc
	}
	return root, detectedBy
}

// serviceOf returns the service name owning a unit and the services of its repo.
func (g *Graph) serviceOf(ctx context.Context, unit *storage.ASTUnit) (string, []svcdetect.Service) {
	svcUnits, err := g.store.GetASTUnits(ctx, storage.QueryOpts{RepoID: unit.RepoID, Kind: storage.KindService})
	if err != nil || len(svcUnits) == 0 {
		return "", nil
	}
	services := make([]svcdetect.Service, 0, len(svcUnits))
	for _, su := range svcUnits {
		root, _ := serviceUnitInfo(su)
		services = append(services, svcdetect.Service{
			Name: su.Name,
			Root: root,
		})
	}
	return svcdetect.ServiceFor(services, unit.FilePath), services
}

func (g *Graph) serviceForUnit(ctx context.Context, unit *storage.ASTUnit) string {
	if unit.Kind == storage.KindService {
		return unit.Name
	}
	name, _ := g.serviceOf(ctx, unit)
	if name == "" {
		return unit.RepoID
	}
	return name
}

// ServiceOfUnit returns the service name owning a unit (repo ID if unknown).
func (g *Graph) ServiceOfUnit(ctx context.Context, unit *storage.ASTUnit) string {
	return g.serviceForUnit(ctx, unit)
}
