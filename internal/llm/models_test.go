package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/config"
)

func TestOllama_BatchedEmbedding(t *testing.T) {
	var requestCount int
	var lastInput []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		lastInput = req.Input

		vectors := make([][]float32, len(req.Input))
		for i := range vectors {
			vectors[i] = make([]float32, 768)
			for j := range vectors[i] {
				vectors[i][j] = float32(i*1000 + j)
			}
		}
		resp := map[string]interface{}{
			"embeddings": vectors,
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}))
	defer server.Close()

	cfg := &config.EmbedderConfig{
		BaseURL:   server.URL,
		Model:     "nomic-embed-text",
		BatchSize: 2,
	}

	embedder, err := NewOllama(cfg)
	if err != nil {
		t.Fatalf("NewOllama() error = %v", err)
	}

	texts := []string{"hello", "world", "test"}
	vectors, err := embedder.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed() error = %v", err)
	}

	if len(vectors) != 3 {
		t.Errorf("expected 3 vectors, got %d", len(vectors))
	}

	if requestCount != 2 {
		t.Errorf("expected 2 requests (batch 2+1), got %d", requestCount)
	}

	if len(lastInput) != 1 || lastInput[0] != "test" {
		t.Errorf("expected last batch to be ['test'], got %v", lastInput)
	}
}

func TestOllama_UnknownModel(t *testing.T) {
	cfg := &config.EmbedderConfig{
		Model: "unknown-model-xyz",
	}

	_, err := NewOllama(cfg)
	if err == nil {
		t.Error("expected error for unknown model")
	}
	if err.Error() == "" {
		t.Error("expected non-empty error message")
	}
}

func TestOpenAI_Embedding(t *testing.T) {
	var requestCount int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		var req openaiReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		vectors := make([]openaiItem, len(req.Input))
		for i := range vectors {
			vectors[i] = openaiItem{
				Index:     len(req.Input) - 1 - i,
				Embedding: make([]float32, 1536),
			}
			for j := range vectors[i].Embedding {
				vectors[i].Embedding[j] = float32(i*1000 + j)
			}
		}

		resp := map[string]interface{}{
			"data": vectors,
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}))
	defer server.Close()

	embedder, err := NewOpenAI(&config.EmbedderConfig{
		BaseURL: server.URL,
		Model:   "text-embedding-3-small",
	}, "fake-api-key")
	if err != nil {
		t.Fatalf("NewOpenAI() error = %v", err)
	}

	texts := []string{"first", "second", "third"}
	vectors, err := embedder.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed() error = %v", err)
	}

	if len(vectors) != 3 {
		t.Errorf("expected 3 vectors, got %d", len(vectors))
	}

	if requestCount != 1 {
		t.Errorf("expected 1 request, got %d", requestCount)
	}
}

func TestOpenAI_NoAPIKey(t *testing.T) {
	_, err := NewOpenAI(&config.EmbedderConfig{Model: "text-embedding-3-small"}, "")
	if err == nil {
		t.Error("expected error for missing API key")
	}
	if err.Error() != "openai api key is required" {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestOpenAI_UnknownModel(t *testing.T) {
	_, err := NewOpenAI(&config.EmbedderConfig{
		Model:   "unknown-model-xyz",
		BaseURL: "https://api.openai.com/v1",
	}, "fake-api-key")
	if err == nil {
		t.Error("expected error for unknown model")
	}
}

func TestOpenAI_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer server.Close()

	embedder, err := NewOpenAI(&config.EmbedderConfig{
		BaseURL: server.URL,
		Model:   "text-embedding-3-small",
	}, "fake-api-key")
	if err != nil {
		t.Fatalf("NewOpenAI() error = %v", err)
	}

	_, err = embedder.Embed(context.Background(), []string{"test"})
	if err == nil {
		t.Error("expected error from HTTP error response")
	}
	if err != nil && fmt.Sprintf("%v", err) == "" {
		t.Error("expected non-empty error message")
	}
}

// TestOpenAIBaseNormalization pins the endpoints down for every way an
// OpenAI-compatible gateway may be configured: with or without the "/v1"
// segment, with or without a trailing slash.
func TestOpenAIBaseNormalization(t *testing.T) {
	for _, suffix := range []string{"", "/", "/v1", "/v1/"} {
		t.Run("base"+suffix, func(t *testing.T) {
			var embedPath, chatPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/embeddings"):
					embedPath = r.URL.Path
					_ = json.NewEncoder(w).Encode(map[string]any{
						"data": []openaiItem{{Index: 0, Embedding: make([]float32, 1536)}},
					})
				default:
					chatPath = r.URL.Path
					_ = json.NewEncoder(w).Encode(map[string]any{
						"choices": []map[string]any{{"message": map[string]string{"content": "ok"}}},
					})
				}
			}))
			defer srv.Close()

			embedder, err := NewOpenAI(&config.EmbedderConfig{
				BaseURL: srv.URL + suffix,
				Model:   "text-embedding-3-small",
			}, "key")
			if err != nil {
				t.Fatalf("NewOpenAI() error = %v", err)
			}
			if _, err := embedder.Embed(context.Background(), []string{"x"}); err != nil {
				t.Fatalf("Embed() error = %v", err)
			}
			if embedPath != "/v1/embeddings" {
				t.Errorf("embeddings path = %q, want /v1/embeddings", embedPath)
			}

			gen, err := NewOpenAIGenerator(srv.URL+suffix, "key", "gpt-4o-mini")
			if err != nil {
				t.Fatalf("NewOpenAIGenerator() error = %v", err)
			}
			if _, err := gen.Generate(context.Background(), "hi"); err != nil {
				t.Fatalf("Generate() error = %v", err)
			}
			if chatPath != "/v1/chat/completions" {
				t.Errorf("chat path = %q, want /v1/chat/completions", chatPath)
			}
		})
	}
}

func TestNormalizeOpenAIBase_DefaultsToPublicEndpoint(t *testing.T) {
	if got := normalizeOpenAIBase(""); got != DefaultOpenAIBaseURL {
		t.Errorf("normalizeOpenAIBase(\"\") = %q, want %q", got, DefaultOpenAIBaseURL)
	}
	if got := normalizeOpenAIBase("https://gw.corp/openai/v1"); got != "https://gw.corp/openai" {
		t.Errorf("normalizeOpenAIBase() = %q, want https://gw.corp/openai", got)
	}
}
