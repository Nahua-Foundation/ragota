package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/graph"
	"github.com/Nahua-Foundation/ragota/internal/indexing"
	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/service/enrich"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// newTestService builds a Service without going through New, so the tests are
// not affected by the background poller.
func newTestService(st storage.Storage, indexers map[indexing.IndexType]indexing.Indexer) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	linker := graph.NewLinker(st)
	return &Service{
		cfg:      &config.Config{},
		storage:  st,
		indexers: indexers,
		graph:    graph.New(st),
		linker:   linker,
		// Built like the real constructor does: the index pass reads it (recon
		// hints, the summary budget), so a Service without one is not a Service
		// this fixture can drive.
		enrich:     enrich.New(st, indexers, linker),
		baseCtx:    ctx,
		cancelBase: cancel,
	}
}

// TestIndexFileSetSkipsFailedFiles is the core of the silent-data-loss bug: a
// file an indexer failed on must not get a file row, because the stored hash
// is what makes every later non-forced pass skip it.
func TestIndexFileSetSkipsFailedFiles(t *testing.T) {
	st := &mockStorage{}
	idx := &mockIndexer{
		name: "vector", indexType: indexing.IndexTypeVector,
		indexFunc: func(req *indexing.IndexRequest) (*indexing.IndexResult, error) {
			return &indexing.IndexResult{
				FilesIndexed: len(req.Files) - 1,
				FilesFailed:  1,
				Errors:       []string{"broken.go: embed: connection refused"},
			}, nil
		},
	}
	svc := newTestService(st, map[indexing.IndexType]indexing.Indexer{indexing.IndexTypeVector: idx})

	toIndex := []*indexing.FileToIndex{
		{Path: "ok.go", Hash: "h1", Language: "go"},
		{Path: "broken.go", Hash: "h2", Language: "go"},
	}
	processed := []*storage.File{
		{RepoID: "r1", Path: "ok.go", Hash: "h1", Indexed: true},
		{RepoID: "r1", Path: "broken.go", Hash: "h2", Indexed: true},
	}

	failed, err := svc.indexFileSet(context.Background(), &repos.Repo{ID: "r1"}, toIndex, processed, false, nil, nil)
	if err != nil {
		t.Fatalf("indexFileSet() error = %v", err)
	}
	if !failed["broken.go"] {
		t.Errorf("failed set = %v, want broken.go", failed)
	}
	if failed["ok.go"] {
		t.Errorf("ok.go was marked failed: %v", failed)
	}

	got := st.storedPaths()
	if len(got) != 1 || got[0] != "ok.go" {
		t.Errorf("stored file rows = %v, want only ok.go", got)
	}
}

// TestIndexFileSetUnattributedFailureFailsBatch: if a failure cannot be pinned
// on a file, no file may be marked indexed — guessing risks the permanent loss
// the attribution exists to prevent.
func TestIndexFileSetUnattributedFailureFailsBatch(t *testing.T) {
	st := &mockStorage{}
	idx := &mockIndexer{
		name: "vector", indexType: indexing.IndexTypeVector,
		indexFunc: func(*indexing.IndexRequest) (*indexing.IndexResult, error) {
			return &indexing.IndexResult{FilesFailed: 1, Errors: []string{"batch upsert rejected"}}, nil
		},
	}
	svc := newTestService(st, map[indexing.IndexType]indexing.Indexer{indexing.IndexTypeVector: idx})

	toIndex := []*indexing.FileToIndex{{Path: "a.go"}, {Path: "b.go"}}
	processed := []*storage.File{{RepoID: "r1", Path: "a.go"}, {RepoID: "r1", Path: "b.go"}}

	failed, err := svc.indexFileSet(context.Background(), &repos.Repo{ID: "r1"}, toIndex, processed, false, nil, nil)
	if err != nil {
		t.Fatalf("indexFileSet() error = %v", err)
	}
	if len(failed) != 2 {
		t.Errorf("failed set = %v, want the whole batch", failed)
	}
	if got := st.storedPaths(); len(got) != 0 {
		t.Errorf("stored file rows = %v, want none", got)
	}
}

func TestIndexFileSetSuccessStoresEverything(t *testing.T) {
	st := &mockStorage{}
	idx := &mockIndexer{name: "ast", indexType: indexing.IndexTypeAST}
	svc := newTestService(st, map[indexing.IndexType]indexing.Indexer{indexing.IndexTypeAST: idx})

	toIndex := []*indexing.FileToIndex{{Path: "a.go"}, {Path: "b.go"}}
	processed := []*storage.File{{RepoID: "r1", Path: "a.go"}, {RepoID: "r1", Path: "b.go"}}

	failed, err := svc.indexFileSet(context.Background(), &repos.Repo{ID: "r1"}, toIndex, processed, false, nil, nil)
	if err != nil {
		t.Fatalf("indexFileSet() error = %v", err)
	}
	if len(failed) != 0 {
		t.Errorf("failed set = %v, want empty", failed)
	}
	if got := st.storedPaths(); len(got) != 2 {
		t.Errorf("stored file rows = %v, want both files", got)
	}
}

func TestAttributeFailures(t *testing.T) {
	batch := []*indexing.FileToIndex{{Path: "a/b.go"}, {Path: "c.go"}}

	failed := map[string]bool{}
	n := attributeFailures([]string{
		"a/b.go: embed: timeout",
		"c.go: chunk: bad utf8",
		"unrelated noise",
	}, batch, failed)
	if n != 2 {
		t.Errorf("attributed = %d, want 2", n)
	}
	if !failed["a/b.go"] || !failed["c.go"] {
		t.Errorf("failed = %v, want both batch paths", failed)
	}
}

// TestAddRepoPreservesExistingState covers re-registering a repo that is
// mid-index: resetting its state there would allow a second concurrent pass
// and drop the commit cursor the gap check depends on.
func TestAddRepoPreservesExistingState(t *testing.T) {
	existing := &repos.Repo{
		ID: "repo-1", Name: "test", Source: repos.SourceTypeLocal,
		Status: repos.StatusIndexing, LastCommit: "sha-1", IndexedAt: 111, CreatedAt: 5,
	}
	st := &mockStorage{repo: existing}
	src := &mockSource{
		name: repos.SourceTypeLocal,
		addFunc: func(context.Context, *repos.AddRequest) (*repos.Repo, error) {
			return &repos.Repo{ID: "repo-1", Name: "test", Source: repos.SourceTypeLocal, Path: "/tmp/a"}, nil
		},
	}
	svc := newTestService(st, nil)
	svc.sources = map[repos.SourceType]repos.RepoSource{repos.SourceTypeLocal: src}

	got, err := svc.AddRepo(context.Background(), repos.SourceTypeLocal, &repos.AddRequest{Name: "test", Path: "/tmp/a"})
	if err != nil {
		t.Fatalf("AddRepo() error = %v", err)
	}
	if got.Status != repos.StatusIndexing {
		t.Errorf("status = %s, want the existing indexing claim to survive", got.Status)
	}
	if got.LastCommit != "sha-1" {
		t.Errorf("last_commit = %q, want sha-1", got.LastCommit)
	}
	if got.IndexedAt != 111 || got.CreatedAt != 5 {
		t.Errorf("indexed_at/created_at = %d/%d, want 111/5", got.IndexedAt, got.CreatedAt)
	}
	// The existing row is out of the working set, and registering it again does
	// not bring it back: membership is decided by SetActiveRepos alone, and
	// every startup re-registers whatever its source finds.
	if got.Active {
		t.Error("re-registering an inactive repository reported it as active")
	}
}

func TestAddRepoNewRepoStartsIdle(t *testing.T) {
	st := &mockStorage{getRepoErr: storage.ErrNotFound}
	src := &mockSource{name: repos.SourceTypeLocal}
	svc := newTestService(st, nil)
	svc.sources = map[repos.SourceType]repos.RepoSource{repos.SourceTypeLocal: src}

	got, err := svc.AddRepo(context.Background(), repos.SourceTypeLocal, &repos.AddRequest{Name: "test", Path: "/tmp/a"})
	if err != nil {
		t.Fatalf("AddRepo() error = %v", err)
	}
	if got.Status != repos.StatusIdle {
		t.Errorf("status = %s, want idle", got.Status)
	}
	if got.CreatedAt == 0 {
		t.Error("created_at was not set for a new repo")
	}
	// A newly registered repository is in the working set. Anything else would
	// hide every repository added through the API from the searches that do not
	// name it — including the eval harness', which registers and then asks.
	if !got.Active {
		t.Error("a newly registered repository was not active")
	}
}

// TestStartIndexBusyReturnsErrRepoBusy: a busy repo is a retry condition, not
// a malformed request, and callers distinguish it by sentinel.
func TestStartIndexBusyReturnsErrRepoBusy(t *testing.T) {
	no := false
	st := &mockStorage{claimOK: &no}
	svc := newTestService(st, nil)

	_, err := svc.StartIndex(context.Background(), "repo-1", false)
	if !errors.Is(err, ErrRepoBusy) {
		t.Fatalf("StartIndex() error = %v, want ErrRepoBusy", err)
	}
}

// TestStartIndexDistributedReportsJob: in distributed mode the work is only
// queued, and the client needs the job id to follow it.
func TestStartIndexDistributedReportsJob(t *testing.T) {
	st := &mockStorage{}
	svc := newTestService(st, nil)
	svc.cfg = &config.Config{Indexes: config.IndexesConfig{Distributed: true}}

	ack, err := svc.StartIndex(context.Background(), "repo-1", true)
	if err != nil {
		t.Fatalf("StartIndex() error = %v", err)
	}
	if ack.Status != "queued" || !ack.Queued {
		t.Errorf("ack = %+v, want a queued status", ack)
	}
	if ack.JobID != "job-1" || ack.JobStatus != storage.JobStatusPending {
		t.Errorf("ack = %+v, want the job id and its queue state", ack)
	}
	if !ack.Force {
		t.Errorf("ack.Force = false, want the effective force flag")
	}
}

func TestStartIndexSingleInstanceReportsIndexing(t *testing.T) {
	st := &mockStorage{}
	svc := newTestService(st, map[indexing.IndexType]indexing.Indexer{})

	ack, err := svc.StartIndex(context.Background(), "repo-1", false)
	if err != nil {
		t.Fatalf("StartIndex() error = %v", err)
	}
	svc.wg.Wait()
	if ack.Status != "indexing" || ack.Queued {
		t.Errorf("ack = %+v, want an in-process indexing status", ack)
	}
}

// TestRecoverStuckReposForcesInSingleInstanceMode: with one instance every
// claim in the database is from a previous life of this process.
func TestRecoverStuckRepos(t *testing.T) {
	st := &mockStorage{}
	svc := newTestService(st, nil)
	svc.recoverStuckRepos()

	st.mu.Lock()
	calls := append([]bool(nil), st.resetCalls...)
	st.mu.Unlock()
	if len(calls) != 1 || !calls[0] {
		t.Fatalf("reset calls = %v, want one forced reset", calls)
	}

	// With the shared queue another instance may hold a live claim, so only
	// expired ones may be released.
	st2 := &mockStorage{}
	svc2 := newTestService(st2, nil)
	svc2.cfg = &config.Config{Indexes: config.IndexesConfig{Distributed: true}}
	svc2.recoverStuckRepos()

	st2.mu.Lock()
	calls2 := append([]bool(nil), st2.resetCalls...)
	st2.mu.Unlock()
	if len(calls2) != 1 || calls2[0] {
		t.Fatalf("distributed reset calls = %v, want one non-forced reset", calls2)
	}
}

// TestTerminalCtxSurvivesCancellation: Close cancels the context indexing runs
// under, and the terminal status write must still land — otherwise the repo
// stays claimed as "indexing" forever.
func TestTerminalCtxSurvivesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tctx, tcancel := terminalCtx(ctx)
	defer tcancel()

	if err := tctx.Err(); err != nil {
		t.Fatalf("terminal context is already done: %v", err)
	}
	if _, ok := tctx.Deadline(); !ok {
		t.Error("terminal context has no deadline; it must be bounded")
	}
}

// --- webhook repository matching (integrations.go) ---

// TestFindRepoByHintsMatchesExactly is the webhook mis-routing bug: substring
// matching let a push to "gateway" reindex "gateway-v2".
func TestFindRepoByHintsMatchesExactly(t *testing.T) {
	all := []*repos.Repo{
		{ID: "r1", Name: "gateway", URL: "https://github.com/acme/gateway.git"},
		{ID: "r2", Name: "gateway-v2", URL: "https://github.com/acme/gateway-v2.git"},
	}
	svc := newTestService(&mockStorage{repoList: all}, nil)

	cases := []struct {
		name   string
		hints  []string
		wantID string
	}{
		{"clone url", []string{"https://github.com/acme/gateway.git"}, "r1"},
		{"clone url of the longer name", []string{"https://github.com/acme/gateway-v2.git"}, "r2"},
		{"ssh url normalizes to the same repo", []string{"git@github.com:acme/gateway.git"}, "r1"},
		{"url without .git suffix", []string{"https://github.com/acme/gateway"}, "r1"},
		{"exact name", []string{"gateway"}, "r1"},
		{"owner/name tail", []string{"acme/gateway"}, "r1"},
		{"owner/name tail of the longer name", []string{"acme/gateway-v2"}, "r2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.FindRepoByHints(context.Background(), tc.hints)
			if err != nil {
				t.Fatalf("FindRepoByHints(%v) error = %v", tc.hints, err)
			}
			if got.ID != tc.wantID {
				t.Errorf("FindRepoByHints(%v) = %s, want %s", tc.hints, got.ID, tc.wantID)
			}
		})
	}
}

func TestFindRepoByHintsRejectsPartialMatches(t *testing.T) {
	all := []*repos.Repo{{ID: "r1", Name: "gateway", URL: "https://github.com/acme/gateway.git"}}
	svc := newTestService(&mockStorage{repoList: all}, nil)

	for _, hint := range []string{"gate", "gateway-v2", "https://github.com/other/gateway-v2.git"} {
		if _, err := svc.FindRepoByHints(context.Background(), []string{hint}); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("FindRepoByHints(%q) error = %v, want ErrNotFound", hint, err)
		}
	}
}

// TestRepoJobsCapsTheLimit: the queue keeps terminal jobs, so an unbounded
// page grows with the repository's push history.
func TestRepoJobsCapsTheLimit(t *testing.T) {
	st := &mockStorage{jobList: []*storage.IndexJob{{ID: "1", RepoID: "repo-1"}}}
	svc := newTestService(st, nil)

	if _, err := svc.RepoJobs(context.Background(), "repo-1", 10_000); err != nil {
		t.Fatalf("RepoJobs() error = %v", err)
	}
	st.mu.Lock()
	limits := append([]int(nil), st.jobListLimits...)
	st.mu.Unlock()
	if len(limits) != 1 || limits[0] != maxJobListLimit {
		t.Errorf("storage limits = %v, want a single capped %d", limits, maxJobListLimit)
	}
}

// TestRepoJobsUnknownRepoIsNotFound: an empty list would be indistinguishable
// from "nothing queued" for a repo that does not exist.
func TestRepoJobsUnknownRepoIsNotFound(t *testing.T) {
	st := &mockStorage{getRepoErr: storage.ErrNotFound}
	svc := newTestService(st, nil)

	if _, err := svc.RepoJobs(context.Background(), "gone", 0); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("RepoJobs() error = %v, want ErrNotFound", err)
	}
}

// TestRepoJobRejectsForeignJob: a job id alone must not be a way to read
// across repositories.
func TestRepoJobRejectsForeignJob(t *testing.T) {
	st := &mockStorage{job: &storage.IndexJob{ID: "5", RepoID: "repo-2", Kind: storage.JobKindIndex}}
	svc := newTestService(st, nil)

	if _, err := svc.RepoJob(context.Background(), "repo-1", "5"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("RepoJob() error = %v, want ErrNotFound", err)
	}
	got, err := svc.RepoJob(context.Background(), "repo-2", "5")
	if err != nil || got.ID != "5" {
		t.Errorf("RepoJob() = %+v, %v; want job 5", got, err)
	}
}

func TestOwnerIDIsPerInstance(t *testing.T) {
	a := newTestService(&mockStorage{}, nil)
	b := newTestService(&mockStorage{}, nil)

	if a.ownerID() == b.ownerID() {
		t.Error("two instances share an owner id; claim ownership cannot be distinguished")
	}
	if first, second := a.ownerID(), a.ownerID(); first != second {
		t.Errorf("owner id is not stable across calls: %q then %q", first, second)
	}
}

// TestIndexFileSetRunsIndexersConcurrently: the indexers write to independent
// backends (SQL, Bleve, the vector store), and running them one after the
// other left the machine idle — one index pass saturated a single core while
// the rest of the pool waited. Each must be in flight while the others are.
func TestIndexFileSetRunsIndexersConcurrently(t *testing.T) {
	const n = 3
	var mu sync.Mutex
	inFlight, peak := 0, 0
	entered := make(chan struct{}, n)
	release := make(chan struct{})

	indexers := make(map[indexing.IndexType]indexing.Indexer, n)
	for _, typ := range []indexing.IndexType{indexing.IndexTypeAST, indexing.IndexTypeBM25, indexing.IndexTypeVector} {
		indexers[typ] = &mockIndexer{
			name: string(typ), indexType: typ,
			indexFunc: func(req *indexing.IndexRequest) (*indexing.IndexResult, error) {
				mu.Lock()
				inFlight++
				peak = max(peak, inFlight)
				mu.Unlock()
				entered <- struct{}{}
				<-release
				mu.Lock()
				inFlight--
				mu.Unlock()
				return &indexing.IndexResult{FilesIndexed: len(req.Files)}, nil
			},
		}
	}

	st := &mockStorage{}
	svc := newTestService(st, indexers)
	toIndex := []*indexing.FileToIndex{{Path: "a.go"}}
	processed := []*storage.File{{RepoID: "r1", Path: "a.go"}}

	done := make(chan error, 1)
	go func() {
		_, err := svc.indexFileSet(context.Background(), &repos.Repo{ID: "r1"}, toIndex, processed, false, nil, nil)
		done <- err
	}()

	for i := 0; i < n; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			mu.Lock()
			got := peak
			mu.Unlock()
			t.Fatalf("only %d of %d indexers started; they are still running one at a time", got, n)
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("indexFileSet() error = %v", err)
	}
	if peak != n {
		t.Errorf("peak concurrent indexers = %d, want %d", peak, n)
	}
}

// TestIndexFileSetGivesEachIndexerItsOwnFiles: the indexers fill
// FileToIndex.Content in when it arrives empty, so sharing the descriptors
// across the fan-out would be a write race on a value another indexer reads.
func TestIndexFileSetGivesEachIndexerItsOwnFiles(t *testing.T) {
	svc := newTestService(&mockStorage{}, map[indexing.IndexType]indexing.Indexer{
		indexing.IndexTypeAST:    &mockIndexer{name: "ast", indexType: indexing.IndexTypeAST},
		indexing.IndexTypeBM25:   &mockIndexer{name: "bm25", indexType: indexing.IndexTypeBM25},
		indexing.IndexTypeVector: &mockIndexer{name: "vector", indexType: indexing.IndexTypeVector},
	})

	files := []*indexing.FileToIndex{{Path: "a.go", Language: "go"}}
	runs := withFiles(svc.indexerRuns(), files)
	if len(runs) != 3 {
		t.Fatalf("runs = %d, want 3", len(runs))
	}
	if want := []indexing.IndexType{indexing.IndexTypeAST, indexing.IndexTypeBM25, indexing.IndexTypeVector}; runs[0].typ != want[0] || runs[1].typ != want[1] || runs[2].typ != want[2] {
		t.Errorf("run order = %v %v %v, want %v", runs[0].typ, runs[1].typ, runs[2].typ, want)
	}
	seen := map[*indexing.FileToIndex]bool{}
	for _, r := range runs {
		if len(r.files) != len(files) {
			t.Fatalf("%s got %d files, want %d", r.typ, len(r.files), len(files))
		}
		if r.files[0].Path != "a.go" || r.files[0].Language != "go" {
			t.Errorf("%s got %+v, want a copy of a.go", r.typ, r.files[0])
		}
		if seen[r.files[0]] {
			t.Errorf("%s shares a *FileToIndex with another indexer; a concurrent Content fill would race", r.typ)
		}
		seen[r.files[0]] = true
	}
}

// TestIndexerTimesLog keeps the per-indexer tally readable and ordered; it is
// the only thing that says which indexer a slow pass was waiting for.
func TestIndexerTimesLog(t *testing.T) {
	spent := indexerTimes{}
	spent.add(indexing.IndexTypeBM25, 63*time.Second)
	spent.add(indexing.IndexTypeAST, 20*time.Second)
	spent.add(indexing.IndexTypeAST, 21*time.Second)
	if got, want := spent.log(), "ast=41s bm25=63s"; got != want {
		t.Errorf("indexerTimes.log() = %q, want %q", got, want)
	}
	// A nil tally is the "not interested" case (partial passes pass nil).
	indexerTimes(nil).add(indexing.IndexTypeAST, time.Second)
}
