package index

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"aitools/internal/bm25"
	"aitools/internal/chunker"
	"aitools/internal/config"
	"aitools/internal/embedder"
	"aitools/internal/fileutil"
	"aitools/internal/parser"
	"aitools/internal/qdrant"
	"aitools/internal/state"
	"aitools/internal/store"
	"aitools/internal/watcher"
)

// codeCollectionLanguages — языки, чанки которых пишутся в коллекцию code
// (qwen3-embedding). Markdown/rst/txt идут в text-коллекцию (nomic).
var codeCollectionLanguages = map[string]bool{
	"go":         true,
	"typescript": true,
	"javascript": true,
	"python":     true,
	"java":       true,
	"proto":      true,
	"json":       true,
	"yaml":       true,
	"toml":       true,
}

// Vector — векторный индексатор с двумя коллекциями (code/text) и
// опциональным параллельным BM25-индексом.
type Vector struct {
	cfg  *config.Config
	qd   *qdrant.Client
	emb  *embedder.Ollama // legacy / fallback (single-model сценарий)
	code *embedder.Ollama // эмбеддер для коллекции кода (qwen3-embedding)
	text *embedder.Ollama // эмбеддер для коллекции markdown/текста (nomic-embed-text)

	bm25    bm25.Index   // опционально; если nil — BM25 не пишется
	parser  *parser.Parser
	chunker *chunker.Chunker
	bus     *state.Bus
	matcher *fileutil.Matcher
	store   *store.SQLite

	mu          sync.Mutex
	totalChunks atomic.Int64
	totalIndexed atomic.Int32
	sem         chan struct{}
}

type preparedFile struct {
	abs      string
	rel      string
	lang     string
	hash     string
	chunks   []chunker.Chunk
	collSpec config.CollectionSpec
	emb      *embedder.Ollama
}

// NewVector создаёт vector-индексатор с двумя коллекциями. emb — legacy
// embedder, code/text создаются автоматически на основе cfg.Collections.*.
func NewVector(cfg *config.Config, qd *qdrant.Client, emb *embedder.Ollama, st *store.SQLite, bus *state.Bus) *Vector {
	codeSpec := cfg.CodeCollection()
	textSpec := cfg.TextCollection()
	code := embedder.New(cfg.Ollama.URL, codeSpec.EmbedModel)
	code.SetDim(int(codeSpec.EmbedDim))
	text := embedder.New(cfg.Ollama.URL, textSpec.EmbedModel)
	text.SetDim(int(textSpec.EmbedDim))

	return &Vector{
		cfg:     cfg,
		qd:      qd,
		emb:     emb,
		code:    code,
		text:    text,
		parser:  parser.New(),
		chunker: chunker.New(cfg.ChunkLines, cfg.ChunkOverlap),
		bus:     bus,
		matcher: fileutil.NewMatcher(cfg.Ignore),
		store:   st,
		sem:     make(chan struct{}, cfg.EmbedParallelism),
	}
}

// SetBM25 подключает Bleve-индекс к индексатору; если bm25 == nil —
// лексический индекс не используется.
func (v *Vector) SetBM25(idx bm25.Index) { v.bm25 = idx }

// Init создаёт обе коллекции в Qdrant и при необходимости запускает
// автоматическую переиндексацию (когда модель эмбеддингов сменилась).
func (v *Vector) Init(ctx context.Context) error {
	codeSpec := v.cfg.CodeCollection()
	textSpec := v.cfg.TextCollection()

	if err := v.ensureCollection(ctx, codeSpec); err != nil {
		return err
	}
	if err := v.ensureCollection(ctx, textSpec); err != nil {
		return err
	}
	return nil
}

func (v *Vector) ensureCollection(ctx context.Context, sp config.CollectionSpec) error {
	if sp.Name == "" {
		return nil
	}
	// 1. Сверяем embed_meta — если модель сменилась, удаляем коллекцию для reindex.
	if v.store != nil {
		prev, err := v.store.GetEmbedMeta(ctx, sp.Name)
		if err == nil && prev != nil && (prev.Model != sp.EmbedModel || uint64(prev.Dim) != sp.EmbedDim) {
			log.Printf("vector: embed model changed for collection %q: %s/%d -> %s/%d — recreating", sp.Name, prev.Model, prev.Dim, sp.EmbedModel, sp.EmbedDim)
			if err := v.qd.DeleteCollection(ctx, sp.Name); err != nil {
				log.Printf("vector: delete %q: %v (continuing)", sp.Name, err)
			}
		}
	}
	if err := v.qd.EnsureCollection(ctx, sp.Name, sp.EmbedDim, qdrant.Cosine); err != nil {
		return fmt.Errorf("ensure %q: %w", sp.Name, err)
	}
	if v.store != nil {
		_ = v.store.SetEmbedMeta(ctx, store.EmbedMeta{
			Collection: sp.Name,
			Model:      sp.EmbedModel,
			Dim:        int(sp.EmbedDim),
		})
	}
	return nil
}

// pickCollection возвращает целевую коллекцию + соответствующий эмбеддер
// по языку.
func (v *Vector) pickCollection(lang string) (config.CollectionSpec, *embedder.Ollama) {
	if codeCollectionLanguages[lang] {
		return v.cfg.CodeCollection(), v.code
	}
	return v.cfg.TextCollection(), v.text
}

// SyncStats обновляет агрегированную статистику по обеим коллекциям.
func (v *Vector) SyncStats(ctx context.Context) {
	total := 0
	for _, name := range []string{v.cfg.CodeCollection().Name, v.cfg.TextCollection().Name} {
		st, err := v.qd.GetCollectionStats(ctx, name)
		if err == nil {
			total += st.PointsCount
		}
	}
	v.totalChunks.Store(int64(total))
	if v.bus != nil {
		v.bus.SetIndexer("vector", func(i *state.Indexer) {
			i.Chunks = total
		})
	}
}

// FullScan индексирует все подходящие файлы с использованием глобального батчинга чанков.
func (v *Vector) FullScan(ctx context.Context) error {
	v.totalIndexed.Store(0)
	v.SyncStats(ctx)
	if v.bus != nil {
		v.bus.SetIndexer("vector", func(i *state.Indexer) {
			i.Status = "scanning"
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
					preparedChan <- pf
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

	// На самом деле, чтобы не копить ВСЕ чанки в памяти, будем обрабатывать их по мере поступления.
	// Но для эффективного батчинга нам нужно собирать их в пачки.
	
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

// Удаляем глобальную переменную totalIndexed

func (v *Vector) prepareFile(ctx context.Context, abs string) (*preparedFile, error) {
	src, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	hash := fileutil.HashBytes(src)
	if hash != "" && v.store != nil {
		prev, _ := v.store.GetFile(ctx, abs)
		if prev != nil && prev.VecHash == hash {
			return nil, nil
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
	return &preparedFile{
		abs:      abs,
		rel:      rel,
		lang:     lang,
		hash:     hash,
		chunks:   chunks,
		collSpec: collection,
		emb:      emb,
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

	for pf := range preparedChan {
		if codeBatcher.spec.Name == pf.collSpec.Name {
			codeBatcher.items <- pf
		} else {
			textBatcher.items <- pf
		}
	}
	close(codeBatcher.items)
	close(textBatcher.items)
	wg.Wait()

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
	const maxBatchChunks = 64
	var currentBatch []*preparedFile
	var currentChunks int

	flush := func() {
		if len(currentBatch) == 0 {
			return
		}
		if err := b.v.processBatch(ctx, b.spec, b.emb, currentBatch, b.total); err != nil {
			log.Printf("vector: batch error: %v", err)
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
					log.Printf("vector: large file error %s: %v", pf.abs, err)
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
	var allTexts []string
	for _, f := range files {
		for _, ch := range f.chunks {
			allTexts = append(allTexts, combinedText(ch))
		}
	}

	// Семафор только на время эмбеддинга
	select {
	case v.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	vecs, err := emb.EmbedBatch(ctx, allTexts)
	<-v.sem

	if err != nil {
		return err
	}

	var allPoints []qdrant.Point
	var allDocs []bm25.Doc
	vecIdx := 0
	for _, f := range files {
		// Старые данные в Qdrant удаляем только для тех коллекций, куда можем писать
		_ = v.qd.DeleteByFilter(ctx, spec.Name, "file", f.abs)
		// Если это текст, а мы в коллекции кода (или наоборот), на всякий случай чистим и другую
		otherColl := v.cfg.TextCollection().Name
		if spec.Name == v.cfg.TextCollection().Name {
			otherColl = v.cfg.CodeCollection().Name
		}
		if otherColl != spec.Name {
			_ = v.qd.DeleteByFilter(ctx, otherColl, "file", f.abs)
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

		// Пишем хеш
		if v.store != nil && f.hash != "" {
			_ = v.store.UpsertVectorHash(ctx, f.abs, f.hash, f.lang)
		}
		if v.bm25 != nil {
			_ = v.bm25.DeleteByPath(ctx, f.abs)
			for i, ch := range f.chunks {
				allDocs = append(allDocs, bm25.Doc{
					ID:        fmt.Sprintf("%s#%d", f.abs, i),
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
		setErr(v.qd.Upsert(ctx, spec.Name, allPoints))
	}()

	if v.bm25 != nil && len(allDocs) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
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

// IndexFile полностью переиндексирует один файл.
func (v *Vector) IndexFile(ctx context.Context, abs string) error {
	start := time.Now()
	pf, err := v.prepareFile(ctx, abs)
	if err != nil {
		return err
	}
	if pf == nil {
		return nil
	}

	// Эмбедим один файл (батчинг внутри файла уже в EmbedBatch)
	allTexts := make([]string, len(pf.chunks))
	for i, ch := range pf.chunks {
		allTexts[i] = combinedText(ch)
	}

	select {
	case v.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	vecs, err := pf.emb.EmbedBatch(ctx, allTexts)
	<-v.sem

	if err != nil {
		return err
	}

	// Очистка старых данных перед вставкой
	for _, name := range []string{v.cfg.CodeCollection().Name, v.cfg.TextCollection().Name} {
		_ = v.qd.DeleteByFilter(ctx, name, "file", abs)
	}
	if v.bm25 != nil {
		_ = v.bm25.DeleteByPath(ctx, abs)
	}

	points := make([]qdrant.Point, len(pf.chunks))
	for i, vec := range vecs {
		ch := pf.chunks[i]
		points[i] = qdrant.Point{
			ID:     chunkID(pf.abs, i),
			Vector: vec,
			Payload: map[string]any{
				"file":       pf.abs,
				"rel":        pf.rel,
				"language":   pf.lang,
				"start_line": ch.StartLine,
				"end_line":   ch.EndLine,
				"kind":       ch.Kind,
				"symbol":     ch.Symbol,
				"text":       ch.Text,
				"comments":   ch.Comments,
				"parent":     ch.Parent,
				"imports":    ch.Imports,
				"collection": pf.collSpec.Name,
			},
		}
	}

	var allDocs []bm25.Doc
	if v.bm25 != nil {
		allDocs = make([]bm25.Doc, len(pf.chunks))
		for i, ch := range pf.chunks {
			allDocs[i] = bm25.Doc{
				ID:        fmt.Sprintf("%s#%d", pf.abs, i),
				Path:      pf.abs,
				Language:  pf.lang,
				Kind:      ch.Kind,
				Symbol:    ch.Symbol,
				Content:   combinedText(ch),
				StartLine: ch.StartLine,
				EndLine:   ch.EndLine,
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
		setErr(v.qd.Upsert(ctx, pf.collSpec.Name, points))
	}()

	if v.bm25 != nil && len(allDocs) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			setErr(v.bm25.IndexDocs(ctx, allDocs))
		}()
	}

	wg.Wait()
	if lastErr != nil {
		return lastErr
	}

	if v.store != nil && pf.hash != "" {
		_ = v.store.UpsertVectorHash(ctx, pf.abs, pf.hash, pf.lang)
	}

	v.totalChunks.Add(int64(len(points)))

	if v.bus != nil {
		v.bus.AddRecent(state.FileEntry{
			Path:       pf.rel,
			Kind:       "vector",
			Chunks:     len(points),
			DurationMs: time.Since(start).Milliseconds(),
		})
		v.bus.SetIndexer("vector", func(i *state.Indexer) {
			i.Chunks = int(v.totalChunks.Load())
		})
	}
	return nil
}

// RemoveFile удаляет все точки файла из обеих коллекций + BM25.
func (v *Vector) RemoveFile(ctx context.Context, abs string) error {
	for _, name := range []string{v.cfg.CodeCollection().Name, v.cfg.TextCollection().Name} {
		_ = v.qd.DeleteByFilter(ctx, name, "file", abs)
	}
	if v.bm25 != nil {
		_ = v.bm25.DeleteByPath(ctx, abs)
	}
	return nil
}

// Watch обрабатывает события вотчера.
func (v *Vector) Watch(ctx context.Context, w *watcher.Watcher) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-w.Events():
			if !ok {
				return nil
			}
			switch ev.Kind {
			case watcher.EventRemove, watcher.EventRename:
				_ = v.RemoveFile(ctx, ev.AbsPath)
			default:
				_ = v.IndexFile(ctx, ev.AbsPath)
			}
		}
	}
}

// Search — семантический поиск top-K. Если в filter указан language —
// автоматически выбирается соответствующая коллекция; иначе ищет в обеих
// и объединяет результаты, отсортировав по score.
func (v *Vector) Search(ctx context.Context, query string, limit int, filter map[string]any) ([]qdrant.SearchHit, error) {
	if limit <= 0 {
		limit = 10
	}
	lang, _ := filter["language"].(string)

	var collections []config.CollectionSpec
	var embs []*embedder.Ollama
	if lang != "" {
		sp, e := v.pickCollection(lang)
		collections = []config.CollectionSpec{sp}
		embs = []*embedder.Ollama{e}
	} else {
		collections = []config.CollectionSpec{v.cfg.CodeCollection(), v.cfg.TextCollection()}
		embs = []*embedder.Ollama{v.code, v.text}
	}

	f := buildFilter(filter)

	var all []qdrant.SearchHit
	for i, sp := range collections {
		vec, err := embs[i].Embed(ctx, query)
		if err != nil {
			// Embed может фейлиться, если модель не загружена; пропускаем
			// коллекцию с warning'ом, но не валим весь поиск.
			log.Printf("vector: embed for %q failed: %v", sp.Name, err)
			continue
		}
		hits, err := v.qd.Search(ctx, sp.Name, vec, limit, f)
		if err != nil {
			log.Printf("vector: qdrant search %q: %v", sp.Name, err)
			continue
		}
		all = append(all, hits...)
	}

	// Сортируем по score (Qdrant отдаёт по убыванию для cosine — это уже так).
	// Стабильно объединяем и обрезаем.
	if len(all) > limit {
		// Простое top-K merge: отсортируем убывая по score.
		for i := 0; i < len(all); i++ {
			for j := i + 1; j < len(all); j++ {
				if all[j].Score > all[i].Score {
					all[i], all[j] = all[j], all[i]
				}
			}
		}
		all = all[:limit]
	}
	return all, nil
}

func buildFilter(filter map[string]any) map[string]any {
	if len(filter) == 0 {
		return nil
	}
	must := make([]map[string]any, 0, len(filter))
	for k, val := range filter {
		must = append(must, map[string]any{"key": k, "match": map[string]any{"value": val}})
	}
	return map[string]any{"must": must}
}

// SimilarToUnit реализует symbols.SimilarSearcher: ищет AST units, чьи
// эмбеддинги ближе всего к содержимому unit'а u. Сейчас — приближение
// через text-based семантический поиск по подписи + первой строке тела.
func (v *Vector) SimilarToUnit(ctx context.Context, u store.ASTUnit, limit int) ([]store.ASTUnit, error) {
	if v.store == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	// Читаем подпись + первые строки тела как query.
	query := u.Signature
	if query == "" {
		query = u.Name
	}
	if u.FilePath != "" && u.StartByte < u.EndByte {
		if src, err := os.ReadFile(u.FilePath); err == nil {
			start := u.StartByte
			end := u.EndByte
			if end > len(src) {
				end = len(src)
			}
			if start < end {
				snippet := string(src[start:end])
				if len(snippet) > 1500 {
					snippet = snippet[:1500]
				}
				query = snippet
			}
		}
	}
	hits, err := v.Search(ctx, query, limit*2, map[string]any{"language": u.Language})
	if err != nil {
		return nil, err
	}
	out := make([]store.ASTUnit, 0, limit)
	seen := map[int64]bool{u.ID: true}
	for _, h := range hits {
		path, _ := h.Payload["file"].(string)
		if path == "" {
			continue
		}
		units, err := v.store.ListASTUnitsByFile(ctx, path)
		if err != nil {
			continue
		}
		// Берём unit, чья область [start_line, end_line] пересекается с
		// чанком hit'а.
		startLine, _ := h.Payload["start_line"].(float64)
		endLine, _ := h.Payload["end_line"].(float64)
		var best *store.ASTUnit
		for i := range units {
			cu := units[i]
			if cu.Kind == "module" || seen[cu.ID] {
				continue
			}
			if int(startLine) >= cu.StartLine && int(endLine) <= cu.EndLine {
				if best == nil || (cu.EndLine-cu.StartLine) < (best.EndLine-best.StartLine) {
					best = &cu
				}
			}
		}
		if best != nil {
			seen[best.ID] = true
			out = append(out, *best)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// combinedText формирует текст для эмбеддинга: лидирующие doc-комментарии
// (если есть) + тело чанка. Это улучшает семантический поиск, так как
// комментарии часто содержат намерение/описание кода на естественном языке.
func combinedText(ch chunker.Chunk) string {
	if ch.Comments == "" {
		return ch.Text
	}
	return ch.Comments + "\n" + ch.Text
}

// chunkID — детерминированный hex-id (sha1[:32]).
func chunkID(file string, idx int) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%s#%d", file, idx)))
	hexStr := hex.EncodeToString(h[:16])
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexStr[0:8], hexStr[8:12], hexStr[12:16], hexStr[16:20], hexStr[20:32])
}

// --- hybrid retriever adapters ---
//
// VectorRetriever / BM25Retriever из internal/hybrid реализуются через
// adapter'ы, которые конвертируют локальные результаты в hybrid.Candidate.
// Сами adapter'ы живут в отдельном файле internal/index/hybrid_adapter.go,
// чтобы избежать import cycle (hybrid импортирует тип Candidate, а Vector
// его конвертирует).

