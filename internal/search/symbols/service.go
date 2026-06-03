// Package symbols — symbol-aware retrieval поверх AST units.
//
// Сервис объединяет:
//   - извлечение AST units из tree-sitter (через internal/parser)
//   - построение рёбер графа (calls/imports/implementations/...)
//   - симметричный API для MCP: find_definition / find_references /
//     find_implementations / find_callers / find_callees /
//     get_file_symbols / get_symbol / get_parent / get_children /
//     get_surrounding_context / get_related_files / get_similar_code
package symbols

import (
	"context"
	"errors"

	"ragota/internal/search/graph"
	"ragota/pkg/lsp/manager"
	"ragota/internal/store"
)

// ErrNotFound — символ не найден.
var ErrNotFound = errors.New("symbols: not found")

// SimilarSearcher — необязательный поставщик «похожего кода» (через
// векторный индекс). Если не передан — get_similar_code вернёт пустой
// список без ошибки.
type SimilarSearcher interface {
	SimilarToUnit(ctx context.Context, u store.ASTUnit, limit int) ([]store.ASTUnit, error)
}

// Service — высокоуровневый сервис символов и AST units.
type Service struct {
	st  *store.SQLite
	g   *graph.Service
	sim SimilarSearcher
	mgr *manager.Manager // LSP для точного поиска (implementations и др.)
}

// New создаёт сервис. sim может быть nil.
func New(st *store.SQLite, g *graph.Service, sim SimilarSearcher) *Service {
	return &Service{st: st, g: g, sim: sim}
}

// SetLSPManager подключает LSP менеджер для уточнения результатов.
func (s *Service) SetLSPManager(mgr *manager.Manager) { s.mgr = mgr }

// SetSimilarSearcher позднее подключает векторный поиск (после создания).
func (s *Service) SetSimilarSearcher(sim SimilarSearcher) { s.sim = sim }

// FileSymbols возвращает все AST units из файла.
func (s *Service) FileSymbols(ctx context.Context, path string) ([]store.ASTUnit, error) {
	return s.st.ListASTUnitsByFile(ctx, path)
}

// Get возвращает AST unit по ID.
func (s *Service) Get(ctx context.Context, id int) (*store.ASTUnit, error) {
	return s.st.GetASTUnit(ctx, id)
}

// Parent возвращает родительский AST unit.
func (s *Service) Parent(ctx context.Context, id int) (*store.ASTUnit, error) {
	u, err := s.st.GetASTUnit(ctx, id)
	if err != nil || u == nil {
		return nil, err
	}
	if !u.ParentID.Valid {
		return nil, nil
	}
	return s.st.GetASTUnit(ctx, int(u.ParentID.Int64))
}

// Children возвращает дочерние AST units.
func (s *Service) Children(ctx context.Context, id int) ([]store.ASTUnit, error) {
	return s.st.ChildrenOf(ctx, id)
}
