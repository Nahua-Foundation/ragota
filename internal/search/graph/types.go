package graph

import (
	"sync"
	"time"

	"ragota/internal/indexing/crossrepo"
	"ragota/pkg/config"
	"ragota/pkg/lsp/manager"
	"ragota/pkg/state"
	"ragota/internal/store"
)

// CrossRepoIndexer — интерфейс cross-repo индексатора.
type CrossRepoIndexer interface {
	GetEdgesByRepo(repo string) ([]crossrepo.CrossEdge, error)
}

// EdgeKind — типы рёбер.
const (
	EdgeCall       = "call"
	EdgeImport     = "import"
	EdgeImplements = "implements"
	EdgeExtends    = "extends"
	EdgeReference  = "reference"
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
	// LLMError содержит текст ошибки, если LLM-модель недоступна.
	LLMError string `json:"llm_error,omitempty"`
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
	// LLMError содержит текст ошибки, если LLM-модель недоступна.
	LLMError string `json:"llm_error,omitempty"`
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
	// LLMError содержит текст ошибки, если LLM-модель недоступна.
	LLMError string `json:"llm_error,omitempty"`
}

type NeighborhoodMap struct {
	DirectCalls []string `json:"direct_calls"`
	Callers     []string `json:"callers"`
	Types       []string `json:"types"`
}

// TraverseResult — результат направленного обхода.
type TraverseResult struct {
	Nodes []store.ASTUnit `json:"nodes"`
	Edges []store.Edge    `json:"edges"`
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
	mgr *manager.Manager // опционально; если nil — работает только tree-sitter
	bus *state.Bus       // опционально; для записи метрик ollama latency
	CRI CrossRepoIndexer // опционально; cross-repo индекс (экспортирован для MCP handlers)

	mu        sync.Mutex
	callCache map[int]cacheEntry // ленивый кэш для Callers (по unit.ID)
	implCache map[int]cacheEntry // ленивый кэш для Implementations
}

type cacheEntry struct {
	units []store.ASTUnit
	at    time.Time
}

const cacheTTL = 5 * time.Minute
const cacheMaxSize = 1000 // cap to prevent unbounded growth

// New создаёт сервис без LSP-обогащения (только tree-sitter).
func New(cfg *config.Config, st *store.SQLite) *Service {
	return &Service{
		cfg:       cfg,
		st:        st,
		callCache: make(map[int]cacheEntry),
		implCache: make(map[int]cacheEntry),
	}
}

// SetBus устанавливает bus для записи метрик.
func (s *Service) SetBus(bus *state.Bus) {
	s.bus = bus
}

// NewWithLSP создаёт сервис с ленивым LSP-обогащением.
func NewWithLSP(cfg *config.Config, st *store.SQLite, mgr *manager.Manager) *Service {
	s := New(cfg, st)
	s.mgr = mgr
	return s
}

// SetCrossRepoIndex подключает cross-repo индекс.
func (s *Service) SetCrossRepoIndex(cri CrossRepoIndexer) { s.CRI = cri }
