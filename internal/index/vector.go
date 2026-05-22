package index

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

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

// Vector — векторный индексатор: парсит файл, режет на чанки, эмбеддит ollama,
// сохраняет в Qdrant. Для каждого файла ведётся переиндексация: при изменении
// удаляются все старые точки по payload.file и вставляются новые.
type Vector struct {
	cfg     *config.Config
	qd      *qdrant.Client
	emb     *embedder.Ollama
	parser  *parser.Parser
	chunker *chunker.Chunker
	bus     *state.Bus
	matcher *fileutil.Matcher
	store   *store.SQLite

	mu          sync.Mutex
	totalChunks atomic.Int64
	sem         chan struct{} // семафор для Ollama
}

// NewVector создаёт vector-индексатор.
func NewVector(cfg *config.Config, qd *qdrant.Client, emb *embedder.Ollama, st *store.SQLite, bus *state.Bus) *Vector {
	return &Vector{
		cfg:     cfg,
		qd:      qd,
		emb:     emb,
		parser:  parser.New(),
		chunker: chunker.New(cfg.ChunkLines, cfg.ChunkOverlap),
		bus:     bus,
		matcher: fileutil.NewMatcher(cfg.Ignore),
		store:   st,
		sem:     make(chan struct{}, 4), // макс 4 параллельных запроса к Ollama
	}
}

// Init обеспечивает существование коллекции с нужной размерностью.
func (v *Vector) Init(ctx context.Context) error {
	return v.qd.EnsureCollection(ctx, v.cfg.Collection, v.cfg.Ollama.EmbedDim, qdrant.Cosine)
}

// SyncStats запрашивает текущее кол-во точек из Qdrant и обновляет счетчик.
func (v *Vector) SyncStats(ctx context.Context) {
	st, err := v.qd.GetCollectionStats(ctx, v.cfg.Collection)
	if err == nil {
		v.totalChunks.Store(int64(st.PointsCount))
		if v.bus != nil {
			v.bus.SetIndexer("vector", func(i *state.Indexer) {
				i.Chunks = st.PointsCount
			})
		}
	}
}

// FullScan индексирует все подходящие файлы.
func (v *Vector) FullScan(ctx context.Context) error {
	v.SyncStats(ctx)
	if v.bus != nil {
		v.bus.SetIndexer("vector", func(i *state.Indexer) {
			i.Status = "scanning"
			i.LastError = ""
		})
	}

	// Сначала собираем список всех файлов для точного прогресса.
	var allFiles []string
	_ = fileutil.WalkFiles(v.cfg.Root, v.matcher, v.cfg.Extensions, func(abs, rel string, _ os.FileInfo) error {
		allFiles = append(allFiles, abs)
		return nil
	})

	total := len(allFiles)
	var indexed int
	var lastErr error

	for idx, abs := range allFiles {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := v.IndexFile(ctx, abs); err == nil {
			indexed++
		} else {
			lastErr = err
		}

		if v.bus != nil {
			v.bus.SetIndexer("vector", func(st *state.Indexer) {
				st.FilesTotal = total
				st.FilesIndexed = idx + 1
				st.Status = "indexing"
				st.Chunks = int(v.totalChunks.Load())
				if lastErr != nil {
					st.LastError = lastErr.Error()
				}
			})
		}
	}

	if v.bus != nil {
		v.bus.SetIndexer("vector", func(i *state.Indexer) {
			if lastErr != nil && indexed < total {
				i.Status = "error"
				i.LastError = "last error: " + lastErr.Error()
			} else {
				i.Status = "idle"
			}
			// Убеждаемся, что статистика верна в конце
			i.FilesTotal = total
			i.FilesIndexed = total
			i.Chunks = int(v.totalChunks.Load())
		})
	}
	return nil
}

// IndexFile полностью переиндексирует один файл.
func (v *Vector) IndexFile(ctx context.Context, abs string) error {
	start := time.Now()
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return nil
	}

	hash, err := fileutil.HashFile(abs)
	if err == nil && v.store != nil {
		prev, _ := v.store.GetFile(ctx, abs)
		if prev != nil && prev.VecHash == hash {
			return nil // Skip: уже проиндексировано
		}
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	src, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	lang := fileutil.LanguageByExt(filepath.Ext(abs))
	syms, _ := v.parser.Parse(ctx, lang, abs, src)

	var chunks []chunker.Chunk
	treeChunks := v.parser.ParseChunks(ctx, lang, abs, src, chunker.MaxChunkBytes)
	if len(treeChunks) > 0 {
		// Используем Tree-sitter чанкинг для покрытия всего файла
		chunks = v.chunker.ChunkByTree(abs, lang, src, treeChunks)
		// Также добавляем символы (функции/классы) для сохранения метаданных имен
		allChunks := v.chunker.Chunk(abs, lang, src, syms)
		for _, c := range allChunks {
			if c.Kind == "symbol" {
				chunks = append(chunks, c)
			}
		}
	} else {
		// Fallback на скользящее окно для неподдерживаемых языков
		chunks = v.chunker.Chunk(abs, lang, src, syms)
	}

	if len(chunks) == 0 {
		// файл пустой/только пробелы — просто удалим возможные старые точки
		_ = v.qd.DeleteByFilter(ctx, v.cfg.Collection, "file", abs)
		if v.store != nil && err == nil {
			_ = v.store.UpsertVectorHash(ctx, abs, hash, lang)
		}
		return nil
	}

	// удаляем старые точки этого файла, чтобы не плодить дубликаты
	if err := v.qd.DeleteByFilter(ctx, v.cfg.Collection, "file", abs); err != nil {
		return fmt.Errorf("vector delete old: %w", err)
	}

	rel, _ := filepath.Rel(v.cfg.Root, abs)
	points := make([]qdrant.Point, len(chunks))

	// Эмбеддинг чанков (параллельно с семафором)
	type embRes struct {
		idx int
		vec []float32
		err error
	}
	resChan := make(chan embRes, len(chunks))
	for i, ch := range chunks {
		go func(i int, text string) {
			select {
			case v.sem <- struct{}{}:
				defer func() { <-v.sem }()
			case <-ctx.Done():
				resChan <- embRes{err: ctx.Err()}
				return
			}
			vec, err := v.emb.Embed(ctx, text)
			resChan <- embRes{idx: i, vec: vec, err: err}
		}(i, ch.Text)
	}

	for range chunks {
		res := <-resChan
		if res.err != nil {
			return res.err
		}
		ch := chunks[res.idx]
		points[res.idx] = qdrant.Point{
			ID:     chunkID(abs, res.idx),
			Vector: res.vec,
			Payload: map[string]any{
				"file":       abs,
				"rel":        rel,
				"language":   lang,
				"start_line": ch.StartLine,
				"end_line":   ch.EndLine,
				"kind":       ch.Kind,
				"symbol":     ch.Symbol,
				"text":       ch.Text,
				"parent":     ch.Parent,
				"imports":    ch.Imports,
			},
		}
	}

	if err := v.qd.Upsert(ctx, v.cfg.Collection, points); err != nil {
		return fmt.Errorf("qdrant upsert: %w", err)
	}
	v.totalChunks.Add(int64(len(points)))

	if v.store != nil {
		_ = v.store.UpsertVectorHash(ctx, abs, hash, lang)
	}

	if v.bus != nil {
		v.bus.AddRecent(state.FileEntry{
			Path:       rel,
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

// RemoveFile удаляет все точки файла из Qdrant.
func (v *Vector) RemoveFile(ctx context.Context, abs string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.qd.DeleteByFilter(ctx, v.cfg.Collection, "file", abs)
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

// Search — семантический поиск top-K. filter может включать "language", "rel" и т.п.
func (v *Vector) Search(ctx context.Context, query string, limit int, filter map[string]any) ([]qdrant.SearchHit, error) {
	vec, err := v.emb.Embed(ctx, query)
	if err != nil {
		return nil, err
	}
	var f map[string]any
	if len(filter) > 0 {
		must := make([]map[string]any, 0, len(filter))
		for k, val := range filter {
			must = append(must, map[string]any{"key": k, "match": map[string]any{"value": val}})
		}
		f = map[string]any{"must": must}
	}
	return v.qd.Search(ctx, v.cfg.Collection, vec, limit, f)
}

// symbolsFromParser — небольшой адаптер, чтобы не тянуть parser-импорт в chunker напрямую.
func symbolsFromParser(in []parser.Symbol) []parser.Symbol { return in }

// chunkID — детерминированный hex-id (sha1[:32]).
// Qdrant принимает строковые id (uuid или произвольные unsigned int).
// Используем uuid-подобную строку из хеша.
func chunkID(file string, idx int) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%s#%d", file, idx)))
	// Превращаем sha1 в UUID-формат 8-4-4-4-12 (первые 16 байт).
	hexStr := hex.EncodeToString(h[:16])
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexStr[0:8], hexStr[8:12], hexStr[12:16], hexStr[16:20], hexStr[20:32])
}
