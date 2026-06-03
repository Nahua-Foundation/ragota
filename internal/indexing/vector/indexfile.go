package vector

// Файл реализует пошаговую обработку одного файла: IndexFile, RemoveFile.
// Используется при инкрементальном обновлении (fs-вотчер) и
// одноразовых вызовах извне.

import (
	"context"
	"fmt"
	"time"

	"ragota/pkg/qdrant"
	"ragota/pkg/state"
)

// IndexFile полностью переиндексирует один файл.
func (v *Vector) IndexFile(ctx context.Context, abs string) error {
	v.wg.Add(1)
	defer v.wg.Done()

	if ctx.Err() != nil {
		return ctx.Err()
	}

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
	if v.writeSink() != nil {
		_ = v.writeSink().DeleteByPath(ctx, abs)
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
				"repo":       pf.repo,
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

	var allDocs []Doc
	if v.writeSink() != nil {
		allDocs = make([]Doc, len(pf.chunks))
		for i, ch := range pf.chunks {
			allDocs[i] = Doc{
				ID:        fmt.Sprintf("%s#%d", pf.abs, i),
				Repo:      pf.repo,
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

	// Последовательная запись: sink first (cheap), Qdrant last (expensive).
	if v.writeSink() != nil && len(allDocs) > 0 {
		if err := v.writeSink().IndexDocs(ctx, allDocs); err != nil {
			return err
		}
	}

	if err := v.qd.Upsert(ctx, pf.collSpec.Name, points); err != nil {
		return err
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

// RemoveFile удаляет все точки файла из обеих коллекций + search sink.
func (v *Vector) RemoveFile(ctx context.Context, abs string) error {
	v.wg.Add(1)
	defer v.wg.Done()

	if ctx.Err() != nil {
		return ctx.Err()
	}

	for _, name := range []string{v.cfg.CodeCollection().Name, v.cfg.TextCollection().Name} {
		_ = v.qd.DeleteByFilter(ctx, name, "file", abs)
	}
	if v.writeSink() != nil {
		_ = v.writeSink().DeleteByPath(ctx, abs)
	}
	return nil
}
