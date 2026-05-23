package rerank

// Файл содержит embedding-fallback для cross-encoder моделей реранкинга
// (bge-reranker-v2-m3 и подобных), у которых нет LM-head и /api/generate
// возвращает мусорный текст. Используется cosine(embed(query), embed(doc))
// через Ollama /api/embed.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
)

// scoreEmbed — fallback для cross-encoder моделей без LM-head (bge-reranker-v2-m3
// и подобные). Считает релевантность как cosine(embed(query), embed(document))
// через /api/embed Ollama. Это аппроксимация, а не настоящий cross-encoder
// score, но она монотонна по смысловой близости и решает проблему мусорного
// текстового вывода. Результат нормализуется из [-1,1] в [0,1].
func (r *ollamaReranker) scoreEmbed(ctx context.Context, query, content string) (float64, error) {
	qv, err := r.embed(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("embed(query): %w", err)
	}
	dv, err := r.embed(ctx, content)
	if err != nil {
		return 0, fmt.Errorf("embed(doc): %w", err)
	}
	cos := cosine(qv, dv)
	// Нормализуем в [0,1]: (cos+1)/2.
	score := (cos + 1) / 2
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	return score, nil
}

// embed — обращение к Ollama /api/embed для получения вектора одного входа.
func (r *ollamaReranker) embed(ctx context.Context, input string) ([]float64, error) {
	body, _ := json.Marshal(map[string]any{
		"model": r.opts.Model,
		"input": input,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(r.opts.URL, "/")+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama embed status %d: %s", resp.StatusCode, string(buf))
	}
	var res struct {
		Embeddings [][]float64 `json:"embeddings"`
		Embedding  []float64   `json:"embedding"`
		Error      string      `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	if res.Error != "" {
		return nil, fmt.Errorf("ollama: %s", res.Error)
	}
	if len(res.Embeddings) > 0 && len(res.Embeddings[0]) > 0 {
		return res.Embeddings[0], nil
	}
	if len(res.Embedding) > 0 {
		return res.Embedding, nil
	}
	return nil, fmt.Errorf("ollama embed: empty response")
}

// cosine — косинусное сходство двух векторов одинаковой длины.
func cosine(a, b []float64) float64 {
	n := len(a)
	if n == 0 || n != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
