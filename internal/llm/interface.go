package llm

import (
	"context"
)

// Embedder is the interface for embedding models.
type Embedder interface {
	// Name returns the embedder name.
	Name() string

	// Embed generates embeddings for texts.
	// Returns a slice of vectors, one per text.
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// Dim returns the dimension of the embedding vectors.
	Dim() int
}
