package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/app"
	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/graph"
	"github.com/Nahua-Foundation/ragota/internal/index"
	"github.com/Nahua-Foundation/ragota/internal/server/api"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

type fakeService struct {
	repos          map[string]*domain.Repo
	addErr         error
	getErr         error
	failWith       error
	indexErr       error
	indexAck       *app.IndexAck
	readyErr       error
	searchRes      *index.SearchResult
	searchErr      error
	pathErr        error
	lastSymbolOpts domain.QueryOpts
	commitsErr     error
	commitsOK      *bool
	commitAck      *app.CommitAck
	lastCommits    []app.CommitEvent
	jobs           []*domain.IndexJob
	lastJobsLimit  int
	coverage       *app.CoverageReport
	contextRes     *app.ContextResult
	services       []*graph.ServiceInfo
	serviceLinks   []*graph.ServiceLink
	compacted      bool
}

func (f *fakeService) AddRepo(ctx context.Context, src domain.SourceType, req *domain.AddRequest) (*domain.Repo, error) {
	if f.addErr != nil {
		return nil, f.addErr
	}
	r := &domain.Repo{ID: "r1", Name: req.Name, Source: src, Path: req.Path, Status: domain.StatusIdle}
	if f.repos == nil {
		f.repos = map[string]*domain.Repo{}
	}
	f.repos[r.ID] = r
	return r, nil
}

func (f *fakeService) ListRepos(ctx context.Context) ([]*domain.Repo, error) {
	if f.failWith != nil {
		return nil, f.failWith
	}
	var out []*domain.Repo
	for _, r := range f.repos {
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeService) GetRepo(ctx context.Context, id string) (*domain.Repo, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	r, ok := f.repos[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return r, nil
}

func (f *fakeService) DeleteRepo(ctx context.Context, id string) error {
	delete(f.repos, id)
	return nil
}

func (f *fakeService) StartIndex(ctx context.Context, id string, force bool) (*app.IndexAck, error) {
	if f.indexErr != nil {
		return nil, f.indexErr
	}
	if f.indexAck != nil {
		return f.indexAck, nil
	}
	return &app.IndexAck{Status: "indexing", Force: force, RepoStatus: "indexing"}, nil
}

func (f *fakeService) ResetRepo(ctx context.Context, id string) (*domain.Repo, error) {
	r, ok := f.repos[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	r.Status = domain.StatusIdle
	return r, nil
}

func (f *fakeService) Ready(ctx context.Context) error { return f.readyErr }

// Search mirrors the real service in the one respect the handler depends on:
// it resolves an empty mode to the default before reporting which one ran.
func (f *fakeService) Search(ctx context.Context, q *index.SearchQuery, mode string) (*index.SearchResult, error) {
	if q.Query == "" {
		return nil, nil
	}
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	if mode == "" {
		mode = app.SearchModeHybrid
	}
	if f.searchRes != nil {
		f.searchRes.Mode = mode
		return f.searchRes, nil
	}
	return &index.SearchResult{Hits: []*index.Hit{}, Total: 0, Query: q.Query, Mode: mode, Duration: 0}, nil
}

func (f *fakeService) Symbols(ctx context.Context, opts domain.QueryOpts) ([]*domain.ASTUnit, error) {
	f.lastSymbolOpts = opts
	return nil, nil
}

func (f *fakeService) Definition(ctx context.Context, repoID, filePath string, line int) (*domain.ASTUnit, error) {
	return nil, nil
}

func (f *fakeService) References(ctx context.Context, repoID, filePath string, line, limit int) ([]*domain.Edge, error) {
	return nil, nil
}

func (f *fakeService) Stats(ctx context.Context) (map[string]*index.IndexerStats, error) {
	return nil, nil
}

func (f *fakeService) GraphNeighbors(ctx context.Context, unitID string) (*graph.NeighborsResult, error) {
	return &graph.NeighborsResult{}, nil
}

func (f *fakeService) GraphPath(ctx context.Context, fromID, toID string, maxDepth int) ([]*graph.PathStep, error) {
	if f.pathErr != nil {
		return nil, f.pathErr
	}
	return nil, nil
}

func (f *fakeService) GraphTrace(ctx context.Context, req *graph.TraceRequest) (*graph.TraceResult, error) {
	return &graph.TraceResult{Param: req.Param}, nil
}

func (f *fakeService) ServicesGraph(ctx context.Context) ([]*graph.ServiceInfo, []*graph.ServiceLink, error) {
	return f.services, f.serviceLinks, nil
}

func (f *fakeService) Topics(ctx context.Context, service string) ([]*graph.TopicInfo, error) {
	return nil, nil
}

func (f *fakeService) BuildContext(ctx context.Context, query string, repos []string, mode string, limit, hops int, intent string) (*app.ContextResult, error) {
	if f.contextRes != nil {
		return f.contextRes, nil
	}
	return &app.ContextResult{Query: query, Mode: mode}, nil
}

func (f *fakeService) SyncRepoAsync(ctx context.Context, repoID string, force bool) error {
	return nil
}

func (f *fakeService) FindRepoByHints(ctx context.Context, hints []string) (*domain.Repo, error) {
	for _, r := range f.repos {
		return r, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeService) IngestRuntimeServiceGraph(ctx context.Context, edges []app.RuntimeServiceEdge) (*app.RuntimeIngestResult, error) {
	return &app.RuntimeIngestResult{Received: len(edges), Stored: len(edges)}, nil
}

func (f *fakeService) ApplyCommits(ctx context.Context, repoID string, commits []app.CommitEvent) (*app.CommitAck, error) {
	f.lastCommits = commits
	if f.commitsErr != nil {
		return nil, f.commitsErr
	}
	before := ""
	if r, ok := f.repos[repoID]; ok {
		before = r.LastCommit
	}
	if f.commitsOK != nil && !*f.commitsOK {
		return &app.CommitAck{Accepted: false, Before: before}, nil
	}
	if f.commitAck != nil {
		return f.commitAck, nil
	}
	return &app.CommitAck{
		Accepted: true, Status: "indexing",
		Target: commits[len(commits)-1].SHA, Before: before,
	}, nil
}

func (f *fakeService) RepoCoverage(ctx context.Context, repoID string) (*app.CoverageReport, error) {
	if _, ok := f.repos[repoID]; !ok {
		return nil, store.ErrNotFound
	}
	if f.coverage != nil {
		return f.coverage, nil
	}
	return &app.CoverageReport{RepoID: repoID, Kinds: []app.CoverageKind{}}, nil
}

func (f *fakeService) RepoJobs(ctx context.Context, repoID string, limit int) ([]*domain.IndexJob, error) {
	if _, ok := f.repos[repoID]; !ok {
		return nil, store.ErrNotFound
	}
	f.lastJobsLimit = limit
	return f.jobs, nil
}

func (f *fakeService) RepoJob(ctx context.Context, repoID, jobID string) (*domain.IndexJob, error) {
	for _, j := range f.jobs {
		if j.ID == jobID && j.RepoID == repoID {
			return j, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeService) CompactIndexes(ctx context.Context) map[string]int64 {
	f.compacted = true
	return map[string]int64{"bm25": 1}
}

func newTestServer(t *testing.T, svc *fakeService, cfg *config.ServerConfig, opts ...api.Option) *httptest.Server {
	t.Helper()
	if cfg == nil {
		cfg = &config.ServerConfig{}
	}
	srv := httptest.NewServer(api.NewServer(svc, cfg, opts...).Router())
	t.Cleanup(srv.Close)
	return srv
}

func TestAddRepo_Valid(t *testing.T) {
	svc := &fakeService{repos: map[string]*domain.Repo{}}
	srv := newTestServer(t, svc, nil)

	reqBody := map[string]interface{}{"name": "test", "source": "local", "path": "/tmp/test"}
	body, _ := json.Marshal(reqBody)
	resp, err := http.Post(srv.URL+"/api/v1/repos", "application/json", bytes.NewBuffer(body))
	if err != nil {
		t.Fatalf("POST /api/v1/repos failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("expected status 201, got %d", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if _, ok := result["id"]; !ok {
		t.Error("expected id in response")
	}
}

func TestAddRepo_BadJSON(t *testing.T) {
	svc := &fakeService{}
	srv := newTestServer(t, svc, nil)

	resp, err := http.Post(srv.URL+"/api/v1/repos", "application/json", bytes.NewBufferString("{invalid json"))
	if err != nil {
		t.Fatalf("POST /api/v1/repos failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400 for bad JSON, got %d", resp.StatusCode)
	}
}

func TestAddRepo_MissingName(t *testing.T) {
	svc := &fakeService{}
	srv := newTestServer(t, svc, nil)

	reqBody := map[string]interface{}{"source": "local", "path": "/tmp/test"}
	body, _ := json.Marshal(reqBody)
	resp, err := http.Post(srv.URL+"/api/v1/repos", "application/json", bytes.NewBuffer(body))
	if err != nil {
		t.Fatalf("POST /api/v1/repos failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400 for missing name, got %d", resp.StatusCode)
	}
}

func TestGetRepo_NotFound(t *testing.T) {
	svc := &fakeService{repos: map[string]*domain.Repo{}, getErr: store.ErrNotFound}
	srv := newTestServer(t, svc, nil)

	resp, err := http.Get(srv.URL + "/api/v1/repos/nonexistent")
	if err != nil {
		t.Fatalf("GET /api/v1/repos/nonexistent failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected status 404 for missing repo, got %d", resp.StatusCode)
	}
}

func TestGetRepo_InternalError(t *testing.T) {
	svc := &fakeService{failWith: &fakeError{}, repos: map[string]*domain.Repo{}, getErr: &fakeError{}}
	srv := newTestServer(t, svc, nil)

	resp, err := http.Get(srv.URL + "/api/v1/repos/test")
	if err != nil {
		t.Fatalf("GET /api/v1/repos/test failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected status 500 for internal error, got %d", resp.StatusCode)
	}

	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if result["error"] != "internal error" {
		t.Errorf("expected error message 'internal error', got '%s'", result["error"])
	}
}

type fakeError struct{}

func (f *fakeError) Error() string { return "test error" }

func TestIndexRepo_Returns202(t *testing.T) {
	svc := &fakeService{repos: map[string]*domain.Repo{}}
	srv := newTestServer(t, svc, nil)

	resp, err := http.Post(srv.URL+"/api/v1/repos/r1/index", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/v1/repos/r1/index failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("expected status 202, got %d", resp.StatusCode)
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if result["status"] != "indexing" {
		t.Errorf("expected status 'indexing', got '%v'", result["status"])
	}
}

func TestSearch_EmptyQuery(t *testing.T) {
	svc := &fakeService{}
	srv := newTestServer(t, svc, nil)

	reqBody := map[string]interface{}{"query": "", "mode": "hybrid"}
	body, _ := json.Marshal(reqBody)
	resp, err := http.Post(srv.URL+"/api/v1/search", "application/json", bytes.NewBuffer(body))
	if err != nil {
		t.Fatalf("POST /api/v1/search failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400 for empty query, got %d", resp.StatusCode)
	}
}

// TestSearch_DamagedIndex pins the answer a query gets when the index cannot be
// read. It used to be an anonymous 500, which put a broken index in the same
// bucket as an ordinary fault and left a caller measuring retrieval unable to
// tell a zero caused by a dead index from one caused by bad ranking.
func TestSearch_DamagedIndex(t *testing.T) {
	svc := &fakeService{
		repos:     map[string]*domain.Repo{},
		searchErr: fmt.Errorf("all searchers failed: %w", index.ErrIndexDamaged),
	}
	srv := newTestServer(t, svc, nil)

	body, _ := json.Marshal(map[string]interface{}{"query": "handler", "mode": "keyword"})
	resp, err := http.Post(srv.URL+"/api/v1/search", "application/json", bytes.NewBuffer(body))
	if err != nil {
		t.Fatalf("POST /api/v1/search failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 for a damaged index", resp.StatusCode)
	}

	var result api.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.Code != api.CodeIndexDamaged {
		t.Errorf("code = %q, want %q", result.Code, api.CodeIndexDamaged)
	}
	if !strings.Contains(result.Error, "reindex") {
		t.Errorf("error does not name the repair: %q", result.Error)
	}
}

func TestRepoCoverage(t *testing.T) {
	svc := &fakeService{
		repos: map[string]*domain.Repo{"r1": {ID: "r1", Name: "n8n"}},
		coverage: &app.CoverageReport{
			RepoID:    "r1",
			Reported:  true,
			UpdatedAt: 1700000000,
			Kinds: []app.CoverageKind{
				{Kind: "http", Candidates: 3000, Edges: 104, Ratio: 104.0 / 3000.0},
			},
			Totals: app.CoverageKind{Kind: "all", Candidates: 3000, Edges: 104},
		},
	}
	srv := newTestServer(t, svc, nil)

	resp, err := http.Get(srv.URL + "/api/v1/repos/r1/coverage")
	if err != nil {
		t.Fatalf("GET coverage failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if got["reported"] != true {
		t.Errorf("reported = %v, want true", got["reported"])
	}
	kinds, ok := got["kinds"].([]any)
	if !ok || len(kinds) != 1 {
		t.Fatalf("kinds = %v, want one entry", got["kinds"])
	}
	http0 := kinds[0].(map[string]any)
	if http0["candidates"] != float64(3000) || http0["edges"] != float64(104) {
		t.Errorf("http entry = %v, want 104 of 3000", http0)
	}
}

func TestRepoCoverage_UnknownRepo(t *testing.T) {
	svc := &fakeService{repos: map[string]*domain.Repo{}}
	srv := newTestServer(t, svc, nil)

	resp, err := http.Get(srv.URL + "/api/v1/repos/nope/coverage")
	if err != nil {
		t.Fatalf("GET coverage failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected status 404 for an unknown repo, got %d", resp.StatusCode)
	}
}
