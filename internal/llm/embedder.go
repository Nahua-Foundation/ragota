package llm

import (
	"context"
	"fmt"

	"github.com/Nahua-Foundation/ragota/internal/config"
)

func resolveDims(cfg *config.EmbedderConfig) (int, error) {
	if cfg.Dimensions > 0 {
		return cfg.Dimensions, nil
	}
	if d, ok := knownDims[cfg.Model]; ok {
		return d, nil
	}
	return 0, fmt.Errorf("unknown embedding model %q: set indexes.vector.embedder.dimensions explicitly", cfg.Model)
}

// embedBatched splits texts into batches and calls fn for each.
func embedBatched(ctx context.Context, batchSize int, texts []string,
	fn func(ctx context.Context, batch []string) ([][]float32, error)) ([][]float32, error) {

	if batchSize <= 0 {
		batchSize = 64
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += batchSize {
		end := min(start+batchSize, len(texts))
		vecs, err := fn(ctx, texts[start:end])
		if err != nil {
			return nil, fmt.Errorf("embed batch %d-%d: %w", start, end, err)
		}
		if len(vecs) != end-start {
			return nil, fmt.Errorf("embed batch %d-%d: got %d vectors, want %d", start, end, len(vecs), end-start)
		}
		out = append(out, vecs...)
	}
	return out, nil
}
