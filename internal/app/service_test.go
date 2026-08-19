package app

import (
	"context"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/index"
	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/search"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// mockStorage is a test double for store.Storage.
type mockStorage struct {
	mu          sync.Mutex
	initFunc    func(context.Context) error
	closeFunc   func() error
	filesByRepo map[string][]*store.File
	getFileErr  error
	// filesByRepoErr fails the snapshot an index pass takes of what is already
	// indexed.
	filesByRepoErr error
	// serveStoredFiles makes the file queries answer from the rows StoreFile
	// recorded, so a second pass can skip unchanged files the way a real store
	// does.
	serveStoredFiles bool
	deletedPaths     []string

	// contract coverage summaries written by an index pass
	coverage        *domain.RepoCoverage
	coverageDeleted []string

	// repo state, used by the tests that exercise claims and cursors
	repo          *domain.Repo
	repoList      []*domain.Repo
	getRepoErr    error
	claimOK       *bool // nil = claim succeeds
	claimCalls    int
	storedRepos   []*domain.Repo
	storedFiles   []*store.File
	lastCommit    string
	pendingCommit string
	statuses      []mockStatus
	resetCalls    []bool
	activeSets    [][]string
	listReposErr  error

	// job queue state
	job              *domain.IndexJob
	jobList          []*domain.IndexJob
	jobListLimits    []int
	commitPayloads   []string
	enqueueCommitErr error
	earlierCommitJob bool
	completed        []mockJobResult
	released         []string
}

type mockStatus struct {
	Status    domain.Status
	LastError string
}

type mockJobResult struct {
	JobID string
	Error string
}

func (m *mockStorage) storedPaths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.storedFiles))
	for _, f := range m.storedFiles {
		out = append(out, f.Path)
	}
	sort.Strings(out)
	return out
}

func (m *mockStorage) Init(ctx context.Context) error {
	if m.initFunc != nil {
		return m.initFunc(ctx)
	}
	return nil
}

func (m *mockStorage) Close() error {
	if m.closeFunc != nil {
		return m.closeFunc()
	}
	return nil
}

// StoreFile honours cancellation, unlike the rest of this fake, because the
// real backends do: a cancelled pass surfaces as "begin transaction: context
// canceled" from SQLite. A fake that ignores the context cannot be used to test
// what the process does when one is cancelled, which is a whole class of
// shutdown behaviour.
func (m *mockStorage) StoreFile(ctx context.Context, f *store.File) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.storedFiles = append(m.storedFiles, f)
	return nil
}
func (m *mockStorage) GetFile(_ context.Context, repoID, path string) (*store.File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getFileErr != nil {
		return nil, m.getFileErr
	}
	// Off by default: most tests want every pass to index everything. With it
	// on, the mock behaves like a real store and a second pass skips the files
	// the first one recorded.
	if !m.serveStoredFiles {
		return nil, nil
	}
	for _, f := range m.storedFiles {
		if f.RepoID == repoID && f.Path == path {
			cp := *f
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}
func (m *mockStorage) BatchStoreFiles(ctx context.Context, files []*store.File) error {
	for _, f := range files {
		if err := m.StoreFile(ctx, f); err != nil {
			return err
		}
	}
	return nil
}
func (m *mockStorage) GetFilesByRepo(ctx context.Context, repoID string) ([]*store.File, error) {
	if m.filesByRepoErr != nil {
		return nil, m.filesByRepoErr
	}
	out := m.filesByRepo[repoID]
	if !m.serveStoredFiles {
		return out, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.storedFiles {
		if f.RepoID == repoID {
			cp := *f
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (m *mockStorage) DeleteFile(context.Context, string, string) error { return nil }
func (m *mockStorage) DeleteFilesByPaths(_ context.Context, _ string, paths []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletedPaths = append(m.deletedPaths, paths...)
	return nil
}
func (m *mockStorage) DeleteFilesByRepo(context.Context, string) error     { return nil }
func (m *mockStorage) StoreASTUnit(context.Context, *domain.ASTUnit) error { return nil }
func (m *mockStorage) StoreRepo(_ context.Context, r *domain.Repo) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *r
	m.storedRepos = append(m.storedRepos, &cp)
	return nil
}
func (m *mockStorage) GetRepo(context.Context, string) (*domain.Repo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getRepoErr != nil {
		return nil, m.getRepoErr
	}
	if m.repo != nil {
		cp := *m.repo
		return &cp, nil
	}
	return &domain.Repo{ID: "repo-1", Name: "test", Source: domain.SourceTypeLocal}, nil
}
func (m *mockStorage) ListRepos(context.Context) ([]*domain.Repo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.repoList, m.listReposErr
}
func (m *mockStorage) ListActiveRepos(context.Context) ([]*domain.Repo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*domain.Repo, 0, len(m.repoList))
	for _, r := range m.repoList {
		if r.Active {
			out = append(out, r)
		}
	}
	return out, m.listReposErr
}

// SetActiveRepos records the sets it was asked for and applies them to
// repoList, so a test can both assert on the call and read the result back
// through ListRepos the way the service does.
func (m *mockStorage) SetActiveRepos(_ context.Context, ids []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activeSets = append(m.activeSets, ids)
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	for _, r := range m.repoList {
		r.Active = wanted[r.ID]
	}
	return nil
}
func (m *mockStorage) DeleteRepo(context.Context, string) error { return nil }
func (m *mockStorage) UpdateRepoStatus(_ context.Context, _ string, st domain.Status, lastErr string, _ int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statuses = append(m.statuses, mockStatus{Status: st, LastError: lastErr})
	m.pendingCommit = ""
	return nil
}
func (m *mockStorage) GetASTUnits(context.Context, domain.QueryOpts) ([]*domain.ASTUnit, error) {
	return nil, nil
}
func (m *mockStorage) DeleteASTUnitsByFile(context.Context, string, string) error { return nil }
func (m *mockStorage) DeleteASTUnitsByFiles(context.Context, string, []string) error {
	return nil
}
func (m *mockStorage) DeleteASTUnitsByRepo(context.Context, string) error { return nil }
func (m *mockStorage) StoreEdge(context.Context, *domain.Edge) error      { return nil }
func (m *mockStorage) GetEdges(context.Context, domain.QueryOpts) ([]*domain.Edge, error) {
	return nil, nil
}
func (m *mockStorage) DeleteEdgesByFile(context.Context, string, string) error    { return nil }
func (m *mockStorage) DeleteEdgesByFiles(context.Context, string, []string) error { return nil }
func (m *mockStorage) DeleteEdgesByRepo(context.Context, string) error            { return nil }
func (m *mockStorage) VectorStore() store.VectorStorage                           { return nil }
func (m *mockStorage) CountASTUnitsByRepo(context.Context, string) (int64, error) {
	return 0, nil
}
func (m *mockStorage) CountASTUnits(context.Context) (int64, error) { return 0, nil }
func (m *mockStorage) DeleteASTUnitsByKind(context.Context, string, string) error {
	return nil
}
func (m *mockStorage) GetASTUnitByID(context.Context, string) (*domain.ASTUnit, error) {
	return nil, store.ErrNotFound
}
func (m *mockStorage) UpdateEdgeResolution(context.Context, string, string, string, float32) error {
	return nil
}
func (m *mockStorage) UpdateEdgeDstName(context.Context, string, string) error { return nil }
func (m *mockStorage) UpdateEdgeMeta(context.Context, string, string) error    { return nil }
func (m *mockStorage) DeleteEdgesByKind(context.Context, string, string) error { return nil }
func (m *mockStorage) GetASTUnitsByIDs(context.Context, []string) ([]*domain.ASTUnit, error) {
	return nil, nil
}
func (m *mockStorage) BatchStoreASTUnits(context.Context, []*domain.ASTUnit) error { return nil }
func (m *mockStorage) BatchStoreEdges(context.Context, []*domain.Edge) error       { return nil }
func (m *mockStorage) ClaimRepoForIndexing(context.Context, string, string, int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.claimCalls++
	if m.claimOK != nil {
		return *m.claimOK, nil
	}
	return true, nil
}
func (m *mockStorage) ResetStuckRepos(_ context.Context, force bool) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resetCalls = append(m.resetCalls, force)
	return 0, nil
}
func (m *mockStorage) SetRepoPendingCommit(_ context.Context, _, sha string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pendingCommit = sha
	return nil
}
func (m *mockStorage) UpdateRepoLastCommit(_ context.Context, _, sha string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastCommit = sha
	return nil
}
func (m *mockStorage) DeleteEdgesByKindAndDst(context.Context, string, string) error { return nil }
func (m *mockStorage) StoreRepoCoverage(_ context.Context, c *domain.RepoCoverage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *c
	m.coverage = &cp
	return nil
}
func (m *mockStorage) GetRepoCoverage(_ context.Context, repoID string) (*domain.RepoCoverage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.coverage == nil || m.coverage.RepoID != repoID {
		return nil, store.ErrNotFound
	}
	cp := *m.coverage
	return &cp, nil
}
func (m *mockStorage) DeleteRepoCoverage(_ context.Context, repoID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.coverageDeleted = append(m.coverageDeleted, repoID)
	return nil
}
func (m *mockStorage) EnqueueIndexJob(_ context.Context, repoID string, force bool) (*domain.IndexJob, error) {
	return &domain.IndexJob{
		ID: "job-1", RepoID: repoID, Kind: domain.JobKindIndex, Force: force,
		Status: domain.JobStatusPending, CreatedAt: 7,
	}, nil
}
func (m *mockStorage) EnqueueCommitJob(_ context.Context, repoID, payload string) (*domain.IndexJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.enqueueCommitErr != nil {
		return nil, m.enqueueCommitErr
	}
	m.commitPayloads = append(m.commitPayloads, payload)
	return &domain.IndexJob{
		ID: "commit-job-1", RepoID: repoID, Kind: domain.JobKindCommits,
		Status: domain.JobStatusPending, CreatedAt: 8,
	}, nil
}
func (m *mockStorage) GetIndexJob(_ context.Context, jobID string) (*domain.IndexJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job != nil {
		cp := *m.job
		return &cp, nil
	}
	return &domain.IndexJob{ID: jobID, Kind: domain.JobKindIndex, Status: domain.JobStatusPending}, nil
}
func (m *mockStorage) ListIndexJobs(_ context.Context, repoID string, limit int) ([]*domain.IndexJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobListLimits = append(m.jobListLimits, limit)
	return m.jobList, nil
}
func (m *mockStorage) HasPendingCommitJobBefore(context.Context, string, string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.earlierCommitJob, nil
}
func (m *mockStorage) ClaimNextIndexJob(context.Context, string) (*domain.IndexJob, error) {
	return nil, store.ErrNotFound
}
func (m *mockStorage) HeartbeatIndexJob(context.Context, string, string) error { return nil }
func (m *mockStorage) CompleteIndexJob(_ context.Context, jobID, _ string, jobErr string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.completed = append(m.completed, mockJobResult{JobID: jobID, Error: jobErr})
	return nil
}
func (m *mockStorage) ReleaseIndexJob(_ context.Context, jobID, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.released = append(m.released, jobID)
	return nil
}
func (m *mockStorage) RequeueStaleIndexJobs(context.Context, int64) (int, error) {
	return 0, nil
}

// mockIndexer is a test double for index.Indexer.
type mockIndexer struct {
	initCalled  bool
	closeCalled bool
	name        string
	indexType   index.IndexType
	// indexFunc overrides the default success result.
	indexFunc func(*index.IndexRequest) (*index.IndexResult, error)
}

func (m *mockIndexer) Name() string          { return m.name }
func (m *mockIndexer) Type() index.IndexType { return m.indexType }
func (m *mockIndexer) Init(ctx context.Context, config map[string]interface{}) error {
	m.initCalled = true
	return nil
}
func (m *mockIndexer) Index(_ context.Context, req *index.IndexRequest) (*index.IndexResult, error) {
	if m.indexFunc != nil {
		return m.indexFunc(req)
	}
	return &index.IndexResult{FilesIndexed: len(req.Files)}, nil
}
func (m *mockIndexer) Remove(context.Context, string, []string) error { return nil }
func (m *mockIndexer) Stats(context.Context) (*index.IndexerStats, error) {
	return &index.IndexerStats{}, nil
}
func (m *mockIndexer) Close() error {
	m.closeCalled = true
	return nil
}

// mockSource is a test double for repos.RepoSource.
type mockSource struct {
	name    domain.SourceType
	addFunc func(context.Context, *domain.AddRequest) (*domain.Repo, error)
}

func (m *mockSource) Name() string                                                  { return string(m.name) }
func (m *mockSource) Type() domain.SourceType                                       { return m.name }
func (m *mockSource) Init(ctx context.Context, config map[string]interface{}) error { return nil }
func (m *mockSource) Add(ctx context.Context, req *domain.AddRequest) (*domain.Repo, error) {
	if m.addFunc != nil {
		return m.addFunc(ctx, req)
	}
	return &domain.Repo{ID: "test-repo", Name: req.Name, Source: m.name}, nil
}
func (m *mockSource) Remove(context.Context, string) error       { return nil }
func (m *mockSource) Update(context.Context, *domain.Repo) error { return nil }
func (m *mockSource) GetFiles(context.Context, *domain.Repo, []string) ([]*domain.RepoFile, error) {
	return nil, nil
}
func (m *mockSource) Clean(context.Context, *domain.Repo) error { return nil }
func (m *mockSource) Close() error                              { return nil }

func TestNew(t *testing.T) {
	t.Run("creates service with all components", func(t *testing.T) {
		cfg := &config.Config{}
		stor := &mockStorage{}
		indexers := map[index.IndexType]index.Indexer{
			index.IndexTypeAST: &mockIndexer{name: "ast", indexType: index.IndexTypeAST},
		}
		sources := map[domain.SourceType]repos.RepoSource{
			domain.SourceTypeLocal: &mockSource{name: domain.SourceTypeLocal},
		}

		svc := New(cfg, stor, indexers, sources, nil)
		if svc == nil {
			t.Fatal("service is nil")
		}
		if err := svc.Close(context.Background()); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	t.Run("closes all components", func(t *testing.T) {
		ctx := context.Background()
		stor := &mockStorage{}
		idx := &mockIndexer{name: "test", indexType: index.IndexTypeCustom}
		src := &mockSource{name: domain.SourceTypeLocal}

		svc := New(&config.Config{}, stor,
			map[index.IndexType]index.Indexer{index.IndexTypeCustom: idx},
			map[domain.SourceType]repos.RepoSource{domain.SourceTypeLocal: src}, nil)

		if err := svc.Close(ctx); err != nil {
			t.Fatalf("Close() error = %v", err)
		}

		if !idx.closeCalled {
			t.Error("indexer Close was not called")
		}

	})
}

func TestAddRepo(t *testing.T) {
	ctx := context.Background()
	// No stored row yet: this is a first registration, so AddRepo owns the
	// initial lifecycle state (see TestAddRepoPreservesExistingState for the
	// re-registration case).
	stor := &mockStorage{getRepoErr: store.ErrNotFound}
	src := &mockSource{
		name: domain.SourceTypeLocal,
		addFunc: func(ctx context.Context, req *domain.AddRequest) (*domain.Repo, error) {
			return &domain.Repo{ID: "repo-1", Name: req.Name, Source: domain.SourceTypeLocal}, nil
		},
	}

	svc := New(&config.Config{}, stor, nil,
		map[domain.SourceType]repos.RepoSource{domain.SourceTypeLocal: src}, nil)

	repo, err := svc.AddRepo(ctx, domain.SourceTypeLocal, &domain.AddRequest{
		Name: "my-repo", Path: "/path/to/repo", URL: "", Branch: "",
	})
	if err != nil {
		t.Fatalf("AddRepo() error = %v", err)
	}

	if repo.ID != "repo-1" {
		t.Errorf("repo.ID = %s, want repo-1", repo.ID)
	}
	if repo.CreatedAt <= 0 {
		t.Errorf("repo.CreatedAt = %d, want > 0", repo.CreatedAt)
	}

	t.Run("invalid source type", func(t *testing.T) {
		_, err := svc.AddRepo(ctx, "nonexistent", &domain.AddRequest{
			Name: "my-repo", Path: "/path", URL: "", Branch: "",
		})
		if err == nil {
			t.Error("expected error for invalid source type")
		}
	})
}

func TestDeleteRepo(t *testing.T) {
	ctx := context.Background()
	stor := &mockStorage{
		filesByRepo: map[string][]*store.File{
			"repo-1": {{Path: "/test.go"}},
		},
	}
	idx := &mockIndexer{name: "test", indexType: index.IndexTypeCustom}
	src := &mockSource{name: domain.SourceTypeLocal}

	svc := New(&config.Config{}, stor,
		map[index.IndexType]index.Indexer{index.IndexTypeCustom: idx},
		map[domain.SourceType]repos.RepoSource{domain.SourceTypeLocal: src}, nil)

	err := svc.DeleteRepo(ctx, "repo-1")
	if err != nil {
		t.Fatalf("DeleteRepo() error = %v", err)
	}
}

func TestIndexRepo(t *testing.T) {
	ctx := context.Background()
	stor := &mockStorage{
		filesByRepo: map[string][]*store.File{
			"repo-1": {{Path: "test.go", Hash: "abc123"}},
		},
	}
	idx := &mockIndexer{name: "test", indexType: index.IndexTypeCustom}
	src := &mockSource{
		name: domain.SourceTypeLocal,
		addFunc: func(ctx context.Context, req *domain.AddRequest) (*domain.Repo, error) {
			return &domain.Repo{ID: "repo-1", Name: req.Name, Path: "/path/to/repo"}, nil
		},
	}

	svc := New(&config.Config{}, stor,
		map[index.IndexType]index.Indexer{index.IndexTypeCustom: idx},
		map[domain.SourceType]repos.RepoSource{domain.SourceTypeLocal: src}, nil)

	// First add the repo
	repo, err := svc.AddRepo(ctx, domain.SourceTypeLocal, &domain.AddRequest{
		Name: "my-repo", Path: "/path/to/repo", URL: "", Branch: "",
	})
	if err != nil {
		t.Fatalf("AddRepo() error = %v", err)
	}

	err = svc.IndexRepo(ctx, repo.ID, false)
	if err != nil {
		t.Fatalf("IndexRepo() error = %v", err)
	}
}

// TestServiceWithSQLite tests service with real SQLite store.
func TestServiceWithSQLite(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	cfg := &config.Config{
		Storage: config.StorageConfig{
			SQLite: &config.SQLiteStorageConfig{
				Path:     dbPath,
				PoolSize: 1,
			},
		},
	}

	ctx := context.Background()

	// We can't fully test without the actual sqlite package being importable
	// in this test context, so we just verify the service structure works
	stor := &mockStorage{filesByRepo: map[string][]*store.File{}}

	svc := New(cfg, stor, nil, nil, search.New(nil, search.DefaultConfig()))

	if svc == nil {
		t.Fatal("service is nil")
	}

	// Verify context lifecycle
	if svc.baseCtx == nil {
		t.Error("base context is nil")
	}

	svc.Close(ctx)

	// Context should be cancelled after close
	select {
	case <-svc.baseCtx.Done():
		// Expected
	default:
		t.Error("base context was not cancelled")
	}
}

func TestResolveWorkers(t *testing.T) {
	def := runtime.NumCPU()
	if def > maxIndexWorkers {
		def = maxIndexWorkers
	}

	if got := resolveWorkers(0); got != def {
		t.Errorf("resolveWorkers(0) = %d, want %d (NumCPU capped at %d)", got, def, maxIndexWorkers)
	}
	if got := resolveWorkers(1); got != 1 {
		t.Errorf("resolveWorkers(1) = %d, want 1", got)
	}
	if got := resolveWorkers(8); got != 8 {
		t.Errorf("resolveWorkers(8) = %d, want 8", got)
	}
	if got := resolveWorkers(100); got != maxIndexWorkers {
		t.Errorf("resolveWorkers(100) = %d, want %d", got, maxIndexWorkers)
	}
	// Negative values are rejected by config validation
	// (indexes.workers must not be negative); the helper still degrades
	// gracefully to the default.
	if got := resolveWorkers(-1); got != def {
		t.Errorf("resolveWorkers(-1) = %d, want %d", got, def)
	}
}

func TestServiceIndexWorkers(t *testing.T) {
	// nil config falls back to the default (NumCPU capped at 32).
	svc := &Service{}
	def := runtime.NumCPU()
	if def > maxIndexWorkers {
		def = maxIndexWorkers
	}
	if got := svc.indexWorkers(); got != def {
		t.Errorf("indexWorkers() with nil cfg = %d, want %d", got, def)
	}

	svc = &Service{cfg: &config.Config{Indexes: config.IndexesConfig{Workers: 4}}}
	if got := svc.indexWorkers(); got != 4 {
		t.Errorf("indexWorkers() with Workers=4 = %d, want 4", got)
	}
}
