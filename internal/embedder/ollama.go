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
	dim     int // желаемая размерность (0 - дефолт модели)
	http    *http.Client
	sem     chan struct{} // опциональный семафор для ограничения параллелизма
}

// New создаёт клиента. baseURL обычно "http://localhost:11434".
func New(baseURL, model string) *Ollama {
	return &Ollama{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		http: &http.Client{
			Timeout: 60 * time.Second, // уменьшено для более быстрого shutdown
		},
	}
}

// SetSemaphore устанавливает канал-семафор для ограничения количества параллельных запросов.
func (o *Ollama) SetSemaphore(sem chan struct{}) {
	o.sem = sem
}

// acquire/release для семафора
func (o *Ollama) acquire(ctx context.Context) error {
	if o.sem == nil {
		return nil
	}
	select {
	case o.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (o *Ollama) release() {
	if o.sem != nil {
		select {
		case <-o.sem:
		default:
		}
	}
}

// SetDim устанавливает желаемую размерность эмбеддингов.
func (o *Ollama) SetDim(dim int) {
	o.dim = dim
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
	if err := o.acquire(ctx); err != nil {
		return nil, err
	}
	defer o.release()

	// Не передаём dimensions — не все модели поддерживают эту опцию.
	// Обрезка вектора делается после получения ответа.
	opts := map[string]interface{}{
		"num_ctx": 8192,
	}
	body, err := json.Marshal(embedRequest{
		Model:   o.model,
		Input:   prompt,
		Options: opts,
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
	vec := er.Embeddings[0]
	if o.dim > 0 {
		if len(vec) > o.dim {
			vec = vec[:o.dim]
		} else if len(vec) < o.dim {
			// Если вектор короче, дополняем нулями (хотя это редкий кейс для Ollama)
			newVec := make([]float32, o.dim)
			copy(newVec, vec)
			vec = newVec
		}
	}
	return vec, nil
}

func (o *Ollama) embedLegacy(ctx context.Context, prompt string) ([]float32, error) {
	if err := o.acquire(ctx); err != nil {
		return nil, err
	}
	defer o.release()

	// Не передаём dimensions — не все модели поддерживают эту опцию.
	opts := map[string]interface{}{
		"num_ctx": 8192,
	}
	body, err := json.Marshal(legacyEmbedRequest{
		Model:   o.model,
		Prompt:  prompt,
		Options: opts,
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
	vec := er.Embedding
	if o.dim > 0 {
		if len(vec) > o.dim {
			vec = vec[:o.dim]
		} else if len(vec) < o.dim {
			newVec := make([]float32, o.dim)
			copy(newVec, vec)
			vec = newVec
		}
	}
	return vec, nil
}

// EmbedBatch генерирует эмбеддинги для нескольких текстов.
// Автоматически разбивает большой список на под-батчи для стабильности
// и использует fallback на последовательные вызовы при ошибках.
func (o *Ollama) EmbedBatch(ctx context.Context, prompts []string) ([][]float32, error) {
	if len(prompts) == 0 {
		return nil, nil
	}

	// Оптимальный размер батча для Ollama (обычно 32-64).
	// Мы используем 32 для большей надежности.
	const subBatchSize = 32
	out := make([][]float32, 0, len(prompts))

	for i := 0; i < len(prompts); i += subBatchSize {
		end := i + subBatchSize
		if end > len(prompts) {
			end = len(prompts)
		}
		sub := prompts[i:end]

		vecs, err := o.tryEmbedBatch(ctx, sub)
		if err != nil {
			// Если батч целиком не прошел (например, один из текстов слишком длинный),
			// обрабатываем этот кусок по одному.
			for _, p := range sub {
				v, err := o.Embed(ctx, p)
				if err != nil {
					return nil, fmt.Errorf("embed fallback failed: %w", err)
				}
				out = append(out, v)
			}
		} else {
			out = append(out, vecs...)
		}
	}

	return out, nil
}

func (o *Ollama) tryEmbedBatch(ctx context.Context, prompts []string) ([][]float32, error) {
	if err := o.acquire(ctx); err != nil {
		return nil, err
	}
	defer o.release()

	// Не передаём dimensions — не все модели поддерживают эту опцию.
	opts := map[string]interface{}{
		"num_ctx": 8192,
	}
	body, err := json.Marshal(embedRequest{
		Model:   o.model,
		Input:   prompts,
		Options: opts,
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

	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama batch status %d: %s", resp.StatusCode, string(buf))
	}

	var er embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, err
	}
	if len(er.Embeddings) != len(prompts) {
		return nil, fmt.Errorf("ollama batch size mismatch: got %d, want %d", len(er.Embeddings), len(prompts))
	}

	res := er.Embeddings
	if o.dim > 0 {
		for i, v := range res {
			if len(v) > o.dim {
				res[i] = v[:o.dim]
			} else if len(v) < o.dim {
				newVec := make([]float32, o.dim)
				copy(newVec, v)
				res[i] = newVec
			}
		}
	}
	return res, nil
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
