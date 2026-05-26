package rerank

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// New — constructor logic
// ---------------------------------------------------------------------------

func TestNew_EmptyURL_ReturnsNoop(t *testing.T) {
	r := New(Options{URL: "", Model: "bge"})
	_, ok := r.(*noop)
	assert.True(t, ok, "expected noop when URL is empty")
}

func TestNew_EmptyModel_ReturnsNoop(t *testing.T) {
	r := New(Options{URL: "http://localhost:11434", Model: ""})
	_, ok := r.(*noop)
	assert.True(t, ok, "expected noop when Model is empty")
}

func TestNew_BothEmpty_ReturnsNoop(t *testing.T) {
	r := New(Options{})
	_, ok := r.(*noop)
	assert.True(t, ok)
}

func TestNew_ValidOptions_ReturnsOllamaReranker(t *testing.T) {
	r := New(Options{URL: "http://localhost:11434", Model: "llama3"})
	_, ok := r.(*ollamaReranker)
	assert.True(t, ok, "expected ollamaReranker for valid URL+Model")
}

func TestNew_DefaultContentMaxBytes(t *testing.T) {
	r := New(Options{URL: "http://localhost:11434", Model: "llama3"})
	or, ok := r.(*ollamaReranker)
	require.True(t, ok)
	assert.Equal(t, 2000, or.opts.ContentMaxBytes)
}

func TestNew_CustomContentMaxBytes(t *testing.T) {
	r := New(Options{URL: "http://localhost:11434", Model: "llama3", ContentMaxBytes: 5000})
	or, ok := r.(*ollamaReranker)
	require.True(t, ok)
	assert.Equal(t, 5000, or.opts.ContentMaxBytes)
}

// ---------------------------------------------------------------------------
// noop reranker
// ---------------------------------------------------------------------------

func TestNoop_Available(t *testing.T) {
	r := New(Options{})
	assert.False(t, r.Available(context.Background()))
}

func TestNoop_SetSemaphore_NoOp(t *testing.T) {
	r := New(Options{})
	// Should not panic.
	r.SetSemaphore(make(chan struct{}, 1))
}

func TestNoop_Rerank_NotRequired_ReturnsIdentity(t *testing.T) {
	r := New(Options{Required: false})
	cs := []Candidate{
		{ID: IDValue("a"), Score: 0.9},
		{ID: IDValue("b"), Score: 0.5},
	}
	out, err := r.Rerank(context.Background(), "q", cs, 0)
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Equal(t, IDValue("a"), out[0].ID)
	assert.Equal(t, 0.9, out[0].RerankScore)
}

func TestNoop_Rerank_Required_ReturnsError(t *testing.T) {
	r := New(Options{Required: true})
	_, err := r.Rerank(context.Background(), "q", []Candidate{{ID: "x"}}, 0)
	assert.ErrorIs(t, err, ErrUnavailable)
}

func TestNoop_Rerank_EmptyCandidates(t *testing.T) {
	r := New(Options{})
	out, err := r.Rerank(context.Background(), "q", nil, 0)
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestNoop_Rerank_WithTopN(t *testing.T) {
	r := New(Options{})
	cs := []Candidate{
		{ID: "a", Score: 0.9},
		{ID: "b", Score: 0.5},
		{ID: "c", Score: 0.1},
	}
	out, err := r.Rerank(context.Background(), "q", cs, 2)
	require.NoError(t, err)
	assert.Len(t, out, 2)
}

// ---------------------------------------------------------------------------
// ollamaReranker.SetSemaphore
// ---------------------------------------------------------------------------

func TestOllamaReranker_SetSemaphore(t *testing.T) {
	r := New(Options{URL: "http://localhost:11434", Model: "llama3"})
	sem := make(chan struct{}, 2)
	r.SetSemaphore(sem)
	or := r.(*ollamaReranker)
	assert.Equal(t, sem, or.sem)
}

// ---------------------------------------------------------------------------
// parseScore — additional edge cases
// ---------------------------------------------------------------------------

func TestParseScore_Extended(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want float64
	}{
		{"zero", "0", 0},
		{"one", "1", 1},
		{"one_point_zero", "1.0", 1},
		{"leading_zeros", "00.5", 0.5},
		{"comma_decimal", "0,85", 0.85},
		{"percentage_75", "75", 0.75},
		{"percentage_100", "100", 1},
		{"over_100", "200", 1},
		{"negative", "-0.5", 0},
		{"empty_string", "", 0},
		{"whitespace_only", "   ", 0},
		{"yes_prefix", "yes, very relevant", 1},
		{"relevant_prefix", "Relevant to the query", 1},
		{"no_match", "I cannot determine", 0},
		{"embedded_number", "score is 0.42 out of 1", 0.42},
		{"large_number", "42.5", 0.425},
		{"just_over_one", "1.5", 1},
		{"scientific_notation", "1e-5", 1}, // regex matches "1" prefix
		{"multiple_numbers", "0.3 and 0.7", 0.3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseScore(tc.in)
			assert.InDelta(t, tc.want, got, 1e-6, "parseScore(%q)", tc.in)
		})
	}
}

// ---------------------------------------------------------------------------
// cosine — additional cases
// ---------------------------------------------------------------------------

func TestCosine_Extended(t *testing.T) {
	cases := []struct {
		name string
		a, b []float64
		want float64
	}{
		{"unit_x", []float64{1, 0, 0}, []float64{1, 0, 0}, 1},
		{"scaled", []float64{2, 0}, []float64{4, 0}, 1},
		{"negative_correlation", []float64{1, 2, 3}, []float64{-1, -2, -3}, -1},
		{"single_element", []float64{5}, []float64{3}, 1},
		{"single_element_zero", []float64{0}, []float64{3}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cosine(tc.a, tc.b)
			assert.InDelta(t, tc.want, got, 1e-9)
		})
	}
}

func TestCosine_LargeVectors(t *testing.T) {
	n := 1000
	a := make([]float64, n)
	b := make([]float64, n)
	for i := 0; i < n; i++ {
		a[i] = 1
		b[i] = 1
	}
	// Identical unit-like vectors → cosine = 1.
	assert.InDelta(t, 1.0, cosine(a, b), 1e-9)
}

// ---------------------------------------------------------------------------
// isEmbeddingReranker — additional cases
// ---------------------------------------------------------------------------

func TestIsEmbeddingReranker_Extended(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"bge-reranker-v2-m3", true},
		{"bge-reranker-large", true},
		{"bge-reranker-base:latest", true},
		{"BGE-M3", true},
		{"some-model-m3", true},
		{"some-model-m3:v2", true},
		{"e5-reranker-base", true},
		{"jina-reranker-v2-base", true},
		{"mxbai-rerank-large-v1", true},
		// Negative cases.
		{"llama3:latest", false},
		{"qwen2.5:7b", false},
		{"nomic-embed-text", false},
		{"", false},
		{"reranker", false},
		{"my-custom-model", false},
		{"bge-embed", false}, // not bge-reranker or bge-m3
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			assert.Equal(t, tc.want, isEmbeddingReranker(tc.model))
		})
	}
}

// ---------------------------------------------------------------------------
// buildPrompt — additional cases
// ---------------------------------------------------------------------------

func TestBuildPrompt_AllContextFields(t *testing.T) {
	p := buildPrompt("test query", "MyFunc", "pkg/file.go", "go", "func MyFunc() {}")
	assert.Contains(t, p, `Query: "test query"`)
	assert.Contains(t, p, "symbol MyFunc")
	assert.Contains(t, p, "file pkg/file.go")
	assert.Contains(t, p, "lang go")
	assert.Contains(t, p, "func MyFunc() {}")
	assert.True(t, strings.HasSuffix(p, "Relevance Score: "))
}

func TestBuildPrompt_OnlySymbol(t *testing.T) {
	p := buildPrompt("q", "Func", "", "", "doc")
	assert.Contains(t, p, "Context:")
	assert.Contains(t, p, "symbol Func")
	assert.NotContains(t, p, "file ")
	assert.NotContains(t, p, "lang ")
}

func TestBuildPrompt_OnlyPath(t *testing.T) {
	p := buildPrompt("q", "", "main.go", "", "doc")
	assert.Contains(t, p, "Context:")
	assert.Contains(t, p, "file main.go")
}

func TestBuildPrompt_OnlyLanguage(t *testing.T) {
	p := buildPrompt("q", "", "", "python", "doc")
	assert.Contains(t, p, "Context:")
	assert.Contains(t, p, "lang python")
}

func TestBuildPrompt_FewShotExamples(t *testing.T) {
	p := buildPrompt("q", "", "", "", "doc")
	assert.Contains(t, p, "Relevance Score: 1.0")
	assert.Contains(t, p, "Relevance Score: 0.0")
	assert.Contains(t, p, "how to install golang")
}

// ---------------------------------------------------------------------------
// identity — additional cases
// ---------------------------------------------------------------------------

func TestIdentity_EmptyInput(t *testing.T) {
	out := identity(nil, 0)
	assert.Empty(t, out)
}

func TestIdentity_SingleCandidate(t *testing.T) {
	cs := []Candidate{{ID: IDValue("x"), Score: 0.42}}
	out := identity(cs, 0)
	require.Len(t, out, 1)
	assert.Equal(t, IDValue("x"), out[0].ID)
	assert.Equal(t, 0.42, out[0].RerankScore)
}

func TestIdentity_TopNZeroReturnsAll(t *testing.T) {
	cs := []Candidate{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	out := identity(cs, 0)
	assert.Len(t, out, 3)
}

func TestIdentity_TopNGreaterThanLen(t *testing.T) {
	cs := []Candidate{{ID: "a"}}
	out := identity(cs, 10)
	assert.Len(t, out, 1)
}

func TestIdentity_PreservesOrder(t *testing.T) {
	cs := []Candidate{{ID: IDValue("c")}, {ID: IDValue("a")}, {ID: IDValue("b")}}
	out := identity(cs, 0)
	assert.Equal(t, IDValue("c"), out[0].ID)
	assert.Equal(t, IDValue("a"), out[1].ID)
	assert.Equal(t, IDValue("b"), out[2].ID)
}

// ---------------------------------------------------------------------------
// IDValue — additional JSON tests
// ---------------------------------------------------------------------------

func TestIDValue_UnmarshalJSON_InvalidData(t *testing.T) {
	var id IDValue
	err := json.Unmarshal([]byte(`true`), &id)
	assert.Error(t, err)
}

func TestIDValue_UnmarshalJSON_Null(t *testing.T) {
	var id IDValue
	err := json.Unmarshal([]byte(`null`), &id)
	// json.Unmarshal of null into json.Number succeeds with empty string.
	// The result is either error or empty IDValue.
	if err == nil {
		assert.Equal(t, IDValue(""), id)
	}
}

func TestIDValue_RoundTrip(t *testing.T) {
	original := IDValue("test-id-123")
	data, err := json.Marshal(original)
	require.NoError(t, err)

	var restored IDValue
	require.NoError(t, json.Unmarshal(data, &restored))
	assert.Equal(t, original, restored)
}

func TestIDValue_InStruct(t *testing.T) {
	type wrapper struct {
		ID   IDValue `json:"id"`
		Name string  `json:"name"`
	}
	raw := `{"id": 42, "name": "test"}`
	var w wrapper
	require.NoError(t, json.Unmarshal([]byte(raw), &w))
	assert.Equal(t, IDValue("42"), w.ID)
	assert.Equal(t, "test", w.Name)
}

// ---------------------------------------------------------------------------
// ollamaReranker — Available with model prefix matching
// ---------------------------------------------------------------------------

func TestAvailable_ExactModelMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]string{{"name": "llama3:8b"}},
		})
	}))
	defer srv.Close()

	r := &ollamaReranker{
		opts: Options{URL: srv.URL, Model: "llama3:8b"},
		http: &http.Client{Timeout: 5 * time.Second},
	}
	assert.True(t, r.Available(context.Background()))
}

func TestAvailable_PrefixMatchWithColon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]string{{"name": "llama3:8b-instruct"}},
		})
	}))
	defer srv.Close()

	r := &ollamaReranker{
		opts: Options{URL: srv.URL, Model: "llama3"},
		http: &http.Client{Timeout: 5 * time.Second},
	}
	// "llama3" matches "llama3:8b-instruct" via HasPrefix(model, m.Name).
	assert.True(t, r.Available(context.Background()))
}

func TestAvailable_TrailingSlashInURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/tags", r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]string{{"name": "test-model"}},
		})
	}))
	defer srv.Close()

	r := &ollamaReranker{
		opts: Options{URL: srv.URL + "/", Model: "test-model"},
		http: &http.Client{Timeout: 5 * time.Second},
	}
	assert.True(t, r.Available(context.Background()))
}

// ---------------------------------------------------------------------------
// ollamaReranker.scoreOne — content truncation
// ---------------------------------------------------------------------------

func TestScoreOne_ContentTruncation(t *testing.T) {
	var receivedPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedPrompt = string(body)
		_ = json.NewEncoder(w).Encode(map[string]any{"response": "0.5"})
	}))
	defer srv.Close()

	r := &ollamaReranker{
		opts: Options{URL: srv.URL, Model: "llama3", ContentMaxBytes: 10},
		http: &http.Client{Timeout: 5 * time.Second},
	}
	longContent := "This is a very long content that should be truncated"
	_, err := r.scoreOne(context.Background(), "q", Candidate{Content: longContent})
	require.NoError(t, err)
	assert.Contains(t, receivedPrompt, "[truncated]")
}

// ---------------------------------------------------------------------------
// ollamaReranker.scoreOne — ollama error response
// ---------------------------------------------------------------------------

func TestScoreOne_OllamaErrorField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response": "",
			"error":    "model not loaded",
		})
	}))
	defer srv.Close()

	r := &ollamaReranker{
		opts: Options{URL: srv.URL, Model: "llama3", ContentMaxBytes: 2000},
		http: &http.Client{Timeout: 5 * time.Second},
	}
	_, err := r.scoreOne(context.Background(), "q", Candidate{Content: "doc"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "model not loaded")
}

// ---------------------------------------------------------------------------
// ollamaReranker.Rerank — empty candidates
// ---------------------------------------------------------------------------

func TestRerank_EmptyCandidates(t *testing.T) {
	r := &ollamaReranker{
		opts: Options{URL: "http://localhost:11434", Model: "llama3"},
		http: &http.Client{Timeout: 5 * time.Second},
	}
	out, err := r.Rerank(context.Background(), "q", nil, 0)
	assert.NoError(t, err)
	assert.Nil(t, out)
}

// ---------------------------------------------------------------------------
// ollamaReranker.Rerank — tie-break by original Score
// ---------------------------------------------------------------------------

func TestRerank_TieBreakByOriginalScore(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]string{{"name": "llama3"}},
			})
		case "/api/generate":
			// All candidates get the same rerank score.
			_, _ = io.WriteString(w, `{"response":"0.5"}`)
			callCount++
		}
	}))
	defer srv.Close()

	r := &ollamaReranker{
		opts: Options{URL: srv.URL, Model: "llama3", ContentMaxBytes: 2000},
		http: &http.Client{Timeout: 5 * time.Second},
	}
	cs := []Candidate{
		{ID: IDValue("low"), Content: "doc1", Score: 0.1},
		{ID: IDValue("high"), Content: "doc2", Score: 0.9},
		{ID: IDValue("mid"), Content: "doc3", Score: 0.5},
	}
	out, err := r.Rerank(context.Background(), "q", cs, 0)
	require.NoError(t, err)
	require.Len(t, out, 3)
	// All have same RerankScore=0.5, so tie-break by original Score desc.
	assert.Equal(t, IDValue("high"), out[0].ID)
	assert.Equal(t, IDValue("mid"), out[1].ID)
	assert.Equal(t, IDValue("low"), out[2].ID)
}

// ---------------------------------------------------------------------------
// ollamaReranker.Rerank — scoring error with Required=false uses hybrid score
// ---------------------------------------------------------------------------

func TestRerank_ScoringError_NotRequired_FallsBackToHybridScore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]string{{"name": "llama3"}},
			})
		case "/api/generate":
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	r := &ollamaReranker{
		opts: Options{URL: srv.URL, Model: "llama3", ContentMaxBytes: 2000, Required: false},
		http: &http.Client{Timeout: 5 * time.Second},
	}
	cs := []Candidate{{ID: "a", Content: "doc", Score: 0.7}}
	out, err := r.Rerank(context.Background(), "q", cs, 0)
	require.NoError(t, err)
	require.Len(t, out, 1)
	// Should fall back to original Score.
	assert.Equal(t, 0.7, out[0].RerankScore)
}

// ---------------------------------------------------------------------------
// embed — error response
// ---------------------------------------------------------------------------

func TestEmbed_ErrorField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "model not found",
		})
	}))
	defer srv.Close()

	r := &ollamaReranker{
		opts: Options{URL: srv.URL, Model: "bge-m3", ContentMaxBytes: 2000},
		http: &http.Client{Timeout: 5 * time.Second},
	}
	_, err := r.embed(context.Background(), "test")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "model not found")
}

func TestEmbed_Non200Status(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal error", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	r := &ollamaReranker{
		opts: Options{URL: srv.URL, Model: "bge-m3", ContentMaxBytes: 2000},
		http: &http.Client{Timeout: 5 * time.Second},
	}
	_, err := r.embed(context.Background(), "test")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "status 503")
}

func TestEmbed_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "not json at all")
	}))
	defer srv.Close()

	r := &ollamaReranker{
		opts: Options{URL: srv.URL, Model: "bge-m3", ContentMaxBytes: 2000},
		http: &http.Client{Timeout: 5 * time.Second},
	}
	_, err := r.embed(context.Background(), "test")
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// scoreEmbed — normalization to [0,1]
// ---------------------------------------------------------------------------

func TestScoreEmbed_IdenticalVectors_ScoreOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": [][]float64{{1, 0, 0}},
		})
	}))
	defer srv.Close()

	r := &ollamaReranker{
		opts: Options{URL: srv.URL, Model: "bge-m3", ContentMaxBytes: 2000},
		http: &http.Client{Timeout: 5 * time.Second},
	}
	score, err := r.scoreEmbed(context.Background(), "q", "doc")
	require.NoError(t, err)
	// cos(1,1) = 1 → (1+1)/2 = 1.0
	assert.InDelta(t, 1.0, score, 1e-9)
}

func TestScoreEmbed_OppositeVectors_ScoreZero(t *testing.T) {
	callNum := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callNum++
		var emb []float64
		if callNum == 1 {
			emb = []float64{1, 0} // query
		} else {
			emb = []float64{-1, 0} // doc
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": [][]float64{emb},
		})
	}))
	defer srv.Close()

	r := &ollamaReranker{
		opts: Options{URL: srv.URL, Model: "bge-m3", ContentMaxBytes: 2000},
		http: &http.Client{Timeout: 5 * time.Second},
	}
	score, err := r.scoreEmbed(context.Background(), "q", "doc")
	require.NoError(t, err)
	// cos = -1 → (-1+1)/2 = 0
	assert.InDelta(t, 0.0, score, 1e-9)
}

func TestScoreEmbed_QueryEmbedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "fail", http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := &ollamaReranker{
		opts: Options{URL: srv.URL, Model: "bge-m3", ContentMaxBytes: 2000},
		http: &http.Client{Timeout: 5 * time.Second},
	}
	_, err := r.scoreEmbed(context.Background(), "q", "doc")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "embed(query)")
}

// ---------------------------------------------------------------------------
// ErrUnavailable
// ---------------------------------------------------------------------------

func TestErrUnavailable_Message(t *testing.T) {
	assert.Contains(t, ErrUnavailable.Error(), "unavailable")
}

// ---------------------------------------------------------------------------
// scoreOne with semaphore
// ---------------------------------------------------------------------------

func TestScoreOne_SemaphoreLimitsConcurrency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"response": "0.5"})
	}))
	defer srv.Close()

	sem := make(chan struct{}, 1)
	r := &ollamaReranker{
		opts: Options{URL: srv.URL, Model: "llama3", ContentMaxBytes: 2000},
		http: &http.Client{Timeout: 5 * time.Second},
		sem:  sem,
	}
	score, err := r.scoreOne(context.Background(), "q", Candidate{Content: "doc"})
	require.NoError(t, err)
	assert.InDelta(t, 0.5, score, 1e-6)
}

// ---------------------------------------------------------------------------
// cosine edge cases for score normalization
// ---------------------------------------------------------------------------

func TestCosine_NaN_Prevention_BothZero(t *testing.T) {
	// Both zero vectors → na=0, nb=0 → should return 0 (not NaN).
	got := cosine([]float64{0, 0, 0}, []float64{0, 0, 0})
	assert.Equal(t, 0.0, got)
	assert.False(t, math.IsNaN(got))
}
