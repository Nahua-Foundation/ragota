package embedder

// HTTP-моки для Ollama embedder через httptest.Server. Покрывают:
//   - Embed: модерный /api/embed (happy path), fallback на legacy /api/embeddings при 404,
//     ошибка при non-404 non-200 (без fallback), пустой ответ;
//   - dim-обрезка и нулевой паддинг;
//   - EmbedBatch: batch happy path, fallback на one-by-one при ошибке батча, пустой prompts;
//   - Ping: happy/non-200/network error.
// Внешних зависимостей нет.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// readReq декодирует JSON-тело запроса в map для проверок.
func readReq(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	body, _ := io.ReadAll(r.Body)
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("bad request json: %v body=%s", err, string(body))
	}
	return m
}

func TestEmbed_Modern_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			http.NotFound(w, r)
			return
		}
		body := readReq(t, r)
		if body["model"] != "m" || body["input"] != "hello" {
			t.Errorf("unexpected body: %+v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": [][]float32{{0.1, 0.2, 0.3, 0.4}},
		})
	}))
	defer srv.Close()
	o := New(srv.URL, "m")
	v, err := o.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(v) != 4 || v[0] != 0.1 {
		t.Errorf("got %v", v)
	}
}

func TestEmbed_DimTruncateAndPad(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": [][]float32{{1, 2, 3, 4, 5}},
		})
	}))
	defer srv.Close()

	// truncate
	o := New(srv.URL, "m")
	o.SetDim(3)
	v, err := o.Embed(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 3 || v[2] != 3 {
		t.Errorf("truncate: %v", v)
	}

	// pad with zeros
	o.SetDim(8)
	v, err = o.Embed(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 8 || v[5] != 0 || v[4] != 5 {
		t.Errorf("pad: %v", v)
	}
}

func TestEmbed_FallbackToLegacyOn404(t *testing.T) {
	var hitLegacy bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/embed":
			http.NotFound(w, r)
		case "/api/embeddings":
			hitLegacy = true
			_ = json.NewEncoder(w).Encode(map[string]any{
				"embedding": []float32{9, 9},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	o := New(srv.URL, "m")
	v, err := o.Embed(context.Background(), "x")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if !hitLegacy {
		t.Error("legacy endpoint not called")
	}
	if len(v) != 2 || v[0] != 9 {
		t.Errorf("legacy vec: %v", v)
	}
}

func TestEmbed_Non404ErrorDoesNotFallback(t *testing.T) {
	var legacyHit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/embeddings" {
			legacyHit = true
		}
		if r.URL.Path == "/api/embed" {
			http.Error(w, "ctx overflow", http.StatusInternalServerError)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	o := New(srv.URL, "m")
	_, err := o.Embed(context.Background(), "x")
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if legacyHit {
		t.Error("legacy must NOT be called on non-404 errors")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err missing status: %v", err)
	}
}

func TestEmbed_EmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{}})
	}))
	defer srv.Close()
	o := New(srv.URL, "m")
	if _, err := o.Embed(context.Background(), "x"); err == nil {
		t.Fatal("expected error on empty embeddings")
	}
}

func TestEmbedBatch_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readReq(t, r)
		input, _ := body["input"].([]any)
		out := make([][]float32, len(input))
		for i := range input {
			out[i] = []float32{float32(i), float32(i) + 0.5}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": out})
	}))
	defer srv.Close()
	o := New(srv.URL, "m")
	vecs, err := o.EmbedBatch(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if len(vecs) != 3 || vecs[2][0] != 2 {
		t.Errorf("got %v", vecs)
	}
}

func TestEmbedBatch_FallbackOneByOne(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readReq(t, r)
		calls++
		switch input := body["input"].(type) {
		case []any:
			// batch — отдаём ошибку, чтобы спровоцировать fallback
			http.Error(w, "batch fail", http.StatusInternalServerError)
			_ = input
		case string:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"embeddings": [][]float32{{1}},
			})
		}
	}))
	defer srv.Close()
	o := New(srv.URL, "m")
	vecs, err := o.EmbedBatch(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("EmbedBatch fallback: %v", err)
	}
	if len(vecs) != 2 {
		t.Errorf("got %d vecs", len(vecs))
	}
	if calls < 3 {
		t.Errorf("expected >=3 calls (1 batch fail + 2 single), got %d", calls)
	}
}

func TestEmbedBatch_EmptyPrompts(t *testing.T) {
	o := New("http://nowhere", "m")
	vecs, err := o.EmbedBatch(context.Background(), nil)
	if err != nil || vecs != nil {
		t.Errorf("expected (nil, nil); got (%v, %v)", vecs, err)
	}
}

func TestPing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	o := New(srv.URL, "m")
	if err := o.Ping(context.Background()); err != nil {
		t.Errorf("Ping happy: %v", err)
	}

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv2.Close()
	o2 := New(srv2.URL, "m")
	if err := o2.Ping(context.Background()); err == nil {
		t.Error("Ping must fail on 503")
	}

	// Network error: закрытый сервер.
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	addr := srv3.URL
	srv3.Close()
	o3 := New(addr, "m")
	if err := o3.Ping(context.Background()); err == nil {
		t.Error("Ping must fail on closed server")
	}
}
