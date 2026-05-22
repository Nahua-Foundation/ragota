// Package embedder реализует клиента ollama для генерации эмбеддингов.
package embedder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Ollama — клиент эмбеддинг-эндпоинта Ollama.
// Используется эндпоинт /api/embeddings (нативный, поддерживается всеми embed-моделями).
type Ollama struct {
	baseURL string
	model   string
	http    *http.Client
}

// New создаёт клиента. baseURL обычно "http://localhost:11434".
func New(baseURL, model string) *Ollama {
	return &Ollama{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		http: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

type embedRequest struct {
	Model   string                 `json:"model"`
	Input   interface{}            `json:"input"` // string or []string
	Options map[string]interface{} `json:"options,omitempty"`
}

type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
	Error      string      `json:"error,omitempty"`
}

// legacy structures for fallback
type legacyEmbedRequest struct {
	Model   string                 `json:"model"`
	Prompt  string                 `json:"prompt"`
	Options map[string]interface{} `json:"options,omitempty"`
}

type legacyEmbedResponse struct {
	Embedding []float32 `json:"embedding"`
	Error     string    `json:"error,omitempty"`
}

// Embed возвращает один вектор для prompt.
func (o *Ollama) Embed(ctx context.Context, prompt string) ([]float32, error) {
	// 1. Пробуем современный эндпоинт /api/embed
	v, err := o.embedModern(ctx, prompt)
	if err == nil {
		return v, nil
	}

	// 2. Если /api/embed не найден (404), пробуем legacy /api/embeddings.
	// Если же произошла другая ошибка (например, 500 Context Length),
	// то fallback на legacy не поможет, так как там лимиты те же или жестче.
	if strings.Contains(err.Error(), "not found") {
		return o.embedLegacy(ctx, prompt)
	}

	return nil, err
}

func (o *Ollama) embedModern(ctx context.Context, prompt string) ([]float32, error) {
	body, err := json.Marshal(embedRequest{
		Model: o.model,
		Input: prompt,
		Options: map[string]interface{}{
			"num_ctx": 8192,
		},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("not found")
	}

	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama embed status %d (input_len=%d): %s", resp.StatusCode, len(prompt), string(buf))
	}

	var er embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, err
	}
	if er.Error != "" {
		return nil, fmt.Errorf("ollama: %s", er.Error)
	}
	if len(er.Embeddings) == 0 {
		return nil, fmt.Errorf("ollama embed: empty embeddings")
	}
	return er.Embeddings[0], nil
}

func (o *Ollama) embedLegacy(ctx context.Context, prompt string) ([]float32, error) {
	body, err := json.Marshal(legacyEmbedRequest{
		Model:  o.model,
		Prompt: prompt,
		Options: map[string]interface{}{
			"num_ctx": 8192,
		},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama legacy embed status %d (input_len=%d): %s", resp.StatusCode, len(prompt), string(buf))
	}

	var er legacyEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, err
	}
	if er.Error != "" {
		return nil, fmt.Errorf("ollama legacy: %s", er.Error)
	}
	if len(er.Embedding) == 0 {
		return nil, fmt.Errorf("ollama legacy embed: empty embedding")
	}
	return er.Embedding, nil
}

// EmbedBatch генерирует эмбеддинги для нескольких текстов.
// Пробует отправить батч через /api/embed, если не выходит — по одному через Embed.
func (o *Ollama) EmbedBatch(ctx context.Context, prompts []string) ([][]float32, error) {
	if len(prompts) == 0 {
		return nil, nil
	}

	// Пробуем батчевый запрос через /api/embed
	body, err := json.Marshal(embedRequest{
		Model: o.model,
		Input: prompts,
		Options: map[string]interface{}{
			"num_ctx": 8192,
		},
	})
	if err == nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/embed", bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			resp, err := o.http.Do(req)
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					var er embedResponse
					if err := json.NewDecoder(resp.Body).Decode(&er); err == nil && len(er.Embeddings) == len(prompts) {
						return er.Embeddings, nil
					}
				}
			}
		}
	}

	// Fallback на последовательные вызовы, если батч не удался
	out := make([][]float32, len(prompts))
	for i, p := range prompts {
		v, err := o.Embed(ctx, p)
		if err != nil {
			return nil, fmt.Errorf("embed[%d]: %w", i, err)
		}
		out[i] = v
	}
	return out, nil
}

// Ping проверяет доступность ollama.
func (o *Ollama) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+"/api/tags", nil)
	if err != nil {
		return err
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ollama ping status %d: %s", resp.StatusCode, string(buf))
	}
	return nil
}
