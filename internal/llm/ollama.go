package llm

import (
	"context"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/httpx"
)

// knownDims maps model names to their embedding dimensions.
var knownDims = map[string]int{
	"nomic-embed-text":       768,
	"nomic-embed-text:v1.5":  768,
	"BAAI/bge-small-en-v1.5": 384,
	"bge-small-en-v1.5":      384,
	"all-minilm":             384,
	"all-MiniLM-L6-v2":       384,
	"qwen3-embedding:0.6b":   1536,
	"qwen3-embedding":        1536,
	"text-embedding-3-small": 1536,
	"text-embedding-3-large": 3072,
	"text-embedding-ada-002": 1536,
}

// Ollama implements llm.Embedder with Ollama backend.
type Ollama struct {
	client *httpx.Client
	model  string
	dims   int
	batch  int
}

// NewOllama creates a new Ollama embedder.
func NewOllama(cfg *config.EmbedderConfig) (*Ollama, error) {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}

	dims, err := resolveDims(cfg)
	if err != nil {
		return nil, err
	}

	return &Ollama{
		client: &httpx.Client{BaseURL: baseURL},
		model:  cfg.Model,
		dims:   dims,
		batch:  cfg.BatchSize,
	}, nil
}

// Name returns the embedder name.
func (e *Ollama) Name() string { return "ollama" }

// Embed generates embeddings for texts.
func (e *Ollama) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return embedBatched(ctx, e.batch, texts, e.embedOnce)
}

type ollamaReq struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type ollamaResp struct {
	Embeddings [][]float32 `json:"embeddings"`
}

func (e *Ollama) embedOnce(ctx context.Context, batch []string) ([][]float32, error) {
	var resp ollamaResp
	if err := e.client.PostJSON(ctx, "/api/embed", ollamaReq{Model: e.model, Input: batch}, &resp); err != nil {
		return nil, err
	}
	return resp.Embeddings, nil
}

// Dim returns the dimension of the embedding vectors.
func (e *Ollama) Dim() int { return e.dims }
