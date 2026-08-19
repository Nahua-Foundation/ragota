package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

func openTestDB(t *testing.T) *SQLite {
	t.Helper()
	s, err := Open(&Config{Path: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestMigrationActivatesExistingRepos covers the upgrade of a database written
// before the working set existed. Every repository a user already has must come
// out of it active: nothing in an old database ever said which repositories a
// run is about, and defaulting the column the other way would hide all of them
// behind a flag the user has never heard of.
func TestMigrationActivatesExistingRepos(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	// The repos table as migration 11 left it, plus a migration log saying so:
	// Open then runs migration 12 and nothing else, which is exactly the step
	// under test.
	stmts := []string{
		`CREATE TABLE repos (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, source TEXT NOT NULL,
			url TEXT NOT NULL DEFAULT '', path TEXT NOT NULL, branch TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'idle', last_error TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL, indexed_at INTEGER NOT NULL DEFAULT 0,
			last_commit TEXT NOT NULL DEFAULT '', claimed_by TEXT NOT NULL DEFAULT '',
			claim_expires_at INTEGER NOT NULL DEFAULT 0, pending_commit TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY)`,
		`INSERT INTO schema_migrations (version)
			SELECT 1 UNION SELECT 2 UNION SELECT 3 UNION SELECT 4 UNION SELECT 5 UNION SELECT 6
			UNION SELECT 7 UNION SELECT 8 UNION SELECT 9 UNION SELECT 10 UNION SELECT 11`,
		`INSERT INTO repos (id, name, source, path, created_at, indexed_at)
			VALUES ('old-1', 'old', 'local', '/tmp/old', 1, 500)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed old schema: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(&Config{Path: path})
	if err != nil {
		t.Fatalf("open (migrate): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	got, err := s.GetRepo(context.Background(), "old-1")
	if err != nil {
		t.Fatalf("GetRepo after migration: %v", err)
	}
	if !got.Active {
		t.Error("a repository that existed before the migration came back inactive")
	}
	if got.IndexedAt != 500 {
		t.Errorf("indexed_at = %d, want the migration to leave the row otherwise alone", got.IndexedAt)
	}
}

func TestGetEdgesFilters(t *testing.T) {
	s := openTestDB(t)
	repoID := "repo-1"

	// Create source AST units first (foreign key constraint on src_id).
	// Don't pre-set IDs — let DB generate integer autoincrement IDs, then fetch them.
	sources := []*domain.ASTUnit{
		{RepoID: repoID, FilePath: "a.go", Language: "go", Kind: "function", Name: "s1"},
		{RepoID: repoID, FilePath: "a.go", Language: "go", Kind: "function", Name: "s2"},
		{RepoID: repoID, FilePath: "a.go", Language: "go", Kind: "function", Name: "s3"},
	}
	for _, u := range sources {
		if err := s.StoreASTUnit(context.Background(), u); err != nil {
			t.Fatalf("store source ast unit: %v", err)
		}
	}

	// Fetch generated IDs
	var srcIDs []string
	for _, name := range []string{"s1", "s2", "s3"} {
		unit, err := s.GetASTUnitByName(context.Background(), repoID, name, "go")
		if err != nil {
			t.Fatalf("get ast unit by name %s: %v", name, err)
		}
		srcIDs = append(srcIDs, unit.ID)
	}

	edges := []*domain.Edge{
		{ID: "e1", RepoID: repoID, SrcID: srcIDs[0], Kind: "call", DstName: "foo", FilePath: "a.go", Line: 10},
		{ID: "e2", RepoID: repoID, SrcID: srcIDs[1], Kind: "call", DstName: "bar", FilePath: "b.go", Line: 20},
		{ID: "e3", RepoID: repoID, SrcID: srcIDs[2], Kind: "import", DstName: "baz", FilePath: "c.go", Line: 30},
	}

	for _, e := range edges {
		if err := s.StoreEdge(context.Background(), e); err != nil {
			t.Fatalf("store edge: %v", err)
		}
	}

	callEdges, err := s.GetEdges(context.Background(), domain.QueryOpts{RepoID: repoID, Kind: "call"})
	if err != nil {
		t.Fatalf("get edges by kind: %v", err)
	}
	if len(callEdges) != 2 {
		t.Fatalf("expected 2 call edges, got %d", len(callEdges))
	}

	nameEdges, err := s.GetEdges(context.Background(), domain.QueryOpts{RepoID: repoID, Name: "bar"})
	if err != nil {
		t.Fatalf("get edges by name: %v", err)
	}
	if len(nameEdges) != 1 || nameEdges[0].DstName != "bar" {
		t.Errorf("expected 1 edge with dst_name=bar, got %d", len(nameEdges))
	}

	limitEdges, err := s.GetEdges(context.Background(), domain.QueryOpts{RepoID: repoID, Limit: 1})
	if err != nil {
		t.Fatalf("get edges with limit: %v", err)
	}
	if len(limitEdges) != 1 {
		t.Fatalf("expected 1 edge with limit=1, got %d", len(limitEdges))
	}
}

func TestClaimRepoExpiredClaimIsTakenOver(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	r := &domain.Repo{ID: "repo-1", Name: "test", Source: "local", Path: "/tmp/test", Status: domain.StatusIdle, CreatedAt: 1000}
	if err := s.StoreRepo(ctx, r); err != nil {
		t.Fatalf("store repo: %v", err)
	}

	if claimed, err := s.ClaimRepoForIndexing(ctx, "repo-1", "dead-owner", 3600); err != nil || !claimed {
		t.Fatalf("initial claim: claimed=%v err=%v", claimed, err)
	}
	// Move the expiry into the past: the owner died without releasing it.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE repos SET claim_expires_at = ? WHERE id = ?`, time.Now().Unix()-1, "repo-1"); err != nil {
		t.Fatalf("expire claim: %v", err)
	}

	claimed, err := s.ClaimRepoForIndexing(ctx, "repo-1", "live-owner", 300)
	if err != nil {
		t.Fatalf("takeover claim: %v", err)
	}
	if !claimed {
		t.Fatal("expected an expired claim to be taken over")
	}
}

func TestResetStuckRepos(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	for _, id := range []string{"repo-1", "repo-2"} {
		r := &domain.Repo{ID: id, Name: id, Source: "local", Path: "/tmp/" + id, Status: domain.StatusIdle, CreatedAt: 1000}
		if err := s.StoreRepo(ctx, r); err != nil {
			t.Fatalf("store repo: %v", err)
		}
	}
	if claimed, err := s.ClaimRepoForIndexing(ctx, "repo-1", "owner", 3600); err != nil || !claimed {
		t.Fatalf("claim repo-1: claimed=%v err=%v", claimed, err)
	}

	// A live claim survives a non-forced reset (another instance may hold it).
	n, err := s.ResetStuckRepos(ctx, false)
	if err != nil {
		t.Fatalf("reset (expired only): %v", err)
	}
	if n != 0 {
		t.Fatalf("reset (expired only) = %d, want 0 while the claim is live", n)
	}

	// A forced reset is what a single instance does at startup: every claim in
	// the database belongs to a previous life of this process.
	n, err = s.ResetStuckRepos(ctx, true)
	if err != nil {
		t.Fatalf("reset (force): %v", err)
	}
	if n != 1 {
		t.Fatalf("reset (force) = %d, want 1", n)
	}
	got, err := s.GetRepo(ctx, "repo-1")
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if got.Status != domain.StatusIdle {
		t.Errorf("status after reset = %s, want idle", got.Status)
	}
	if got.LastError != store.RepoResetMessage {
		t.Errorf("last_error after reset = %q, want %q", got.LastError, store.RepoResetMessage)
	}
}

func TestIndexJobQueue(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	j1, err := s.EnqueueIndexJob(ctx, "repo-a", false)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// A second enqueue must not create a duplicate, and a forced request must
	// not be downgraded by the cheap one already in the queue.
	j2, err := s.EnqueueIndexJob(ctx, "repo-a", true)
	if err != nil {
		t.Fatalf("enqueue again: %v", err)
	}
	if j2.ID != j1.ID {
		t.Fatalf("second enqueue created job %s, want the existing %s", j2.ID, j1.ID)
	}
	if !j2.Force {
		t.Error("force flag was dropped when merging into the queued job")
	}

	claimed, err := s.ClaimNextIndexJob(ctx, "worker-1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.ID != j1.ID || !claimed.Force {
		t.Fatalf("claimed job = %+v, want %s with force", claimed, j1.ID)
	}

	// Bookkeeping is scoped to the claiming worker: a worker whose job was
	// requeued and re-claimed elsewhere must not overwrite the new result.
	if err := s.HeartbeatIndexJob(ctx, claimed.ID, "worker-2"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("heartbeat from a foreign worker = %v, want ErrNotFound", err)
	}
	if err := s.CompleteIndexJob(ctx, claimed.ID, "worker-2", "boom"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("complete from a foreign worker = %v, want ErrNotFound", err)
	}
	if err := s.HeartbeatIndexJob(ctx, claimed.ID, "worker-1"); err != nil {
		t.Errorf("heartbeat from the owner: %v", err)
	}
	if err := s.CompleteIndexJob(ctx, claimed.ID, "worker-1", ""); err != nil {
		t.Errorf("complete from the owner: %v", err)
	}

	done, err := s.GetIndexJob(ctx, claimed.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if done.Status != domain.JobStatusDone || done.Error != "" {
		t.Errorf("finished job = %+v, want done with no error", done)
	}
}

// TestReleaseIndexJob covers the "repo busy" path: the repo is being indexed
// by another pass, which is a retry condition, so the job goes back to the
// queue instead of being recorded as a failure.
func TestReleaseIndexJob(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	job, err := s.EnqueueIndexJob(ctx, "repo-a", false)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, err := s.ClaimNextIndexJob(ctx, "worker-1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.ReleaseIndexJob(ctx, claimed.ID, "worker-1"); err != nil {
		t.Fatalf("release: %v", err)
	}

	back, err := s.GetIndexJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if back.Status != domain.JobStatusPending || back.ClaimedBy != "" {
		t.Errorf("released job = %+v, want pending and unclaimed", back)
	}
}

// TestRequeueStaleIndexJobsKeepsOnePending covers the interaction between the
// stale-job sweep and the one-pending-job-per-repo constraint.
func TestRequeueStaleIndexJobsKeepsOnePending(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	old := time.Now().Unix() - 1000
	for i := 0; i < 2; i++ {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO index_jobs (repo_id, force, status, error, created_at, claimed_at, heartbeat_at, claimed_by)
			 VALUES (?, 0, 'running', '', ?, ?, ?, 'dead-worker')`, "repo-a", old, old, old); err != nil {
			t.Fatalf("insert stale job: %v", err)
		}
	}

	if _, err := s.RequeueStaleIndexJobs(ctx, 120); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	var pending int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM index_jobs WHERE repo_id = ? AND status = 'pending'`, "repo-a").Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 1 {
		t.Errorf("pending jobs for repo-a = %d, want exactly 1", pending)
	}
}

func TestCommitJobQueue(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	first, err := s.EnqueueCommitJob(ctx, "repo-a", `[{"sha":"sha-1"}]`)
	if err != nil {
		t.Fatalf("enqueue first batch: %v", err)
	}
	second, err := s.EnqueueCommitJob(ctx, "repo-a", `[{"sha":"sha-2"}]`)
	if err != nil {
		t.Fatalf("enqueue second batch: %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("second batch merged into %s; a commit batch is not interchangeable with another", first.ID)
	}
	if first.Kind != domain.JobKindCommits {
		t.Errorf("kind = %q, want %q", first.Kind, domain.JobKindCommits)
	}

	// An index job for the same repo still merges: only commit jobs are exempt
	// from the one-pending-job-per-repo rule.
	idx1, err := s.EnqueueIndexJob(ctx, "repo-a", false)
	if err != nil {
		t.Fatalf("enqueue index job: %v", err)
	}
	idx2, err := s.EnqueueIndexJob(ctx, "repo-a", true)
	if err != nil {
		t.Fatalf("enqueue index job again: %v", err)
	}
	if idx2.ID != idx1.ID || !idx2.Force {
		t.Errorf("index jobs = %s then %s (force %v), want one merged job with force", idx1.ID, idx2.ID, idx2.Force)
	}

	// Ordering: the oldest batch is claimed first and carries its payload.
	claimed, err := s.ClaimNextIndexJob(ctx, "w1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.ID != first.ID {
		t.Fatalf("claimed %s, want the oldest batch %s", claimed.ID, first.ID)
	}
	if claimed.Payload != `[{"sha":"sha-1"}]` {
		t.Errorf("claimed payload = %q, want the enqueued batch", claimed.Payload)
	}

	// The batch still ahead of the queue is worth waiting for; nothing
	// precedes the first one.
	ahead, err := s.HasPendingCommitJobBefore(ctx, "repo-a", second.ID)
	if err != nil || !ahead {
		t.Errorf("HasPendingCommitJobBefore(second) = %v, %v; want true", ahead, err)
	}
	ahead, err = s.HasPendingCommitJobBefore(ctx, "repo-a", first.ID)
	if err != nil || ahead {
		t.Errorf("HasPendingCommitJobBefore(first) = %v, %v; want false", ahead, err)
	}

	// Releasing a commit job requeues it even though another commit job for
	// the same repo is pending; dropping it as "superseded" would drop a piece
	// of the repository's history.
	if err := s.ReleaseIndexJob(ctx, claimed.ID, "w1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	back, err := s.GetIndexJob(ctx, first.ID)
	if err != nil {
		t.Fatalf("get released job: %v", err)
	}
	if back.Status != domain.JobStatusPending {
		t.Fatalf("released commit job = %+v, want it back in the queue", back)
	}

	// A finished job keeps its result but not its payload.
	again, err := s.ClaimNextIndexJob(ctx, "w1")
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if err := s.CompleteIndexJob(ctx, again.ID, "w1", ""); err != nil {
		t.Fatalf("complete: %v", err)
	}
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM index_jobs WHERE id = ?`, again.ID).Scan(&payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if payload != "" {
		t.Errorf("payload of a finished job = %q, want it dropped with the result", payload)
	}
}

// TestRequeueStaleCommitJobsKeepsEveryBatch: the sweep may collapse index jobs
// (they are interchangeable) but never commit jobs.
func TestRequeueStaleCommitJobsKeepsEveryBatch(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	old := time.Now().Unix() - 1000
	for i := 0; i < 2; i++ {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO index_jobs (repo_id, kind, force, status, error, created_at, claimed_at, heartbeat_at, claimed_by, payload)
			 VALUES (?, 'commits', 0, 'running', '', ?, ?, ?, 'dead-worker', ?)`,
			"repo-a", old, old, old, fmt.Sprintf(`[{"sha":"sha-%d"}]`, i)); err != nil {
			t.Fatalf("insert stale commit job: %v", err)
		}
	}

	if _, err := s.RequeueStaleIndexJobs(ctx, 120); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	var pending int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM index_jobs WHERE repo_id = ? AND status = 'pending'`, "repo-a").Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 2 {
		t.Errorf("pending commit jobs = %d, want both batches requeued", pending)
	}
}

func TestListIndexJobs(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	if _, err := s.EnqueueIndexJob(ctx, "repo-a", true); err != nil {
		t.Fatalf("enqueue index job: %v", err)
	}
	if _, err := s.EnqueueCommitJob(ctx, "repo-a", `[{"sha":"sha-1"}]`); err != nil {
		t.Fatalf("enqueue commit job: %v", err)
	}
	if _, err := s.EnqueueIndexJob(ctx, "repo-b", false); err != nil {
		t.Fatalf("enqueue other repo: %v", err)
	}

	jobs, err := s.ListIndexJobs(ctx, "repo-a", 0)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d, want the repo's two entries", len(jobs))
	}
	if jobs[0].Kind != domain.JobKindCommits {
		t.Errorf("first listed job kind = %q, want the newest entry (%q)", jobs[0].Kind, domain.JobKindCommits)
	}
	// The listing reports queue state, not batch contents.
	if jobs[0].Payload != "" {
		t.Errorf("listed payload = %q, want it left in the database", jobs[0].Payload)
	}

	if jobs, err = s.ListIndexJobs(ctx, "repo-a", 1); err != nil || len(jobs) != 1 {
		t.Errorf("limited list = %d jobs, %v; want 1", len(jobs), err)
	}
}

func TestDeleteEdgesByKindAndDst(t *testing.T) {
	s := openTestDB(t)
	repoID := "repo-1"

	src := &domain.ASTUnit{RepoID: repoID, FilePath: "a.go", Language: "go", Kind: "function", Name: "src"}
	if err := s.StoreASTUnit(context.Background(), src); err != nil {
		t.Fatalf("store ast unit: %v", err)
	}

	edges := []*domain.Edge{
		{RepoID: repoID, SrcID: src.ID, Kind: "call", DstName: "foo", FilePath: "a.go", Line: 1},
		{RepoID: repoID, SrcID: src.ID, Kind: "call", DstName: "bar", FilePath: "a.go", Line: 2},
		{RepoID: repoID, SrcID: src.ID, Kind: "import", DstName: "foo", FilePath: "a.go", Line: 3},
	}
	if err := s.BatchStoreEdges(context.Background(), edges); err != nil {
		t.Fatalf("batch store edges: %v", err)
	}

	if err := s.DeleteEdgesByKindAndDst(context.Background(), "call", "foo"); err != nil {
		t.Fatalf("delete edges by kind and dst: %v", err)
	}

	rest, err := s.GetEdges(context.Background(), domain.QueryOpts{RepoID: repoID})
	if err != nil {
		t.Fatalf("get edges: %v", err)
	}
	if len(rest) != 2 {
		t.Fatalf("expected 2 edges left, got %d", len(rest))
	}
	for _, e := range rest {
		if e.Kind == "call" && e.DstName == "foo" {
			t.Errorf("edge should have been deleted: %+v", e)
		}
	}
}

func TestOpenAppliesPerformancePragmas(t *testing.T) {
	st, err := Open(&Config{Path: filepath.Join(t.TempDir(), "pragmas.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	pragmas := map[string]int{
		"cache_size":         -defaultCacheKB,
		"wal_autocheckpoint": defaultWALCheckpointPages,
		"temp_store":         2, // memory
		"foreign_keys":       1,
	}
	for name, want := range pragmas {
		var got int
		if err := st.db.QueryRow("PRAGMA " + name).Scan(&got); err != nil {
			t.Fatalf("PRAGMA %s: %v", name, err)
		}
		if got != want {
			t.Errorf("PRAGMA %s = %d, want %d", name, got, want)
		}
	}

	var mode string
	if err := st.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("PRAGMA journal_mode = %q, want wal", mode)
	}
}

func TestOpenHonoursPragmaOverrides(t *testing.T) {
	st, err := Open(&Config{
		Path:                   filepath.Join(t.TempDir(), "pragmas.db"),
		CacheSizeKB:            4096,
		WALAutoCheckpointPages: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var cache, checkpoint int
	if err := st.db.QueryRow("PRAGMA cache_size").Scan(&cache); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow("PRAGMA wal_autocheckpoint").Scan(&checkpoint); err != nil {
		t.Fatal(err)
	}
	if cache != -4096 || checkpoint != 500 {
		t.Errorf("cache_size=%d wal_autocheckpoint=%d, want -4096 and 500", cache, checkpoint)
	}
}

func TestDeleteFilesByPathsChunks(t *testing.T) {
	ctx := context.Background()
	s := openTestDB(t)

	const n = pathChunk*2 + 7
	files := make([]*store.File, n)
	paths := make([]string, n)
	for i := range files {
		paths[i] = fmt.Sprintf("f%d.go", i)
		files[i] = &store.File{RepoID: "r1", Path: paths[i], Language: "go", Hash: "h", Indexed: true}
	}
	if err := s.BatchStoreFiles(ctx, files); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFilesByPaths(ctx, "r1", paths); err != nil {
		t.Fatalf("DeleteFilesByPaths() = %v", err)
	}
	got, err := s.GetFilesByRepo(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("%d files left after deleting %d paths", len(got), n)
	}
}

func TestDeleteByFilesChunksBeyondParameterLimit(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	const nFiles = pathChunk*2 + 5
	paths := make([]string, 0, nFiles)
	for i := 0; i < nFiles; i++ {
		path := fmt.Sprintf("pkg%04d/file.go", i)
		paths = append(paths, path)
		u := &domain.ASTUnit{RepoID: "repo-1", FilePath: path, Language: "go", Kind: "function", Name: "F"}
		if err := s.StoreASTUnit(ctx, u); err != nil {
			t.Fatalf("store unit: %v", err)
		}
		if err := s.StoreEdge(ctx, &domain.Edge{RepoID: "repo-1", SrcID: u.ID, Kind: store.EdgeCall,
			DstName: "G", FilePath: path, Line: 1}); err != nil {
			t.Fatalf("store edge: %v", err)
		}
	}

	if err := s.DeleteASTUnitsByFiles(ctx, "repo-1", paths); err != nil {
		t.Fatalf("delete units by files: %v", err)
	}
	if err := s.DeleteEdgesByFiles(ctx, "repo-1", paths); err != nil {
		t.Fatalf("delete edges by files: %v", err)
	}

	units, err := s.GetASTUnits(ctx, domain.QueryOpts{RepoID: "repo-1"})
	if err != nil {
		t.Fatalf("get units: %v", err)
	}
	if len(units) != 0 {
		t.Errorf("units left = %d, want 0", len(units))
	}
	edges, err := s.GetEdges(ctx, domain.QueryOpts{RepoID: "repo-1"})
	if err != nil {
		t.Fatalf("get edges: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("edges left = %d, want 0", len(edges))
	}
}

// An empty path list is a no-op, not a repo-wide delete.
func TestDeleteByFilesEmptyIsNoOp(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	u := &domain.ASTUnit{RepoID: "repo-1", FilePath: "a.go", Language: "go", Kind: "function", Name: "A"}
	if err := s.StoreASTUnit(ctx, u); err != nil {
		t.Fatalf("store unit: %v", err)
	}
	if err := s.StoreEdge(ctx, &domain.Edge{RepoID: "repo-1", SrcID: u.ID, Kind: store.EdgeCall,
		DstName: "B", FilePath: "a.go", Line: 1}); err != nil {
		t.Fatalf("store edge: %v", err)
	}

	if err := s.DeleteASTUnitsByFiles(ctx, "repo-1", nil); err != nil {
		t.Fatalf("delete units by no files: %v", err)
	}
	if err := s.DeleteEdgesByFiles(ctx, "repo-1", nil); err != nil {
		t.Fatalf("delete edges by no files: %v", err)
	}

	units, err := s.GetASTUnits(ctx, domain.QueryOpts{RepoID: "repo-1"})
	if err != nil {
		t.Fatalf("get units: %v", err)
	}
	edges, err := s.GetEdges(ctx, domain.QueryOpts{RepoID: "repo-1"})
	if err != nil {
		t.Fatalf("get edges: %v", err)
	}
	if len(units) != 1 || len(edges) != 1 {
		t.Errorf("after deleting no paths: %d units, %d edges; want 1 and 1", len(units), len(edges))
	}
}
