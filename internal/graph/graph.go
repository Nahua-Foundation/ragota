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
	"path/filepath"
	"sync"
	"time"

	"aitools/internal/fileutil"
	"aitools/internal/lsp"
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
	st  *store.SQLite
	mgr *lsp.Manager // опционально; если nil — работает только tree-sitter

	mu        sync.Mutex
	callCache map[int64]cacheEntry // ленивый кэш для Callers (по unit.ID)
	implCache map[int64]cacheEntry // ленивый кэш для Implementations
}

type cacheEntry struct {
	units []store.ASTUnit
	at    time.Time
}

const cacheTTL = 5 * time.Minute

// New создаёт сервис без LSP-обогащения (только tree-sitter).
func New(st *store.SQLite) *Service {
	return &Service{
		st:        st,
		callCache: make(map[int64]cacheEntry),
		implCache: make(map[int64]cacheEntry),
	}
}

// NewWithLSP создаёт сервис с ленивым LSP-обогащением.
func NewWithLSP(st *store.SQLite, mgr *lsp.Manager) *Service {
	s := New(st)
	s.mgr = mgr
	return s
}

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

// Callers — AST units, вызывающие unitID. tree-sitter дополняется LSP
// (textDocument/references на позиции определения функции).
func (s *Service) Callers(ctx context.Context, unitID int64) ([]store.ASTUnit, error) {
	es, err := s.st.EdgesTo(ctx, unitID, EdgeCall)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(es))
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

// Implementations — реализации интерфейса interfaceID. tree-sitter дополняется
// LSP-батчем (textDocument/implementation на позиции имени интерфейса).
func (s *Service) Implementations(ctx context.Context, interfaceID int64) ([]store.ASTUnit, error) {
	es, err := s.st.EdgesTo(ctx, interfaceID, EdgeImplements)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(es))
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
	any, err := s.st.FindASTUnits(ctx, name, "", "", 1)
	if err != nil {
		return nil, err
	}
	if len(any) > 0 {
		return &any[0], nil
	}
	return nil, nil
}

// ---------- LSP enrichment ----------

// lspCallers выполняет textDocument/references на позиции определения функции
// и резолвит локации обратно в AST units. При любой ошибке возвращает nil
// (fallback на tree-sitter обеспечивается вызывающим).
func (s *Service) lspCallers(ctx context.Context, unitID int64) []store.ASTUnit {
	if s.mgr == nil {
		return nil
	}
	s.mu.Lock()
	if e, ok := s.callCache[unitID]; ok && time.Since(e.at) < cacheTTL {
		s.mu.Unlock()
		return e.units
	}
	s.mu.Unlock()

	u, err := s.st.GetASTUnit(ctx, unitID)
	if err != nil || u == nil {
		return nil
	}
	lang := fileutil.LanguageByExt(filepath.Ext(u.FilePath))
	if lang == "" {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cli, err := s.mgr.EnsureOpen(cctx, lang, u.FilePath)
	if err != nil {
		return nil
	}
	locs, err := cli.References(cctx, u.FilePath, max0(u.StartLine-1), nameColumn(u.Signature, u.Name), false)
	if err != nil || len(locs) == 0 {
		return nil
	}
	units := s.locationsToUnits(ctx, locs, true /* enclosing */)
	s.mu.Lock()
	s.callCache[unitID] = cacheEntry{units: units, at: time.Now()}
	s.mu.Unlock()
	return units
}

// lspImplementations выполняет textDocument/implementation для интерфейса и
// возвращает AST units реализаций.
func (s *Service) lspImplementations(ctx context.Context, interfaceID int64) []store.ASTUnit {
	if s.mgr == nil {
		return nil
	}
	s.mu.Lock()
	if e, ok := s.implCache[interfaceID]; ok && time.Since(e.at) < cacheTTL {
		s.mu.Unlock()
		return e.units
	}
	s.mu.Unlock()

	u, err := s.st.GetASTUnit(ctx, interfaceID)
	if err != nil || u == nil {
		return nil
	}
	lang := fileutil.LanguageByExt(filepath.Ext(u.FilePath))
	if lang == "" {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cli, err := s.mgr.EnsureOpen(cctx, lang, u.FilePath)
	if err != nil {
		return nil
	}
	locs, err := cli.Implementation(cctx, u.FilePath, max0(u.StartLine-1), nameColumn(u.Signature, u.Name))
	if err != nil || len(locs) == 0 {
		return nil
	}
	units := s.locationsToUnits(ctx, locs, false /* exact unit at position */)
	s.mu.Lock()
	s.implCache[interfaceID] = cacheEntry{units: units, at: time.Now()}
	s.mu.Unlock()
	return units
}

// locationsToUnits сопоставляет LSP-локации с AST units из SQLite.
// Если enclosing=true — ищем юнит, чей диапазон [start_line; end_line]
// содержит позицию (для callers — функцию, в теле которой есть вызов).
// Иначе берём ближайший по start_line юнит того же файла.
func (s *Service) locationsToUnits(ctx context.Context, locs []lsp.Location, enclosing bool) []store.ASTUnit {
	seen := map[int64]bool{}
	out := make([]store.ASTUnit, 0, len(locs))
	for _, l := range locs {
		path := uriToPath(l.URI)
		if path == "" {
			continue
		}
		units, err := s.st.ListASTUnitsByFile(ctx, path)
		if err != nil || len(units) == 0 {
			continue
		}
		line := l.StartLine + 1 // store хранит 1-based
		var best *store.ASTUnit
		if enclosing {
			// Самый «глубокий» (с наибольшим start_line) функциональный юнит,
			// который ещё включает строку.
			for i := range units {
				u := &units[i]
				if u.StartLine <= line && line <= u.EndLine && isFuncKind(u.Kind) {
					if best == nil || u.StartLine > best.StartLine {
						best = u
					}
				}
			}
		} else {
			for i := range units {
				u := &units[i]
				if u.StartLine == line || (u.StartLine <= line && line <= u.EndLine) {
					if best == nil || u.StartLine > best.StartLine {
						best = u
					}
				}
			}
		}
		if best == nil || seen[best.ID] {
			continue
		}
		seen[best.ID] = true
		out = append(out, *best)
	}
	return out
}

func isFuncKind(kind string) bool {
	switch kind {
	case "function", "method", "constructor":
		return true
	}
	return false
}

func mergeUnits(a, b []store.ASTUnit) []store.ASTUnit {
	seen := make(map[int64]bool, len(a)+len(b))
	out := make([]store.ASTUnit, 0, len(a)+len(b))
	for _, u := range a {
		if !seen[u.ID] {
			seen[u.ID] = true
			out = append(out, u)
		}
	}
	for _, u := range b {
		if !seen[u.ID] {
			seen[u.ID] = true
			out = append(out, u)
		}
	}
	return out
}

func uriToPath(uri string) string {
	const p = "file://"
	if len(uri) > len(p) && uri[:len(p)] == p {
		return uri[len(p):]
	}
	return uri
}

func max0(x int) int {
	if x < 0 {
		return 0
	}
	return x
}

// nameColumn эвристически вычисляет колонку (0-based) имени символа на строке
// определения. Если сигнатура содержит имя — берём индекс в сигнатуре, иначе 0.
func nameColumn(signature, name string) int {
	if name == "" {
		return 0
	}
	for i := 0; i+len(name) <= len(signature); i++ {
		if signature[i:i+len(name)] == name {
			return i
		}
	}
	return 0
}
