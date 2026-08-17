package e2e_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/service"
	"github.com/Nahua-Foundation/ragota/internal/setup"
	"github.com/Nahua-Foundation/ragota/internal/storage"
	"github.com/Nahua-Foundation/ragota/internal/testutil"
)

// distributedConfig builds a config for one instance of a distributed pair
// sharing a single SQLite database file. The config is constructed by hand
// (not via testutil/env switches) so the test is deterministic regardless of
// RAGOTA_TEST_STORAGE. Only the AST indexer is enabled: bm25/vector indexers
// keep local on-disk state and would conflict between two instances.
func distributedConfig(dbPath string) *config.Config {
	return &config.Config{
		Server: config.ServerConfig{Host: "127.0.0.1", Port: 0},
		Storage: config.StorageConfig{
			SQLite: &config.SQLiteStorageConfig{Path: dbPath, PoolSize: 2},
		},
		Indexes: config.IndexesConfig{
			Distributed:     true,
			JobPollSeconds:  1,
			StaleJobSeconds: 120,
			AST: &config.ASTIndexConfig{
				Enabled:   true,
				Languages: []string{"go"},
			},
		},
		Models: config.ModelsConfig{Providers: map[string]config.ProviderConfig{}},
		Repos: config.ReposConfig{
			Sources: config.ReposSourcesConfig{
				Local: &config.LocalSourceConfig{Enabled: true},
			},
		},
	}
}

// jobRow is the raw index_jobs row state observed by the test.
type jobRow struct {
	ID        int64
	Status    string
	Error     string
	ClaimedBy string
}

func lastJobForRepo(t *testing.T, db *sql.DB, repoID string) (jobRow, bool) {
	t.Helper()
	var j jobRow
	err := db.QueryRow(
		`SELECT id, status, error, claimed_by FROM index_jobs WHERE repo_id = ? ORDER BY id DESC LIMIT 1`,
		repoID,
	).Scan(&j.ID, &j.Status, &j.Error, &j.ClaimedBy)
	if err == sql.ErrNoRows {
		return jobRow{}, false
	}
	if err != nil {
		t.Fatalf("query index_jobs: %v", err)
	}
	return j, true
}

func jobByID(t *testing.T, db *sql.DB, id int64) jobRow {
	t.Helper()
	j := jobRow{ID: id}
	err := db.QueryRow(
		`SELECT status, error, claimed_by FROM index_jobs WHERE id = ?`, id,
	).Scan(&j.Status, &j.Error, &j.ClaimedBy)
	if err != nil {
		t.Fatalf("query index_jobs id=%d: %v", id, err)
	}
	return j
}

// TestDistributedIndexing runs two service instances over one shared SQLite
// database with the distributed job queue enabled, enqueues an indexing job
// through the first instance and verifies that the pool of workers executes
// it: the job row ends up done and symbols become visible via either
// instance. A subtest verifies that a stale running job (dead worker) is
// requeued by the pollers and executed.
func TestDistributedIndexing(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "shared.db")

	buildInstance := func() *service.Service {
		t.Helper()
		svc, err := setup.Build(ctx, distributedConfig(dbPath))
		if err != nil {
			t.Fatalf("setup build: %v", err)
		}
		t.Cleanup(func() { _ = svc.Close(context.Background()) })
		return svc
	}
	svc1 := buildInstance()
	svc2 := buildInstance()

	// Raw connection for inspecting/seeding the job table directly.
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(30000)")
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	repo, err := svc1.AddRepo(ctx, repos.SourceTypeLocal, &repos.AddRequest{
		Name: "microservices",
		Path: testutil.TestdataPath(t, "microservices"),
	})
	if err != nil {
		t.Fatalf("add repo: %v", err)
	}

	// In distributed mode IndexRepo only enqueues a job.
	if err := svc1.IndexRepo(ctx, repo.ID, false); err != nil {
		t.Fatalf("index repo: %v", err)
	}
	job, ok := lastJobForRepo(t, db, repo.ID)
	if !ok {
		t.Fatalf("IndexRepo did not enqueue a job")
	}

	// Some worker (either instance) must pick the job up and finish it.
	waitForJobDone(t, db, job.ID, 30*time.Second)

	done := jobByID(t, db, job.ID)
	if done.Error != "" || done.ClaimedBy == "" {
		t.Fatalf("job after completion = %+v, want empty error and non-empty claimed_by", done)
	}

	// Symbols are visible through the other instance over the shared DB.
	syms, err := svc2.Symbols(ctx, storage.QueryOpts{RepoID: repo.ID, Name: "publishOrderCreated", Limit: 5})
	if err != nil {
		t.Fatalf("symbols via second instance: %v", err)
	}
	if len(syms) == 0 {
		t.Fatalf("no symbols visible via second instance after distributed indexing")
	}

	t.Run("StaleRunningJobRequeued", func(t *testing.T) {
		// Simulate a job claimed by a worker that died: running status with a
		// heartbeat far in the past. The pollers must requeue it (stale after
		// 120s) and then execute it to completion.
		old := time.Now().Unix() - 1000
		res, err := db.Exec(
			`INSERT INTO index_jobs (repo_id, force, status, error, created_at, claimed_at, heartbeat_at, claimed_by)
			 VALUES (?, 1, 'running', '', ?, ?, ?, 'dead-worker')`,
			repo.ID, old, old, old,
		)
		if err != nil {
			t.Fatalf("insert stale job: %v", err)
		}
		staleID, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("stale job id: %v", err)
		}

		waitForJobDone(t, db, staleID, 30*time.Second)

		j := jobByID(t, db, staleID)
		if j.Error != "" {
			t.Fatalf("requeued job finished with error: %+v", j)
		}
		if j.ClaimedBy == "dead-worker" || j.ClaimedBy == "" {
			t.Fatalf("requeued job claimed_by = %q, want a live worker", j.ClaimedBy)
		}
	})
}

func waitForJobDone(t *testing.T, db *sql.DB, jobID int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		j := jobByID(t, db, jobID)
		switch j.Status {
		case storage.JobStatusDone:
			return
		case storage.JobStatusError:
			t.Fatalf("job %d failed: %s", jobID, j.Error)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("job %d did not complete in %s", jobID, timeout)
}
