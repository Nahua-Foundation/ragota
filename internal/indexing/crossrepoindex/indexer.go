// Package crossrepoindex — отдельный индексатор cross-repo связей.
// Запускается после AST FullScan, работает с уже построенным графом.
//
// Алгоритм FullScan:
//   1. Scanning — собираем unresolved import edges
//   2. Resolving — manifest resolution (go.mod, package.json)
//   3. Detecting — ищем HTTP/gRPC/Kafka вызовы
//   4. Classifying — LLM классификация (если подключён classifier)
//   5. Writing — записываем cross-call edges
//
// Инкрементальная индексация (IndexFile):
//   - При изменении файла удаляем старые cross-edges для этого файла
//   - Запускаем detect + classify только для этого файла
//   - Записываем новые edges
package crossrepoindex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"ragota/internal/indexing/crossrepo"
	"ragota/internal/indexing/crossrepo/classifier"
	"ragota/internal/indexing/crossrepo/detector"
	"ragota/internal/indexing/crossrepo/manifests"
	"ragota/internal/store"
	"ragota/pkg/repos"
	"ragota/pkg/state"
)

// Indexer — отдельный cross-repo индексатор.
type Indexer struct {
	resolver   *repos.Resolver
	manifests  *manifests.Registry
	classifier *classifier.Classifier
	st         *store.SQLite
	bus        *state.Bus

	filesTotal  int
	filesDone   int
}

// New создаёт cross-repo индексатор.
func New(resolver *repos.Resolver, st *store.SQLite) *Indexer {
	return &Indexer{
		resolver:  resolver,
		manifests: manifests.New(),
		st:        st,
	}
}

// SetClassifier подключает LLM-классификатор.
func (idx *Indexer) SetClassifier(c *classifier.Classifier) {
	idx.classifier = c
}

// SetBus устанавливает шину событий.
func (idx *Indexer) SetBus(bus *state.Bus) {
	idx.bus = bus
}

// computeAggregateHash вычисляет SHA256 от сортированного списка всех file.hash.
// Если AST не менялся, aggregate hash остаётся тем же.
func (idx *Indexer) computeAggregateHash(ctx context.Context) (string, error) {
	hashes, err := idx.st.AllFileHashes(ctx)
	if err != nil {
		return "", err
	}
	sort.Strings(hashes)
	h := sha256.New()
	for _, hash := range hashes {
		h.Write([]byte(hash))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hasChanged сравнивает текущий aggregate hash с сохранённым cross_hash.
func (idx *Indexer) hasChanged(ctx context.Context) (bool, string, error) {
	prevHash, _ := idx.st.GetCrossHash(ctx)
	curHash, err := idx.computeAggregateHash(ctx)
	if err != nil {
		return false, "", err
	}
	return prevHash != curHash, curHash, nil
}

// InitManifests сканирует все репо и парсит их манифесты.
func (idx *Indexer) InitManifests() {
	for _, repo := range idx.resolver.All() {
		idx.manifests.AddRepo(repo.Name, repo.Path)
	}
}

// report обновляет статус в bus.
func (idx *Indexer) report(status string, done, total, symbols, chunks int) {
	if idx.bus == nil {
		return
	}
	idx.bus.SetIndexer("crossrepo", func(st *state.Indexer) {
		st.Status = status
		st.FilesIndexed = done
		st.FilesTotal = total
		st.Symbols = symbols
		st.Chunks = chunks
	})
}

// FullScan выполняет полную cross-repo индексацию.
// Вызывается после AST FullScan, когда все edges уже в БД.
// Если AST не менялся (aggregate hash совпадает) — skip.
func (idx *Indexer) FullScan(ctx context.Context) error {
	// ═══ Check if AST changed ═══
	changed, curHash, err := idx.hasChanged(ctx)
	if err != nil {
		idx.report("error", 0, 0, 0, 0)
		return fmt.Errorf("crossrepo: compute hash: %w", err)
	}
	if !changed {
		// AST не менялся — crossrepo edges те же, skip
		idx.report("idle (unchanged)", 0, 0, 0, 0)
		return nil
	}

	// ═══ Scanning ═══
	idx.report("scanning", 0, 0, 0, 0)

	// ═══ Resolving: manifest resolution для import-edges ═══
	idx.report("resolving", 0, 0, 0, 0)

	resolved, err := idx.st.ResolvePendingEdgesCrossRepo(ctx, idx.manifests, nil)
	if err != nil {
		idx.report("error", 0, 0, 0, 0)
		return err
	}

	// ═══ Detecting: ищем HTTP/gRPC/Kafka вызовы ═══
	idx.report("detecting", 0, 0, resolved, 0)

	det := detector.New()
	candidates := det.DetectFromStore(ctx, idx.st)

	// ═══ Classifying: LLM классификация ═══
	idx.report("classifying", 0, len(candidates), resolved, 0)

	var edges []crossrepo.CrossEdge
	edges = append(edges, importEdgesToCross(resolved)...)

	if idx.classifier != nil && len(candidates) > 0 {
		// Классифицируем батчами с обновлением прогресса
		batchSize := 20
		classifiedTotal := 0
		var allLlmEdges []crossrepo.CrossEdge

		for i := 0; i < len(candidates); i += batchSize {
			end := i + batchSize
			if end > len(candidates) {
				end = len(candidates)
			}
			batch := candidates[i:end]

			n, llmEdges, _ := idx.classifier.ClassifyBatch(ctx, batch, idx.resolver)
			classifiedTotal += n
			allLlmEdges = append(allLlmEdges, llmEdges...)

			// Обновляем прогресс: FilesIndexed = processed, FilesTotal = total candidates
			idx.report("classifying", end, len(candidates), resolved, len(allLlmEdges))
		}

		edges = append(edges, allLlmEdges...)
	}

	// ═══ Writing: записываем cross-call edges в БД ═══
	idx.report("writing", 0, len(edges), resolved, 0)

	if len(edges) > 0 {
		if err := idx.writeCrossEdges(ctx, edges); err != nil {
			idx.report("error", 0, 0, 0, 0)
			return err
		}
	}

	// ═══ Save aggregate hash ═══
	if err := idx.st.SetCrossHash(ctx, curHash); err != nil {
		return fmt.Errorf("crossrepo: save hash: %w", err)
	}

	// ═══ Idle ═══
	idx.report("idle", 0, 0, resolved, len(edges))
	return nil
}

// importEdgesToCross конвертирует количество resolved import edges в CrossEdge stubs.
func importEdgesToCross(count int) []crossrepo.CrossEdge {
	// Import edges уже записаны через ResolvePendingEdgesCrossRepo (обновляют dst_repo)
	// Возвращаем пустой список — они уже в БД
	return nil
}

// IndexFile выполняет инкрементальную cross-repo индексацию одного файла.
// Вызывается при изменении файла через watcher.
func (idx *Indexer) IndexFile(ctx context.Context, filePath string) error {
	if idx.bus != nil {
		idx.bus.SetIndexer("crossrepo", func(st *state.Indexer) {
			st.Status = "indexing"
		})
	}

	// Удаляем старые cross-edges для этого файла
	if err := idx.st.DeleteEdgesByFilePath(ctx, filePath, "cross_call"); err != nil {
		return err
	}

	// Детектим вызовы только для этого файла
	candidates := idx.detectFile(ctx, filePath)
	if len(candidates) == 0 {
		return nil
	}

	// Классифицируем
	var edges []crossrepo.CrossEdge
	if idx.classifier != nil {
		_, llmEdges, _ := idx.classifier.ClassifyBatch(ctx, candidates, idx.resolver)
		edges = llmEdges
	}

	// Записываем
	if len(edges) > 0 {
		if err := idx.writeCrossEdges(ctx, edges); err != nil {
			return err
		}
	}

	return nil
}

// RemoveFile удаляет cross-edges для удалённого файла.
func (idx *Indexer) RemoveFile(ctx context.Context, filePath string) error {
	return idx.st.DeleteEdgesByFilePath(ctx, filePath, "cross_call")
}

// detectFile детектит cross-repo вызовы в одном файле.
func (idx *Indexer) detectFile(ctx context.Context, filePath string) []detector.Candidate {
	units, err := idx.st.ListASTUnitsByFile(ctx, filePath)
	if err != nil || len(units) == 0 {
		return nil
	}

	// Находим unresolved call edges для этого файла
	allEdges, _ := idx.st.AllEdgesByKind(ctx, "call")
	var candidates []detector.Candidate
	seen := make(map[string]bool)

	for _, e := range allEdges {
		if e.DstID != 0 {
			continue
		}
		if e.FilePath != filePath {
			continue
		}

		srcUnit, _ := idx.st.GetASTUnit(ctx, e.SrcID)
		if srcUnit == nil {
			continue
		}

		key := filePath + ":" + string(rune(e.Line))
		if seen[key] {
			continue
		}
		seen[key] = true

		det := detector.New()
		if !det.IsExternalCall(e.DstName, srcUnit.Language) {
			continue
		}

		rawCode := det.ExtractRawCode(srcUnit, e.Line)
		candidates = append(candidates, detector.Candidate{
			Repo:     srcUnit.Repo,
			FilePath: srcUnit.FilePath,
			Line:     e.Line,
			Symbol:   srcUnit.Name,
			RawCode:  rawCode,
			Language: srcUnit.Language,
		})
	}

	return candidates
}

// writeCrossEdges записывает cross-repo edges в таблицу edges.
func (idx *Indexer) writeCrossEdges(ctx context.Context, edges []crossrepo.CrossEdge) error {
	for _, e := range edges {
		// Находим src unit по file + line
		units, err := idx.st.FindASTUnitsByFileLine(ctx, e.SrcFile, e.SrcLine)
		if err != nil || len(units) == 0 {
			continue
		}
		srcUnit := units[0]

		edge := store.Edge{
			Repo:       e.SrcRepo,
			SrcID:      srcUnit.ID,
			DstID:      0, // unresolved — target в другом репо
			DstName:    e.DstName,
			DstRepo:    e.DstRepo,
			Kind:       "cross_call",
			Confidence: e.Confidence,
			FilePath:   e.SrcFile,
			Line:       e.SrcLine,
		}

		if _, err := idx.st.InsertEdge(ctx, edge); err != nil {
			return err
		}
	}
	return nil
}

// GetEdgesByRepo возвращает все cross-repo edges для данного репо.
func (idx *Indexer) GetEdgesByRepo(repo string) ([]crossrepo.CrossEdge, error) {
	allEdges, err := idx.st.AllCrossRepoEdges(context.Background())
	if err != nil {
		return nil, err
	}

	var out []crossrepo.CrossEdge
	for _, e := range allEdges {
		if e.Repo == repo || e.DstRepo == repo {
			out = append(out, crossrepo.CrossEdge{
				SrcRepo:    e.Repo,
				SrcFile:    e.FilePath,
				SrcLine:    e.Line,
				DstRepo:    e.DstRepo,
				DstName:    e.DstName,
				Protocol:   e.Kind,
				Confidence: e.Confidence,
			})
		}
	}
	return out, nil
}
