package graph

import (
	"context"
	"sort"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// RelatedUnit is a unit reachable from a context item through the graph.
type RelatedUnit struct {
	Unit      *domain.ASTUnit `json:"unit"`
	Service   string          `json:"service,omitempty"`
	Via       string          `json:"via"`       // edge kind of the first hop
	Direction string          `json:"direction"` // "out" (callee/target) or "in" (caller/source)
	Distance  int             `json:"distance"`  // hops from the item unit
}

// maxRelated caps graph expansion per context item.
const maxRelated = 24

// Expand walks the graph around a unit in both directions up to the given
// number of hops and returns the related units, nearest first.
//
// Traversal follows the same contract indirections as Path: rpc_method units
// expand to their implementations, http_route units to their handlers, so an
// expansion from a client call site reaches code in other services.
func (g *Graph) Expand(ctx context.Context, unitID string, hops int) ([]*RelatedUnit, error) {
	if hops <= 0 {
		hops = 1
	}
	if hops > 3 {
		hops = 3
	}

	ld := newLoader(g)
	visited := map[string]bool{unitID: true}
	var out []*RelatedUnit

	type frontierItem struct {
		id  string
		via string
	}
	frontier := []frontierItem{{id: unitID}}

	for depth := 1; depth <= hops && len(frontier) > 0 && len(out) < maxRelated; depth++ {
		var next []frontierItem
		for _, item := range frontier {
			if len(out) >= maxRelated {
				break
			}
			// Outgoing (including contract indirections).
			succ, err := g.expand(ctx, ld, item.id)
			if err != nil {
				return nil, err
			}
			for _, s := range succ {
				if s.Unit == nil || visited[s.Unit.ID] {
					continue
				}
				visited[s.Unit.ID] = true
				out = append(out, &RelatedUnit{
					Unit: s.Unit, Service: ld.serviceForUnit(ctx, s.Unit),
					Via: s.Edge.Kind, Direction: "out", Distance: depth,
				})
				next = append(next, frontierItem{id: s.Unit.ID, via: s.Edge.Kind})
			}
			// Incoming (callers, clients of this contract).
			in, err := g.store.GetEdges(ctx, domain.QueryOpts{DstID: item.id})
			if err != nil {
				return nil, err
			}
			srcIDs := make([]string, 0, len(in))
			for _, e := range in {
				srcIDs = append(srcIDs, e.SrcID)
			}
			if err := ld.unitsByIDs(ctx, srcIDs); err != nil {
				return nil, err
			}
			for _, e := range in {
				u := ld.cached(e.SrcID)
				if u == nil || visited[u.ID] {
					continue
				}
				visited[u.ID] = true
				out = append(out, &RelatedUnit{
					Unit: u, Service: ld.serviceForUnit(ctx, u),
					Via: e.Kind, Direction: "in", Distance: depth,
				})
				next = append(next, frontierItem{id: u.ID, via: e.Kind})
			}
		}
		frontier = next
	}

	if len(out) > maxRelated {
		out = out[:maxRelated]
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Distance < out[j].Distance })
	return out, nil
}

// UnitAt returns the innermost unit containing the given line of a file.
func (g *Graph) UnitAt(ctx context.Context, repoID, filePath string, line int) (*domain.ASTUnit, error) {
	return g.UnitInRange(ctx, repoID, filePath, line, line)
}

// UnitInRange returns the unit that best overlaps a line range: the one with
// the largest overlap, ties broken toward the innermost (latest-starting)
// unit. Search hits reference chunks, which rarely align with unit bounds.
func (g *Graph) UnitInRange(ctx context.Context, repoID, filePath string, startLine, endLine int) (*domain.ASTUnit, error) {
	if endLine < startLine {
		endLine = startLine
	}
	units, err := g.store.GetASTUnits(ctx, domain.QueryOpts{RepoID: repoID, FilePath: filePath, Limit: 500})
	if err != nil {
		return nil, err
	}
	var best *domain.ASTUnit
	bestOverlap := 0
	for _, u := range units {
		lo, hi := max(u.StartLine, startLine), min(u.EndLine, endLine)
		overlap := hi - lo + 1
		if overlap <= 0 {
			continue
		}
		if overlap > bestOverlap || (overlap == bestOverlap && best != nil && u.StartLine > best.StartLine) {
			best, bestOverlap = u, overlap
		}
	}
	if best == nil {
		return nil, store.ErrNotFound
	}
	return best, nil
}
