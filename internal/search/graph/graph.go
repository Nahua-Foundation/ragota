// Package graph — code-graph (calls/imports/implementations/extends/references)
// поверх таблиц store.ast_units и store.edges.
//
// Используется для:
//   - find_callers / find_callees
//   - find_implementations
//   - find_references
//   - expand_neighbors (graph expansion вокруг найденных hit'ов)
//   - get_dependency_graph, get_call_graph
//
// Источники данных:
//   - tree-sitter — базовый слой (быстро, всегда доступен).
//   - LSP — ленивое обогащение по запросу: для call-рёбер используется
//     textDocument/references на позиции функции; для implements/extends —
//     батчем через textDocument/implementation.
//
// Поведение: если LSP-клиент недоступен или вернул ошибку/таймаут, всегда
// возвращается результат tree-sitter (fallback). Результаты LSP мёрджатся
// с tree-sitter (дедупликация по ID).
package graph

import (
	"context"
	"strings"

	"ragota/pkg/logger"
	"ragota/internal/store"
)

// nodesByIDs подгружает AST units по списку id, сохраняя порядок и убирая
// дубликаты.
func (s *Service) nodesByIDs(ctx context.Context, ids []int) ([]store.ASTUnit, error) {
	seen := map[int]struct{}{}
	out := make([]store.ASTUnit, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
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

// Callers — AST units, вызывающие unitID. tree-sitter дополняется LSP
// (textDocument/references на позиции определения функции).
func (s *Service) Callers(ctx context.Context, unitID int) ([]store.ASTUnit, error) {
	es, err := s.st.EdgesTo(ctx, unitID, EdgeCall)
	if err != nil {
		return nil, err
	}
	ids := make([]int, 0, len(es))
	for _, e := range es {
		ids = append(ids, e.SrcID)
	}
	base, err := s.nodesByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	// Ленивое обогащение через LSP (fallback на tree-sitter при любой ошибке).
	if extra := s.lspCallers(ctx, unitID); len(extra) > 0 {
		base = mergeUnits(base, extra)
	}
	return base, nil
}

// Callees — AST units, которые вызывает unitID (исходящие edges kind=call).
// Для исходящих вызовов LSP-обогащение неэффективно (нужно резолвить каждый
// идентификатор в теле функции), поэтому используем только tree-sitter.
func (s *Service) Callees(ctx context.Context, unitID int) ([]store.ASTUnit, error) {
	es, err := s.st.EdgesFrom(ctx, unitID, EdgeCall)
	if err != nil {
		return nil, err
	}
	ids := make([]int, 0, len(es))
	for _, e := range es {
		ids = append(ids, e.DstID)
	}
	return s.nodesByIDs(ctx, ids)
}

// Implementations — реализации интерфейса interfaceID. tree-sitter дополняется
// LSP-батчем (textDocument/implementation на позиции имени интерфейса).
func (s *Service) Implementations(ctx context.Context, interfaceID int) ([]store.ASTUnit, error) {
	es, err := s.st.EdgesTo(ctx, interfaceID, EdgeImplements)
	if err != nil {
		return nil, err
	}
	ids := make([]int, 0, len(es))
	for _, e := range es {
		ids = append(ids, e.SrcID)
	}
	// Дополним extends'ами — для не-интерфейсных языков (Python/TS) часто
	// используется наследование.
	if esExt, err := s.st.EdgesTo(ctx, interfaceID, EdgeExtends); err == nil {
		for _, e := range esExt {
			ids = append(ids, e.SrcID)
		}
	}
	base, err := s.nodesByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	if extra := s.lspImplementations(ctx, interfaceID); len(extra) > 0 {
		base = mergeUnits(base, extra)
	}
	return base, nil
}

// References — возвращает рёбра, указывающие на данный юнит (ссылки, реализации, наследование, вызовы).
func (s *Service) References(ctx context.Context, unitID int) ([]store.Edge, error) {
	var out []store.Edge
	for _, kind := range []string{EdgeReference, EdgeImplements, EdgeExtends, EdgeCall} {
		es, err := s.st.EdgesTo(ctx, unitID, kind)
		if err == nil {
			out = append(out, es...)
		}
	}
	return out, nil
}

// ExpandNeighbors — делегирует SQLite BFS-обходчику.
func (s *Service) ExpandNeighbors(ctx context.Context, nodeID int, depth int, kinds []string) (*Neighborhood, error) {
	nodes, edges, err := s.st.ExpandNeighbors(ctx, nodeID, depth, kinds)
	if err != nil {
		return nil, err
	}
	return &Neighborhood{Nodes: nodes, Edges: edges}, nil
}

// DependencyGraph — обход по рёбрам kind=import.
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
func (s *Service) CallGraph(ctx context.Context, functionID int, depth int) (*Neighborhood, error) {
	return s.ExpandNeighbors(ctx, functionID, depth, []string{EdgeCall})
}

// TraverseGraph — направленный семантический обход по графу.
func (s *Service) TraverseGraph(ctx context.Context, startID int, depth int, kinds []string) (*TraverseResult, error) {
	nodes, edges, err := s.st.TraverseGraph(ctx, startID, depth, kinds)
	if err != nil {
		return nil, err
	}
	return &TraverseResult{Nodes: nodes, Edges: edges}, nil
}

// GetExecutionContext собирает богатый контекст вокруг символа.
func (s *Service) GetExecutionContext(ctx context.Context, symbolID int) (*ExecutionContext, error) {
	u, err := s.st.GetASTUnit(ctx, symbolID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, nil
	}

	ctxRes := &ExecutionContext{
		Definition: u,
	}

	// 1. Callers
	callers, err := s.Callers(ctx, symbolID)
	if err == nil {
		ctxRes.Callers = callers
	}

	// 2. Callees
	callees, err := s.Callees(ctx, symbolID)
	if err == nil {
		ctxRes.Callees = callees
	}

	// 3. References (LSP + TS edges) — M3: log errors instead of silent ignore
	refs, err := s.st.EdgesTo(ctx, symbolID, EdgeReference)
	if err != nil {
		logger.Log().Warn().Err(err).Int("symbol_id", symbolID).Msg("graph: EdgesTo(Reference) failed")
	}
	ctxRes.References = refs

	// 4. Related Types (Implements, Extends)
	impls, err := s.Implementations(ctx, symbolID)
	if err != nil {
		logger.Log().Warn().Err(err).Int("symbol_id", symbolID).Msg("graph: Implementations failed")
	}
	ctxRes.RelatedTypes = append(ctxRes.RelatedTypes, impls...)

	// Добавляем то, на что САМ символ ссылается (extends/implements)
	outEdges, err := s.st.EdgesFrom(ctx, symbolID, "")
	if err != nil {
		logger.Log().Warn().Err(err).Int("symbol_id", symbolID).Msg("graph: EdgesFrom failed")
	}
	for _, e := range outEdges {
		if e.Kind == EdgeExtends || e.Kind == EdgeImplements || e.Kind == EdgeReference {
			if e.DstID != 0 {
				du, err := s.st.GetASTUnit(ctx, e.DstID)
				if err != nil {
					logger.Log().Warn().Err(err).Int("dst_id", e.DstID).Msg("graph: GetASTUnit failed")
				}
				if du != nil {
					ctxRes.RelatedTypes = append(ctxRes.RelatedTypes, *du)
				}
			}
		}
		if e.Kind == EdgeImport {
			if e.DstID != 0 {
				iu, err := s.st.GetASTUnit(ctx, e.DstID)
				if err != nil {
					logger.Log().Warn().Err(err).Int("dst_id", e.DstID).Msg("graph: GetASTUnit(imports) failed")
				}
				if iu != nil {
					ctxRes.Imports = append(ctxRes.Imports, *iu)
				}
			}
		}
	}

	// 5. Important Files
	fileMap := make(map[string]struct{})
	fileMap[u.FilePath] = struct{}{}
	for _, c := range ctxRes.Callers {
		fileMap[c.FilePath] = struct{}{}
	}
	for _, c := range ctxRes.Callees {
		fileMap[c.FilePath] = struct{}{}
	}
	for _, r := range ctxRes.RelatedTypes {
		fileMap[r.FilePath] = struct{}{}
	}
	for _, i := range ctxRes.Imports {
		fileMap[i.FilePath] = struct{}{}
	}

	for f := range fileMap {
		ctxRes.ImportantFiles = append(ctxRes.ImportantFiles, f)
	}

	return ctxRes, nil
}

func (s *Service) findModuleNode(ctx context.Context, name string) (*store.ASTUnit, error) {
	// 1. Точное совпадение (по имени или qualified).
	units, err := s.st.FindASTUnits(ctx, name, "module", "", "", 1)
	if err != nil {
		return nil, err
	}
	if len(units) > 0 {
		return &units[0], nil
	}

	// 2. Если name — путь, пробуем найти модуль, чей FilePath содержит этот путь.
	if strings.Contains(name, "/") || strings.Contains(name, "\\") {
		units, err := s.st.FindModuleUnits(ctx, name, 1)
		if err == nil && len(units) > 0 {
			return &units[0], nil
		}
	}

	// 3. Fallback на любой тип с таким именем.
	any, err := s.st.FindASTUnits(ctx, name, "", "", "", 1)
	if err != nil {
		return nil, err
	}
	if len(any) > 0 {
		return &any[0], nil
	}
	return nil, nil
}
