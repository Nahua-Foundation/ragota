package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/storage"
	"github.com/Nahua-Foundation/ragota/internal/storage/sqlite"
)

// TestIngestRuntimeServiceGraphNamesAndDiagnostics covers the two things that
// make this endpoint usable at all: a tracing backend's spelling of a service
// name does not have to match the repository's, and a payload that matches
// nothing says so instead of quietly reporting success.
func TestIngestRuntimeServiceGraphNamesAndDiagnostics(t *testing.T) {
	st, err := sqlite.Open(&sqlite.Config{Path: filepath.Join(t.TempDir(), "rt.db"), PoolSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	for _, name := range []string{"checkoutservice", "shippingservice"} {
		if err := st.StoreASTUnit(ctx, &storage.ASTUnit{
			RepoID: "r1", FilePath: "src/" + name, Kind: storage.KindService,
			Name: name, Qualified: "service:" + name, StartLine: 1, EndLine: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	svc := New(nil, st, nil, nil, nil)
	defer svc.Close(ctx)

	// Jaeger spells them with a separator; detection did not.
	res, err := svc.IngestRuntimeServiceGraph(ctx, []RuntimeServiceEdge{
		{Client: "checkout-service", Server: "shipping_service", Calls: 42},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Stored != 1 {
		t.Errorf("stored = %d, want 1 (names differ only by separator): %+v", res.Stored, res)
	}

	edges, err := st.GetEdges(ctx, storage.QueryOpts{Kind: storage.EdgeRuntimeCall})
	if err != nil || len(edges) != 1 {
		t.Fatalf("edges = %v (err %v), want one runtime_call", edges, err)
	}

	// A payload naming services nobody detected must not report success, must
	// name what it could not place, and must not wipe what is already there.
	res, err = svc.IngestRuntimeServiceGraph(ctx, []RuntimeServiceEdge{
		{Client: "frontend", Server: "cartservice"},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Stored != 0 || len(res.Unmatched) != 2 {
		t.Errorf("result = %+v, want stored 0 and both names unmatched", res)
	}
	if len(res.Known) == 0 {
		t.Error("result does not list the detected service names, so the caller cannot see why nothing matched")
	}
	edges, err = st.GetEdges(ctx, storage.QueryOpts{Kind: storage.EdgeRuntimeCall})
	if err != nil || len(edges) != 1 {
		t.Fatalf("a payload that matched nothing destroyed the existing graph: %v", edges)
	}
}
