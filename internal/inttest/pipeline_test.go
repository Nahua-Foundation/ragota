// Package integration_test exercises the full indexing pipeline over the testdata
// microservice fixtures: a Go monorepo (gateway + orders + proto contract),
// a Java Kafka consumer, a .NET HTTP service and a TypeScript client.
//
// The scenario under test is the chain:
//
//	web-ts ──HTTP──▶ gateway ──gRPC──▶ orders ──Kafka──▶ billing-java ──HTTP──▶ notifier-dotnet
package inttest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/servertest"
)

type repoIDs struct {
	mono, billing, notifier, web, analytics string
}

func (r repoIDs) all() []string {
	return []string{r.mono, r.billing, r.notifier, r.web, r.analytics}
}

func setupIndexedRepos(t *testing.T) (string, *http.Client, repoIDs) {
	t.Helper()
	srv, _ := servertest.SetupServer(t)
	client := &http.Client{Timeout: 30 * time.Second}

	ids := repoIDs{
		mono:      addRepo(t, client, srv.URL, "microservices", servertest.TestdataPath(t, "microservices")),
		billing:   addRepo(t, client, srv.URL, "billing", servertest.TestdataPath(t, "billing-java")),
		notifier:  addRepo(t, client, srv.URL, "notifier", servertest.TestdataPath(t, "notifier-dotnet")),
		web:       addRepo(t, client, srv.URL, "web", servertest.TestdataPath(t, "web-ts")),
		analytics: addRepo(t, client, srv.URL, "analytics", servertest.TestdataPath(t, "analytics-py")),
	}
	for _, id := range ids.all() {
		indexRepo(t, client, srv.URL, id)
	}
	for _, id := range ids.all() {
		waitIdle(t, client, srv.URL, id)
	}
	return srv.URL, client, ids
}

func TestCrossServiceE2E(t *testing.T) {
	base, client, ids := setupIndexedRepos(t)

	t.Run("ServicesDetected", func(t *testing.T) {
		res := getJSON[servicesResponse](t, client, base+"/api/v1/services")

		byName := map[string]serviceInfo{}
		for _, s := range res.Services {
			byName[s.Name] = s
		}
		// Monorepo services come from docker-compose; single-service repos
		// fall back to the repo name.
		for _, want := range []string{"gateway", "orders", "billing", "notifier", "web", "analytics"} {
			if _, ok := byName[want]; !ok {
				t.Errorf("service %q not detected; got %+v", want, res.Services)
			}
		}
		if g := byName["gateway"]; g.RepoID != ids.mono || g.Root != "services/gateway" {
			t.Errorf("gateway service = %+v, want repo %s root services/gateway", g, ids.mono)
		}
	})

	t.Run("ServiceLinks", func(t *testing.T) {
		res := getJSON[servicesResponse](t, client, base+"/api/v1/services")

		type link struct{ src, dst, kind string }
		var links []link
		for _, l := range res.Links {
			links = append(links, link{l.SrcService, l.DstService, l.Kind})
		}
		wants := []link{
			{"web", "gateway", "http_call"},       // TS client -> Go HTTP route
			{"gateway", "orders", "rpc_call"},     // Go client -> gRPC contract impl
			{"orders", "billing", "kafka_flow"},   // Go producer -> Java consumer
			{"billing", "notifier", "http_call"},  // Java RestTemplate -> C# route
			{"orders", "analytics", "kafka_flow"}, // Go producer -> Python consumer (env-driven topic)
		}
		for _, w := range wants {
			found := false
			for _, l := range links {
				if l == w {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("missing service link %v; got %+v", w, links)
			}
		}
	})

	t.Run("Topics", func(t *testing.T) {
		res := getJSON[topicsResponse](t, client, base+"/api/v1/topics")
		var found *topicInfo
		for i := range res.Topics {
			if res.Topics[i].Topic == "orders.created" {
				found = &res.Topics[i]
			}
		}
		if found == nil {
			t.Fatalf("topic orders.created not found; got %+v", res.Topics)
		}
		if len(found.Producers) == 0 || found.Producers[0].Service != "orders" {
			t.Errorf("producers = %+v, want service orders", found.Producers)
		}
		consumerServices := map[string]bool{}
		for _, c := range found.Consumers {
			consumerServices[c.Service] = true
		}
		// billing consumes via a literal topic, analytics via ORDERS_TOPIC
		// resolved from its .env file by the linker.
		if !consumerServices["billing"] || !consumerServices["analytics"] {
			t.Errorf("consumers = %+v, want billing and analytics", found.Consumers)
		}

		// service filter
		filtered := getJSON[topicsResponse](t, client, base+"/api/v1/topics?service=analytics")
		if len(filtered.Topics) != 1 || filtered.Topics[0].Topic != "orders.created" {
			t.Errorf("filtered topics = %+v", filtered.Topics)
		}
	})

	t.Run("TraceUserIDFromGateway", func(t *testing.T) {
		res := postJSON[traceResponse](t, client, base+"/api/v1/graph/trace", map[string]any{
			"repo_id": ids.mono,
			"symbol":  "CreateOrderHandler",
			"param":   "user_id",
		})
		assertTraceCrossesServices(t, res, []string{"orders", "billing", "notifier"})
	})

	t.Run("TraceUserIDFromWebClient", func(t *testing.T) {
		res := postJSON[traceResponse](t, client, base+"/api/v1/graph/trace", map[string]any{
			"repo_id": ids.web,
			"symbol":  "checkoutHandler",
			"param":   "user_id",
		})
		// The full chain: web -> gateway -> orders -> billing -> notifier.
		assertTraceCrossesServices(t, res, []string{"gateway", "orders", "billing", "notifier"})

		last := res.Steps[len(res.Steps)-1]
		if last.Unit.Name != "Save" && last.Unit.Name != "SaveNotification" {
			t.Errorf("trace should end in the notifier store, got %s (%s)", last.Unit.Name, last.Unit.FilePath)
		}
	})

	t.Run("TraceReachesDatabaseSink", func(t *testing.T) {
		res := postJSON[traceResponse](t, client, base+"/api/v1/graph/trace", map[string]any{
			"repo_id": ids.mono,
			"symbol":  "CreateOrderHandler",
			"param":   "user_id",
		})
		chains := append([][]traceStep{res.Steps}, res.Alternatives...)
		found := false
		for _, chain := range chains {
			for _, s := range chain {
				if s.Unit.Kind == "db_table" && s.Unit.Name == "analytics_events" {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("no chain reaches the analytics_events table; chains: %d, best: %s",
				res.Chains, chainString(res.Steps))
		}
	})

	t.Run("Context", func(t *testing.T) {
		res := postJSON[contextResponse](t, client, base+"/api/v1/context", map[string]any{
			"query": "publishes an OrderCreated event",
			"mode":  "keyword",
			"limit": 3,
			"hops":  2,
		})
		if len(res.Items) == 0 {
			t.Fatalf("context returned no items")
		}
		var item *contextItem
		for i := range res.Items {
			if res.Items[i].Unit.Name == "publishOrderCreated" {
				item = &res.Items[i]
			}
		}
		if item == nil {
			t.Fatalf("no context item anchored at publishOrderCreated; items: %+v", res.Items)
		}
		// Graph expansion must cross the Kafka boundary to the consumers.
		foundConsumer := false
		for _, rel := range item.Related {
			if rel.Unit.Name == "onOrderCreated" || rel.Unit.Name == "start_consumer" {
				foundConsumer = true
			}
		}
		if !foundConsumer {
			t.Errorf("context expansion did not reach Kafka consumers; related: %+v", item.Related)
		}
	})

	t.Run("MermaidExport", func(t *testing.T) {
		text := getText(t, client, base+"/api/v1/services/export?format=mermaid")
		if !strings.Contains(text, "flowchart LR") || !strings.Contains(text, "gateway") {
			t.Errorf("unexpected mermaid output:\n%s", text)
		}
		dot := getText(t, client, base+"/api/v1/services/export?format=dot")
		if !strings.Contains(dot, "digraph services") {
			t.Errorf("unexpected dot output:\n%s", dot)
		}
	})

	t.Run("Webhook", func(t *testing.T) {
		// The endpoint fails closed without a shared secret (configured by
		// SetupServer), so the caller must present it like a CI system would.
		res := postJSONWithHeaders[map[string]string](t, client, base+"/webhooks/git",
			map[string]any{"repository": map[string]any{"name": "web"}},
			map[string]string{"X-Webhook-Token": servertest.WebhookSecret})
		if res["repo_id"] != ids.web {
			t.Errorf("webhook matched repo %q, want %q", res["repo_id"], ids.web)
		}
		waitIdle(t, client, base, ids.web)
	})

	t.Run("OTelServiceGraph", func(t *testing.T) {
		res := postJSON[map[string]any](t, client, base+"/api/v1/otel/service-graph", map[string]any{
			"edges": []map[string]any{
				{"client": "gateway", "server": "orders", "calls": 1200},
				{"client": "ghost", "server": "orders"}, // unknown service — skipped
			},
		})
		if stored, _ := res["stored"].(float64); stored != 1 {
			t.Errorf("stored = %v, want 1", res["stored"])
		}
		// The unknown name has to come back, or a caller whose tracing backend
		// spells services differently has no way to find that out.
		unmatched, _ := res["unmatched"].([]any)
		if len(unmatched) != 1 || unmatched[0] != "ghost" {
			t.Errorf("unmatched = %v, want [ghost]", res["unmatched"])
		}
		svcRes := getJSON[servicesResponse](t, client, base+"/api/v1/services")
		found := false
		for _, l := range svcRes.Links {
			if l.Kind == "runtime_call" && l.SrcService == "gateway" && l.DstService == "orders" {
				found = true
			}
		}
		if !found {
			t.Errorf("runtime_call link missing; links: %+v", svcRes.Links)
		}
	})

	t.Run("Metrics", func(t *testing.T) {
		text := getText(t, client, base+"/metrics")
		for _, want := range []string{"ragota_http_requests_total", "ragota_repos", "ragota_indexer_documents"} {
			if !strings.Contains(text, want) {
				t.Errorf("metrics output missing %s:\n%s", want, text)
			}
		}
	})

	t.Run("OpenAPISpec", func(t *testing.T) {
		text := getText(t, client, base+"/openapi.yaml")
		for _, want := range []string{"openapi:", "/api/v1/context"} {
			if !strings.Contains(text, want) {
				t.Errorf("openapi.yaml missing %q", want)
			}
		}
	})

	t.Run("ObsMetrics", func(t *testing.T) {
		text := getText(t, client, base+"/metrics")
		for _, want := range []string{"ragota_index_repo_seconds_count", "ragota_link_seconds_count"} {
			if !strings.Contains(text, want) {
				t.Errorf("metrics output missing %s after indexing:\n%s", want, text)
			}
		}
	})

	t.Run("SymbolsAcrossLanguages", func(t *testing.T) {
		cases := []struct{ repo, name, kind, lang string }{
			{ids.mono, "publishOrderCreated", "function", "go"},
			{ids.billing, "onOrderCreated", "method", "java"},
			{ids.notifier, "Send", "method", "csharp"},
			{ids.web, "submitOrder", "function", "typescript"},
			{ids.mono, "CreateOrder", "rpc_method", "proto"},
			{ids.analytics, "store_event", "function", "python"},
			{ids.analytics, "analytics_events", "db_table", "sql"},
			{ids.analytics, "ORDERS_TOPIC", "config_key", "properties"},
		}
		for _, c := range cases {
			res := postJSON[symbolResponse](t, client, base+"/api/v1/nav/symbol", map[string]any{
				"repo_id": c.repo, "name": c.name, "kind": c.kind, "limit": 10,
			})
			if res.Total == 0 {
				t.Errorf("symbol %s (%s, %s) not found", c.name, c.kind, c.lang)
				continue
			}
			if got := res.Symbols[0].Language; got != c.lang {
				t.Errorf("symbol %s language = %q, want %q", c.name, got, c.lang)
			}
		}
	})

	t.Run("References", func(t *testing.T) {
		// createOrder is defined at services/gateway/main.go and called from
		// CreateOrderHandler; references are edges pointing at its unit.
		res := postJSON[referencesResponse](t, client, base+"/api/v1/nav/references", map[string]any{
			"repo_id":   ids.mono,
			"file_path": "services/gateway/main.go",
			"position":  map[string]int{"line": 45, "character": 6}, // inside createOrder
			"limit":     20,
		})
		if res.Total == 0 {
			t.Fatalf("expected references to createOrder, got none")
		}
	})

	t.Run("KeywordSearch", func(t *testing.T) {
		res := postJSON[searchResponse](t, client, base+"/api/v1/search", map[string]any{
			"query": "publishes an OrderCreated event",
			"mode":  "keyword",
			"limit": 5,
		})
		if res.Total == 0 {
			t.Fatalf("keyword search returned no hits")
		}
		found := false
		for _, h := range res.Hits {
			if strings.Contains(h.FilePath, "services/orders/main.go") {
				found = true
			}
		}
		if !found {
			t.Errorf("expected hit in services/orders/main.go, got %+v", res.Hits)
		}
	})

	t.Run("IncrementalReindexKeepsGraph", func(t *testing.T) {
		indexRepo(t, client, base, ids.mono)
		waitIdle(t, client, base, ids.mono)

		res := getJSON[servicesResponse](t, client, base+"/api/v1/services")
		found := false
		for _, l := range res.Links {
			if l.SrcService == "gateway" && l.DstService == "orders" && l.Kind == "rpc_call" {
				found = true
			}
		}
		if !found {
			t.Errorf("gateway->orders rpc link lost after reindex; links: %+v", res.Links)
		}
	})
}

// fakeGenerator is a deterministic stand-in for an LLM.
type fakeGenerator struct{}

func (fakeGenerator) Name() string { return "fake" }

func (fakeGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	first := prompt
	if i := strings.IndexByte(first, '\n'); i > 0 {
		first = first[:i]
	}
	return "FAKE-SUMMARY: " + first, nil
}

// TestLLMSummaries verifies that a configured generator produces file and
// service summary units during index.
func TestLLMSummaries(t *testing.T) {
	srv, svc := servertest.SetupServer(t)
	svc.SetGenerator(fakeGenerator{}, 10, true)
	client := &http.Client{Timeout: 30 * time.Second}

	id := addRepo(t, client, srv.URL, "web-sum", servertest.TestdataPath(t, "web-ts"))
	indexRepo(t, client, srv.URL, id)
	waitIdle(t, client, srv.URL, id)

	res := postJSON[symbolResponse](t, client, srv.URL+"/api/v1/nav/symbol", map[string]any{
		"repo_id": id, "kind": "summary", "limit": 20, "name": "orders.ts",
	})
	if res.Total == 0 {
		t.Fatalf("no file summary for orders.ts")
	}

	svcSum := postJSON[symbolResponse](t, client, srv.URL+"/api/v1/nav/symbol", map[string]any{
		"repo_id": id, "kind": "summary", "limit": 20, "name": "web-sum",
	})
	if svcSum.Total == 0 {
		t.Fatalf("no service summary for web-sum")
	}
}

// TestStaleFileCleanup verifies that files deleted from a repo disappear from
// the index on the next run.
func TestStaleFileCleanup(t *testing.T) {
	srv, _ := servertest.SetupServer(t)
	client := &http.Client{Timeout: 30 * time.Second}

	// Work on a mutable copy of the web-ts fixture.
	dir := t.TempDir()
	copyDir(t, servertest.TestdataPath(t, "web-ts"), dir)

	id := addRepo(t, client, srv.URL, "web-copy", dir)
	indexRepo(t, client, srv.URL, id)
	waitIdle(t, client, srv.URL, id)

	res := postJSON[symbolResponse](t, client, srv.URL+"/api/v1/nav/symbol", map[string]any{
		"repo_id": id, "name": "submitOrder", "limit": 5,
	})
	if res.Total == 0 {
		t.Fatalf("submitOrder not indexed")
	}

	if err := os.Remove(filepath.Join(dir, "src", "orders.ts")); err != nil {
		t.Fatal(err)
	}
	indexRepo(t, client, srv.URL, id)
	waitIdle(t, client, srv.URL, id)

	res = postJSON[symbolResponse](t, client, srv.URL+"/api/v1/nav/symbol", map[string]any{
		"repo_id": id, "name": "submitOrder", "limit": 5,
	})
	if res.Total != 0 {
		t.Errorf("submitOrder still indexed after file deletion: %+v", res.Symbols)
	}
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// assertTraceCrossesServices checks that the trace visits the given services in order.
func assertTraceCrossesServices(t *testing.T, res traceResponse, services []string) {
	t.Helper()
	if len(res.Steps) < 2 {
		t.Fatalf("trace produced %d steps: %+v", len(res.Steps), res.Steps)
	}
	var visited []string
	for _, s := range res.Steps {
		if len(visited) == 0 || visited[len(visited)-1] != s.Service {
			visited = append(visited, s.Service)
		}
	}
	pos := 0
	for _, svc := range visited {
		if pos < len(services) && svc == services[pos] {
			pos++
		}
	}
	if pos != len(services) {
		steps := make([]string, 0, len(res.Steps))
		for _, s := range res.Steps {
			steps = append(steps, fmt.Sprintf("%s/%s(%s)", s.Service, s.Unit.Name, s.Via))
		}
		t.Fatalf("trace did not cross services %v; visited %v; steps: %s",
			services, visited, strings.Join(steps, " -> "))
	}
}

// --- API response types (subset of fields the tests need) ---

type serviceInfo struct {
	RepoID     string `json:"repo_id"`
	Name       string `json:"name"`
	Root       string `json:"root"`
	DetectedBy string `json:"detected_by"`
}

type serviceLink struct {
	SrcService string  `json:"src_service"`
	DstService string  `json:"dst_service"`
	Kind       string  `json:"kind"`
	Via        string  `json:"via"`
	Confidence float32 `json:"confidence"`
}

type servicesResponse struct {
	Services []serviceInfo `json:"services"`
	Links    []serviceLink `json:"links"`
}

type unitRef struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	RepoID   string `json:"repo_id"`
	FilePath string `json:"file_path"`
	Language string `json:"language"`
}

type nodeRef struct {
	Unit    unitRef `json:"unit"`
	Service string  `json:"service"`
}

type topicInfo struct {
	Topic     string    `json:"topic"`
	Producers []nodeRef `json:"producers"`
	Consumers []nodeRef `json:"consumers"`
}

type topicsResponse struct {
	Topics []topicInfo `json:"topics"`
}

type traceStep struct {
	Unit       unitRef  `json:"unit"`
	Service    string   `json:"service"`
	Tracked    []string `json:"tracked"`
	Via        string   `json:"via"`
	Note       string   `json:"note"`
	Confidence float32  `json:"confidence"`
}

type traceResponse struct {
	Param        string        `json:"param"`
	Steps        []traceStep   `json:"steps"`
	Alternatives [][]traceStep `json:"alternatives"`
	Chains       int           `json:"chains"`
}

type relatedUnit struct {
	Unit      unitRef `json:"unit"`
	Service   string  `json:"service"`
	Via       string  `json:"via"`
	Direction string  `json:"direction"`
}

type contextItem struct {
	Unit    unitRef       `json:"unit"`
	Service string        `json:"service"`
	Related []relatedUnit `json:"related"`
}

type contextResponse struct {
	Query string        `json:"query"`
	Items []contextItem `json:"items"`
}

type symbolResponse struct {
	Symbols []unitRef `json:"symbols"`
	Total   int       `json:"total"`
}

type referencesResponse struct {
	References []json.RawMessage `json:"references"`
	Total      int               `json:"total"`
}

type searchHit struct {
	FilePath string `json:"file_path"`
}

type searchResponse struct {
	Hits  []searchHit `json:"hits"`
	Total int         `json:"total"`
}

// --- HTTP helpers ---

func addRepo(t *testing.T, client *http.Client, base, name, path string) string {
	t.Helper()
	repo := postJSON[struct {
		ID string `json:"id"`
	}](t, client, base+"/api/v1/repos", map[string]any{
		"name": name, "source": "local", "path": path,
	})
	if repo.ID == "" {
		t.Fatalf("add repo %s: empty id", name)
	}
	return repo.ID
}

func indexRepo(t *testing.T, client *http.Client, base, id string) {
	t.Helper()
	resp, err := client.Post(base+"/api/v1/repos/"+id+"/index", "application/json", nil)
	if err != nil {
		t.Fatalf("index repo %s: %v", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("index repo %s: status %d: %s", id, resp.StatusCode, body)
	}
}

func waitIdle(t *testing.T, client *http.Client, base, id string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		repo := getJSON[struct {
			Status    string `json:"status"`
			LastError string `json:"last_error"`
		}](t, client, base+"/api/v1/repos/"+id)
		switch repo.Status {
		case "idle":
			return
		case "error":
			t.Fatalf("repo %s failed to index: %s", id, repo.LastError)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("repo %s did not become idle in time", id)
}

func chainString(steps []traceStep) string {
	parts := make([]string, 0, len(steps))
	for _, s := range steps {
		parts = append(parts, s.Service+"/"+s.Unit.Name)
	}
	return strings.Join(parts, " -> ")
}

func getText(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status %d: %s", url, resp.StatusCode, data)
	}
	return string(data)
}

func getJSON[T any](t *testing.T, client *http.Client, url string) T {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	return decodeBody[T](t, url, resp)
}

func postJSON[T any](t *testing.T, client *http.Client, url string, body any) T {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := client.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	return decodeBody[T](t, url, resp)
}

func decodeBody[T any](t *testing.T, url string, resp *http.Response) T {
	t.Helper()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("%s: status %d: %s", url, resp.StatusCode, data)
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("%s: decode: %v (body: %s)", url, err, data)
	}
	return v
}

// postJSONWithHeaders is postJSON with extra request headers, used for the
// webhook endpoint's shared-secret token.
func postJSONWithHeaders[T any](t *testing.T, client *http.Client, url string, body any, headers map[string]string) T {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new request %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	return decodeBody[T](t, url, resp)
}
