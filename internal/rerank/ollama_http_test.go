package rerank

// HTTP-моки для ollamaReranker через httptest.Server. Покрывают:
//   - Available (/api/tags) — happy/missing/non-200/bad-json/network error;
//   - scoreOne (/api/generate) — числовой ответ, ошибка сервера, мусор;
//   - scoreEmbed/embed (/api/embed) — нормализация в [0,1] для cross-encoder моделей;
//   - Rerank end-to-end — сортировка + fallback при недоступности.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newOllamaForTest создаёт ollamaReranker с указанной моделью, подключённый
// к моковому серверу.
func newOllamaForTest(t *testing.T, url, model string) *ollamaReranker {
	t.Helper()
	return &ollamaReranker{
		opts: Options{URL: url, Model: model, ContentMaxBytes: 2000},
		http: &http.Client{Timeout: 5 * time.Second},
	}
}

// ---- Available ----

func TestAvailable_ModelPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]string{{"name": "qllama/bge-reranker-v2-m3:latest"}, {"name": "llama3"}},
		})
	}))
	defer srv.Close()
	r := newOllamaForTest(t, srv.URL, "qllama/bge-reranker-v2-m3")
	if !r.Available(context.Background()) {
		t.Errorf("Available: expected true (prefix match)")
	}
}

func TestAvailable_ModelMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]string{{"name": "llama3"}},
		})
	}))
	defer srv.Close()
	r := newOllamaForTest(t, srv.URL, "bge-reranker-v2-m3")
	if r.Available(context.Background()) {
		t.Errorf("Available: expected false")
	}
}

func TestAvailable_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	r := newOllamaForTest(t, srv.URL, "any")
	if r.Available(context.Background()) {
		t.Errorf("Available: expected false on 500")
	}
}

func TestAvailable_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "not json")
	}))
	defer srv.Close()
	r := newOllamaForTest(t, srv.URL, "any")
	if r.Available(context.Background()) {
		t.Errorf("Available: expected false on bad JSON")
	}
}

func TestAvailable_NetworkError(t *testing.T) {
	r := newOllamaForTest(t, "http://127.0.0.1:1", "any")
	if r.Available(context.Background()) {
		t.Errorf("Available: expected false on network error")
	}
}

// ---- scoreOne (/api/generate) ----

func TestScoreOne_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"response": "0.83"})
	}))
	defer srv.Close()
	r := newOllamaForTest(t, srv.URL, "llama3") // not embedding reranker
	score, err := r.scoreOne(context.Background(), "q", Candidate{Content: "doc"})
	if err != nil {
		t.Fatalf("scoreOne: %v", err)
	}
	if score <= 0.82 || score >= 0.84 {
		t.Errorf("score = %v; want ~0.83", score)
	}
}

func TestScoreOne_EmptyContent(t *testing.T) {
	// сервер не должен быть вызван — но на всякий случай поднимем.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("server must not be called for empty content")
	}))
	defer srv.Close()
	r := newOllamaForTest(t, srv.URL, "llama3")
	score, err := r.scoreOne(context.Background(), "q", Candidate{Content: ""})
	if err != nil || score != 0 {
		t.Errorf("scoreOne empty: score=%v err=%v", score, err)
	}
}

func TestScoreOne_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	r := newOllamaForTest(t, srv.URL, "llama3")
	_, err := r.scoreOne(context.Background(), "q", Candidate{Content: "doc"})
	if err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Errorf("scoreOne: expected status 500 error, got %v", err)
	}
}

func TestScoreOne_NonNumericResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"response": "I don't know"})
	}))
	defer srv.Close()
	r := newOllamaForTest(t, srv.URL, "llama3")
	score, err := r.scoreOne(context.Background(), "q", Candidate{Content: "doc"})
	if err != nil {
		t.Fatalf("scoreOne: %v", err)
	}
	if score != 0 {
		t.Errorf("non-numeric: score=%v; want 0", score)
	}
}

// ---- scoreEmbed + embed (/api/embed) ----

func TestScoreEmbed_NormalizedToUnitInterval(t *testing.T) {
	// Возвращаем разные вектора для query и content, ровно ортогональные → score=0.5.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Input string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var emb []float64
		if req.Input == "q" {
			emb = []float64{1, 0}
		} else {
			emb = []float64{0, 1}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{emb}})
	}))
	defer srv.Close()
	r := newOllamaForTest(t, srv.URL, "bge-reranker-v2-m3") // triggers embedding fallback
	score, err := r.scoreOne(context.Background(), "q", Candidate{Content: "doc"})
	if err != nil {
		t.Fatalf("scoreOne (embed): %v", err)
	}
	if score < 0.49 || score > 0.51 {
		t.Errorf("orthogonal vectors: score=%v; want ~0.5", score)
	}
}

func TestEmbed_FallbackToEmbeddingField(t *testing.T) {
	// Сервер возвращает старый формат { "embedding": [...] }.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": []float64{1, 2, 3}})
	}))
	defer srv.Close()
	r := newOllamaForTest(t, srv.URL, "bge-m3")
	v, err := r.embed(context.Background(), "x")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(v) != 3 || v[0] != 1 {
		t.Errorf("embed = %v; want [1 2 3]", v)
	}
}

func TestEmbed_EmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer srv.Close()
	r := newOllamaForTest(t, srv.URL, "bge-m3")
	_, err := r.embed(context.Background(), "x")
	if err == nil {
		t.Fatal("embed: expected error on empty response")
	}
}

// ---- Rerank end-to-end ----

func TestRerank_SortsByScore(t *testing.T) {
	// Возвращаем разные числа в зависимости от content в prompt'е.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]string{{"name": "llama3"}},
			})
		case "/api/generate":
			body, _ := io.ReadAll(r.Body)
			s := string(body)
			score := "0.1"
			switch {
			case strings.Contains(s, "ALPHA"):
				score = "0.9"
			case strings.Contains(s, "BETA"):
				score = "0.5"
			}
			_, _ = io.WriteString(w, `{"response":"`+score+`"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	r := newOllamaForTest(t, srv.URL, "llama3")
	cs := []Candidate{
		{ID: "g", Content: "GAMMA doc", Score: 0.4},
		{ID: "a", Content: "ALPHA doc", Score: 0.3},
		{ID: "b", Content: "BETA doc", Score: 0.2},
	}
	out, err := r.Rerank(context.Background(), "q", cs, 0)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(out) != 3 || out[0].ID != "a" || out[1].ID != "b" || out[2].ID != "g" {
		t.Fatalf("Rerank order: %+v", out)
	}

	// TopN срезает.
	out2, _ := r.Rerank(context.Background(), "q", cs, 2)
	if len(out2) != 2 || out2[0].ID != "a" || out2[1].ID != "b" {
		t.Errorf("Rerank topN=2: %+v", out2)
	}
}

func TestRerank_FallbackWhenUnavailable(t *testing.T) {
	// /api/tags возвращает пустой список → модель не доступна → fallback на identity.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{}})
	}))
	defer srv.Close()

	r := newOllamaForTest(t, srv.URL, "llama3")
	cs := []Candidate{{ID: "x", Score: 0.7}, {ID: "y", Score: 0.3}}
	out, err := r.Rerank(context.Background(), "q", cs, 0)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(out) != 2 || out[0].ID != "x" || out[1].ID != "y" {
		t.Errorf("identity order broken: %+v", out)
	}
}

func TestRerank_RequiredReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{}})
	}))
	defer srv.Close()

	r := newOllamaForTest(t, srv.URL, "llama3")
	r.opts.Required = true
	_, err := r.Rerank(context.Background(), "q", []Candidate{{ID: "x"}}, 0)
	if err == nil {
		t.Fatal("Rerank: expected error when Required=true and model unavailable")
	}
}
