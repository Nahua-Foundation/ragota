package cli

import (
	"context"
	"time"

	"ragota/internal/indexing/embedder"
	"ragota/internal/indexing/vector"
	"ragota/pkg/qdrant"
	"ragota/pkg/state"
)

// waitAndScanVector ждёт готовности qdrant и ollama, затем full-scan.
func waitAndScanVector(ctx context.Context, qd *qdrant.Client, emb *embedder.Ollama, vIdx *vector.Vector, bus *state.Bus) {
	for range 30 {
		if ctx.Err() != nil {
			return
		}
		pCtx, c2 := context.WithTimeout(ctx, 3*time.Second)
		qErr := qd.Ping(pCtx)
		var oErr error
		if emb != nil {
			oErr = emb.Ping(pCtx)
		}
		c2()
		if qErr == nil && oErr == nil {
			if err := vIdx.Init(ctx); err != nil {
				bus.SetIndexer("vector", func(i *state.Indexer) {
					i.Status = "error"
					i.LastError = "qdrant init: " + err.Error()
				})
				return
			}
			_ = vIdx.FullScan(ctx)
			return
		}
		bus.SetIndexer("vector", func(i *state.Indexer) {
			i.Status = "waiting"
			if qErr != nil {
				i.LastError = "waiting qdrant: " + qErr.Error()
			} else if oErr != nil {
				i.LastError = "waiting ollama: " + oErr.Error()
			}
		})
		time.Sleep(2 * time.Second)
	}
}
