package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// openTestStore connects to the Postgres instance in RAGOTA_TEST_POSTGRES_DSN
// or skips the test. Example:
//
//	RAGOTA_TEST_POSTGRES_DSN=postgres://postgres:postgres@localhost:5432/ragota_test go test ./internal/storage/postgres/
func openTestStore(t *testing.T) *Postgres {
	t.Helper()
	dsn := os.Getenv("RAGOTA_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("RAGOTA_TEST_POSTGRES_DSN not set; skipping postgres integration test")
	}
	st, err := Open(&Config{DSN: dsn, PoolSize: 4})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestPostgresBatchAndClaim(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	repoID := fmt.Sprintf("test-batch-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = st.DeleteEdgesByRepo(ctx, repoID)
		_ = st.DeleteASTUnitsByRepo(ctx, repoID)
		_ = st.DeleteRepo(ctx, repoID)
	})

	// Batch store units: IDs must be assigned.
	meta := storage.EncodeUnitMeta(&storage.UnitMeta{Root: "svc", DetectedBy: "go.mod"})
	units := []*storage.ASTUnit{
		{RepoID: repoID, FilePath: "a.go", Language: "go", Kind: "function", Name: "Foo", Meta: meta},
		{RepoID: repoID, FilePath: "a.go", Language: "go", Kind: "function", Name: "Bar"},
		{RepoID: repoID, FilePath: "a.go", Language: "go", Kind: "function", Name: "Baz"},
	}
	if err := st.BatchStoreASTUnits(ctx, units); err != nil {
		t.Fatalf("batch store units: %v", err)
	}
	for i, u := range units {
		if u.ID == "" {
			t.Fatalf("unit %d: ID not assigned", i)
		}
	}

	// GetASTUnitsByIDs: empty input -> nil, subset returns matching units.
	if got, err := st.GetASTUnitsByIDs(ctx, nil); err != nil || got != nil {
		t.Fatalf("empty ids: %v %v", got, err)
	}
	got, err := st.GetASTUnitsByIDs(ctx, []string{units[0].ID, units[2].ID})
	if err != nil || len(got) != 2 {
		t.Fatalf("get by ids: %v %v", got, err)
	}
	names := map[string]bool{}
	for _, u := range got {
		names[u.Name] = true
	}
	if !names["Foo"] || !names["Baz"] {
		t.Fatalf("expected Foo and Baz, got %v", names)
	}

	// Meta roundtrip.
	byID, err := st.GetASTUnitByID(ctx, units[0].ID)
	if err != nil || byID.Meta != meta {
		t.Fatalf("meta roundtrip: %+v %v", byID, err)
	}

	// Batch store edges: IDs must be assigned.
	edges := []*storage.Edge{
		{RepoID: repoID, SrcID: units[0].ID, Kind: storage.EdgeCall, DstName: "Bar", FilePath: "a.go", Line: 1},
		{RepoID: repoID, SrcID: units[1].ID, Kind: storage.EdgeCall, DstName: "Baz", FilePath: "a.go", Line: 2},
		{RepoID: repoID, SrcID: units[2].ID, Kind: storage.EdgeImport, DstName: "Bar", FilePath: "a.go", Line: 3},
	}
	if err := st.BatchStoreEdges(ctx, edges); err != nil {
		t.Fatalf("batch store edges: %v", err)
	}
	for i, e := range edges {
		if e.ID == "" {
			t.Fatalf("edge %d: ID not assigned", i)
		}
	}
	if edges[0].ID == edges[1].ID {
		t.Fatalf("edges got the same ID: %s", edges[0].ID)
	}

	// DeleteEdgesByKindAndDst removes only matching edges.
	if err := st.DeleteEdgesByKindAndDst(ctx, storage.EdgeCall, "Bar"); err != nil {
		t.Fatalf("delete edges by kind and dst: %v", err)
	}
	rest, err := st.GetEdges(ctx, storage.QueryOpts{RepoID: repoID})
	if err != nil || len(rest) != 2 {
		t.Fatalf("edges after delete: %v %v", rest, err)
	}
	for _, e := range rest {
		if e.Kind == storage.EdgeCall && e.DstName == "Bar" {
			t.Fatalf("edge should have been deleted: %+v", e)
		}
	}

	// ClaimRepoForIndexing: first claim wins, second returns false, missing -> ErrNotFound.
	repo := &repos.Repo{ID: repoID, Name: "t", Source: repos.SourceTypeLocal, Path: "/tmp/x", Status: repos.StatusIdle, CreatedAt: 1}
	if err := st.StoreRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimRepoForIndexing(ctx, repoID, "owner-a", 3600)
	if err != nil || !claimed {
		t.Fatalf("first claim: %v %v", claimed, err)
	}
	r, err := st.GetRepo(ctx, repoID)
	if err != nil || r.Status != repos.StatusIndexing {
		t.Fatalf("repo after claim: %+v %v", r, err)
	}
	claimed, err = st.ClaimRepoForIndexing(ctx, repoID, "owner-b", 3600)
	if err != nil || claimed {
		t.Fatalf("second claim: %v %v", claimed, err)
	}
	if _, err := st.ClaimRepoForIndexing(ctx, repoID+"-missing", "owner-a", 3600); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing repo claim: %v", err)
	}

	// An expired claim is taken over: otherwise a crashed indexer wedges the
	// repo in "indexing" forever.
	if _, err := st.pool.Exec(ctx, `UPDATE repos SET claim_expires_at = $1 WHERE id = $2`,
		time.Now().Unix()-1, repoID); err != nil {
		t.Fatal(err)
	}
	claimed, err = st.ClaimRepoForIndexing(ctx, repoID, "owner-c", 3600)
	if err != nil || !claimed {
		t.Fatalf("takeover of an expired claim: %v %v", claimed, err)
	}

	// A forced reset releases it again (single-instance startup recovery).
	if n, err := st.ResetStuckRepos(ctx, true); err != nil || n == 0 {
		t.Fatalf("reset stuck repos: n=%d err=%v", n, err)
	}
	r, err = st.GetRepo(ctx, repoID)
	if err != nil || r.Status != repos.StatusIdle {
		t.Fatalf("repo after reset: %+v %v", r, err)
	}

	// The in-flight commit SHA round-trips and is cleared by a terminal write.
	if err := st.SetRepoPendingCommit(ctx, repoID, "sha-in-flight"); err != nil {
		t.Fatalf("set pending commit: %v", err)
	}
	if r, err = st.GetRepo(ctx, repoID); err != nil || r.PendingCommit != "sha-in-flight" {
		t.Fatalf("pending commit: %+v %v", r, err)
	}
	if err := st.UpdateRepoStatus(ctx, repoID, repos.StatusIdle, "", 7); err != nil {
		t.Fatal(err)
	}
	if r, err = st.GetRepo(ctx, repoID); err != nil || r.PendingCommit != "" {
		t.Fatalf("pending commit after terminal write: %+v %v", r, err)
	}
}

func TestPostgresCommitJobQueue(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if _, err := st.pool.Exec(ctx, `DELETE FROM index_jobs`); err != nil {
		t.Fatalf("clean index_jobs: %v", err)
	}
	t.Cleanup(func() { _, _ = st.pool.Exec(context.Background(), `DELETE FROM index_jobs`) })

	first, err := st.EnqueueCommitJob(ctx, "repo-a", `[{"sha":"sha-1"}]`)
	if err != nil {
		t.Fatalf("enqueue first batch: %v", err)
	}
	second, err := st.EnqueueCommitJob(ctx, "repo-a", `[{"sha":"sha-2"}]`)
	if err != nil {
		t.Fatalf("enqueue second batch: %v", err)
	}
	if first.ID == second.ID || first.Kind != storage.JobKindCommits {
		t.Fatalf("batches = %+v and %+v, want two distinct commit jobs", first, second)
	}

	claimed, err := st.ClaimNextIndexJob(ctx, "w1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.ID != first.ID || claimed.Payload != `[{"sha":"sha-1"}]` {
		t.Fatalf("claimed = %+v, want the oldest batch with its payload", claimed)
	}

	ahead, err := st.HasPendingCommitJobBefore(ctx, "repo-a", second.ID)
	if err != nil || !ahead {
		t.Errorf("HasPendingCommitJobBefore(second) = %v, %v; want true", ahead, err)
	}
	ahead, err = st.HasPendingCommitJobBefore(ctx, "repo-a", first.ID)
	if err != nil || ahead {
		t.Errorf("HasPendingCommitJobBefore(first) = %v, %v; want false", ahead, err)
	}

	if err := st.ReleaseIndexJob(ctx, claimed.ID, "w1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	back, err := st.GetIndexJob(ctx, first.ID)
	if err != nil {
		t.Fatalf("get released job: %v", err)
	}
	if back.Status != storage.JobStatusPending {
		t.Fatalf("released commit job = %+v, want it back in the queue", back)
	}

	again, err := st.ClaimNextIndexJob(ctx, "w1")
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if err := st.CompleteIndexJob(ctx, again.ID, "w1", ""); err != nil {
		t.Fatalf("complete: %v", err)
	}
	var payload string
	if err := st.pool.QueryRow(ctx, `SELECT payload FROM index_jobs WHERE id = $1`, intOrZero(again.ID)).Scan(&payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if payload != "" {
		t.Errorf("payload of a finished job = %q, want it dropped with the result", payload)
	}

	jobs, err := st.ListIndexJobs(ctx, "repo-a", 0)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("list jobs = %d, %v; want the repo's two entries", len(jobs), err)
	}
	if jobs[0].ID != second.ID {
		t.Errorf("first listed job = %s, want the newest (%s)", jobs[0].ID, second.ID)
	}
}

func TestPostgresJobQueue(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	// The job queue is global (not per-repo-id namespaced), so start from a
	// clean table to make claim ordering deterministic.
	if _, err := st.pool.Exec(ctx, `DELETE FROM index_jobs`); err != nil {
		t.Fatalf("clean index_jobs: %v", err)
	}
	t.Cleanup(func() { _, _ = st.pool.Exec(context.Background(), `DELETE FROM index_jobs`) })

	// Claim on an empty queue: ErrNotFound.
	if _, err := st.ClaimNextIndexJob(ctx, "w1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("claim on empty queue: err = %v, want ErrNotFound", err)
	}

	// Enqueue + dedup: a second enqueue for the same repo returns the
	// existing pending job.
	j1, err := st.EnqueueIndexJob(ctx, "repo-a", false)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if j1.ID == "" || j1.Status != storage.JobStatusPending || j1.CreatedAt == 0 {
		t.Fatalf("enqueued job = %+v", j1)
	}
	dup, err := st.EnqueueIndexJob(ctx, "repo-a", true)
	if err != nil {
		t.Fatalf("enqueue dup: %v", err)
	}
	if dup.ID != j1.ID {
		t.Fatalf("dedup failed: got job %s, want existing %s", dup.ID, j1.ID)
	}
	if !dup.Force {
		t.Fatal("force flag was dropped when merging into the queued job")
	}
	j2, err := st.EnqueueIndexJob(ctx, "repo-b", true)
	if err != nil {
		t.Fatalf("enqueue repo-b: %v", err)
	}

	// Claim order: oldest pending job first.
	c1, err := st.ClaimNextIndexJob(ctx, "w1")
	if err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	if c1.ID != j1.ID || c1.Status != storage.JobStatusRunning || c1.ClaimedBy != "w1" ||
		c1.ClaimedAt == 0 || c1.HeartbeatAt == 0 {
		t.Fatalf("claimed job = %+v, want job %s running by w1", c1, j1.ID)
	}
	// repo-a is no longer pending, so a new enqueue creates a fresh job.
	j3, err := st.EnqueueIndexJob(ctx, "repo-a", false)
	if err != nil {
		t.Fatalf("re-enqueue repo-a: %v", err)
	}
	if j3.ID == j1.ID {
		t.Fatalf("re-enqueue returned the running job %s", j1.ID)
	}
	c2, err := st.ClaimNextIndexJob(ctx, "w2")
	if err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	if c2.ID != j2.ID || !c2.Force {
		t.Fatalf("second claim = %+v, want job %s with force", c2, j2.ID)
	}

	// Heartbeat: running job ok, pending job -> ErrNotFound.
	if err := st.HeartbeatIndexJob(ctx, c1.ID, "w1"); err != nil {
		t.Fatalf("heartbeat running: %v", err)
	}
	if err := st.HeartbeatIndexJob(ctx, j3.ID, "w1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("heartbeat pending: err = %v, want ErrNotFound", err)
	}
	// Bookkeeping is scoped to the claiming worker.
	if err := st.HeartbeatIndexJob(ctx, c1.ID, "someone-else"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("heartbeat from a foreign worker: err = %v, want ErrNotFound", err)
	}
	if err := st.CompleteIndexJob(ctx, c1.ID, "someone-else", "boom"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("complete from a foreign worker: err = %v, want ErrNotFound", err)
	}

	// Complete: empty error -> done, non-empty -> error; completing a
	// non-running job -> ErrNotFound.
	if err := st.CompleteIndexJob(ctx, c1.ID, "w1", ""); err != nil {
		t.Fatalf("complete done: %v", err)
	}
	if err := st.CompleteIndexJob(ctx, c2.ID, "w2", "boom"); err != nil {
		t.Fatalf("complete error: %v", err)
	}
	var status, jobErr string
	if err := st.pool.QueryRow(ctx, `SELECT status, error FROM index_jobs WHERE id = $1`, intOrZero(c1.ID)).Scan(&status, &jobErr); err != nil {
		t.Fatal(err)
	}
	if status != storage.JobStatusDone || jobErr != "" {
		t.Fatalf("job 1 after complete: status=%s error=%q", status, jobErr)
	}
	if err := st.pool.QueryRow(ctx, `SELECT status, error FROM index_jobs WHERE id = $1`, intOrZero(c2.ID)).Scan(&status, &jobErr); err != nil {
		t.Fatal(err)
	}
	if status != storage.JobStatusError || jobErr != "boom" {
		t.Fatalf("job 2 after complete: status=%s error=%q", status, jobErr)
	}
	if err := st.CompleteIndexJob(ctx, c1.ID, "w1", ""); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("double complete: err = %v, want ErrNotFound", err)
	}

	// Requeue stale: nothing stale yet.
	c3, err := st.ClaimNextIndexJob(ctx, "w1")
	if err != nil || c3.ID != j3.ID {
		t.Fatalf("claim 3: %+v %v", c3, err)
	}
	if n, err := st.RequeueStaleIndexJobs(ctx, 120); err != nil || n != 0 {
		t.Fatalf("requeue fresh: n=%d err=%v", n, err)
	}
	// Age the heartbeat past the threshold and requeue.
	if _, err := st.pool.Exec(ctx, `UPDATE index_jobs SET heartbeat_at = $1 WHERE id = $2`,
		time.Now().Unix()-1000, intOrZero(c3.ID)); err != nil {
		t.Fatal(err)
	}
	n, err := st.RequeueStaleIndexJobs(ctx, 120)
	if err != nil || n != 1 {
		t.Fatalf("requeue stale: n=%d err=%v", n, err)
	}
	re, err := st.ClaimNextIndexJob(ctx, "w2")
	if err != nil {
		t.Fatalf("claim requeued: %v", err)
	}
	if re.ID != j3.ID || re.ClaimedBy != "w2" || re.Status != storage.JobStatusRunning {
		t.Fatalf("requeued claim = %+v, want job %s running by w2", re, j3.ID)
	}
}

// TestMigrationActivatesExistingRepos covers the upgrade of a database written
// before the working set existed: every repository a user already has must come
// out of it active, since nothing in an old database ever said which
// repositories a run is about.
//
// It runs the real migration text against a repos table with the column removed
// again, all inside a transaction that is rolled back — DDL is transactional on
// Postgres, so the shared test database is left exactly as it was found.
func TestMigrationActivatesExistingRepos(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	var stmts []string
	for _, m := range migrations {
		if m.version == 11 {
			stmts = m.stmts
		}
	}
	if len(stmts) == 0 {
		t.Fatal("migration 11 (the active column) is gone; this test names the wrong version")
	}

	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `ALTER TABLE repos DROP COLUMN active`); err != nil {
		t.Fatalf("undo the column: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO repos (id, name, source, path, created_at, indexed_at)
		VALUES ('migration-old-1', 'old', 'local', '/tmp/old', 1, 500)`); err != nil {
		t.Fatalf("seed an old row: %v", err)
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatalf("migration 11: %v", err)
		}
	}

	var active bool
	var indexedAt int64
	if err := tx.QueryRow(ctx,
		`SELECT active, indexed_at FROM repos WHERE id = 'migration-old-1'`,
	).Scan(&active, &indexedAt); err != nil {
		t.Fatalf("read the migrated row: %v", err)
	}
	if !active {
		t.Error("a repository that existed before the migration came back inactive")
	}
	if indexedAt != 500 {
		t.Errorf("indexed_at = %d, want the migration to leave the row otherwise alone", indexedAt)
	}
}
