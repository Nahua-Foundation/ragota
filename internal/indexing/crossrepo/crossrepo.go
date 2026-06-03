// Package crossrepo — слой межрепозиторной связности поверх AST-графа.
//
// Архитектура (3 подпакета):
//
//   - manifests — парсинг go.mod, package.json, requirements.txt → маппинг
//     import_path → repo_name для cross-repo import resolution.
//   - detector  — обнаружение HTTP/gRPC/Kafka вызовов через AST-patterns
//     и эвристики (без LLM, быстро).
//   - classifier — LLM-классификация кандидатов из detector через Ollama
//     (qwen2.5-coder:3b, temp 0.1) с LRU + file-backed кэшем.
//
// Основной тип — Index — объединяет все три подпакета и предоставляет
// API для построения, запроса и инкрементального обновления cross-repo графа.
package crossrepo

import (
	"context"

	"ragota/internal/indexing/crossrepo/classifier"
	"ragota/internal/indexing/crossrepo/detector"
	"ragota/internal/indexing/crossrepo/manifests"
	"ragota/internal/store"
	"ragota/pkg/repos"
)

// CrossEdge — связь между репозиториями (алиас для удобства).
type CrossEdge = detector.CrossEdge

// RepoSummary — сводка по одному репозиторию.
type RepoSummary struct {
	Name         string       `json:"name"`
	Path         string       `json:"path"`
	Dependencies []DepSummary `json:"dependencies"`
	DependedBy   []string     `json:"depended_by"` // repos that depend on this one
}

// DepSummary — зависимость от другого репо.
type DepSummary struct {
	TargetRepo string `json:"target_repo"`
	Protocol   string `json:"protocol"`
	Count      int    `json:"count"` // количество edges
}

// Stats — статистика cross-repo индекса.
type Stats struct {
	ReposCount  int `json:"repos_count"`
	EdgesCount  int `json:"edges_count"`
	ImportEdges int `json:"import_edges"`
	CallEdges   int `json:"call_edges"`
}

// Index — cross-repo индекс.
type Index struct {
	resolver   *repos.Resolver
	manifests  *manifests.Registry
	detector   *detector.Detector
	classifier *classifier.Classifier
	st         *store.SQLite
}

// New создаёт cross-repo индекс.
func New(resolver *repos.Resolver, st *store.SQLite) *Index {
	return &Index{
		resolver:  resolver,
		manifests: manifests.New(),
		detector:  detector.New(),
		st:        st,
	}
}

// SetClassifier подключает LLM-классификатор.
func (idx *Index) SetClassifier(c *classifier.Classifier) {
	idx.classifier = c
}

// Resolver возвращает repo resolver.
func (idx *Index) Resolver() *repos.Resolver { return idx.resolver }

// Manifests возвращает registry манифестов.
func (idx *Index) Manifests() *manifests.Registry { return idx.manifests }

// DetectAndClassify сканирует файлы с unresolved import/call edges,
// обнаруживает cross-repo вызовы и классифицирует их через LLM.
// Возвращает список edges и количество классифицированных.
func (idx *Index) DetectAndClassify(ctx context.Context) ([]CrossEdge, int, error) {
	// Шаг 1: manifest resolution (быстро, без LLM)
	importEdges, err := idx.resolveImportEdges(ctx)
	if err != nil {
		return nil, 0, err
	}

	// Шаг 2: detect HTTP/gRPC/Kafka вызовы
	candidates := idx.detectCrossCalls(ctx)

	// Шаг 3: LLM классификация кандидатов
	classified := 0
	edges := make([]CrossEdge, 0, len(importEdges))
	edges = append(edges, importEdges...)

	if idx.classifier != nil && len(candidates) > 0 {
		classified, llmEdges, err := idx.classifier.ClassifyBatch(ctx, candidates, idx.resolver)
		if err != nil {
			return edges, classified, err
		}
		edges = append(edges, llmEdges...)
	}

	return edges, classified, nil
}

// resolveImportEdges разрешает import-edges через manifest registry.
func (idx *Index) resolveImportEdges(ctx context.Context) ([]CrossEdge, error) {
	// Получаем все unresolved import edges
	allEdges, err := idx.st.AllEdgesByKind(ctx, "import")
	if err != nil {
		return nil, err
	}

	var resolved []CrossEdge
	for _, e := range allEdges {
		if e.DstID != 0 {
			continue // уже разрешён
		}

		targetRepo := idx.manifests.ResolveImport(e.DstName)
		if targetRepo == "" {
			continue
		}

		// Находим source unit для мета
		srcUnit, _ := idx.st.GetASTUnit(ctx, e.SrcID)
		srcSymbol := ""
		if srcUnit != nil {
			srcSymbol = srcUnit.Name
		}

		resolved = append(resolved, CrossEdge{
			SrcRepo:    e.Repo,
			SrcFile:    e.FilePath,
			SrcLine:    e.Line,
			SrcSymbol:  srcSymbol,
			DstRepo:    targetRepo,
			DstName:    e.DstName,
			Protocol:   "import",
			Confidence: 1.0,
		})
	}

	return resolved, nil
}

// detectCrossCalls сканирует файлы через detector.
func (idx *Index) detectCrossCalls(ctx context.Context) []detector.Candidate {
	return idx.detector.DetectFromStore(ctx, idx.st)
}

// GetEdgesByRepo возвращает все cross-repo edges для данного репо.
func (idx *Index) GetEdgesByRepo(repo string) ([]CrossEdge, error) {
	return nil, nil // реализуется после записи в SQLite
}

// GetServiceGraph возвращает граф зависимостей между репозиториями.
func (idx *Index) GetServiceGraph() map[string]RepoSummary {
	return nil // реализуется после записи в SQLite
}

// Stats возвращает статистику cross-repo индекса.
func (idx *Index) GetStats() Stats {
	return Stats{} // реализуется после записи в SQLite
}
