// Package rerank — реранкинг кандидатов через Ollama (BGE Reranker).
//
// Модель по умолчанию: qllama/bge-reranker-v2-m3
//
// Контракт: принимает query и список кандидатов (с content), возвращает их
// в перестроенном порядке вместе с relevance score.
//
// Graceful fallback: если Ollama недоступна или модель не подгружена, и
// при этом cfg.Rerank.Required = false — возвращается исходный порядок
// без ошибки (вызывающий должен залогировать warning).
package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// isEmbeddingReranker — эвристика: модели семейства bge-reranker / *-m3 /
// e5-reranker и подобные — это cross-encoder классификаторы без LM-head.
// Через /api/generate они физически не умеют выдать осмысленный текст
// (Ollama не экспонирует sequence-classification logits). Для них
// используем embedding-fallback: cosine(query, document) через /api/embed.
func isEmbeddingReranker(model string) bool {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "bge-reranker"),
		strings.Contains(m, "bge-m3"),
		strings.HasSuffix(m, "-m3"),
		strings.Contains(m, "-m3:"),
		strings.Contains(m, "e5-reranker"),
		strings.Contains(m, "jina-reranker"),
		strings.Contains(m, "mxbai-rerank"):
		return true
	}
	return false
}

// ErrUnavailable — модель/сервер реранкера недоступны.
var ErrUnavailable = errors.New("rerank: model unavailable")

// Candidate — кандидат для реранкинга.
type Candidate struct {
	ID       string  `json:"id"`
	Path     string  `json:"path"`
	Language string  `json:"language"`
	Kind     string  `json:"kind"`
	Symbol   string  `json:"symbol"`
	Content  string  `json:"content"` // фрагмент кода/текста, который оценивается
	Score    float64 `json:"score"`   // исходный (hybrid/vector/bm25) score, для tie-break
}

// Scored — кандидат с новым relevance score после реранкинга.
type Scored struct {
	Candidate
	RerankScore float64 `json:"rerank_score"`
}

// Reranker — интерфейс реранкера.
type Reranker interface {
	Rerank(ctx context.Context, query string, candidates []Candidate, topN int) ([]Scored, error)
	Available(ctx context.Context) bool
}

// Options — настройки конкретного экземпляра реранкера.
type Options struct {
	URL      string // Ollama base URL
	Model    string // имя модели реранкера в Ollama
	Required bool   // если true — отсутствие модели приводит к ошибке, иначе fallback
	TopN     int
	// ContentMaxBytes — максимальная длина content в одном prompt'е (truncate).
	ContentMaxBytes int
	// Logger опционально подменяет стандартный log.* для предупреждений.
	Logger *log.Logger
}

// New создаёт реранкер. Если URL пустой — возвращается noop.
func New(opts Options) Reranker {
	if opts.URL == "" || opts.Model == "" {
		return &noop{opts: opts}
	}
	if opts.ContentMaxBytes <= 0 {
		opts.ContentMaxBytes = 2000
	}
	return &ollamaReranker{
		opts: opts,
		http: &http.Client{Timeout: 120 * time.Second},
		log:  opts.Logger,
	}
}

// ---------------- noop (graceful fallback / unconfigured) ----------------

type noop struct{ opts Options }

func (n *noop) Available(ctx context.Context) bool { return false }

func (n *noop) Rerank(ctx context.Context, query string, candidates []Candidate, topN int) ([]Scored, error) {
	if n.opts.Required {
		return nil, ErrUnavailable
	}
	return identity(candidates, topN), nil
}

// identity возвращает кандидатов в исходном порядке с RerankScore=Score.
func identity(candidates []Candidate, topN int) []Scored {
	out := make([]Scored, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, Scored{Candidate: c, RerankScore: c.Score})
	}
	if topN > 0 && len(out) > topN {
		out = out[:topN]
	}
	return out
}

// ---------------- ollamaReranker (HTTP к Ollama) ----------------

type ollamaReranker struct {
	opts Options
	http *http.Client
	log  *log.Logger
}

func (r *ollamaReranker) warnf(format string, a ...any) {
	if r.log != nil {
		r.log.Printf(format, a...)
		return
	}
	log.Printf(format, a...)
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

func buildPrompt(query, symbol, path, language, content string) string {
	var b strings.Builder
	b.WriteString("Instruction: Evaluate the relevance of the Document to the Query. Output only a single number between 0.0 and 1.0.\n\n")

	// Few-shot examples
	b.WriteString("Query: \"how to install golang\"\n")
	b.WriteString("Document: \"To install Go, download the installer from golang.org...\"\n")
	b.WriteString("Relevance Score: 1.0\n\n")

	b.WriteString("Query: \"weather forecast\"\n")
	b.WriteString("Document: \"package main; func main() {}\"\n")
	b.WriteString("Relevance Score: 0.0\n\n")

	b.WriteString("Query: \"")
	b.WriteString(query)
	b.WriteString("\"\n")
	if symbol != "" || path != "" || language != "" {
		b.WriteString("Context: ")
		if symbol != "" {
			b.WriteString("symbol " + symbol + "; ")
		}
		if path != "" {
			b.WriteString("file " + path + "; ")
		}
		if language != "" {
			b.WriteString("lang " + language + "; ")
		}
		b.WriteString("\n")
	}
	b.WriteString("Document:\n")
	b.WriteString(content)
	b.WriteString("\nRelevance Score: ")
	return b.String()
}

var numRe = regexp.MustCompile(`-?\d+([.,]\d+)?`)

// parseScore — извлекает первое float-число из LLM-ответа и нормализует
// его в [0,1]. Если ничего не найдено — возвращает 0.
func parseScore(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	m := numRe.FindString(s)
	if m == "" {
		// Эвристика: yes/no.
		lower := strings.ToLower(s)
		if strings.HasPrefix(lower, "yes") || strings.HasPrefix(lower, "relevant") {
			return 1
		}
		return 0
	}
	m = strings.Replace(m, ",", ".", 1)
	v, err := strconv.ParseFloat(m, 64)
	if err != nil {
		return 0
	}
	// Некоторые модели возвращают score в [0,100] или logit'ы — клампим.
	if v > 1 && v <= 100 {
		v = v / 100.0
	}
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return v
}
