package embedder

// Файл содержит методы embedModern и embedLegacy.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

func (o *Ollama) embedModern(ctx context.Context, prompt string) ([]float32, error) {
	if err := o.acquire(ctx); err != nil {
		return nil, err
	}
	defer o.release()

	opts := map[string]interface{}{"num_ctx": 8192}
	body, err := json.Marshal(embedRequest{Model: o.model, Input: prompt, Options: opts})
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
	return o.applyDim(er.Embeddings[0]), nil
}

func (o *Ollama) embedLegacy(ctx context.Context, prompt string) ([]float32, error) {
	if err := o.acquire(ctx); err != nil {
		return nil, err
	}
	defer o.release()

	opts := map[string]interface{}{"num_ctx": 8192}
	body, err := json.Marshal(legacyEmbedRequest{Model: o.model, Prompt: prompt, Options: opts})
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
		return nil, fmt.Errorf("ollama legacy embed status %d: %s", resp.StatusCode, string(buf))
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
	return o.applyDim(er.Embedding), nil
}

func (o *Ollama) tryEmbedBatch(ctx context.Context, prompts []string) ([][]float32, error) {
	if err := o.acquire(ctx); err != nil {
		return nil, err
	}
	defer o.release()

	opts := map[string]interface{}{"num_ctx": 8192}
	body, err := json.Marshal(embedRequest{Model: o.model, Input: prompts, Options: opts})
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
			res[i] = o.applyDim(v)
		}
	}
	return res, nil
}

func (o *Ollama) recordLatency(err error, start time.Time) {
	if o.bus == nil {
		return
	}
	ms := float64(time.Since(start).Milliseconds())
	o.bus.SetOllamaLatency(o.model, ms, err != nil)
}

// applyDim обрезает или дополняет вектор до нужной размерности.
func (o *Ollama) applyDim(vec []float32) []float32 {
	if o.dim <= 0 {
		return vec
	}
	if len(vec) > o.dim {
		return vec[:o.dim]
	}
	if len(vec) < o.dim {
		newVec := make([]float32, o.dim)
		copy(newVec, vec)
		return newVec
	}
	return vec
}
