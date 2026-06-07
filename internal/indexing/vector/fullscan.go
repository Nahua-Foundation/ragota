package vector

// Файл реализует FullScan: оркестрация воркеров и подготовка файлов.
// Логика батчинга вынесена в batcher.go.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"ragota/internal/indexing/chunker"
	"ragota/pkg/fileutil"
	"ragota/pkg/logger"
	"ragota/pkg/state"
)

// FullScan индексирует все подходящие файлы с использованием глобального батчинга чанков.
func (v *Vector) FullScan(ctx context.Context) error {
	v.scanMu.Lock()
	if v.scanning {
		v.scanMu.Unlock()
		return fmt.Errorf("vector: FullScan already in progress")
	}
	v.scanning = true
	defer func() { v.scanMu.Unlock(); v.scanning = false }()

	v.wg.Add(1)
	defer v.wg.Done()

	v.totalChunks.Store(0)
	v.totalIndexed.Store(0)
	if v.bus != nil {
		v.bus.SetIndexer("vector", func(i *state.Indexer) {
			i.Status = "scanning"
			i.FilesTotal = 0
			i.FilesIndexed = 0
			i.Chunks = 0
			i.LastError = ""
		})
	}

	var allFiles []string
	_ = fileutil.WalkFiles(v.cfg.Root, v.matcher, v.cfg.Extensions, func(abs, rel string, _ os.FileInfo) error {
		allFiles = append(allFiles, abs)
		return nil
	})

	total := len(allFiles)
	if total == 0 {
		if v.bus != nil {
			v.bus.SetIndexer("vector", func(i *state.Indexer) {
				i.Status = "idle"
			})
		}
		return nil
	}

	// Сразу показываем FilesTotal — пользователь видит прогресс без ожидания Ollama
	if v.bus != nil {
		v.bus.SetIndexer("vector", func(i *state.Indexer) {
			i.Status = "indexing"
			i.FilesTotal = total
			i.FilesIndexed = 0
		})
	}

	needFullBM25Reindex := v.detectStaleHashes(ctx)

	if v.writeSink() != nil && needFullBM25Reindex {
		if err := v.writeSink().Clear(ctx); err != nil {
			logger.Log().Warn().Err(err).Msg("vector: BM25 Clear")
		}
		// Сбрасываем vec_hashes чтобы prepareFile не пропускал файлы —
		// иначе файлы с неизменным хэшем пропустятся и BM25 останется пустым
		if v.store != nil {
			if err := v.store.ResetVecHashes(ctx); err != nil {
				logger.Log().Warn().Err(err).Msg("vector: ResetVecHashes for BM25 reindex")
			}
		}
	}

	numWorkers := v.cfg.VectorWorkers
	if numWorkers <= 0 {
		numWorkers = 1
	}

	preparedChan := make(chan *preparedFile, numWorkers*2)
	var scanWg sync.WaitGroup
	var lastErr atomic.Value

	jobs := make(chan string, total)
	for _, f := range allFiles {
		jobs <- f
	}
	close(jobs)

	scanWg.Add(numWorkers)
	for i := 0; i < numWorkers; i++ {
		go func() {
			defer scanWg.Done()
			for abs := range jobs {
				select {
				case <-ctx.Done():
					return
				default:
				}

				pf, err := v.prepareFile(ctx, abs)
				if err != nil {
					lastErr.Store(err)
					v.updateProgress(total)
					continue
				}
				if pf != nil {
					select {
					case preparedChan <- pf:
					case <-ctx.Done():
						return
					}
				}
				// Считаем каждый обработанный файл (skipped + prepared + error)
				v.updateProgress(total)
			}
		}()
	}

	go func() {
		scanWg.Wait()
		close(preparedChan)
	}()

	return v.processPreparedFiles(ctx, preparedChan, total, &lastErr)
}

// detectStaleHashes проверяет рассинхронизацию Qdrant/SQLite/BM25.
func (v *Vector) detectStaleHashes(ctx context.Context) bool {
	if v.qd == nil || v.store == nil {
		return false
	}
	codeSpec := v.cfg.CodeCollection()
	textSpec := v.cfg.TextCollection()
	codeCount, _ := v.qd.Count(ctx, codeSpec.Name)
	textCount, _ := v.qd.Count(ctx, textSpec.Name)
	if codeCount+textCount == 0 {
		hasHashes, _ := v.store.HasVecHashes(ctx)
		if hasHashes {
			logger.Log().Info().Msg("vector: Qdrant empty but SQLite has vec_hashes — resetting")
			_ = v.store.ResetVecHashes(ctx)
		}
		return true
	}
	if v.writeSink() != nil {
		bm25Count, _ := v.writeSink().Count(ctx)
		if bm25Count == 0 {
			logger.Log().Info().Msg("vector: Qdrant has data but BM25 is empty — reindex")
			return true
		}
	}
	return false
}

func (v *Vector) updateProgress(total int) {
	staticIndexed := v.totalIndexed.Add(1)
	if v.bus != nil && total > 0 && (int(staticIndexed)%10 == 0 || int(staticIndexed) == total) {
		v.bus.SetIndexer("vector", func(st *state.Indexer) {
			st.FilesTotal = total
			st.FilesIndexed = int(staticIndexed)
			st.Status = "indexing"
			st.Chunks = int(v.totalChunks.Load())
		})
	}
}

func (v *Vector) prepareFile(ctx context.Context, abs string) (*preparedFile, error) {
	src, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	hash := fileutil.HashBytes(src)
	var isKnown bool
	if hash != "" && v.store != nil {
		prev, _ := v.store.GetFile(ctx, abs)
		if prev != nil {
			isKnown = prev.VecHash != ""
			if prev.VecHash == hash {
				return nil, nil
			}
		}
	}

	lang := fileutil.LanguageByExt(filepath.Ext(abs))
	collection, emb := v.pickCollection(lang)

	syms, treeSymbols, _ := v.parser.ParseAll(ctx, lang, abs, src, chunker.MaxChunkBytes)

	var chunks []chunker.Chunk
	if len(treeSymbols) > 0 {
		chunks = v.chunker.ChunkByTree(abs, lang, src, treeSymbols)
		allChunks := v.chunker.Chunk(abs, lang, src, syms)
		for _, c := range allChunks {
			if c.Kind == "symbol" {
				chunks = append(chunks, c)
			}
		}
	} else {
		chunks = v.chunker.Chunk(abs, lang, src, syms)
	}

	if len(chunks) == 0 {
		if v.store != nil && hash != "" {
			_ = v.store.UpsertVectorHash(ctx, abs, hash, lang)
		}
		return nil, nil
	}

	rel, _ := filepath.Rel(v.cfg.Root, abs)
	var repo string
	if v.resolv != nil {
		repo = v.resolv.For(abs)
	}
	return &preparedFile{
		abs:      abs,
		rel:      rel,
		lang:     lang,
		repo:     repo,
		hash:     hash,
		chunks:   chunks,
		collSpec: collection,
		emb:      emb,
		isKnown:  isKnown,
	}, nil
}

func (v *Vector) processPreparedFiles(ctx context.Context, preparedChan <-chan *preparedFile, total int, lastErr *atomic.Value) error {
	codeBatcher := newBatcher(v, v.cfg.CodeCollection(), v.code, total)
	textBatcher := newBatcher(v, v.cfg.TextCollection(), v.text, total)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); codeBatcher.run(ctx) }()
	go func() { defer wg.Done(); textBatcher.run(ctx) }()

	var ctxErr error
loop:
	for {
		select {
		case <-ctx.Done():
			ctxErr = ctx.Err()
			break loop
		case pf, ok := <-preparedChan:
			if !ok {
				break loop
			}
			var target chan *preparedFile
			if codeBatcher.spec.Name == pf.collSpec.Name {
				target = codeBatcher.items
			} else {
				target = textBatcher.items
			}
			select {
			case target <- pf:
			case <-ctx.Done():
				ctxErr = ctx.Err()
				break loop
			}
		}
	}
	close(codeBatcher.items)
	close(textBatcher.items)
	go func() {
		for range preparedChan {
		}
	}()
	wg.Wait()
	if ctxErr != nil {
		return ctxErr
	}

	v.SyncStats(ctx)

	if v.bus != nil {
		v.bus.SetIndexer("vector", func(i *state.Indexer) {
			err := lastErr.Load()
			if err != nil {
				i.Status = "error"
				i.LastError = "last error: " + err.(error).Error()
			} else {
				i.Status = "idle"
			}
			i.FilesTotal = total
			i.FilesIndexed = total
			i.Chunks = int(v.totalChunks.Load())
		})
	}
	return nil
}
