package vector

import (
	"context"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/index"
)

// TestIndexReturnsErrorOnCanceledContext: a run whose context is already
// canceled must report the cancellation, not return (result, nil) that a caller
// would read as a complete short pass.
func TestIndexReturnsErrorOnCanceledContext(t *testing.T) {
	idx := New(&Config{
		Embedder: fakeEmbedder{},
		Storage:  &memVecStore{},
		Chunking: index.ChunkConfig{Method: "window", WindowLines: 60, Overlap: 10},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := idx.Index(ctx, &index.IndexRequest{
		RepoID:   "repo1",
		RepoPath: t.TempDir(),
		Files: []*index.FileToIndex{
			{Path: "demo/math.go", Language: "go", Content: []byte(goSource)},
		},
	})
	if err == nil {
		t.Fatal("Index with a canceled context should return a non-nil error")
	}
}
