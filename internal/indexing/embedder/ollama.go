// Package embedder реализует клиента ollama для генерации эмбеддингов.
package embedder

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Ollama — клиент эмбеддинг-эндпоинта Ollama.
type Ollama struct {
	baseURL string
	model   string
	dim     int
	http    *http.Client
	sem     chan struct{}
	bus     metricsSink
}

// metricsSink — интерфейс для записи метрик.
type metricsSink interface {
	SetOllamaLatency(model string, latencyMs float64, isError bool)
}

func (o *Ollama) SetBus(bus metricsSink) { o.bus = bus }

// New создаёт клиента.
func New(baseURL, model string) *Ollama {
	return &Ollama{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (o *Ollama) SetSemaphore(sem chan struct{}) { o.sem = sem }
func (o *Ollama) SetDim(dim int)                 { o.dim = dim }

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

type embedRequest struct {
	Model   string                 `json:"model"`
	Input   interface{}            `json:"input"`
	Options map[string]interface{} `json:"options,omitempty"`
}

type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
	Error      string      `json:"error,omitempty"`
}

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
	start := time.Now()
	v, err := o.embedModern(ctx, prompt)
	if err == nil {
		o.recordLatency(err, start)
		return v, nil
	}
	if strings.Contains(err.Error(), "not found") {
		v, err = o.embedLegacy(ctx, prompt)
		o.recordLatency(err, start)
		return v, err
	}
	o.recordLatency(err, start)
	return nil, err
}

// EmbedBatch генерирует эмбеддинги для нескольких текстов.
func (o *Ollama) EmbedBatch(ctx context.Context, prompts []string) ([][]float32, error) {
	if len(prompts) == 0 {
		return nil, nil
	}
	start := time.Now()
	hadFallback := false
	defer func() {
		if o.bus != nil && !hadFallback {
			avgMs := float64(time.Since(start).Milliseconds()) / float64(len(prompts))
			o.bus.SetOllamaLatency(o.model, avgMs, false)
		}
	}()

	const subBatchSize = 64
	out := make([][]float32, 0, len(prompts))

	for i := 0; i < len(prompts); i += subBatchSize {
		end := i + subBatchSize
		if end > len(prompts) {
			end = len(prompts)
		}
		sub := prompts[i:end]
		vecs, err := o.tryEmbedBatch(ctx, sub)
		if err != nil {
			hadFallback = true
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
