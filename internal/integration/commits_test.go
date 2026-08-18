package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/testutil"
)

// ordersWithCancel is the new content of src/orders.ts pushed in commit c1:
// the original functions plus a new cancelOrder.
const ordersWithCancel = `import axios from 'axios';

const GATEWAY_URL = 'http://gateway:8080/api/v1/orders';

// submitOrder sends the order to the gateway service.
export async function submitOrder(userId: string, amount: number) {
  const response = await axios.post(GATEWAY_URL, {
    user_id: userId,
    amount: amount,
  });
  return response.data;
}

// cancelOrder cancels an order in the gateway by id.
export async function cancelOrder(orderId: string) {
  const response = await axios.delete(GATEWAY_URL + '/' + orderId);
  return response.data;
}
`

// ordersFromDisk adds refundOrder on top of the c1 content. It is written to
// the working tree by the test and pushed as a commit without content, so the
// server must pick it up from disk.
const ordersFromDisk = ordersWithCancel + `
// refundOrder refunds an order in the gateway by id.
export async function refundOrder(orderId: string) {
  const response = await axios.post(GATEWAY_URL + '/' + orderId + '/refund', {});
  return response.data;
}
`

// TestCommitIngestion exercises the incremental commit-based indexing flow:
// push commits with diffs, verify partial reindex, commit-gap rejection and
// reading file content from disk when the commit carries none.
func TestCommitIngestion(t *testing.T) {
	srv, _ := testutil.SetupServer(t)
	client := &http.Client{Timeout: 30 * time.Second}
	base := srv.URL

	// Work on a mutable copy of the web-ts fixture.
	dir := t.TempDir()
	copyDir(t, testutil.TestdataPath(t, "web-ts"), dir)

	id := addRepo(t, client, base, "web-commits", dir)
	indexRepo(t, client, base, id)
	waitIdle(t, client, base, id)

	// Baseline: symbols from both files are indexed.
	if total := symbolCount(t, client, base, id, "checkoutHandler"); total == 0 {
		t.Fatalf("checkoutHandler not indexed after full index")
	}

	t.Run("ApplyCommitWithContentAndDelete", func(t *testing.T) {
		status, body := postCommits(t, client, base, id, map[string]any{
			"commits": []map[string]any{{
				"sha":     "c1",
				"parents": []string{},
				"files": []map[string]any{
					{"path": "src/orders.ts", "status": "M", "content": ordersWithCancel},
					{"path": "src/server.ts", "status": "D"},
				},
			}},
		})
		if status != http.StatusAccepted {
			t.Fatalf("push commits: status %d: %s", status, body)
		}
		waitIdle(t, client, base, id)

		if total := symbolCount(t, client, base, id, "cancelOrder"); total == 0 {
			t.Errorf("cancelOrder not indexed after commit c1")
		}
		if total := symbolCount(t, client, base, id, "checkoutHandler"); total != 0 {
			t.Errorf("checkoutHandler still indexed after src/server.ts deletion")
		}

		state := getJSON[syncState](t, client, base+"/api/v1/repos/"+id+"/sync-state")
		if state.LastCommit != "c1" {
			t.Errorf("sync-state last_commit = %q, want c1", state.LastCommit)
		}
		if state.RepoID != id {
			t.Errorf("sync-state repo_id = %q, want %q", state.RepoID, id)
		}
	})

	t.Run("CommitGapRejected", func(t *testing.T) {
		status, body := postCommits(t, client, base, id, map[string]any{
			"commits": []map[string]any{{
				"sha":     "c9",
				"parents": []string{"nonexistent"},
				"files": []map[string]any{
					{"path": "src/orders.ts", "status": "M", "content": "export const x = 1;\n"},
				},
			}},
		})
		if status != http.StatusConflict {
			t.Fatalf("gap commit: status %d, want 409: %s", status, body)
		}
		var conflict struct {
			Error      string `json:"error"`
			LastCommit string `json:"last_commit"`
		}
		if err := json.Unmarshal(body, &conflict); err != nil {
			t.Fatalf("decode conflict body: %v (%s)", err, body)
		}
		if conflict.LastCommit != "c1" {
			t.Errorf("conflict last_commit = %q, want c1", conflict.LastCommit)
		}
		// The gap must not have indexed anything.
		if total := symbolCount(t, client, base, id, "cancelOrder"); total == 0 {
			t.Errorf("cancelOrder lost after rejected commit")
		}
	})

	t.Run("ApplyCommitWithoutContentReadsDisk", func(t *testing.T) {
		// The external client modified the file but pushes no content: the
		// server must read the working tree.
		if err := os.WriteFile(filepath.Join(dir, "src", "orders.ts"), []byte(ordersFromDisk), 0o644); err != nil {
			t.Fatal(err)
		}
		status, body := postCommits(t, client, base, id, map[string]any{
			"commits": []map[string]any{{
				"sha":     "c2",
				"parents": []string{"c1"},
				"files": []map[string]any{
					{"path": "src/orders.ts", "status": "M"},
				},
			}},
		})
		if status != http.StatusAccepted {
			t.Fatalf("push commit c2: status %d: %s", status, body)
		}
		waitIdle(t, client, base, id)

		if total := symbolCount(t, client, base, id, "refundOrder"); total == 0 {
			t.Errorf("refundOrder not indexed from disk after commit c2")
		}
		state := getJSON[syncState](t, client, base+"/api/v1/repos/"+id+"/sync-state")
		if state.LastCommit != "c2" {
			t.Errorf("sync-state last_commit = %q, want c2", state.LastCommit)
		}
	})
}

type syncState struct {
	RepoID     string `json:"repo_id"`
	LastCommit string `json:"last_commit"`
	Status     string `json:"status"`
}

// symbolCount returns how many symbols with the given name are indexed.
func symbolCount(t *testing.T, client *http.Client, base, repoID, name string) int {
	t.Helper()
	res := postJSON[symbolResponse](t, client, base+"/api/v1/nav/symbol", map[string]any{
		"repo_id": repoID, "name": name, "limit": 5,
	})
	return res.Total
}

// postCommits posts a commits payload and returns the raw status and body
// (unlike postJSON, it does not fail on non-2xx responses).
func postCommits(t *testing.T, client *http.Client, base, repoID string, payload any) (int, []byte) {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal commits: %v", err)
	}
	resp, err := client.Post(base+"/api/v1/repos/"+repoID+"/commits", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST commits: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}
