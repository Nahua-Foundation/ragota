// Package index реализует векторный + BM25 индексатор кодовой базы.
//
// Публичный тип — Vector. Он держит две Qdrant-коллекции (code/text)
// с разными моделями эмбеддингов, опциональный BM25 (Bleve) и адаптеры
// для пакета hybrid.
//
// Реализация декомпозирована по доменам (все файлы — package index):
//
//   - vector.go         — типы (Vector, preparedFile), конструктор,
//     Init/ensureCollection/pickCollection/SyncStats,
//     SetBM25 и константа codeCollectionLanguages.
//   - fullscan.go       — FullScan и пакетный пайплайн
//     (prepareFile → batcher.run → processBatch).
//   - indexfile.go      — инкрементальный путь: IndexFile, RemoveFile, Watch.
//   - search.go         — Search, SimilarToUnit и общие helper'ы
//     (combinedText, chunkID, buildFilter).
//   - hybrid_adapter.go — реализация интерфейсов internal/hybrid.
//   - treesitter.go     — вспомогательные функции tree-sitter символьного
//     поиска поверх векторного индекса.
package index

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"ragota/internal/bm25"
	"ragota/internal/chunker"
	"ragota/internal/config"
	"ragota/internal/embedder"
	"ragota/internal/fileutil"
	"ragota/internal/logger"
	"ragota/internal/parser"
	"ragota/internal/qdrant"
	"ragota/internal/repos"
	"ragota/internal/state"
	"ragota/internal/store"
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

	bm25    atomic.Pointer[bm25.Index] // atomic для безопасного concurrent read/write
	parser  *parser.Parser
	chunker *chunker.Chunker
	bus     *state.Bus
	matcher *fileutil.Matcher
	store   *store.SQLite
	// resolv — резолвер репо (multi-repo workspace). nil = single-root.
	resolv *repos.Resolver

	mu           sync.Mutex
	wg           sync.WaitGroup
	totalChunks  atomic.Int64
	totalIndexed atomic.Int32
	sem          chan struct{}
	scanMu       sync.Mutex // guard against concurrent FullScan
	scanning     bool
}

// preparedFile — промежуточное представление файла между сканером и
// батчером: содержит уже распарсенные чанки и целевую коллекцию.
type preparedFile struct {
	abs      string
	rel      string
	lang     string
	repo     string // имя репы (multi-repo workspace), "" = legacy/single-root
	hash     string
	chunks   []chunker.Chunk
	collSpec config.CollectionSpec
	emb      *embedder.Ollama
	// isKnown — файл ранее индексировался (VecHash был непустой).
	// Если false — файл новый и DeleteByFilter не нужен.
	isKnown bool
}

// NewVector создаёт vector-индексатор с двумя коллекциями. emb — legacy
// embedder, code/text создаются автоматически на основе cfg.Collections.*.
func NewVector(cfg *config.Config, qd *qdrant.Client, emb *embedder.Ollama, st *store.SQLite, bus *state.Bus) *Vector {
	codeSpec := cfg.CodeCollection()
	textSpec := cfg.TextCollection()

	sem := make(chan struct{}, cfg.EmbedParallelism)

	code := embedder.New(cfg.Ollama.URL, codeSpec.EmbedModel)
	code.SetDim(int(codeSpec.EmbedDim))
	code.SetSemaphore(sem)

	text := embedder.New(cfg.Ollama.URL, textSpec.EmbedModel)
	text.SetDim(int(textSpec.EmbedDim))
	text.SetSemaphore(sem)

	if emb != nil {
		emb.SetSemaphore(sem)
	}

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
		sem:     sem,
	}
}

func (v *Vector) GetSemaphore() chan struct{} {
	return v.sem
}

// SetBM25 подключает Bleve-индекс к индексатору; если bm25 == nil —
// лексический индекс не используется.
func (v *Vector) SetBM25(idx bm25.Index) { v.bm25.Store(&idx) }

// bm25Index возвращает текущий BM25 индекс (или nil).
func (v *Vector) bm25Index() bm25.Index {
	p := v.bm25.Load()
	if p == nil {
		return nil
	}
	return *p
}

// SetRepoResolver подключает резолвер репозиториев для multi-repo
// workspace. Если не вызван — поле `repo` в payload остаётся пустым.
func (v *Vector) SetRepoResolver(r *repos.Resolver) { v.resolv = r }

// Close ждет завершения всех активных операций индексации.
// Имеет встроенный таймаут, чтобы не зависнуть навсегда при остановке.
func (v *Vector) Close() {
	done := make(chan struct{})
	go func() {
		v.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		logger.Log().Warn().Msg("vector: Close timeout, some background tasks may still be running")
	}
}

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
			logger.Log().Info().Str("collection", sp.Name).
				Str("old_model", prev.Model).Int("old_dim", prev.Dim).
				Str("new_model", sp.EmbedModel).Uint64("new_dim", sp.EmbedDim).
				Msg("vector: embed model changed — recreating collection")
			if err := v.qd.DeleteCollection(ctx, sp.Name); err != nil {
				logger.Log().Warn().Err(err).Str("collection", sp.Name).Msg("vector: delete collection failed, continuing")
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
