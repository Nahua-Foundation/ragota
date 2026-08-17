package qdrant

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// The tests in qdrant_test.go run against a hand-written stub of the parts of
// the API this client uses, which is what makes them fast and hermetic — and
// also what they cannot prove: that the requests the client actually sends are
// the ones Qdrant accepts. A stub agrees with whatever the client does.
//
// These run against a real instance instead. Point RAGOTA_TEST_QDRANT_URL at
// one to enable them:
//
//	docker run --rm -d -p 6333:6333 qdrant/qdrant
//	RAGOTA_TEST_QDRANT_URL=http://127.0.0.1:6333 go test ./internal/storage/qdrant/

func openLive(t *testing.T) (*Qdrant, string) {
	t.Helper()
	url := os.Getenv("RAGOTA_TEST_QDRANT_URL")
	if url == "" {
		t.Skip("RAGOTA_TEST_QDRANT_URL not set; skipping live Qdrant test")
	}
	// A prefix of its own per test, so a failed run cannot leave state another
	// one reads, and so a shared instance is safe.
	prefix := fmt.Sprintf("livetest_%d_", time.Now().UnixNano())
	q := Open(&Config{URL: url, CollectionPrefix: prefix})
	if err := q.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() {
		_ = q.Close()
	})
	return q, prefix
}

func livePoint(id, repo, path string, vec []float32) *storage.VectorPoint {
	return &storage.VectorPoint{
		ID: id, Vector: vec, RepoID: repo, FilePath: path,
		Language: "go", StartLine: 1, EndLine: 10,
		Kind: "function", Symbol: "Add", Text: "func Add(a, b int) int",
	}
}

// TestLiveRoundTrip is the path a vector index pass takes: create the
// collection on first write, upsert, search, delete by file, delete by repo.
func TestLiveRoundTrip(t *testing.T) {
	q, _ := openLive(t)
	ctx := context.Background()
	repo := "repo-live"

	points := []*storage.VectorPoint{
		livePoint("00000000-0000-0000-0000-000000000001", repo, "a.go", []float32{1, 0, 0, 0}),
		livePoint("00000000-0000-0000-0000-000000000002", repo, "b.go", []float32{0, 1, 0, 0}),
	}
	if err := q.Upsert(ctx, points); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	t.Cleanup(func() { _ = q.Delete(ctx, repo, "") })

	// Qdrant indexes asynchronously; a fresh point is searchable within a few
	// hundred milliseconds, so the search is retried rather than raced.
	var res []*storage.VectorResult
	var err error
	for i := 0; i < 20; i++ {
		res, err = q.Search(ctx, storage.VectorSearchOpts{
			Query: []float32{1, 0, 0, 0}, Repos: []string{repo}, Limit: 5,
		})
		if err == nil && len(res) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("Search returned nothing for a vector that was just written")
	}
	if res[0].FilePath != "a.go" {
		t.Errorf("nearest = %s, want a.go (the identical vector)", res[0].FilePath)
	}
	if res[0].Text == "" || res[0].RepoID != repo {
		t.Errorf("payload did not round trip: %+v", res[0])
	}

	// A repo filter that matches nothing returns nothing rather than everything.
	other, err := q.Search(ctx, storage.VectorSearchOpts{
		Query: []float32{1, 0, 0, 0}, Repos: []string{"repo-that-does-not-exist"}, Limit: 5,
	})
	if err != nil {
		t.Fatalf("Search (filtered): %v", err)
	}
	if len(other) != 0 {
		t.Errorf("filter by an unknown repo returned %d results", len(other))
	}

	if err := q.Delete(ctx, repo, "a.go"); err != nil {
		t.Fatalf("Delete by file: %v", err)
	}
	if err := q.Delete(ctx, repo, ""); err != nil {
		t.Fatalf("Delete by repo: %v", err)
	}
}

// TestLiveDeleteOnMissingCollection: deleting from a collection that was never
// created is how a reset behaves on a fresh instance, and it must not be an
// error — the caller asked for the rows to be gone, and they are.
func TestLiveDeleteOnMissingCollection(t *testing.T) {
	q, _ := openLive(t)
	if err := q.Delete(context.Background(), "repo-never-written", ""); err != nil {
		t.Errorf("Delete on a missing collection = %v, want nil", err)
	}
}

// TestLivePayloadIndexesAreCreated pins the fix that made filtered deletes stop
// being full scans, and the narrowing that came with it: ensurePayloadIndexes
// used to treat a 400 as "already indexed" alongside the benign 409, which
// would have hidden a request Qdrant rejected. Against a real instance the
// only way this passes is if the requests are actually accepted.
func TestLivePayloadIndexesAreCreated(t *testing.T) {
	q, _ := openLive(t)
	ctx := context.Background()
	repo := "repo-payload"

	if err := q.Upsert(ctx, []*storage.VectorPoint{
		livePoint("00000000-0000-0000-0000-000000000010", repo, "c.go", []float32{0, 0, 1, 0}),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	t.Cleanup(func() { _ = q.Delete(ctx, repo, "") })

	// A second pass re-runs ensurePayloadIndexes over indexes that now exist:
	// that answer is the benign conflict, and it must stay benign.
	if err := q.Upsert(ctx, []*storage.VectorPoint{
		livePoint("00000000-0000-0000-0000-000000000011", repo, "d.go", []float32{0, 0, 0, 1}),
	}); err != nil {
		t.Fatalf("Upsert (second pass, indexes already present): %v", err)
	}

	// Qdrant applies an upsert asynchronously unless the caller asks it to
	// wait, and the indexer does not — so the count catches up shortly after
	// the write returns, and the assertion polls rather than races.
	var stats *storage.VectorStats
	var err error
	for i := 0; i < 30; i++ {
		stats, err = q.Stats(ctx)
		if err == nil && stats.Documents >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Documents < 2 {
		t.Errorf("Stats documents = %d, want the two points written to be counted", stats.Documents)
	}
	if stats.Repos < 1 {
		t.Errorf("Stats collections = %d, want the collection this test created", stats.Repos)
	}
}
