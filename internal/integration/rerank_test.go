package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/api"
	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/setup"
	"github.com/Nahua-Foundation/ragota/internal/testutil"
)

// rerankHit exposes the fields the rerank assertions need (Hit has no json
// tags, so fields are serialized under their Go names).
type rerankHit struct {
	FilePath string `json:"file_path"`
	Line     int    `json:"line"`
	Reason   string `json:"reason"`
}

type rerankSearchResponse struct {
	Hits  []rerankHit `json:"hits"`
	Total int         `json:"total"`
}

func (h rerankHit) key() string { return fmt.Sprintf("%s:%d", h.FilePath, h.Line) }

func hitOrder(hits []rerankHit) []string {
	order := make([]string, len(hits))
	for i, h := range hits {
		order[i] = h.key()
	}
	return order
}

func sameOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// rerankTestConfig is a minimal keyword-search config with the rerank stage
// pointed at rerankURL.
func rerankTestConfig(rerankURL string) *config.Config {
	return &config.Config{
		Server: config.ServerConfig{Host: "127.0.0.1", Port: 0},
		Storage: config.StorageConfig{
			SQLite: &config.SQLiteStorageConfig{Path: ":memory:", PoolSize: 1},
		},
		Indexes: config.IndexesConfig{
			BM25: &config.BM25IndexConfig{Enabled: true},
		},
		Models: config.ModelsConfig{Providers: map[string]config.ProviderConfig{}},
		Repos: config.ReposConfig{
			Sources: config.ReposSourcesConfig{
				Local: &config.LocalSourceConfig{Enabled: true},
			},
		},
		Search: &config.SearchConfig{
			Rerank: &config.RerankConfig{
				Enabled: true,
				BaseURL: rerankURL,
				TopN:    50,
			},
		},
	}
}

const rerankQuery = "gateway order"

func keywordSearch(t *testing.T, client *http.Client, base string) rerankSearchResponse {
	t.Helper()
	return postJSON[rerankSearchResponse](t, client, base+"/api/v1/search", map[string]any{
		"query": rerankQuery,
		"mode":  "keyword",
		"limit": 5,
	})
}

// TestRerankE2E indexes the web-ts fixture twice — once without and once with
// an order-inverting rerank service — and verifies that the reranker changes
// the result order and annotates Reason, and that an upstream 500 degrades
// gracefully to the original order.
func TestRerankE2E(t *testing.T) {
	// Rerank stub: score = position in the candidate list, so the last
	// candidate wins and the order inverts. fail switches it to 500s.
	var fail atomic.Bool
	rerankSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "rerank down", http.StatusInternalServerError)
			return
		}
		var req struct {
			Documents []string `json:"documents"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		entries := make([]map[string]any, len(req.Documents))
		for i := range req.Documents {
			entries[i] = map[string]any{"index": i, "score": float64(i)}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entries)
	}))
	defer rerankSrv.Close()

	client := &http.Client{Timeout: 30 * time.Second}

	// Baseline instance without a reranker.
	baseSrv, _ := testutil.SetupServer(t)
	baseID := addRepo(t, client, baseSrv.URL, "web-rerank-base", testutil.TestdataPath(t, "web-ts"))
	indexRepo(t, client, baseSrv.URL, baseID)
	waitIdle(t, client, baseSrv.URL, baseID)

	baseline := keywordSearch(t, client, baseSrv.URL)
	if len(baseline.Hits) < 2 {
		t.Fatalf("baseline search returned %d hits, need at least 2 to observe reordering", len(baseline.Hits))
	}
	baseOrder := hitOrder(baseline.Hits)
	for i, h := range baseline.Hits {
		if strings.Contains(h.Reason, "rerank") {
			t.Fatalf("baseline hit[%d] unexpectedly marked reranked: %q", i, h.Reason)
		}
	}

	// Instance with the rerank stage enabled.
	os.Setenv("RAGOTA_BM25_PATH", t.TempDir())
	cfg := rerankTestConfig(rerankSrv.URL)
	svc, err := setup.Build(context.Background(), cfg)
	if err != nil {
		t.Fatalf("setup build: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close(context.Background()) })
	httpSrv := httptest.NewServer(api.NewServer(svc, &cfg.Server).Router())
	t.Cleanup(httpSrv.Close)

	id := addRepo(t, client, httpSrv.URL, "web-rerank", testutil.TestdataPath(t, "web-ts"))
	indexRepo(t, client, httpSrv.URL, id)
	waitIdle(t, client, httpSrv.URL, id)

	reranked := keywordSearch(t, client, httpSrv.URL)
	if len(reranked.Hits) != len(baseline.Hits) {
		t.Fatalf("reranked search returned %d hits, baseline %d", len(reranked.Hits), len(baseline.Hits))
	}
	if sameOrder(hitOrder(reranked.Hits), baseOrder) {
		t.Errorf("reranked order equals baseline order; inverting reranker had no effect: %v", baseOrder)
	}
	for i, h := range reranked.Hits {
		if !strings.Contains(h.Reason, "rerank") {
			t.Errorf("reranked hit[%d].Reason = %q, want it to contain rerank", i, h.Reason)
		}
	}

	// Upstream failure: search must still work and keep the original order.
	fail.Store(true)
	degraded := keywordSearch(t, client, httpSrv.URL)
	if degraded.Total == 0 {
		t.Fatalf("search returned no hits while reranker is down")
	}
	if !sameOrder(hitOrder(degraded.Hits), baseOrder) {
		t.Errorf("order with failing reranker = %v, want baseline order %v",
			hitOrder(degraded.Hits), baseOrder)
	}
	for i, h := range degraded.Hits {
		if strings.Contains(h.Reason, "rerank") {
			t.Errorf("degraded hit[%d] marked reranked despite upstream failure: %q", i, h.Reason)
		}
	}
}
