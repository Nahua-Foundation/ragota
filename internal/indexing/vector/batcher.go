package vector

// Файл реализует batcher: накопление подготовленных файлов и запись
// в Qdrant/BM25 пачками.

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"ragota/pkg/config"
	"ragota/internal/indexing/embedder"
	"ragota/pkg/logger"
	"ragota/pkg/qdrant"
)

// batcher собирает подготовленные файлы по целевой коллекции и
// сбрасывает их пачками в processBatch.
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
		for range currentBatch {
			b.v.updateProgress(b.total)
		}
		currentBatch = nil
		currentChunks = 0
	}

	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case pf, ok := <-b.items:
			if !ok {
				flush()
				return
			}

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
	var allTexts []string
	var deletePaths []string
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

	var vecs [][]float32
	var embedErr error
	var embedWg sync.WaitGroup
	embedWg.Add(1)
	go func() {
		defer embedWg.Done()
		if ctx.Err() != nil {
			embedErr = ctx.Err()
			return
		}
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

	allPoints, allDocs := v.buildPointsAndDocs(ctx, spec, files, vecs)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	return v.upsertBatch(ctx, spec, allPoints, allDocs)
}

// buildPointsAndDocs формирует Qdrant points и sink docs из подготовленных файлов.
func (v *Vector) buildPointsAndDocs(ctx context.Context, spec config.CollectionSpec, files []*preparedFile, vecs [][]float32) ([]qdrant.Point, []Doc) {
	var allPoints []qdrant.Point
	var allDocs []Doc
	vecIdx := 0
	for _, f := range files {
		if ctx.Err() != nil {
			return allPoints, allDocs
		}

		for i, ch := range f.chunks {
			allPoints = append(allPoints, qdrant.Point{
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
			})
			vecIdx++
		}

		if v.store != nil && f.hash != "" {
			_ = v.store.UpsertVectorHash(ctx, f.abs, f.hash, f.lang)
		}
		if v.writeSink() != nil {
			for i, ch := range f.chunks {
				allDocs = append(allDocs, Doc{
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
	return allPoints, allDocs
}

// upsertBatch записывает points в Qdrant и docs в search sink последовательно
// (BM25 first, Qdrant last) для обеспечения консистентности: если Qdrant
// упал, BM25 можно откатить; если BM25 упал, Qdrant не пишет.
func (v *Vector) upsertBatch(ctx context.Context, spec config.CollectionSpec, allPoints []qdrant.Point, allDocs []Doc) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Сначала cheap локальная операция (BM25/sink).
	if v.writeSink() != nil && len(allDocs) > 0 {
		if err := v.writeSink().IndexDocs(ctx, allDocs); err != nil {
			return err
		}
	}

	// Потом expensive remote операция (Qdrant).
	if len(allPoints) > 0 {
		if err := v.qd.Upsert(ctx, spec.Name, allPoints); err != nil {
			return err
		}
	}

	v.totalChunks.Add(int64(len(allPoints)))
	return nil
}
