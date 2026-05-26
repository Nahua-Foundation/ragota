package index

// Файл реализует пакетный FullScan: параллельное сканирование +
// подготовка чанков воркерами и их слияние батчером перед эмбеддингом.
// Логика разделена по доменам:
//
//   - FullScan — точка входа, оркестрация воркеров.
//   - prepareFile — чтение, парсинг, чанкинг одного файла.
//   - processPreparedFiles — фан-аут подготовленных файлов на 2 батчера
//     (code / text) и ожидание их завершения.
//   - batcher.run / processBatch — упаковка чанков в батчи и запись
//     в Qdrant/BM25.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"ragota/internal/bm25"
	"ragota/internal/chunker"
	"ragota/internal/config"
	"ragota/internal/embedder"
	"ragota/internal/fileutil"
	"ragota/internal/logger"
	"ragota/internal/qdrant"
	"ragota/internal/state"
)

// FullScan индексирует все подходящие файлы с использованием глобального батчинга чанков.
func (v *Vector) FullScan(ctx context.Context) error {
	v.wg.Add(1)
	defer v.wg.Done()

	// Сбрасываем статус перед началом — чтобы TUI не показывал старые данные
	// от предыдущей директории (Qdrant общий, коллекции не очищаются).
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

	// Detect stale hashes: Qdrant collections empty but SQLite has vec_hashes.
	// This happens when the container is restarted from a different directory
	// (new volume mount) while the local DB still has old hashes.
	if v.qd != nil && v.store != nil {
		codeSpec := v.cfg.CodeCollection()
		textSpec := v.cfg.TextCollection()
		codeCount, _ := v.qd.Count(ctx, codeSpec.Name)
		textCount, _ := v.qd.Count(ctx, textSpec.Name)
		if codeCount+textCount == 0 {
			hasHashes, _ := v.store.HasVecHashes(ctx)
			if hasHashes {
				logger.Log().Info().Msg("vector: Qdrant empty but SQLite has vec_hashes — resetting hashes for reindex")
				if err := v.store.ResetVecHashes(ctx); err != nil {
					logger.Log().Warn().Err(err).Msg("vector: ResetVecHashes")
				}
			}
		}
	}

	// BM25: очищаем индекс один раз перед полной переиндексацией,
	// вместо per-file DeleteByPath — это значительно быстрее.
	if v.bm25 != nil {
		if err := v.bm25.Clear(ctx); err != nil {
			logger.Log().Warn().Err(err).Msg("vector: BM25 Clear")
		}
	}

	numWorkers := v.cfg.VectorWorkers
	if numWorkers <= 0 {
		numWorkers = 1
	}

	// Канал для подготовленных файлов
	preparedChan := make(chan *preparedFile, numWorkers*2)
	var scanWg sync.WaitGroup
	var lastErr atomic.Value

	// Воркеры для сканирования и парсинга файлов
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
					continue
				}
				if pf != nil {
					select {
					case preparedChan <- pf:
					case <-ctx.Done():
						return
					}
				} else {
					// Файл пропущен (не изменился)
					v.updateProgress(total)
				}
			}
		}()
	}

	// Закрываем канал после завершения сканирования
	go func() {
		scanWg.Wait()
		close(preparedChan)
	}()

	return v.processPreparedFiles(ctx, preparedChan, total, &lastErr)
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
	// Проверяем, индексировался ли файл ранее — результат нужен для isKnown.
	var isKnown bool
	if hash != "" && v.store != nil {
		prev, _ := v.store.GetFile(ctx, abs)
		if prev != nil {
			isKnown = prev.VecHash != ""
			if prev.VecHash == hash {
				return nil, nil // файл не изменился
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
	// Батчеры для кода и текста
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
	// Дренируем preparedChan, чтобы разблокировать воркеров-сканеров,
	// которые могут висеть на отправке в канал после отмены ctx.
	go func() {
		for range preparedChan {
		}
	}()
	wg.Wait()
	if ctxErr != nil {
		return ctxErr
	}

	// Синхронизируем счётчик чанков с Qdrant — файлы могли быть пропущены
	// (хеш совпал), но чанки уже есть в индексе. SyncStats читает реальное
	// количество точек из обеих коллекций.
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

// batcher собирает подготовленные файлы по целевой коллекции и
// сбрасывает их пачками в processBatch, чтобы амортизировать сетевые
// вызовы EmbedBatch/Upsert.
type batcher struct {
	v     *Vector
	spec  config.CollectionSpec
	emb   *embedder.Ollama
	items chan *preparedFile
	total int
}

func newBatcher(v *Vector, spec config.CollectionSpec, emb *embedder.Ollama, total int) *batcher {
	return &batcher{
		v:     v,
		spec:  spec,
		emb:   emb,
		items: make(chan *preparedFile, 100),
		total: total,
	}
}

func (b *batcher) run(ctx context.Context) {
	const maxBatchChunks = 256
	var currentBatch []*preparedFile
	var currentChunks int

	flush := func() {
		if len(currentBatch) == 0 {
			return
		}
		if err := b.v.processBatch(ctx, b.spec, b.emb, currentBatch, b.total); err != nil {
			if !errors.Is(err, context.Canceled) {
				logger.Log().Error().Err(err).Int("files", len(currentBatch)).Msg("vector: batch error")
			}
		}
		// Обновляем прогресс для всех файлов в батче в любом случае.
		for range currentBatch {
			b.v.updateProgress(b.total)
		}
		currentBatch = nil
		currentChunks = 0
	}

	for {
		select {
		case <-ctx.Done():
			return
		case pf, ok := <-b.items:
			if !ok {
				flush()
				return
			}

			// Если файл большой, обрабатываем его сразу, чтобы не задерживать другие файлы.
			if len(pf.chunks) > maxBatchChunks {
				flush()
				if err := b.v.processBatch(ctx, b.spec, b.emb, []*preparedFile{pf}, b.total); err != nil {
					if !errors.Is(err, context.Canceled) {
						logger.Log().Error().Err(err).Str("file", pf.abs).Msg("vector: large file error")
					}
				}
				b.v.updateProgress(b.total)
				continue
			}

			currentBatch = append(currentBatch, pf)
			currentChunks += len(pf.chunks)
			if currentChunks >= maxBatchChunks {
				flush()
			}
		}
	}
}

func (v *Vector) processBatch(ctx context.Context, spec config.CollectionSpec, emb *embedder.Ollama, files []*preparedFile, total int) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Собираем тексты для эмбеддинга и файлы для удаления — параллельно.
	var allTexts []string
	var deletePaths []string // файлы, требующие DeleteByFilter (isKnown=true)
	for _, f := range files {
		for _, ch := range f.chunks {
			allTexts = append(allTexts, combinedText(ch))
		}
		if f.isKnown {
			deletePaths = append(deletePaths, f.abs)
		}
	}

	if len(allTexts) == 0 {
		return nil
	}

	// EmbedBatch (Ollama) и DeleteByFilter (Qdrant) работают параллельно —
	// они независимы и обращаются к разным системам.
	var vecs [][]float32
	var embedErr error
	var embedWg sync.WaitGroup
	embedWg.Add(1)
	go func() {
		defer embedWg.Done()
		vecs, embedErr = emb.EmbedBatch(ctx, allTexts)
		if embedErr != nil {
			logger.Log().Error().Err(embedErr).Int("texts", len(allTexts)).Msg("vector: EmbedBatch failed")
		}
	}()

	if len(deletePaths) > 0 {
		for _, path := range deletePaths {
			_ = v.qd.DeleteByFilter(ctx, spec.Name, "file", path)
		}
	}

	embedWg.Wait()
	if embedErr != nil {
		return embedErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	var allPoints []qdrant.Point
	var allDocs []bm25.Doc
	vecIdx := 0
	for _, f := range files {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		points := make([]qdrant.Point, len(f.chunks))
		for i := range f.chunks {
			ch := f.chunks[i]
			points[i] = qdrant.Point{
				ID:     chunkID(f.abs, i),
				Vector: vecs[vecIdx],
				Payload: map[string]any{
					"file":       f.abs,
					"rel":        f.rel,
					"repo":       f.repo,
					"language":   f.lang,
					"start_line": ch.StartLine,
					"end_line":   ch.EndLine,
					"kind":       ch.Kind,
					"symbol":     ch.Symbol,
					"text":       ch.Text,
					"comments":   ch.Comments,
					"parent":     ch.Parent,
					"imports":    ch.Imports,
					"collection": spec.Name,
				},
			}
			vecIdx++
		}
		allPoints = append(allPoints, points...)

		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Пишем хеш
		if v.store != nil && f.hash != "" {
			_ = v.store.UpsertVectorHash(ctx, f.abs, f.hash, f.lang)
		}
		if v.bm25 != nil {
			// BM25 delete не нужен — индекс очищен целиком в начале FullScan.
			for i, ch := range f.chunks {
				allDocs = append(allDocs, bm25.Doc{
					ID:        fmt.Sprintf("%s#%d", f.abs, i),
					Repo:      f.repo,
					Path:      f.abs,
					Language:  f.lang,
					Kind:      ch.Kind,
					Symbol:    ch.Symbol,
					Content:   combinedText(ch),
					StartLine: ch.StartLine,
					EndLine:   ch.EndLine,
				})
			}
		}
	}

	var wg sync.WaitGroup
	var lastErr error
	var mu sync.Mutex

	setErr := func(e error) {
		if e != nil {
			mu.Lock()
			lastErr = e
			mu.Unlock()
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if ctx.Err() != nil {
			return
		}
		setErr(v.qd.Upsert(ctx, spec.Name, allPoints))
	}()

	if v.bm25 != nil && len(allDocs) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ctx.Err() != nil {
				return
			}
			setErr(v.bm25.IndexDocs(ctx, allDocs))
		}()
	}

	wg.Wait()
	if lastErr != nil {
		return lastErr
	}

	v.totalChunks.Add(int64(len(allPoints)))
	return nil
}
