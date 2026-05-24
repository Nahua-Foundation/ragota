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
//
// Реализация декомпозирована по доменам (все файлы — package rerank):
//
//   - rerank.go  — публичный API: типы Candidate/Scored/Options/Reranker,
//     ErrUnavailable, конструктор New, helper identity.
//   - noop.go    — noop-реализация (URL пуст или Required=false fallback).
//   - ollama.go  — ollamaReranker: HTTP-клиент, Available, Rerank, scoreOne
//     (вызов /api/generate), warnf.
//   - embed.go   — embedding-fallback для cross-encoder моделей:
//     scoreEmbed, embed (/api/embed), cosine.
//   - prompt.go  — построение prompt'а (buildPrompt), парсинг ответа
//     (parseScore) и эвристика isEmbeddingReranker.
package rerank

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"
)

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
	SetSemaphore(sem chan struct{})
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
