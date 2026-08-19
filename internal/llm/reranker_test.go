package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/config"
)

// testReranker builds an HTTPReranker from cfg with fast retries so error
// cases don't slow the suite down.
func testReranker(t *testing.T, cfg *config.RerankConfig) *HTTPReranker {
	t.Helper()
	r, err := NewHTTPReranker(cfg)
	if err != nil {
		t.Fatalf("NewHTTPReranker() error = %v", err)
	}
	r.client.Retries = 1
	r.client.Backoff = time.Millisecond
	return r
}

func TestHTTPReranker_TEIResponse(t *testing.T) {
	var gotReq rerankReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rerank" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		// TEI style: bare array, out of input order.
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"index": 1, "score": 0.9},
			{"index": 0, "score": 0.1},
		})
	}))
	defer srv.Close()

	r := testReranker(t, &config.RerankConfig{BaseURL: srv.URL, Model: "test-model"})
	scores, err := r.Rerank(context.Background(), "the query", []string{"doc a", "doc b"})
	if err != nil {
		t.Fatalf("Rerank() error = %v", err)
	}
	if len(scores) != 2 || scores[0] != 0.1 || scores[1] != 0.9 {
		t.Errorf("scores = %v, want [0.1 0.9]", scores)
	}
	if gotReq.Query != "the query" {
		t.Errorf("request query = %q, want %q", gotReq.Query, "the query")
	}
	if len(gotReq.Documents) != 2 || gotReq.Documents[0] != "doc a" {
		t.Errorf("request documents = %v", gotReq.Documents)
	}
	if gotReq.Model != "test-model" {
		t.Errorf("request model = %q, want test-model", gotReq.Model)
	}
}

func TestHTTPReranker_CohereResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"index": 0, "relevance_score": 0.7},
				{"index": 2, "relevance_score": 0.3},
				{"index": 1, "relevance_score": 0.5},
			},
		})
	}))
	defer srv.Close()

	r := testReranker(t, &config.RerankConfig{BaseURL: srv.URL})
	scores, err := r.Rerank(context.Background(), "q", []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Rerank() error = %v", err)
	}
	want := []float64{0.7, 0.5, 0.3}
	for i := range want {
		if scores[i] != want[i] {
			t.Errorf("scores = %v, want %v", scores, want)
			break
		}
	}
}

func TestHTTPReranker_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := testReranker(t, &config.RerankConfig{BaseURL: srv.URL})
	if _, err := r.Rerank(context.Background(), "q", []string{"a", "b"}); err == nil {
		t.Fatalf("Rerank() expected error on upstream 500, got nil")
	}
}

func TestHTTPReranker_BadIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"index": 5, "score": 0.9}})
	}))
	defer srv.Close()

	r := testReranker(t, &config.RerankConfig{BaseURL: srv.URL})
	if _, err := r.Rerank(context.Background(), "q", []string{"a"}); err == nil {
		t.Fatalf("Rerank() expected error on out-of-range index, got nil")
	}
}

// TestHTTPReranker_MissingScoreErrors covers a service that answers for only
// some documents. Zero-filling the gaps is not safe: logit-based rerankers
// return negative scores, so an unscored document would outrank every real
// answer. The caller must be able to fall back to fusion order instead.
func TestHTTPReranker_MissingScoreErrors(t *testing.T) {
	tests := []struct {
		name string
		body []map[string]any
	}{
		{
			name: "document omitted entirely",
			body: []map[string]any{{"index": 0, "score": -0.33}, {"index": 2, "score": -11.0}},
		},
		{
			name: "entry without any score field",
			body: []map[string]any{{"index": 0, "score": -0.33}, {"index": 1}, {"index": 2, "score": -11.0}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(tt.body)
			}))
			defer srv.Close()

			r := testReranker(t, &config.RerankConfig{BaseURL: srv.URL})
			scores, err := r.Rerank(context.Background(), "q", []string{"a", "b", "c"})
			if err == nil {
				t.Fatalf("Rerank() = %v, want an error for the unscored document", scores)
			}
		})
	}
}

func TestHTTPReranker_AllScoresPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"index": 2, "score": -11.0},
			{"index": 0, "score": -0.33},
			{"index": 1, "score": -4.0},
		})
	}))
	defer srv.Close()

	r := testReranker(t, &config.RerankConfig{BaseURL: srv.URL})
	scores, err := r.Rerank(context.Background(), "q", []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Rerank() error = %v", err)
	}
	want := []float64{-0.33, -4.0, -11.0}
	for i := range want {
		if scores[i] != want[i] {
			t.Fatalf("scores = %v, want %v", scores, want)
		}
	}
}

// TestHTTPReranker_TimeoutBoundsWholeStage pins the fix for a dead reranker
// costing ~90s per search: timeout_seconds bounds the whole stage, retries
// included, not one attempt.
func TestHTTPReranker_TimeoutBoundsWholeStage(t *testing.T) {
	release := make(chan struct{})
	var attempts atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	// Release the handler before Close, which waits for outstanding requests.
	defer srv.Close()
	defer close(release)

	r, err := NewHTTPReranker(&config.RerankConfig{BaseURL: srv.URL, TimeoutSeconds: 1})
	if err != nil {
		t.Fatalf("NewHTTPReranker() error = %v", err)
	}

	start := time.Now()
	if _, err := r.Rerank(context.Background(), "q", []string{"a"}); err == nil {
		t.Fatal("Rerank() expected an error from the hanging service")
	}
	elapsed := time.Since(start)

	// One second of timeout plus at most one retry, with slack for CI.
	if elapsed > 3*time.Second {
		t.Errorf("Rerank() took %s; the stage must be bounded by timeout_seconds", elapsed)
	}
	if n := attempts.Load(); n > 2 {
		t.Errorf("service saw %d attempts, want at most 2 (one retry)", n)
	}
}

func TestNewHTTPReranker_RequiresBaseURL(t *testing.T) {
	if _, err := NewHTTPReranker(nil); err == nil {
		t.Fatalf("NewHTTPReranker(nil) expected error, got nil")
	}
	if _, err := NewHTTPReranker(&config.RerankConfig{Model: "m"}); err == nil {
		t.Fatalf("NewHTTPReranker() without base url expected error, got nil")
	}
	if r, err := NewHTTPReranker(&config.RerankConfig{BaseURL: "http://localhost:9999"}); err != nil || r == nil {
		t.Fatalf("NewHTTPReranker() error = %v", err)
	}
}

// TestHTTPReranker_VLLMEndpoint covers a vLLM-served reranker: the rerank API
// lives at /v1/rerank behind a bearer token, answers in the Cohere shape and,
// for Qwen3-Reranker, takes its instruction inside the query text.
func TestHTTPReranker_VLLMEndpoint(t *testing.T) {
	var gotAuth, gotPath string
	var gotReq rerankReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{"index": 0, "relevance_score": 0.42}},
		})
	}))
	defer srv.Close()

	r := testReranker(t, &config.RerankConfig{
		BaseURL:          srv.URL + "/v1",
		APIKey:           "secret",
		Model:            "Qwen/Qwen3-Reranker-4B",
		Instruction:      "Find the code",
		DocumentTemplate: "<Document>: {doc}",
	})
	scores, err := r.Rerank(context.Background(), "where is auth", []string{"func Auth() {}"})
	if err != nil {
		t.Fatalf("Rerank() error = %v", err)
	}
	if len(scores) != 1 || scores[0] != 0.42 {
		t.Errorf("scores = %v, want [0.42]", scores)
	}
	if gotPath != "/v1/rerank" {
		t.Errorf("path = %q, want /v1/rerank", gotPath)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("authorization = %q, want %q", gotAuth, "Bearer secret")
	}
	if want := "<Instruct>: Find the code\n<Query>: where is auth"; gotReq.Query != want {
		t.Errorf("query = %q, want %q", gotReq.Query, want)
	}
	if len(gotReq.Documents) != 1 || gotReq.Documents[0] != "<Document>: func Auth() {}" {
		t.Errorf("documents = %q", gotReq.Documents)
	}
}

// TestHTTPReranker_Templates checks that templates stay off by default and
// that an explicit query template overrides the built-in instruction format.
func TestHTTPReranker_Templates(t *testing.T) {
	tests := []struct {
		name     string
		cfg      config.RerankConfig
		wantQ    string
		wantDoc  string
		wantPath string
	}{
		{
			name:     "no instruction leaves query untouched",
			cfg:      config.RerankConfig{},
			wantQ:    "q",
			wantDoc:  "d",
			wantPath: "/rerank",
		},
		{
			name:     "explicit template wins over the default format",
			cfg:      config.RerankConfig{Instruction: "task", QueryTemplate: "{instruction}||{query}", Path: "score"},
			wantQ:    "task||q",
			wantDoc:  "d",
			wantPath: "/score",
		},
		{
			name:     "template without an instruction still applies",
			cfg:      config.RerankConfig{QueryTemplate: "<Query>: {query}"},
			wantQ:    "<Query>: q",
			wantDoc:  "d",
			wantPath: "/rerank",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotReq rerankReq
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				_ = json.NewDecoder(r.Body).Decode(&gotReq)
				_ = json.NewEncoder(w).Encode([]map[string]any{{"index": 0, "score": 1.0}})
			}))
			defer srv.Close()

			cfg := tt.cfg
			cfg.BaseURL = srv.URL
			if _, err := testReranker(t, &cfg).Rerank(context.Background(), "q", []string{"d"}); err != nil {
				t.Fatalf("Rerank() error = %v", err)
			}
			if gotReq.Query != tt.wantQ {
				t.Errorf("query = %q, want %q", gotReq.Query, tt.wantQ)
			}
			if len(gotReq.Documents) != 1 || gotReq.Documents[0] != tt.wantDoc {
				t.Errorf("documents = %q, want [%q]", gotReq.Documents, tt.wantDoc)
			}
			if gotPath != tt.wantPath {
				t.Errorf("path = %q, want %q", gotPath, tt.wantPath)
			}
		})
	}
}
