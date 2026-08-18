package llm

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/httpx"
)

// normalizeOpenAIBase strips a trailing slash and a trailing "/v1" from a
// configured base URL. Callers append the full "/v1/..." path, so both
// "https://host" and "https://host/v1" — the two ways OpenAI-compatible
// gateways (vLLM, LiteLLM, Ollama) publish their endpoint — resolve to the
// same request.
// authHeader returns the bearer header for an API key, or nil when there is
// none — a keyless gateway must not receive an empty "Bearer " credential.
func authHeader(apiKey string) http.Header {
	if apiKey == "" {
		return nil
	}
	return http.Header{"Authorization": []string{"Bearer " + apiKey}}
}

func normalizeOpenAIBase(baseURL string) string {
	if baseURL == "" {
		return DefaultOpenAIBaseURL
	}
	return strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
}

// OpenAI implements llm.Embedder with OpenAI backend.
type OpenAI struct {
	client *httpx.Client
	model  string
	dims   int
	batch  int
}

// NewOpenAI creates a new OpenAI embedder.
func NewOpenAI(cfg *config.EmbedderConfig, apiKey string) (*OpenAI, error) {
	base := normalizeOpenAIBase(cfg.BaseURL)
	if apiKey == "" && base == DefaultOpenAIBaseURL {
		return nil, fmt.Errorf("openai api key is required")
	}

	dims, err := resolveDims(cfg)
	if err != nil {
		return nil, err
	}

	return &OpenAI{
		client: &httpx.Client{
			BaseURL: base,
			Header:  authHeader(apiKey),
		},
		model: cfg.Model,
		dims:  dims,
		batch: cfg.BatchSize,
	}, nil
}

// Name returns the embedder name.
func (e *OpenAI) Name() string { return "openai" }

// Embed generates embeddings for texts.
func (e *OpenAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return embedBatched(ctx, e.batch, texts, e.embedOnce)
}

type openaiReq struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type openaiItem struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

type openaiResp struct {
	Data []openaiItem `json:"data"`
}

func (e *OpenAI) embedOnce(ctx context.Context, batch []string) ([][]float32, error) {
	var resp openaiResp
	if err := e.client.PostJSON(ctx, "/v1/embeddings", openaiReq{Model: e.model, Input: batch}, &resp); err != nil {
		return nil, err
	}

	items := make([]openaiItem, len(resp.Data))
	copy(items, resp.Data)
	sort.Slice(items, func(i, j int) bool { return items[i].Index < items[j].Index })

	results := make([][]float32, len(items))
	for i, item := range items {
		results[i] = item.Embedding
	}
	return results, nil
}

// Dim returns the dimension of the embedding vectors.
func (e *OpenAI) Dim() int { return e.dims }
