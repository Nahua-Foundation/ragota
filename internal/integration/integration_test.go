//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/api"
	"github.com/Nahua-Foundation/ragota/internal/testutil"
)

// getRaw and postRaw return the status code and the undecoded body. They sit
// beside the generic getJSON/postJSON of the untagged e2e tests, which decode
// into a type and fail the test on a non-2xx: these tests assert on the status
// codes themselves (201, 202, 204, 404), so they need the response before it is
// interpreted.
func getRaw(t *testing.T, client *http.Client, url string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func postRaw(t *testing.T, client *http.Client, url string, body any) (int, []byte) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func TestBM25AndLocalSource(t *testing.T) {
	srv, _ := testutil.SetupServer(t)
	client := &http.Client{}

	// 1. GET /health → 200
	t.Run("Health", func(t *testing.T) {
		status, body := getRaw(t, client, srv.URL+"/health")
		if status != 200 {
			t.Fatalf("expected 200, got %d: %s", status, body)
		}
	})

	repoPath := testutil.TestdataPath(t, "go-test-project")

	// 2. POST /api/v1/repos with path → created
	t.Run("AddRepo", func(t *testing.T) {
		status, body := postRaw(t, client, srv.URL+"/api/v1/repos", map[string]any{
			"name":   "test-project",
			"source": "local",
			"path":   repoPath,
		})
		if status != http.StatusCreated {
			t.Fatalf("expected 201, got %d: %s", status, body)
		}
		var resp map[string]any
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		id, ok := resp["id"].(string)
		if !ok || id == "" {
			t.Fatal("missing repo id")
		}

		// 3. POST /api/v1/repos/{id}/index → 200
		t.Run("IndexRepo", func(t *testing.T) {
			status, _ = postRaw(t, client, srv.URL+"/api/v1/repos/"+id+"/index", map[string]any{
				"force": true,
			})
			if status != http.StatusAccepted {
				t.Fatalf("expected 202, got %d", status)
			}

			// 4. Poll GET /api/v1/repos/{id} until status == "idle" (timeout 30s, interval 200ms)
			t.Run("WaitIdle", func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				interval := 200 * time.Millisecond
				for {
					_, body := getRaw(t, client, srv.URL+"/api/v1/repos/"+id)
					var repo map[string]any
					if err := json.Unmarshal(body, &repo); err != nil {
						t.Fatalf("unmarshal: %v", err)
					}
					if repo["status"] == "idle" {
						break
					}
					select {
					case <-ctx.Done():
						t.Fatalf("timeout waiting for idle status, last: %v", repo["status"])
					case <-time.After(interval):
					}
				}
			})

			// 5. POST /api/v1/search {"query":"Add","mode":"keyword"} → 200, >= 1 hit with non-empty FilePath and Snippet
			t.Run("Search", func(t *testing.T) {
				status, body := postRaw(t, client, srv.URL+"/api/v1/search", map[string]any{
					"query": "Add",
					"mode":  "keyword",
				})
				if status != http.StatusOK {
					t.Fatalf("expected 200, got %d: %s", status, body)
				}
				var resp api.SearchResponse
				if err := json.Unmarshal(body, &resp); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if len(resp.Hits) < 1 {
					t.Fatal("expected at least 1 hit")
				}
				found := false
				for _, h := range resp.Hits {
					if h.FilePath != "" && h.Snippet != "" {
						found = true
						break
					}
				}
				if !found {
					t.Fatal("no hit with non-empty FilePath and Snippet")
				}
			})

			// 6. DELETE /api/v1/repos/{id} → 204; repeat GET → 404
			t.Run("DeleteRepo", func(t *testing.T) {
				// GET before delete to confirm it exists
				getStatus, _ := getRaw(t, client, srv.URL+"/api/v1/repos/"+id)
				if getStatus != http.StatusOK {
					t.Fatalf("expected 200 before delete, got %d", getStatus)
				}

				req, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete, srv.URL+"/api/v1/repos/"+id, nil)
				delResp, err := client.Do(req)
				if err != nil {
					t.Fatalf("delete request: %v", err)
				}
				defer delResp.Body.Close()
				if delResp.StatusCode != http.StatusNoContent {
					t.Fatalf("expected 204 on delete, got %d", delResp.StatusCode)
				}

				getStatus, _ = getRaw(t, client, srv.URL+"/api/v1/repos/"+id)
				if getStatus != http.StatusNotFound {
					t.Fatalf("expected 404 after delete, got %d", getStatus)
				}
			})
		})
	})
}
