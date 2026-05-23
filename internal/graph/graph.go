// Package graph — code-graph (calls/imports/implementations/extends/references)
// поверх таблиц store.ast_units и store.edges.
//
// Используется для:
//   - find_callers / find_callees
//   - find_implementations
//   - find_references
//   - expand_neighbors (graph expansion вокруг найденных hit'ов)
//   - get_dependency_graph, get_call_graph
package graph

import (
	"context"

	"aitools/internal/store"
)

// EdgeKind — типы рёбер.
const (
	EdgeCall       = "call"
	EdgeImport     = "import"
	EdgeImplements = "implements"
	EdgeExtends    = "extends"
	EdgeReference  = "reference"
	EdgeContains   = "contains"
)

// Neighborhood — результат expand_neighbors.
type Neighborhood struct {
	Nodes []store.ASTUnit `json:"nodes"`
	Edges []store.Edge    `json:"edges"`
}

// Service — высокоуровневый API кода-графа.
type Service struct {
	st *store.SQLite
}

// New создаёт сервис.
func New(st *store.SQLite) *Service { return &Service{st: st} }

// nodesByIDs подгружает AST units по списку id, сохраняя порядок и убирая
// дубликаты.
func (s *Service) nodesByIDs(ctx context.Context, ids []int64) ([]store.ASTUnit, error) {
	seen := map[int64]bool{}
	out := make([]store.ASTUnit, 0, len(ids))
	for _, id := range ids {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		u, err := s.st.GetASTUnit(ctx, id)
		if err != nil {
			return nil, err
		}
		if u != nil {
			out = append(out, *u)
		}
	}
	return out, nil
}

// Callers — AST units, вызывающие unitID (входящие edges kind=call).
func (s *Service) Callers(ctx context.Context, unitID int64) ([]store.ASTUnit, error) {
	es, err := s.st.EdgesTo(ctx, unitID, EdgeCall)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(es))
	for _, e := range es {
		ids = append(ids, e.SrcID)
	}
	return s.nodesByIDs(ctx, ids)
}

// Callees — AST units, которые вызывает unitID (исходящие edges kind=call).
func (s *Service) Callees(ctx context.Context, unitID int64) ([]store.ASTUnit, error) {
	es, err := s.st.EdgesFrom(ctx, unitID, EdgeCall)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(es))
	for _, e := range es {
		ids = append(ids, e.DstID)
	}
	return s.nodesByIDs(ctx, ids)
}

// Implementations — реализации интерфейса interfaceID (входящие kind=implements).
func (s *Service) Implementations(ctx context.Context, interfaceID int64) ([]store.ASTUnit, error) {
	es, err := s.st.EdgesTo(ctx, interfaceID, EdgeImplements)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(es))
	for _, e := range es {
		ids = append(ids, e.SrcID)
	}
	return s.nodesByIDs(ctx, ids)
}

// References — все входящие рёбра (любого вида, кроме contains).
func (s *Service) References(ctx context.Context, unitID int64) ([]store.Edge, error) {
	es, err := s.st.EdgesTo(ctx, unitID, "")
	if err != nil {
		return nil, err
	}
	out := make([]store.Edge, 0, len(es))
	for _, e := range es {
		if e.Kind == EdgeContains {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// ExpandNeighbors — делегирует SQLite BFS-обходчику.
func (s *Service) ExpandNeighbors(ctx context.Context, nodeID int64, depth int, kinds []string) (*Neighborhood, error) {
	nodes, edges, err := s.st.ExpandNeighbors(ctx, nodeID, depth, kinds)
	if err != nil {
		return nil, err
	}
	return &Neighborhood{Nodes: nodes, Edges: edges}, nil
}

// DependencyGraph — обход по рёбрам kind=import.
// modulePath трактуется как имя/qualified модуля (для Go — package, для
// прочих языков — basename файла).
func (s *Service) DependencyGraph(ctx context.Context, modulePath string, depth int) (*Neighborhood, error) {
	root, err := s.findModuleNode(ctx, modulePath)
	if err != nil {
		return nil, err
	}
	if root == nil {
		return &Neighborhood{}, nil
	}
	return s.ExpandNeighbors(ctx, root.ID, depth, []string{EdgeImport})
}

// CallGraph — обход по рёбрам kind=call.
func (s *Service) CallGraph(ctx context.Context, functionID int64, depth int) (*Neighborhood, error) {
	return s.ExpandNeighbors(ctx, functionID, depth, []string{EdgeCall})
}

func (s *Service) findModuleNode(ctx context.Context, name string) (*store.ASTUnit, error) {
	units, err := s.st.FindASTUnits(ctx, name, "module", "", 1)
	if err != nil {
		return nil, err
	}
	if len(units) > 0 {
		return &units[0], nil
	}
	// fallback — любая единица с таким именем.
	any, err := s.st.FindASTUnits(ctx, name, "", "", 1)
	if err != nil {
		return nil, err
	}
	if len(any) > 0 {
		return &any[0], nil
	}
	return nil, nil
}
