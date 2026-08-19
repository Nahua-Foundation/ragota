package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/app"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/graph"
	"github.com/Nahua-Foundation/ragota/internal/index"
	"github.com/Nahua-Foundation/ragota/internal/server/api"
)

// postRaw sends a JSON body and returns the status and the exact bytes of the
// response, which the max_bytes tests have to measure rather than re-encode.
func postRaw(t *testing.T, url string, body any) (int, []byte) {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, out
}

func getRawBody(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, out
}

// hitsFixture is a ranked result whose snippets are large enough that a byte
// budget has to drop something.
func hitsFixture(n int) []*index.Hit {
	out := make([]*index.Hit, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, &index.Hit{
			RepoID:   "r1",
			FilePath: "src/handler.go",
			Path:     "src/handler.go",
			Line:     10 + i,
			EndLine:  40 + i,
			Symbol:   "Handle",
			Kind:     "function",
			Language: "go",
			Score:    float32(n-i) / float32(n),
			Snippet:  "func Handle() {\n" + strings.Repeat("\tdoWork()\n", 40) + "}",
			Reason:   "keyword",
		})
	}
	return out
}

// TestSearchHitWireShape pins what a hit looks like on the wire. The endpoint
// used to serialize the indexer's own struct, so it shipped the same value as
// `file_path` and again as `path`, and any field the indexer grew appeared in
// responses without anyone choosing it.
func TestSearchHitWireShape(t *testing.T) {
	svc := &fakeService{searchRes: &index.SearchResult{
		Hits: hitsFixture(1), Total: 1, Query: "handler",
	}}
	srv := newTestServer(t, svc, nil)

	status, body := postRaw(t, srv.URL+"/api/v1/search", map[string]any{"query": "handler"})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}

	var resp struct {
		Hits []map[string]any `json:"hits"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(resp.Hits))
	}
	hit := resp.Hits[0]
	if _, ok := hit["path"]; ok {
		t.Errorf("hit still carries the duplicate `path` field: %v", hit)
	}
	if hit["file_path"] != "src/handler.go" {
		t.Errorf("file_path = %v, want src/handler.go", hit["file_path"])
	}
	for _, key := range []string{"repo_id", "line", "end_line", "score", "snippet", "reason"} {
		if _, ok := hit[key]; !ok {
			t.Errorf("hit is missing %q: %v", key, hit)
		}
	}
}

// TestSearchDefaultsUnchanged: every new knob has to be inert when it is not
// set, or a retrieval baseline measured before them cannot be compared with one
// measured after.
func TestSearchDefaultsUnchanged(t *testing.T) {
	svc := &fakeService{searchRes: &index.SearchResult{
		Hits: hitsFixture(5), Total: 5, Query: "handler",
	}}
	srv := newTestServer(t, svc, nil)

	_, body := postRaw(t, srv.URL+"/api/v1/search", map[string]any{"query": "handler"})
	var resp api.SearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Hits) != 5 || resp.Total != 5 {
		t.Errorf("hits = %d, total = %d, want 5 and 5", len(resp.Hits), resp.Total)
	}
	if resp.Truncated {
		t.Error("truncated = true without a max_bytes")
	}
	if !strings.HasPrefix(resp.Hits[0].Snippet, "func Handle() {\n\tdoWork()") {
		t.Errorf("snippet = %q, want the whole chunk by default", resp.Hits[0].Snippet)
	}
}

// TestSearchReportsTheModeThatRan: the response used to echo the request's
// mode, so the most common call — one that omits it and gets hybrid — read back
// an empty string, and a client that inspects `mode` to learn what ran could
// not learn it on exactly that call.
func TestSearchReportsTheModeThatRan(t *testing.T) {
	for _, tt := range []struct{ sent, want string }{
		{"", "hybrid"},
		{"keyword", "keyword"},
		{"semantic", "semantic"},
		{"hybrid", "hybrid"},
	} {
		name := tt.sent
		if name == "" {
			name = "omitted"
		}
		t.Run(name, func(t *testing.T) {
			svc := &fakeService{searchRes: &index.SearchResult{
				Hits: hitsFixture(1), Total: 1, Query: "handler",
			}}
			srv := newTestServer(t, svc, nil)

			req := map[string]any{"query": "handler"}
			if tt.sent != "" {
				req["mode"] = tt.sent
			}
			_, body := postRaw(t, srv.URL+"/api/v1/search", req)
			var resp api.SearchResponse
			if err := json.Unmarshal(body, &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Mode != tt.want {
				t.Errorf("mode = %q, want %q", resp.Mode, tt.want)
			}
		})
	}
}

// degradedResult is a search answered from one index because the other was
// down, with the metadata the search layer records about it.
func degradedResult() *index.SearchResult {
	return &index.SearchResult{
		Hits: hitsFixture(2), Total: 2, Query: "handler",
		Metadata: map[string]interface{}{
			"searchers":         []string{"bm25"},
			"degraded":          true,
			"failed_searchers":  []string{"vector"},
			"searcher_errors":   map[string]string{"vector": "dial tcp: connection refused"},
			"reranked":          true,
			"rerank_candidates": 2,
		},
	}
}

// TestSearchDiagnosticsAreOptIn: the search layer computes this either way, and
// putting it in every response would undo the size budgeting. A caller asks for
// it and pays for it.
func TestSearchDiagnosticsAreOptIn(t *testing.T) {
	svc := &fakeService{searchRes: degradedResult()}
	srv := newTestServer(t, svc, nil)

	_, body := postRaw(t, srv.URL+"/api/v1/search", map[string]any{"query": "handler"})
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["diagnostics"]; ok {
		t.Errorf("an unasked-for response carries diagnostics: %v", raw["diagnostics"])
	}
}

// TestSearchDiagnosticsReportDegradation: the signal a client needs is whether
// a zero is the corpus's answer or a dead backend's, and it has to be a
// boolean it can branch on rather than a free-form map it has to interpret.
func TestSearchDiagnosticsReportDegradation(t *testing.T) {
	svc := &fakeService{searchRes: degradedResult()}
	srv := newTestServer(t, svc, nil)

	status, body := postRaw(t, srv.URL+"/api/v1/search",
		map[string]any{"query": "handler", "diagnostics": true})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var resp api.SearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	diag := resp.Diagnostics
	if diag == nil {
		t.Fatal("diagnostics were asked for and are missing")
	}
	if !diag.Degraded {
		t.Error("degraded = false, but a searcher failed")
	}
	if len(diag.Searchers) != 1 || diag.Searchers[0] != "bm25" {
		t.Errorf("searchers = %v, want the one index that answered", diag.Searchers)
	}
	if len(diag.FailedSearchers) != 1 || diag.FailedSearchers[0] != "vector" {
		t.Errorf("failed_searchers = %v, want [vector]", diag.FailedSearchers)
	}
	if !strings.Contains(diag.SearcherErrors["vector"], "connection refused") {
		t.Errorf("searcher_errors = %v, want what the failed searcher reported", diag.SearcherErrors)
	}
	if !diag.Reranked || diag.RerankCandidates != 2 {
		t.Errorf("reranked = %v over %d candidates, want true over 2", diag.Reranked, diag.RerankCandidates)
	}
	// The hits are the same either way: this describes the run, it does not
	// change it.
	if len(resp.Hits) != 2 || resp.Total != 2 {
		t.Errorf("hits = %d, total = %d; diagnostics must not alter the answer", len(resp.Hits), resp.Total)
	}
}

// TestSearchDiagnosticsOnAHealthySearch: the quiet case has to be legible too —
// an empty metadata map means every configured searcher answered, not that
// nothing is known.
func TestSearchDiagnosticsOnAHealthySearch(t *testing.T) {
	svc := &fakeService{searchRes: &index.SearchResult{
		Hits: hitsFixture(1), Total: 1, Query: "handler",
		Metadata: map[string]interface{}{"searchers": []string{"vector", "bm25"}},
	}}
	srv := newTestServer(t, svc, nil)

	_, body := postRaw(t, srv.URL+"/api/v1/search",
		map[string]any{"query": "handler", "diagnostics": true})
	var resp api.SearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Diagnostics == nil {
		t.Fatal("diagnostics were asked for and are missing")
	}
	if resp.Diagnostics.Degraded {
		t.Error("degraded = true with no failed searcher")
	}
	if len(resp.Diagnostics.Searchers) != 2 {
		t.Errorf("searchers = %v, want both indexes", resp.Diagnostics.Searchers)
	}
	if resp.Diagnostics.Reranked || resp.Diagnostics.RerankError != "" {
		t.Errorf("reranked = %v / %q, want the no-reranker-configured shape",
			resp.Diagnostics.Reranked, resp.Diagnostics.RerankError)
	}
}

// TestSearchDiagnosticsCountAgainstTheBudget: they are part of the body, so
// max_bytes has to be measured over a response that carries them.
func TestSearchDiagnosticsCountAgainstTheBudget(t *testing.T) {
	svc := &fakeService{searchRes: degradedResult()}
	srv := newTestServer(t, svc, nil)

	const budget = 1200
	_, body := postRaw(t, srv.URL+"/api/v1/search",
		map[string]any{"query": "handler", "diagnostics": true, "max_bytes": budget})
	if len(body) > budget {
		t.Errorf("body is %d bytes, over the %d asked for", len(body), budget)
	}
	var resp api.SearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("body is not valid JSON (%v): %s", err, body)
	}
	// The diagnostics survive the trim: they are why the caller asked, and
	// dropping them would leave a truncated answer with no account of itself.
	if resp.Diagnostics == nil || !resp.Diagnostics.Degraded {
		t.Errorf("the budget dropped the diagnostics: %+v", resp.Diagnostics)
	}
}

func TestSearchSnippetModes(t *testing.T) {
	tests := []struct {
		mode string
		want string
	}{
		{"", "func Handle() {"},
		{"chunk", "func Handle() {"},
		{"line", "func Handle() {"},
		{"none", ""},
	}
	for _, tt := range tests {
		t.Run("mode="+tt.mode, func(t *testing.T) {
			svc := &fakeService{searchRes: &index.SearchResult{
				Hits: hitsFixture(1), Total: 1, Query: "handler",
			}}
			srv := newTestServer(t, svc, nil)

			req := map[string]any{"query": "handler"}
			if tt.mode != "" {
				req["snippet"] = tt.mode
			}
			status, body := postRaw(t, srv.URL+"/api/v1/search", req)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", status, body)
			}
			var resp api.SearchResponse
			if err := json.Unmarshal(body, &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			got := resp.Hits[0].Snippet
			switch tt.mode {
			case "line":
				if got != tt.want {
					t.Errorf("snippet = %q, want the anchor line %q", got, tt.want)
				}
			case "none":
				if got != "" {
					t.Errorf("snippet = %q, want no code body at all", got)
				}
			default:
				if !strings.HasPrefix(got, tt.want) || !strings.Contains(got, "doWork") {
					t.Errorf("snippet = %q, want the whole chunk", got)
				}
			}
			// The location survives every mode: dropping the body must not cost
			// a caller the ability to open the file itself.
			if resp.Hits[0].FilePath != "src/handler.go" || resp.Hits[0].Line != 10 {
				t.Errorf("location = %s:%d, want src/handler.go:10", resp.Hits[0].FilePath, resp.Hits[0].Line)
			}
		})
	}
}

func TestSearchSnippetModeRejectsUnknown(t *testing.T) {
	svc := &fakeService{}
	srv := newTestServer(t, svc, nil)

	status, body := postRaw(t, srv.URL+"/api/v1/search",
		map[string]any{"query": "handler", "snippet": "off"})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown snippet mode: %s", status, body)
	}
	var errResp api.ErrorResponse
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Code != api.CodeValidationFailed {
		t.Errorf("code = %q, want %q", errResp.Code, api.CodeValidationFailed)
	}
}

// TestSearchMaxBytes: the budget is a promise about the body, and the hits that
// survive it are the ones ranked highest.
func TestSearchMaxBytes(t *testing.T) {
	svc := &fakeService{searchRes: &index.SearchResult{
		Hits: hitsFixture(10), Total: 10, Query: "handler",
	}}
	srv := newTestServer(t, svc, nil)

	const budget = 2000
	status, body := postRaw(t, srv.URL+"/api/v1/search",
		map[string]any{"query": "handler", "max_bytes": budget})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if len(body) > budget {
		t.Errorf("body is %d bytes, over the %d asked for", len(body), budget)
	}

	var resp api.SearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Truncated {
		t.Error("truncated = false, but hits were dropped")
	}
	if len(resp.Hits) == 0 || len(resp.Hits) >= 10 {
		t.Fatalf("hits = %d, want some but not all of the 10", len(resp.Hits))
	}
	// total still counts what was retrieved, so a caller can tell how much of
	// the answer the budget cost it.
	if resp.Total != 10 {
		t.Errorf("total = %d, want the 10 hits the query retrieved", resp.Total)
	}
	if resp.Hits[0].Line != 10 {
		t.Errorf("first surviving hit is line %d, want the top-ranked one (10)", resp.Hits[0].Line)
	}
	for i := 1; i < len(resp.Hits); i++ {
		if resp.Hits[i].Score > resp.Hits[i-1].Score {
			t.Fatalf("hits came back out of rank order at %d", i)
		}
	}
}

// TestSearchMaxBytesBelowEmptyResponse: a budget no answer can meet returns an
// empty, well-formed answer that says it was truncated — never a partial
// encoding, which would not parse.
func TestSearchMaxBytesBelowEmptyResponse(t *testing.T) {
	svc := &fakeService{searchRes: &index.SearchResult{
		Hits: hitsFixture(3), Total: 3, Query: "handler",
	}}
	srv := newTestServer(t, svc, nil)

	status, body := postRaw(t, srv.URL+"/api/v1/search",
		map[string]any{"query": "handler", "max_bytes": 1})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var resp api.SearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("body is not valid JSON (%v): %s", err, body)
	}
	if len(resp.Hits) != 0 || !resp.Truncated {
		t.Errorf("hits = %d, truncated = %v; want 0 and true", len(resp.Hits), resp.Truncated)
	}
}

func TestSearchMaxBytesRejectsNegative(t *testing.T) {
	svc := &fakeService{}
	srv := newTestServer(t, svc, nil)

	status, _ := postRaw(t, srv.URL+"/api/v1/search",
		map[string]any{"query": "handler", "max_bytes": -1})
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a negative max_bytes", status)
	}
}

// TestSearchSnippetNoneIsTheCheapAnswer records why the mode exists: the same
// query with no code body is a fraction of the tokens.
func TestSearchSnippetNoneIsTheCheapAnswer(t *testing.T) {
	svc := &fakeService{searchRes: &index.SearchResult{
		Hits: hitsFixture(10), Total: 10, Query: "handler",
	}}
	srv := newTestServer(t, svc, nil)

	_, full := postRaw(t, srv.URL+"/api/v1/search", map[string]any{"query": "handler"})
	_, bare := postRaw(t, srv.URL+"/api/v1/search",
		map[string]any{"query": "handler", "snippet": "none"})

	if len(bare)*4 > len(full) {
		t.Errorf("snippet=none is %d bytes against %d for the default; expected a far smaller answer",
			len(bare), len(full))
	}
}

func contextFixture(n int) *app.ContextResult {
	res := &app.ContextResult{Query: "handler", Mode: "keyword"}
	for _, h := range hitsFixture(n) {
		res.Items = append(res.Items, &app.ContextItem{
			Hit:     h,
			Unit:    &domain.ASTUnit{ID: "u1", RepoID: "r1", FilePath: h.FilePath, Name: "Handle", Kind: "function"},
			Service: "orders",
			Related: []*graph.RelatedUnit{{
				Unit:      &domain.ASTUnit{ID: "u2", RepoID: "r1", FilePath: "src/router.go", Name: "Route"},
				Via:       "call",
				Direction: "in",
				Distance:  1,
			}},
		})
	}
	return res
}

func TestContextMaxBytesAndSnippet(t *testing.T) {
	svc := &fakeService{contextRes: contextFixture(8)}
	srv := newTestServer(t, svc, nil)

	const budget = 2500
	status, body := postRaw(t, srv.URL+"/api/v1/context",
		map[string]any{"query": "handler", "max_bytes": budget})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if len(body) > budget {
		t.Errorf("body is %d bytes, over the %d asked for", len(body), budget)
	}
	var resp api.ContextResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Truncated || len(resp.Items) == 0 || len(resp.Items) >= 8 {
		t.Fatalf("items = %d, truncated = %v; want a proper prefix of the 8",
			len(resp.Items), resp.Truncated)
	}
	// An item keeps the graph expansion that explains it: items are dropped
	// whole, never split from their related units.
	if len(resp.Items[0].Related) != 1 {
		t.Errorf("first item lost its related units: %+v", resp.Items[0])
	}

	// snippet=none leaves the locations and the graph, and drops the bodies.
	_, bare := postRaw(t, srv.URL+"/api/v1/context",
		map[string]any{"query": "handler", "snippet": "none"})
	var noBody api.ContextResponse
	if err := json.Unmarshal(bare, &noBody); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(noBody.Items) != 8 || noBody.Truncated {
		t.Fatalf("items = %d, truncated = %v; want all 8 untruncated",
			len(noBody.Items), noBody.Truncated)
	}
	for i, item := range noBody.Items {
		if item.Hit.Snippet != "" {
			t.Fatalf("item %d still carries a snippet: %q", i, item.Hit.Snippet)
		}
		if item.Hit.FilePath == "" || item.Hit.Line == 0 {
			t.Fatalf("item %d lost its location: %+v", i, item.Hit)
		}
	}
}

func TestContextDefaultsUnchanged(t *testing.T) {
	svc := &fakeService{contextRes: contextFixture(3)}
	srv := newTestServer(t, svc, nil)

	_, body := postRaw(t, srv.URL+"/api/v1/context", map[string]any{"query": "handler"})
	var resp api.ContextResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 3 || resp.Truncated {
		t.Errorf("items = %d, truncated = %v; want all 3 and no truncation",
			len(resp.Items), resp.Truncated)
	}
	if !strings.Contains(resp.Items[0].Hit.Snippet, "doWork") {
		t.Errorf("snippet = %q, want the whole chunk by default", resp.Items[0].Hit.Snippet)
	}
}

func servicesFixture() ([]*graph.ServiceInfo, []*graph.ServiceLink) {
	return []*graph.ServiceInfo{
			{RepoID: "r1", Name: "orders", UnitID: "u1"},
			{RepoID: "r1", Name: "billing", UnitID: "u2"},
			{RepoID: "r2", Name: "gateway", UnitID: "u3"},
		}, []*graph.ServiceLink{
			{SrcRepo: "r1", SrcService: "orders", DstRepo: "r1", DstService: "billing", Kind: "rpc_call"},
			{SrcRepo: "r2", SrcService: "gateway", DstRepo: "r1", DstService: "orders", Kind: "http_call"},
			{SrcRepo: "r2", SrcService: "gateway", DstRepo: "r2", DstService: "gateway", Kind: "kafka_flow"},
		}
}

func TestServicesRepoFilter(t *testing.T) {
	services, links := servicesFixture()
	svc := &fakeService{services: services, serviceLinks: links}
	srv := newTestServer(t, svc, nil)

	status, body := getRawBody(t, srv.URL+"/api/v1/services?repo=r2")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var resp api.ServicesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Services) != 1 || resp.Services[0].Name != "gateway" {
		t.Errorf("services = %v, want only gateway", resp.Services)
	}
	// The cross-repo link is the point of asking: r2's gateway calls r1's
	// orders, and dropping it would leave the filter useless.
	if len(resp.Links) != 2 {
		t.Errorf("links = %d, want the two that touch r2", len(resp.Links))
	}
	if resp.Truncated {
		t.Error("truncated = true without a limit")
	}
}

func TestServicesRepoFilterAcceptsRepeatedAndCommaLists(t *testing.T) {
	services, links := servicesFixture()
	for _, query := range []string{"?repo=r1&repo=r2", "?repo=r1,r2"} {
		svc := &fakeService{services: services, serviceLinks: links}
		srv := newTestServer(t, svc, nil)
		_, body := getRawBody(t, srv.URL+"/api/v1/services"+query)
		var resp api.ServicesResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("%s: decode: %v", query, err)
		}
		if len(resp.Services) != 3 {
			t.Errorf("%s: services = %d, want all 3", query, len(resp.Services))
		}
	}
}

func TestServicesLimit(t *testing.T) {
	services, links := servicesFixture()
	svc := &fakeService{services: services, serviceLinks: links}
	srv := newTestServer(t, svc, nil)

	_, body := getRawBody(t, srv.URL+"/api/v1/services?limit=2")
	var resp api.ServicesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Services) != 2 || len(resp.Links) != 2 {
		t.Errorf("services = %d, links = %d; want 2 and 2", len(resp.Services), len(resp.Links))
	}
	if !resp.Truncated {
		t.Error("truncated = false, but both lists were cut")
	}
}

func TestServicesRejectsBadLimit(t *testing.T) {
	svc := &fakeService{}
	srv := newTestServer(t, svc, nil)

	for _, raw := range []string{"0", "-3", "many"} {
		status, _ := getRawBody(t, srv.URL+"/api/v1/services?limit="+raw)
		if status != http.StatusBadRequest {
			t.Errorf("limit=%s: status = %d, want 400", raw, status)
		}
	}
}

func TestServicesDefaultsUnchanged(t *testing.T) {
	services, links := servicesFixture()
	svc := &fakeService{services: services, serviceLinks: links}
	srv := newTestServer(t, svc, nil)

	_, body := getRawBody(t, srv.URL+"/api/v1/services")
	var resp api.ServicesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Services) != 3 || len(resp.Links) != 3 || resp.Truncated {
		t.Errorf("services = %d, links = %d, truncated = %v; want the whole graph",
			len(resp.Services), len(resp.Links), resp.Truncated)
	}
}

// TestHealthReportsVersions: an external client cannot read the server's build
// flags, so /health is the only place it can learn what it is talking to.
func TestHealthReportsVersions(t *testing.T) {
	svc := &fakeService{}
	srv := newTestServer(t, svc, nil, api.WithVersion("v1.2.3-4-gabcdef"))

	status, body := getRawBody(t, srv.URL+"/health")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var resp api.HealthResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("status = %q, want ok", resp.Status)
	}
	if resp.Version != "v1.2.3-4-gabcdef" {
		t.Errorf("version = %q, want the stamped build version", resp.Version)
	}
	if resp.APIVersion != api.SchemaVersion {
		t.Errorf("api_version = %q, want %q", resp.APIVersion, api.SchemaVersion)
	}
}

// TestHealthVersionDefaultsToDev: a build nobody stamped still answers, and
// says so, rather than reporting an empty version a client cannot interpret.
func TestHealthVersionDefaultsToDev(t *testing.T) {
	svc := &fakeService{}
	srv := newTestServer(t, svc, nil)

	_, body := getRawBody(t, srv.URL+"/health")
	var resp api.HealthResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Version != "dev" {
		t.Errorf("version = %q, want dev", resp.Version)
	}
}
