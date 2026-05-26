package rerank

// Файл содержит реализацию реранкера через Ollama HTTP API:
//   - ollamaReranker (структура и helper warnf для логирования),
//   - Available — пинг /api/tags и проверка наличия модели,
//   - Rerank — основной цикл оценки кандидатов с graceful fallback,
//   - scoreOne — один вызов /api/generate с инструкцией вернуть число
//     (для cross-encoder моделей делегирует scoreEmbed из embed.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"ragota/internal/logger"
)

type ollamaReranker struct {
	opts Options
	http *http.Client
	sem  chan struct{}
}

func (r *ollamaReranker) warnf(format string, a ...any) {
	logger.Log().Warn().Msgf(format, a...)
}

// Available — пингует /api/tags и проверяет наличие модели в списке.
func (r *ollamaReranker) Available(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(r.opts.URL, "/")+"/api/tags", nil)
	if err != nil {
		return false
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false
	}
	for _, m := range body.Models {
		if m.Name == r.opts.Model || strings.HasPrefix(m.Name, r.opts.Model+":") || strings.HasPrefix(r.opts.Model, m.Name) {
			return true
		}
	}
	return false
}

// Rerank — последовательно (по одному запросу к Ollama на кандидата) — это
// устойчиво при низком RPS и при больших content'ах, а параллелизм
// добавим, если потребуется. Для каждого кандидата запрашиваем у LLM
// число от 0 до 1 (релевантность query документу), которое и становится
// RerankScore. При ошибке доступа — graceful fallback.
func (r *ollamaReranker) Rerank(ctx context.Context, query string, candidates []Candidate, topN int) ([]Scored, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	if !r.Available(ctx) {
		if r.opts.Required {
			return nil, fmt.Errorf("%w: %s @ %s", ErrUnavailable, r.opts.Model, r.opts.URL)
		}
		r.warnf("rerank: model %q is not available on %s — falling back to hybrid order", r.opts.Model, r.opts.URL)
		return identity(candidates, topN), nil
	}

	scored := make([]Scored, 0, len(candidates))
	for i, c := range candidates {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		s, err := r.scoreOne(ctx, query, c)
		if err != nil {
			if r.opts.Required {
				return nil, err
			}
			r.warnf("rerank: scoring candidate %d (%s) failed: %v — using hybrid score", i, c.ID, err)
			s = c.Score
		}
		scored = append(scored, Scored{Candidate: c, RerankScore: s})
	}

	// Стабильная сортировка по убыванию RerankScore (tie-break — исходный Score).
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].RerankScore == scored[j].RerankScore {
			return scored[i].Score > scored[j].Score
		}
		return scored[i].RerankScore > scored[j].RerankScore
	})
	if topN > 0 && len(scored) > topN {
		scored = scored[:topN]
	}
	return scored, nil
}

// scoreOne — один вызов Ollama /api/generate с инструкцией вернуть число.
//
// Использование /api/generate с параметром raw=true позволяет избежать влияния
// чат-шаблонов, что критично для специализированных моделей типа bge-reranker.
// Промпт включает инструкцию и контекст, завершаясь призывом к выводу оценки.
func (r *ollamaReranker) scoreOne(ctx context.Context, query string, c Candidate) (float64, error) {
	if r.sem != nil {
		select {
		case r.sem <- struct{}{}:
			defer func() { <-r.sem }()
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}

	content := c.Content
	if content == "" {
		return 0, nil
	}
	if r.opts.ContentMaxBytes > 0 && len(content) > r.opts.ContentMaxBytes {
		content = content[:r.opts.ContentMaxBytes] + "...[truncated]"
	}

	// Cross-encoder классификаторы (bge-reranker-v2-m3 и т.п.) не имеют LM-head,
	// поэтому /api/generate выдаёт мусор. Для них используем embedding-fallback.
	if isEmbeddingReranker(r.opts.Model) {
		return r.scoreEmbed(ctx, query, content)
	}

	prompt := buildPrompt(query, c.Symbol, c.Path, c.Language, content)

	body, _ := json.Marshal(map[string]any{
		"model":  r.opts.Model,
		"stream": false,
		"prompt": prompt,
		"options": map[string]any{
			"temperature": 0,
			"num_ctx":     4096,
			"num_predict": 20, // для числа достаточно, но даем запас на случай префиксов
			"stop":        []string{"\n", "Query:", "Document:"},
		},
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(r.opts.URL, "/")+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("ollama generate status %d: %s", resp.StatusCode, string(buf))
	}
	var res struct {
		Response string `json:"response"`
		Error    string `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return 0, err
	}
	if res.Error != "" {
		return 0, fmt.Errorf("ollama: %s", res.Error)
	}
	score := parseScore(res.Response)
	if score == 0 && res.Response != "" && res.Response != "0" && res.Response != "0.0" {
		// Ограничиваем длину вывода в логе
		displayRes := res.Response
		if len(displayRes) > 50 {
			displayRes = displayRes[:50] + "..."
		}
		r.warnf("rerank: model returned non-numeric content %q, using score 0", displayRes)
	}
	return score, nil
}

func (r *ollamaReranker) SetSemaphore(sem chan struct{}) {
	r.sem = sem
}
