package graph

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/storage"
	"github.com/Nahua-Foundation/ragota/internal/storage/sqlite"
)

// resolutionSpy counts how the linker reached storage and can fail one edge
// inside a batch, which is the case the per-edge error accounting exists for.
type resolutionSpy struct {
	*sqlite.SQLite
	failEdge string
	batches  int // BatchUpdateEdgeResolutions calls
	rows     int // resolutions written one statement at a time
}

var errResolutionRejected = errors.New("resolution rejected")

func (s *resolutionSpy) BatchUpdateEdgeResolutions(ctx context.Context, res []storage.EdgeResolution) ([]storage.EdgeResolutionFailure, error) {
	s.batches++
	pass := make([]storage.EdgeResolution, 0, len(res))
	at := make([]int, 0, len(res)) // pass index -> res index
	var failures []storage.EdgeResolutionFailure
	for i, r := range res {
		if r.EdgeID == s.failEdge {
			failures = append(failures, storage.EdgeResolutionFailure{Index: i, EdgeID: r.EdgeID, Err: errResolutionRejected})
			continue
		}
		pass = append(pass, r)
		at = append(at, i)
	}
	got, err := s.SQLite.BatchUpdateEdgeResolutions(ctx, pass)
	for _, f := range got {
		f.Index = at[f.Index]
		failures = append(failures, f)
	}
	return failures, err
}

func (s *resolutionSpy) UpdateEdgeResolution(ctx context.Context, edgeID, dstID, dstRepoID string, confidence float32) error {
	s.rows++
	return s.SQLite.UpdateEdgeResolution(ctx, edgeID, dstID, dstRepoID, confidence)
}

// unbatchedStore hides the batching capability, as a test double or a backend
// that never implemented it does.
type unbatchedStore struct {
	storage.Storage
	rows int
}

func (s *unbatchedStore) UpdateEdgeResolution(ctx context.Context, edgeID, dstID, dstRepoID string, confidence float32) error {
	s.rows++
	return s.Storage.UpdateEdgeResolution(ctx, edgeID, dstID, dstRepoID, confidence)
}

// storeCallGraph stores n functions in repoID and one call edge into each,
// all from a single caller, so that resolveLocal has exactly n resolutions to
// write and one candidate for each.
func storeCallGraph(t *testing.T, st *sqlite.SQLite, repoID string, n int) []*storage.Edge {
	t.Helper()
	ctx := context.Background()
	caller := storeFunc(t, st, repoID, "Caller", "()")
	targets := make([]*storage.ASTUnit, n)
	for i := range targets {
		name := fmt.Sprintf("Target%d", i)
		targets[i] = &storage.ASTUnit{
			RepoID: repoID, FilePath: "src/" + name + ".go", Language: "go",
			Kind: "function", Name: name, Qualified: "pkg." + name,
		}
	}
	if err := st.BatchStoreASTUnits(ctx, targets); err != nil {
		t.Fatal(err)
	}
	calls := make([]*storage.Edge, n)
	for i, u := range targets {
		calls[i] = &storage.Edge{
			RepoID: repoID, SrcID: caller.ID, Kind: storage.EdgeCall,
			DstName: u.Name, FilePath: "src/Caller.go", Line: i, Confidence: 1,
		}
	}
	if err := st.BatchStoreEdges(ctx, calls); err != nil {
		t.Fatal(err)
	}
	return calls
}

// resolvedCount counts the repo's call edges that carry a destination.
func resolvedCount(t *testing.T, st storage.Storage, repoID string) int {
	t.Helper()
	edges, err := st.GetEdges(context.Background(), storage.QueryOpts{RepoID: repoID, Kind: storage.EdgeCall})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range edges {
		if e.DstID != "" {
			n++
		}
	}
	return n
}

// TestLinkerBatchesLocalResolutions pins that local linking writes one batch
// per resolutionBuffer edges rather than one statement per edge.
func TestLinkerBatchesLocalResolutions(t *testing.T) {
	st := openTestStore(t)
	const n = resolutionBuffer + 5
	edges := storeCallGraph(t, st, "repoA", n)

	spy := &resolutionSpy{SQLite: st}
	stats, err := NewLinker(spy).RunWithStats(context.Background(), "repoA")
	if err != nil {
		t.Fatal(err)
	}
	if stats.ResolvedLocal != n {
		t.Errorf("ResolvedLocal = %d, want %d", stats.ResolvedLocal, n)
	}
	if want := (n + resolutionBuffer - 1) / resolutionBuffer; spy.batches != want {
		t.Errorf("batches = %d, want %d for %d resolutions", spy.batches, want, n)
	}
	if spy.rows != 0 {
		t.Errorf("single-row writes = %d, want 0 once the store batches", spy.rows)
	}
	if got := resolvedCount(t, spy, "repoA"); got != len(edges) {
		t.Errorf("resolved edges = %d, want %d", got, len(edges))
	}

	// A re-link resolves everything to the same targets, so it must write
	// nothing at all: that skip is what makes re-indexing cheap.
	spy.batches, spy.rows = 0, 0
	if _, err := NewLinker(spy).RunWithStats(context.Background(), "repoA"); err != nil {
		t.Fatal(err)
	}
	if spy.batches != 0 || spy.rows != 0 {
		t.Errorf("re-link wrote %d batches / %d rows, want none", spy.batches, spy.rows)
	}
}

// TestLinkerReportsMidBatchFailure pins that one rejected resolution costs
// only itself: it is counted and logged, and the rest of its batch still
// lands.
func TestLinkerReportsMidBatchFailure(t *testing.T) {
	st := openTestStore(t)
	const n = resolutionBuffer + 5
	edges := storeCallGraph(t, st, "repoA", n)

	failed := edges[n/2]
	spy := &resolutionSpy{SQLite: st, failEdge: failed.ID}
	stats, err := NewLinker(spy).RunWithStats(context.Background(), "repoA")
	if err != nil {
		t.Fatal(err)
	}
	if stats.Errors != 1 {
		t.Errorf("Errors = %d, want 1", stats.Errors)
	}
	if stats.ResolvedLocal != n-1 {
		t.Errorf("ResolvedLocal = %d, want %d", stats.ResolvedLocal, n-1)
	}
	if got := resolvedCount(t, spy, "repoA"); got != n-1 {
		t.Errorf("resolved edges = %d, want %d", got, n-1)
	}
	if got := edgeByID(t, st, storage.EdgeCall, failed.ID); got.DstID != "" {
		t.Errorf("rejected edge %s resolved to %q", failed.ID, got.DstID)
	}
}

// TestLinkerResolvesWithoutBatchingSupport pins the fallback: a store that
// does not implement storage.EdgeResolutionBatcher still gets every edge
// resolved, one statement at a time.
func TestLinkerResolvesWithoutBatchingSupport(t *testing.T) {
	st := openTestStore(t)
	const n = resolutionBuffer + 5
	storeCallGraph(t, st, "repoA", n)

	plain := &unbatchedStore{Storage: st}
	stats, err := NewLinker(plain).RunWithStats(context.Background(), "repoA")
	if err != nil {
		t.Fatal(err)
	}
	if stats.ResolvedLocal != n || stats.Errors != 0 {
		t.Errorf("ResolvedLocal = %d, Errors = %d, want %d and 0", stats.ResolvedLocal, stats.Errors, n)
	}
	if plain.rows != n {
		t.Errorf("single-row writes = %d, want %d", plain.rows, n)
	}
	if got := resolvedCount(t, st, "repoA"); got != n {
		t.Errorf("resolved edges = %d, want %d", got, n)
	}
}
