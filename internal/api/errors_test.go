package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/api"
	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/service"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// decodeError reads an ErrorResponse body.
func decodeError(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return body
}

func post(t *testing.T, url string, body []byte) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestErrorCodesArePresent: every error body carries a stable machine-readable
// code, which is what clients are supposed to branch on.
func TestErrorCodesArePresent(t *testing.T) {
	svc := &fakeService{repos: map[string]*repos.Repo{}, getErr: storage.ErrNotFound}
	srv := newTestServer(t, svc, nil)

	t.Run("not found", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/api/v1/repos/nope")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		if code := decodeError(t, resp)["code"]; code != api.CodeNotFound {
			t.Errorf("code = %v, want %s", code, api.CodeNotFound)
		}
	})

	t.Run("validation failed", func(t *testing.T) {
		resp := post(t, srv.URL+"/api/v1/repos", []byte(`{"source":"local","path":"/tmp"}`))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		if code := decodeError(t, resp)["code"]; code != api.CodeValidationFailed {
			t.Errorf("code = %v, want %s", code, api.CodeValidationFailed)
		}
	})
}

// TestRepoBusyIsConflictWithRetryAfter: busy is a retry condition, not a
// malformed request, so it must not be a 400.
func TestRepoBusyIsConflictWithRetryAfter(t *testing.T) {
	svc := &fakeService{
		repos:    map[string]*repos.Repo{"r1": {ID: "r1"}},
		indexErr: fmt.Errorf("%w: r1", service.ErrRepoBusy),
	}
	srv := newTestServer(t, svc, nil)

	resp := post(t, srv.URL+"/api/v1/repos/r1/index", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After header on a busy response")
	}
	body := decodeError(t, resp)
	if body["code"] != api.CodeRepoBusy {
		t.Errorf("code = %v, want %s", body["code"], api.CodeRepoBusy)
	}
}

// TestInvalidPathCode: a rejected commit path is reported specifically, so a
// client can tell it apart from a generic validation failure.
func TestInvalidPathCode(t *testing.T) {
	svc := &fakeService{
		repos:      map[string]*repos.Repo{"r1": {ID: "r1"}},
		commitsErr: fmt.Errorf("%w: ../etc/passwd", service.ErrInvalidPath),
	}
	srv := newTestServer(t, svc, nil)

	resp := post(t, srv.URL+"/api/v1/repos/r1/commits",
		[]byte(`{"commits":[{"sha":"a","files":[{"path":"../etc/passwd","status":"M"}]}]}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if code := decodeError(t, resp)["code"]; code != api.CodeInvalidPath {
		t.Errorf("code = %v, want %s", code, api.CodeInvalidPath)
	}
}

// TestCommitGapCode covers the 409 the sync protocol relies on.
func TestCommitGapCode(t *testing.T) {
	no := false
	svc := &fakeService{
		repos:     map[string]*repos.Repo{"r1": {ID: "r1", LastCommit: "sha-old"}},
		commitsOK: &no,
	}
	srv := newTestServer(t, svc, nil)

	resp := post(t, srv.URL+"/api/v1/repos/r1/commits",
		[]byte(`{"commits":[{"sha":"sha-new","parents":["unrelated"],"files":[]}]}`))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	body := decodeError(t, resp)
	if body["code"] != api.CodeCommitGap {
		t.Errorf("code = %v, want %s", body["code"], api.CodeCommitGap)
	}
	if body["last_commit"] != "sha-old" {
		t.Errorf("last_commit = %v, want sha-old", body["last_commit"])
	}
}

// TestCommitsAcceptLargeBody: the endpoint that carries file contents needs a
// much larger cap than the general 1 MiB one — a realistic commit exceeds it.
func TestCommitsAcceptLargeBody(t *testing.T) {
	svc := &fakeService{repos: map[string]*repos.Repo{"r1": {ID: "r1"}}}
	srv := newTestServer(t, svc, nil)

	// ~1.3 MB of file content, which the old shared 1 MiB cap rejected as a
	// malformed body.
	content := strings.Repeat("x", 1_300_000)
	payload, err := json.Marshal(map[string]any{
		"commits": []map[string]any{{
			"sha":   "sha-1",
			"files": []map[string]any{{"path": "big.go", "status": "M", "content": content}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	resp := post(t, srv.URL+"/api/v1/repos/r1/commits", payload)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 for a 1.3 MB commit", resp.StatusCode)
	}
	if len(svc.lastCommits) != 1 || len(svc.lastCommits[0].Files) != 1 {
		t.Fatalf("service received %+v", svc.lastCommits)
	}
	if len(svc.lastCommits[0].Files[0].Content) != len(content) {
		t.Error("file content was truncated on the way through")
	}
}

// TestOversizedBodyIs413 covers the other half: too large is a 413 naming the
// limit, not a 400 indistinguishable from malformed JSON.
func TestOversizedBodyIs413(t *testing.T) {
	t.Setenv("RAGOTA_MAX_BODY_BYTES", "1024")
	svc := &fakeService{repos: map[string]*repos.Repo{}}
	srv := newTestServer(t, svc, nil)

	payload, err := json.Marshal(map[string]any{
		"name": strings.Repeat("n", 4096), "source": "local", "path": "/tmp",
	})
	if err != nil {
		t.Fatal(err)
	}

	resp := post(t, srv.URL+"/api/v1/repos", payload)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	body := decodeError(t, resp)
	if body["code"] != api.CodePayloadTooLarge {
		t.Errorf("code = %v, want %s", body["code"], api.CodePayloadTooLarge)
	}
	if body["limit_bytes"] != float64(1024) {
		t.Errorf("limit_bytes = %v, want 1024", body["limit_bytes"])
	}
}

// TestIndexAckReportsQueueState: in distributed mode "202 indexing" was a lie —
// the work is only queued, and the client needs the job id to follow it.
func TestIndexAckReportsQueueState(t *testing.T) {
	svc := &fakeService{
		repos: map[string]*repos.Repo{"r1": {ID: "r1"}},
		indexAck: &service.IndexAck{
			Status: "queued", Queued: true, JobID: "42",
			JobStatus: storage.JobStatusPending, Force: true,
		},
	}
	srv := newTestServer(t, svc, nil)

	resp := post(t, srv.URL+"/api/v1/repos/r1/index", []byte(`{"force":true}`))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var ack map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		t.Fatal(err)
	}
	if ack["status"] != "queued" || ack["job_id"] != "42" || ack["job_status"] != storage.JobStatusPending {
		t.Errorf("ack = %v, want the queued job's id and state", ack)
	}
}

// TestSyncStateReportsInFlightBatch: without these fields a client cannot tell
// "being applied" from "lost".
func TestSyncStateReportsInFlightBatch(t *testing.T) {
	svc := &fakeService{repos: map[string]*repos.Repo{"r1": {
		ID: "r1", Status: repos.StatusIndexing, LastCommit: "sha-1",
		PendingCommit: "sha-2", IndexedAt: 1234, LastError: "previous attempt failed",
	}}}
	srv := newTestServer(t, svc, nil)

	resp, err := http.Get(srv.URL + "/api/v1/repos/r1/sync-state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var st map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st["pending_commit"] != "sha-2" {
		t.Errorf("pending_commit = %v, want sha-2", st["pending_commit"])
	}
	if st["indexed_at"] != float64(1234) {
		t.Errorf("indexed_at = %v, want 1234", st["indexed_at"])
	}
	if st["last_error"] != "previous attempt failed" {
		t.Errorf("last_error = %v", st["last_error"])
	}
}

// TestCommitAckReportsQueueState: a queued batch is not "indexing" here, and
// the client needs the job id to follow it under /jobs.
func TestCommitAckReportsQueueState(t *testing.T) {
	svc := &fakeService{
		repos:     map[string]*repos.Repo{"r1": {ID: "r1", LastCommit: "sha-1"}},
		commitAck: &service.CommitAck{Accepted: true, Status: "queued", Queued: true, JobID: "42", Target: "sha-2", Before: "sha-1"},
	}
	srv := newTestServer(t, svc, nil)

	resp := post(t, srv.URL+"/api/v1/repos/r1/commits",
		[]byte(`{"commits":[{"sha":"sha-2","parents":["sha-1"],"files":[{"path":"a.go","status":"A","content":"x"}]}]}`))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var ack map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]any{
		"status": "queued", "queued": true, "job_id": "42",
		"pending_commit": "sha-2", "last_commit_before": "sha-1",
	} {
		if ack[field] != want {
			t.Errorf("ack %s = %v, want %v", field, ack[field], want)
		}
	}
}

// TestRepoJobsEndpoint: /index answers 202 whether the pass started or was
// only queued, so the queue itself has to be observable — otherwise "queued",
// "running on some instance" and "failed" all look the same to a client.
func TestRepoJobsEndpoint(t *testing.T) {
	svc := &fakeService{
		repos: map[string]*repos.Repo{"r1": {ID: "r1"}},
		jobs: []*storage.IndexJob{
			{ID: "2", RepoID: "r1", Kind: storage.JobKindCommits, Status: storage.JobStatusRunning,
				CreatedAt: 10, ClaimedAt: 11, HeartbeatAt: 12, ClaimedBy: "worker-a",
				Payload: `[{"sha":"sha-1"}]`},
			{ID: "1", RepoID: "r1", Kind: storage.JobKindIndex, Force: true,
				Status: storage.JobStatusError, Error: "boom", CreatedAt: 5},
		},
	}
	srv := newTestServer(t, svc, nil)

	resp, err := http.Get(srv.URL + "/api/v1/repos/r1/jobs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Jobs  []map[string]any `json:"jobs"`
		Total int              `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 2 || len(body.Jobs) != 2 {
		t.Fatalf("jobs = %d (total %d), want 2", len(body.Jobs), body.Total)
	}
	running := body.Jobs[0]
	for field, want := range map[string]any{
		"id": "2", "repo_id": "r1", "kind": storage.JobKindCommits,
		"status": storage.JobStatusRunning, "claimed_by": "worker-a",
		"created_at": float64(10), "claimed_at": float64(11), "heartbeat_at": float64(12),
	} {
		if running[field] != want {
			t.Errorf("job %s = %v, want %v", field, running[field], want)
		}
	}
	if _, ok := running["payload"]; ok {
		t.Error("the commit payload is serialized to clients; it can be tens of megabytes")
	}
	if failed := body.Jobs[1]; failed["error"] != "boom" || failed["force"] != true {
		t.Errorf("failed job = %v, want its error and force flag", failed)
	}

	if svc.lastJobsLimit != 50 {
		t.Errorf("default limit = %d, want 50", svc.lastJobsLimit)
	}
	if _, err := http.Get(srv.URL + "/api/v1/repos/r1/jobs?limit=5"); err != nil {
		t.Fatal(err)
	}
	if svc.lastJobsLimit != 5 {
		t.Errorf("limit = %d, want the requested 5", svc.lastJobsLimit)
	}

	bad, err := http.Get(srv.URL + "/api/v1/repos/r1/jobs?limit=nope")
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Errorf("bad limit = %d, want 400", bad.StatusCode)
	}
	if code := decodeError(t, bad)["code"]; code != api.CodeValidationFailed {
		t.Errorf("bad limit code = %v, want %s", code, api.CodeValidationFailed)
	}

	missing, err := http.Get(srv.URL + "/api/v1/repos/other/jobs")
	if err != nil {
		t.Fatal(err)
	}
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("jobs of an unknown repo = %d, want 404", missing.StatusCode)
	}
}

// TestRepoJobEndpoint follows the job_id a distributed /index hands out. A job
// of another repository is not readable through this repo's path.
func TestRepoJobEndpoint(t *testing.T) {
	svc := &fakeService{
		repos: map[string]*repos.Repo{"r1": {ID: "r1"}, "r2": {ID: "r2"}},
		jobs: []*storage.IndexJob{
			{ID: "7", RepoID: "r1", Kind: storage.JobKindIndex, Status: storage.JobStatusPending, CreatedAt: 3},
			{ID: "8", RepoID: "r2", Kind: storage.JobKindIndex, Status: storage.JobStatusPending, CreatedAt: 4},
		},
	}
	srv := newTestServer(t, svc, nil)

	resp, err := http.Get(srv.URL + "/api/v1/repos/r1/jobs/7")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var job map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	if job["id"] != "7" || job["status"] != storage.JobStatusPending {
		t.Errorf("job = %v, want job 7 pending", job)
	}

	foreign, err := http.Get(srv.URL + "/api/v1/repos/r1/jobs/8")
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Body.Close()
	if foreign.StatusCode != http.StatusNotFound {
		t.Errorf("job of another repo = %d, want 404", foreign.StatusCode)
	}
}

func TestResetRepoEndpoint(t *testing.T) {
	svc := &fakeService{repos: map[string]*repos.Repo{"r1": {ID: "r1", Status: repos.StatusIndexing}}}
	srv := newTestServer(t, svc, nil)

	resp := post(t, srv.URL+"/api/v1/repos/r1/reset", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var repo map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&repo); err != nil {
		t.Fatal(err)
	}
	if repo["status"] != string(repos.StatusIdle) {
		t.Errorf("status after reset = %v, want idle", repo["status"])
	}

	missing := post(t, srv.URL+"/api/v1/repos/other/reset", nil)
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("reset of an unknown repo = %d, want 404", missing.StatusCode)
	}
}

// TestGraphPathNoPathIs200: "no path exists" is an answer, and the spec
// documents it as a 200 with an empty steps array.
func TestGraphPathNoPathIs200(t *testing.T) {
	svc := &fakeService{pathErr: storage.ErrNotFound}
	srv := newTestServer(t, svc, nil)

	resp := post(t, srv.URL+"/api/v1/graph/path", []byte(`{"from_unit_id":"1","to_unit_id":"2"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	steps, ok := body["steps"].([]any)
	if !ok || len(steps) != 0 {
		t.Errorf("steps = %v, want an empty array", body["steps"])
	}
	if body["length"] != float64(0) {
		t.Errorf("length = %v, want 0", body["length"])
	}
}

// TestSymbolSearchRequiresSelector: with no parameters the endpoint used to
// dump the whole unit table.
func TestSymbolSearchRequiresSelector(t *testing.T) {
	svc := &fakeService{}
	srv := newTestServer(t, svc, nil)

	resp := post(t, srv.URL+"/api/v1/nav/symbol", []byte(`{}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if code := decodeError(t, resp)["code"]; code != api.CodeValidationFailed {
		t.Errorf("code = %v, want %s", code, api.CodeValidationFailed)
	}
}

// TestSymbolSearchPassesQualified: the filter is documented and was silently
// dropped by the handler.
func TestSymbolSearchPassesQualified(t *testing.T) {
	svc := &fakeService{}
	srv := newTestServer(t, svc, nil)

	resp := post(t, srv.URL+"/api/v1/nav/symbol", []byte(`{"qualified":"pkg.Foo"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if svc.lastSymbolOpts.Qualified != "pkg.Foo" {
		t.Errorf("qualified reached the service as %q, want pkg.Foo", svc.lastSymbolOpts.Qualified)
	}
}

// TestSymbolSearchPassesSymbolAsOneTerm: `symbol` is the field for a caller
// holding a single identifier out of a question and not knowing whether it is
// the bare or the qualified name. It has to reach the storage layer as the
// one-term selector, because name and qualified narrow together — sending the
// identifier as both would match only a symbol whose two names are equal.
func TestSymbolSearchPassesSymbolAsOneTerm(t *testing.T) {
	svc := &fakeService{}
	srv := newTestServer(t, svc, nil)

	resp := post(t, srv.URL+"/api/v1/nav/symbol", []byte(`{"symbol":"ShipOrder"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	opts := svc.lastSymbolOpts
	if opts.NameOrQualified != "ShipOrder" {
		t.Errorf("NameOrQualified = %q, want ShipOrder", opts.NameOrQualified)
	}
	if opts.Name != "" || opts.Qualified != "" {
		t.Errorf("symbol leaked into the narrowing filters: Name=%q Qualified=%q", opts.Name, opts.Qualified)
	}
}

// TestSymbolSearchAcceptsSymbolAlone: `symbol` counts as a selector, or the
// only field that answers "I have one identifier" would still be a 400.
func TestSymbolSearchAcceptsSymbolAlone(t *testing.T) {
	svc := &fakeService{}
	srv := newTestServer(t, svc, nil)

	resp := post(t, srv.URL+"/api/v1/nav/symbol", []byte(`{"symbol":"Handle"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestHealthAndReady(t *testing.T) {
	svc := &fakeService{}
	srv := newTestServer(t, svc, nil)

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/health = %d, want 200", resp.StatusCode)
	}

	ready, err := http.Get(srv.URL + "/ready")
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Body.Close()
	if ready.StatusCode != http.StatusOK {
		t.Errorf("/ready = %d, want 200", ready.StatusCode)
	}
}

// TestReadyFailsWhileHealthStays200: a dependency outage must not make an
// orchestrator restart otherwise live instances.
func TestReadyFailsWhileHealthStays200(t *testing.T) {
	svc := &fakeService{readyErr: fmt.Errorf("storage: connection refused")}
	srv := newTestServer(t, svc, nil)

	health, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Errorf("/health = %d, want 200 while a dependency is down", health.StatusCode)
	}

	ready, err := http.Get(srv.URL + "/ready")
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Body.Close()
	if ready.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/ready = %d, want 503", ready.StatusCode)
	}
	if code := decodeError(t, ready)["code"]; code != api.CodeNotReady {
		t.Errorf("code = %v, want %s", code, api.CodeNotReady)
	}
}

// --- webhook ---

func TestWebhookFailsClosedWithoutSecret(t *testing.T) {
	t.Setenv("RAGOTA_WEBHOOK_SECRET", "")
	svc := &fakeService{repos: map[string]*repos.Repo{"r1": {ID: "r1", Name: "gateway"}}}
	srv := newTestServer(t, svc, nil)

	resp := post(t, srv.URL+"/webhooks/git", []byte(`{"repository":{"name":"gateway"}}`))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when no secret is configured", resp.StatusCode)
	}
}

func TestWebhookRejectsWrongToken(t *testing.T) {
	t.Setenv("RAGOTA_WEBHOOK_SECRET", "s3cret")
	svc := &fakeService{repos: map[string]*repos.Repo{"r1": {ID: "r1", Name: "gateway"}}}
	srv := newTestServer(t, svc, nil)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/webhooks/git",
		bytes.NewReader([]byte(`{"repository":{"name":"gateway"}}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Webhook-Token", "wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestWebhookAcceptsCorrectToken(t *testing.T) {
	t.Setenv("RAGOTA_WEBHOOK_SECRET", "s3cret")
	svc := &fakeService{repos: map[string]*repos.Repo{"r1": {ID: "r1", Name: "gateway"}}}
	srv := newTestServer(t, svc, nil)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/webhooks/git",
		bytes.NewReader([]byte(`{"repository":{"name":"gateway"}}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Webhook-Token", "s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
}

// TestWebhookBodyIsCapped: registered outside /api/v1, the route used to skip
// the body cap entirely.
func TestWebhookBodyIsCapped(t *testing.T) {
	t.Setenv("RAGOTA_WEBHOOK_SECRET", "s3cret")
	t.Setenv("RAGOTA_MAX_BODY_BYTES", "512")
	svc := &fakeService{repos: map[string]*repos.Repo{"r1": {ID: "r1", Name: "gateway"}}}
	srv := newTestServer(t, svc, nil)

	payload, err := json.Marshal(map[string]any{
		"repository": map[string]any{"name": strings.Repeat("g", 4096)},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/webhooks/git", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Webhook-Token", "s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

// TestWebhookIsRateLimited: the route used to sit outside the limiter too.
func TestWebhookIsRateLimited(t *testing.T) {
	t.Setenv("RAGOTA_WEBHOOK_SECRET", "s3cret")
	svc := &fakeService{repos: map[string]*repos.Repo{"r1": {ID: "r1", Name: "gateway"}}}
	srv := newTestServer(t, svc, &config.ServerConfig{
		RateLimit: &config.RateLimitConfig{Enabled: true, RequestsPerMinute: 1, Burst: 1},
	})

	send := func() int {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/webhooks/git",
			bytes.NewReader([]byte(`{"repository":{"name":"gateway"}}`)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Webhook-Token", "s3cret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := send(); got != http.StatusAccepted {
		t.Fatalf("first webhook = %d, want 202", got)
	}
	if got := send(); got != http.StatusTooManyRequests {
		t.Errorf("second webhook = %d, want 429", got)
	}
}

// TestRateLimit429CarriesRetryAfter documents the header clients need.
func TestRateLimit429CarriesRetryAfter(t *testing.T) {
	svc := &fakeService{repos: map[string]*repos.Repo{}}
	srv := newTestServer(t, svc, &config.ServerConfig{
		RateLimit: &config.RateLimitConfig{Enabled: true, RequestsPerMinute: 1, Burst: 1},
	})

	if _, err := http.Get(srv.URL + "/api/v1/repos"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(srv.URL + "/api/v1/repos")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		t.Fatal("no Retry-After header on a 429")
	}
	if n, err := strconv.Atoi(ra); err != nil || n < 1 {
		t.Errorf("Retry-After = %q, want a positive integer", ra)
	}
	if code := decodeError(t, resp)["code"]; code != api.CodeRateLimited {
		t.Errorf("code = %v, want %s", code, api.CodeRateLimited)
	}
}

// TestRateLimitCannotBeBypassedWithRandomKeys is the bug itself: with auth
// disabled a client-supplied API key is not an identity, so rotating it must
// not mint a fresh bucket per request.
func TestRateLimitCannotBeBypassedWithRandomKeys(t *testing.T) {
	svc := &fakeService{repos: map[string]*repos.Repo{}}
	srv := newTestServer(t, svc, &config.ServerConfig{
		RateLimit: &config.RateLimitConfig{Enabled: true, RequestsPerMinute: 2, Burst: 2},
	})

	statuses := make([]int, 0, 5)
	for i := 0; i < 5; i++ {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/repos", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-API-Key", fmt.Sprintf("random-key-%d", i))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		statuses = append(statuses, resp.StatusCode)
	}

	limited := 0
	for _, s := range statuses {
		if s == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Errorf("statuses = %v, want the limit to apply despite a fresh key per request", statuses)
	}
}
