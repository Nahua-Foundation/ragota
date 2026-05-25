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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ragota/internal/config"
	"ragota/internal/fileutil"
	"ragota/internal/lsp"
	"ragota/internal/store"
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

// SymbolSummary — результат get_symbol_summary.
type SymbolSummary struct {
	// Deterministic
	Name      string   `json:"name"`
	Signature string   `json:"signature"`
	Calls     []string `json:"calls"`
	Callers   []string `json:"callers"`
	// Semantic (LLM)
	Purpose    string `json:"purpose"`
	Role       string `json:"role"`
	Importance string `json:"importance"`
}

// FileIntent — результат get_file_intent.
type FileIntent struct {
	// Tree-sitter extract
	Symbols []string `json:"symbols"`
	Imports []string `json:"imports"`
	// LLM extract
	Purpose          string   `json:"purpose"`
	Layer            string   `json:"layer"`
	Responsibilities []string `json:"responsibilities"`
}

// SemanticNeighborhood — результат get_semantic_neighborhood.
type SemanticNeighborhood struct {
	// Step 1: Deterministic
	Center    string          `json:"center"`
	Neighbors NeighborhoodMap `json:"neighbors"`
	// Step 2: LLM (Optional)
	Cluster      string   `json:"cluster,omitempty"`
	Core         []string `json:"core,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
	Boundary     []string `json:"boundary,omitempty"`
}

type NeighborhoodMap struct {
	DirectCalls []string `json:"direct_calls"`
	Callers     []string `json:"callers"`
	Types       []string `json:"types"`
}

// ExecutionContext — результат get_execution_context.
type ExecutionContext struct {
	Definition     *store.ASTUnit  `json:"definition"`
	Callers        []store.ASTUnit `json:"callers"`
	Callees        []store.ASTUnit `json:"callees"`
	References     []store.Edge    `json:"references"`
	RelatedTypes   []store.ASTUnit `json:"related_types"`
	Imports        []store.ASTUnit `json:"imports"`
	ImportantFiles []string        `json:"important_files"`
}

// Service — высокоуровневый API кода-графа.
type Service struct {
	cfg *config.Config
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
func New(cfg *config.Config, st *store.SQLite) *Service {
	return &Service{
		cfg:       cfg,
		st:        st,
		callCache: make(map[int64]cacheEntry),
		implCache: make(map[int64]cacheEntry),
	}
}

// NewWithLSP создаёт сервис с ленивым LSP-обогащением.
func NewWithLSP(cfg *config.Config, st *store.SQLite, mgr *lsp.Manager) *Service {
	s := New(cfg, st)
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

// References — возвращает рёбра, указывающие на данный юнит (ссылки, реализации, наследование, вызовы).
func (s *Service) References(ctx context.Context, unitID int64) ([]store.Edge, error) {
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

// TraverseGraph — направленный семантический обход по графу.
func (s *Service) TraverseGraph(ctx context.Context, startID int64, depth int, kinds []string) (*store.TraverseResult, error) {
	nodes, edges, err := s.st.TraverseGraph(ctx, startID, depth, kinds)
	if err != nil {
		return nil, err
	}
	return &store.TraverseResult{Nodes: nodes, Edges: edges}, nil
}

// GetExecutionContext собирает богатый контекст вокруг символа.
func (s *Service) GetExecutionContext(ctx context.Context, symbolID int64) (*ExecutionContext, error) {
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

	// 3. References (LSP + TS edges)
	refs, _ := s.st.EdgesTo(ctx, symbolID, EdgeReference)
	ctxRes.References = refs

	// 4. Related Types (Implements, Extends)
	impls, _ := s.Implementations(ctx, symbolID)
	ctxRes.RelatedTypes = append(ctxRes.RelatedTypes, impls...)

	// Добавляем то, на что САМ символ ссылается (extends/implements)
	outEdges, _ := s.st.EdgesFrom(ctx, symbolID, "")
	for _, e := range outEdges {
		if e.Kind == EdgeExtends || e.Kind == EdgeImplements || e.Kind == EdgeReference {
			if e.DstID != 0 {
				du, _ := s.st.GetASTUnit(ctx, e.DstID)
				if du != nil {
					ctxRes.RelatedTypes = append(ctxRes.RelatedTypes, *du)
				}
			}
		}
		if e.Kind == EdgeImport {
			if e.DstID != 0 {
				iu, _ := s.st.GetASTUnit(ctx, e.DstID)
				if iu != nil {
					ctxRes.Imports = append(ctxRes.Imports, *iu)
				}
			}
		}
	}

	// 5. Important Files
	fileMap := make(map[string]bool)
	fileMap[u.FilePath] = true
	for _, c := range ctxRes.Callers {
		fileMap[c.FilePath] = true
	}
	for _, c := range ctxRes.Callees {
		fileMap[c.FilePath] = true
	}
	for _, r := range ctxRes.RelatedTypes {
		fileMap[r.FilePath] = true
	}
	for _, i := range ctxRes.Imports {
		fileMap[i.FilePath] = true
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
	// Для Go это может быть путь к папке.
	if strings.Contains(name, "/") || strings.Contains(name, "\\") {
		// Ищем юниты типа module, у которых file_path заканчивается на name или содержит его.
		// В SQLite нет простого ENDS_WITH, используем LIKE.
		q := `SELECT id, repo, file_path, language, kind, name, qualified, parent_id, start_line, end_line, start_byte, end_byte, signature, doc, hash 
              FROM ast_units WHERE kind = 'module' AND (file_path LIKE ? OR file_path LIKE ?) LIMIT 1`
		// Проверяем как точное окончание /path, так и просто наличие.
		rows, err := s.st.GetDB().QueryContext(ctx, q, "%/"+name, "%"+name+"%")
		if err == nil {
			defer rows.Close()
			if rows.Next() {
				var u store.ASTUnit
				err := rows.Scan(
					&u.ID, &u.Repo, &u.FilePath, &u.Language, &u.Kind, &u.Name, &u.Qualified,
					&u.ParentID, &u.StartLine, &u.EndLine, &u.StartByte, &u.EndByte,
					&u.Signature, &u.Doc, &u.Hash,
				)
				if err == nil {
					return &u, nil
				}
			}
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

// GetSymbolSummary собирает детерминированные данные и обогащает их через LLM.
func (s *Service) GetSymbolSummary(ctx context.Context, symbolID int64) (*SymbolSummary, error) {
	u, err := s.st.GetASTUnit(ctx, symbolID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, fmt.Errorf("symbol not found")
	}

	res := &SymbolSummary{
		Name:      u.Name,
		Signature: u.Signature,
	}

	// Deterministic part
	callers, _ := s.Callers(ctx, symbolID)
	for _, c := range callers {
		res.Callers = append(res.Callers, c.Qualified)
	}
	callees, _ := s.Callees(ctx, symbolID)
	for _, c := range callees {
		res.Calls = append(res.Calls, c.Qualified)
	}

	// Semantic part (LLM)
	prompt := fmt.Sprintf(`Analyze the following code symbol and provide its purpose, role, and importance in the system.
Name: %s
Signature: %s
Calls: %s
Callers: %s
Doc: %s

Return ONLY a JSON object with fields: "purpose", "role", "importance".`,
		u.Name, u.Signature, strings.Join(res.Calls, ", "), strings.Join(res.Callers, ", "), u.Doc)

	llmRes, err := s.callOllama(ctx, prompt, "phi3:mini")
	if err == nil {
		var sem struct {
			Purpose    string `json:"purpose"`
			Role       string `json:"role"`
			Importance string `json:"importance"`
		}
		if err := json.Unmarshal([]byte(llmRes), &sem); err == nil {
			res.Purpose = sem.Purpose
			res.Role = sem.Role
			res.Importance = sem.Importance
		}
	}

	return res, nil
}

// GetFileIntent анализирует файл через Tree-sitter и LLM.
func (s *Service) GetFileIntent(ctx context.Context, path string) (*FileIntent, error) {
	units, err := s.st.ListASTUnitsByFile(ctx, path)
	if err != nil {
		return nil, err
	}

	res := &FileIntent{}
	importSet := make(map[string]bool)
	for _, u := range units {
		if u.Kind != "file" && u.Kind != "module" && u.Kind != "package" {
			res.Symbols = append(res.Symbols, u.Name)
		}
		// Пытаемся найти импорты через исходящие рёбра
		edges, _ := s.st.EdgesFrom(ctx, u.ID, EdgeImport)
		for _, e := range edges {
			importSet[e.DstName] = true
		}
	}
	for imp := range importSet {
		res.Imports = append(res.Imports, imp)
	}

	prompt := fmt.Sprintf(`Analyze the purpose of this source file.
Path: %s
Symbols: %s
Imports: %s

Return ONLY a JSON object with fields: "purpose", "layer", "responsibilities" (array of strings).`,
		path, strings.Join(res.Symbols, ", "), strings.Join(res.Imports, ", "))

	llmRes, err := s.callOllama(ctx, prompt, "phi3:mini")
	if err == nil {
		var sem struct {
			Purpose          string   `json:"purpose"`
			Layer            string   `json:"layer"`
			Responsibilities []string `json:"responsibilities"`
		}
		if err := json.Unmarshal([]byte(llmRes), &sem); err == nil {
			res.Purpose = sem.Purpose
			res.Layer = sem.Layer
			res.Responsibilities = sem.Responsibilities
		}
	}

	return res, nil
}

// GetSemanticNeighborhood выполняет детерминированное расширение и LLM-кластеризацию.
func (s *Service) GetSemanticNeighborhood(ctx context.Context, symbolID int64) (*SemanticNeighborhood, error) {
	u, err := s.st.GetASTUnit(ctx, symbolID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, fmt.Errorf("symbol not found")
	}

	res := &SemanticNeighborhood{
		Center: u.Name,
	}

	// Step 1: Deterministic
	callers, _ := s.Callers(ctx, symbolID)
	for _, c := range callers {
		res.Neighbors.Callers = append(res.Neighbors.Callers, c.Name)
	}
	callees, _ := s.Callees(ctx, symbolID)
	for _, c := range callees {
		res.Neighbors.DirectCalls = append(res.Neighbors.DirectCalls, c.Name)
	}
	outEdges, _ := s.st.EdgesFrom(ctx, symbolID, EdgeReference)
	for _, e := range outEdges {
		res.Neighbors.Types = append(res.Neighbors.Types, e.DstName)
	}

	// Step 2: LLM Compression
	prompt := fmt.Sprintf(`Summarize this code neighborhood into a logical cluster.
Center: %s
Direct Calls: %s
Callers: %s
Types/References: %s

Return ONLY a JSON object with fields: "cluster", "core" (list), "dependencies" (list), "boundary" (list).`,
		u.Name, strings.Join(res.Neighbors.DirectCalls, ", "), strings.Join(res.Neighbors.Callers, ", "), strings.Join(res.Neighbors.Types, ", "))

	llmRes, err := s.callOllama(ctx, prompt, "phi3:mini")
	if err == nil {
		var sem struct {
			Cluster      string   `json:"cluster"`
			Core         []string `json:"core"`
			Dependencies []string `json:"dependencies"`
			Boundary     []string `json:"boundary"`
		}
		if err := json.Unmarshal([]byte(llmRes), &sem); err == nil {
			res.Cluster = sem.Cluster
			res.Core = sem.Core
			res.Dependencies = sem.Dependencies
			res.Boundary = sem.Boundary
		}
	}

	return res, nil
}

func (s *Service) callOllama(ctx context.Context, prompt, model string) (string, error) {
	url := strings.TrimRight(s.cfg.Ollama.URL, "/") + "/api/generate"
	body, _ := json.Marshal(map[string]any{
		"model":  model,
		"prompt": prompt,
		"stream": false,
		"format": "json",
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ollama error %d: %s", resp.StatusCode, string(b))
	}

	var res struct {
		Response string `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	return res.Response, nil
}
