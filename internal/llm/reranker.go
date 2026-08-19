package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/pkg/httpx"
)

const (
	defaultRerankPath    = "/rerank"
	defaultRerankTimeout = 30 * time.Second
	// rerankRetryBackoff is the pause before the single allowed retry.
	rerankRetryBackoff = 200 * time.Millisecond
	// defaultQueryTemplate is the instruction format Qwen3-Reranker was trained
	// on. It is applied only when an instruction is configured.
	defaultQueryTemplate = "<Instruct>: {instruction}\n<Query>: {query}"
)

// Reranker scores documents by relevance to a query.
type Reranker interface {
	// Name returns the reranker name.
	Name() string
	// Rerank returns one relevance score per document, preserving the input
	// order (scores[i] corresponds to documents[i]).
	Rerank(ctx context.Context, query string, documents []string) ([]float64, error)
}

// HTTPReranker calls a rerank service over HTTP: POST {BaseURL}{Path} with
// {"query": ..., "documents": [...], "model": ...}. It understands both the
// TEI-style response ([{"index":0,"score":0.9}, ...]) and the Cohere-style
// response ({"results":[{"index":0,"relevance_score":0.9}, ...]}), which is
// also what vLLM's /v1/rerank returns.
type HTTPReranker struct {
	client      *httpx.Client
	path        string
	model       string
	instruction string
	queryTmpl   string
	docTmpl     string
	// timeout bounds the whole rerank stage, not a single attempt: search is
	// on the request path behind a 15s write timeout, so a dead reranker must
	// not be able to burn timeout × (1 + retries) + backoff.
	timeout time.Duration
}

// NewHTTPReranker creates an HTTP reranker client from its configuration.
func NewHTTPReranker(cfg *config.RerankConfig) (*HTTPReranker, error) {
	if cfg == nil || cfg.BaseURL == "" {
		return nil, fmt.Errorf("http reranker: base url is required")
	}

	timeout := defaultRerankTimeout
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}

	path := cfg.Path
	switch {
	case path == "":
		path = defaultRerankPath
	case !strings.HasPrefix(path, "/"):
		path = "/" + path
	}

	// Rendering the query through a template only makes sense once there is an
	// instruction to place in it, so the default template stays off otherwise.
	queryTmpl := cfg.QueryTemplate
	if queryTmpl == "" && cfg.Instruction != "" {
		queryTmpl = defaultQueryTemplate
	}

	var header http.Header
	if cfg.APIKey != "" {
		header = http.Header{"Authorization": []string{"Bearer " + cfg.APIKey}}
	}

	return &HTTPReranker{
		client: &httpx.Client{
			BaseURL: cfg.BaseURL,
			Header:  header,
			HTTP:    &http.Client{Timeout: timeout},
			// A single retry at most: the rerank stage runs inside a search
			// request, and httpx's default (2 extra attempts with doubling
			// backoff) turns an unreachable reranker into minutes of latency.
			Retries: 1,
			Backoff: rerankRetryBackoff,
		},
		path:        path,
		model:       cfg.Model,
		instruction: cfg.Instruction,
		queryTmpl:   queryTmpl,
		docTmpl:     cfg.DocumentTemplate,
		timeout:     timeout,
	}, nil
}

// renderQuery applies the configured query template, if any.
func (r *HTTPReranker) renderQuery(query string) string {
	if r.queryTmpl == "" {
		return query
	}
	return strings.NewReplacer("{instruction}", r.instruction, "{query}", query).Replace(r.queryTmpl)
}

// renderDocuments applies the configured document template, if any.
func (r *HTTPReranker) renderDocuments(documents []string) []string {
	if r.docTmpl == "" {
		return documents
	}
	out := make([]string, len(documents))
	for i, doc := range documents {
		// Replacer makes a single pass over the template, so a placeholder
		// literal inside a document is never expanded again.
		out[i] = strings.NewReplacer("{instruction}", r.instruction, "{doc}", doc).Replace(r.docTmpl)
	}
	return out
}

// Name returns the reranker name.
func (r *HTTPReranker) Name() string { return "http-rerank" }

type rerankReq struct {
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	Model     string   `json:"model,omitempty"`
}

// rerankEntry covers both TEI ({"index","score"}) and Cohere
// ({"index","relevance_score"}) result entries.
type rerankEntry struct {
	Index     int      `json:"index"`
	Score     *float64 `json:"score"`
	Relevance *float64 `json:"relevance_score"`
}

// Rerank scores documents against the query via the remote app. The whole
// stage — including the retry — is bounded by the configured timeout.
func (r *HTTPReranker) Rerank(ctx context.Context, query string, documents []string) ([]float64, error) {
	if len(documents) == 0 {
		return nil, nil
	}

	timeout := r.timeout
	if timeout <= 0 {
		timeout = defaultRerankTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var raw json.RawMessage
	req := rerankReq{
		Query:     r.renderQuery(query),
		Documents: r.renderDocuments(documents),
		Model:     r.model,
	}
	if err := r.client.PostJSON(ctx, r.path, req, &raw); err != nil {
		rerankRequestFailures.Inc()
		return nil, fmt.Errorf("rerank: %w", err)
	}
	entries, err := parseRerankResponse(raw)
	if err != nil {
		rerankRequestFailures.Inc()
		return nil, err
	}

	// A missing score must not become 0.0: logit-based rerankers return
	// negative scores, so an unscored document would outrank every scored one.
	scores := make([]float64, len(documents))
	scored := make([]bool, len(documents))
	for _, e := range entries {
		if e.Index < 0 || e.Index >= len(documents) {
			rerankRequestFailures.Inc()
			return nil, fmt.Errorf("rerank: index %d out of range (%d documents)", e.Index, len(documents))
		}
		switch {
		case e.Score != nil:
			scores[e.Index] = *e.Score
		case e.Relevance != nil:
			scores[e.Index] = *e.Relevance
		default:
			continue
		}
		scored[e.Index] = true
	}
	for i, ok := range scored {
		if !ok {
			rerankRequestFailures.Inc()
			return nil, fmt.Errorf("rerank: no score for document %d of %d", i, len(documents))
		}
	}
	return scores, nil
}

// parseRerankResponse accepts either a bare array (TEI) or an object with a
// "results" field (Cohere).
func parseRerankResponse(raw json.RawMessage) ([]rerankEntry, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("rerank: empty response")
	}
	if trimmed[0] == '[' {
		var entries []rerankEntry
		if err := json.Unmarshal(trimmed, &entries); err != nil {
			return nil, fmt.Errorf("rerank: decode response: %w", err)
		}
		return entries, nil
	}
	var wrapped struct {
		Results []rerankEntry `json:"results"`
	}
	if err := json.Unmarshal(trimmed, &wrapped); err != nil {
		return nil, fmt.Errorf("rerank: decode response: %w", err)
	}
	if wrapped.Results == nil {
		return nil, fmt.Errorf("rerank: unrecognized response shape")
	}
	return wrapped.Results, nil
}
