package rerank

// Тесты для чистых helper'ов пакета rerank:
//   - isEmbeddingReranker (эвристика по имени модели),
//   - buildPrompt (формирование few-shot prompt),
//   - cosine (косинусное сходство),
//   - identity (fallback в исходном порядке).
//
// HTTP-вызовы к Ollama (/api/generate, /api/embed, /api/tags) отдельным
// набором не покрываются — для них нужен моковый httptest.Server.

import (
	"math"
	"strings"
	"testing"
)

func TestIsEmbeddingReranker(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"bge-reranker-v2-m3", true},
		{"BGE-Reranker-Large", true},
		{"bge-m3", true},
		{"some-model-m3", true},
		{"some-model-m3:latest", true},
		{"e5-reranker-base", true},
		{"jina-reranker-v1", true},
		{"mxbai-rerank-large", true},
		{"qwen2:7b", false},
		{"llama3", false},
		{"", false},
		{"reranker-generic", false},
	}
	for _, tc := range cases {
		got := isEmbeddingReranker(tc.model)
		if got != tc.want {
			t.Errorf("isEmbeddingReranker(%q) = %v; want %v", tc.model, got, tc.want)
		}
	}
}

func TestBuildPrompt(t *testing.T) {
	p := buildPrompt("find handler", "ServeHTTP", "internal/api/handler.go", "go", "func (h *H) ServeHTTP() {}")
	// Проверяем ключевые маркеры — без жёсткого матча всего текста.
	mustContain := []string{
		"Instruction:",
		"Query: \"find handler\"",
		"symbol ServeHTTP",
		"file internal/api/handler.go",
		"lang go",
		"Document:",
		"func (h *H) ServeHTTP() {}",
		"Relevance Score:",
	}
	for _, m := range mustContain {
		if !strings.Contains(p, m) {
			t.Errorf("buildPrompt: missing %q in prompt:\n%s", m, p)
		}
	}

	// Без контекста (symbol/path/language пустые) — секции Context: быть не должно.
	p2 := buildPrompt("q", "", "", "", "doc")
	if strings.Contains(p2, "Context:") {
		t.Errorf("buildPrompt: unexpected Context: section when all hints empty:\n%s", p2)
	}
}

func TestCosine(t *testing.T) {
	cases := []struct {
		name string
		a, b []float64
		want float64
	}{
		{"identical", []float64{1, 0, 0}, []float64{1, 0, 0}, 1},
		{"orthogonal", []float64{1, 0}, []float64{0, 1}, 0},
		{"opposite", []float64{1, 0}, []float64{-1, 0}, -1},
		{"empty", nil, nil, 0},
		{"length-mismatch", []float64{1, 0}, []float64{1, 0, 0}, 0},
		{"zero-vector", []float64{0, 0}, []float64{1, 1}, 0},
	}
	for _, tc := range cases {
		got := cosine(tc.a, tc.b)
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("cosine(%s) = %v; want %v", tc.name, got, tc.want)
		}
	}
}

func TestIdentity(t *testing.T) {
	cs := []Candidate{
		{ID: "a", Score: 0.9},
		{ID: "b", Score: 0.5},
		{ID: "c", Score: 0.1},
	}
	// topN=0 → все.
	out := identity(cs, 0)
	if len(out) != 3 {
		t.Fatalf("identity topN=0: len=%d, want 3", len(out))
	}
	for i, s := range out {
		if s.ID != cs[i].ID || s.RerankScore != cs[i].Score {
			t.Errorf("identity[%d] = %+v; want id=%s score=%v", i, s, cs[i].ID, cs[i].Score)
		}
	}
	// topN=2 → срез.
	out2 := identity(cs, 2)
	if len(out2) != 2 || out2[0].ID != "a" || out2[1].ID != "b" {
		t.Errorf("identity topN=2: %+v", out2)
	}
	// topN больше длины → все.
	out3 := identity(cs, 10)
	if len(out3) != 3 {
		t.Errorf("identity topN=10: len=%d, want 3", len(out3))
	}
}
