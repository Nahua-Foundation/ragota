package index

// Файл реализует пошаговую обработку одного файла: IndexFile, RemoveFile,
// Watch. Используется при инкрементальном обновлении (fs-вотчер) и
// одноразовых вызовах извне.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"ragota/internal/bm25"
	"ragota/internal/qdrant"
	"ragota/internal/state"
	"ragota/internal/watcher"
)

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

	var allDocs []bm25.Doc
	if v.bm25 != nil {
		allDocs = make([]bm25.Doc, len(pf.chunks))
		for i, ch := range pf.chunks {
			allDocs[i] = bm25.Doc{
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
