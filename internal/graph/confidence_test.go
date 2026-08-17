package graph

import (
	"context"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

func TestConfidenceAfterResolve(t *testing.T) {
	factor := contract.ConfCrossFile // 0.8

	// With a base recorded in meta, the result is base*factor even though the
	// stored Confidence already carries a folded-in factor (0.9*0.8) — the
	// accumulated value must be ignored, or reindexing compounds it.
	withMeta := &storage.Edge{
		Confidence: contract.ConfHigh * contract.ConfCrossFile, // 0.72, already resolved once
		Meta:       `{"base_conf":0.9}`,
	}
	if got, want := confidenceAfterResolve(withMeta, factor), float32(0.9)*factor; got != want {
		t.Errorf("with meta base: got %v, want %v (accumulated Confidence must be ignored)", got, want)
	}

	// A legacy edge with no recorded base falls back to the stored Confidence.
	legacy := &storage.Edge{Confidence: 0.9}
	if got, want := confidenceAfterResolve(legacy, factor), float32(0.9)*factor; got != want {
		t.Errorf("legacy fallback: got %v, want %v", got, want)
	}
}

// TestConfidenceStableAcrossReindex is the regression test for the decay bug:
// reindexing a destination repo unresolves the edges pointing into it (dst_id
// cleared, confidence left as-is), so re-resolution must recompute base*factor
// from the recorded base rather than multiply the factor into the stored value
// again. The confidence must be identical after every reindex.
func TestConfidenceStableAcrossReindex(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	caller := storeFunc(t, st, "repoA", "CallThings", "(id string)")
	storeRoute := func() {
		storeUnit(t, st, &storage.ASTUnit{
			RepoID: "repoB", FilePath: "b/routes.go", Language: "go",
			Kind: storage.KindHTTPRoute, Name: "GET /api/things",
			Qualified: "http:GET /api/things",
		})
	}
	storeRoute()

	// An http_call edge as the parser writes it: base recorded in meta.
	storeEdge(t, st, &storage.Edge{
		RepoID: "repoA", SrcID: caller.ID, Kind: storage.EdgeHTTPCall,
		DstName: "http:GET /api/things", FilePath: "src/CallThings.go", Line: 10,
		Confidence: contract.ConfHigh,
		Meta:       `{"base_conf":0.9}`,
	})

	run := func() {
		if err := NewLinker(st).Run(ctx, "repoA"); err != nil {
			t.Fatal(err)
		}
	}
	run()
	first := httpEdgeBySrc(t, st, caller.ID)
	if first.DstID == "" || first.Confidence <= 0 {
		t.Fatalf("edge did not resolve: dst=%q confidence=%v", first.DstID, first.Confidence)
	}

	for i := 1; i <= 3; i++ {
		// Reindex repoB's route file: delete its units (unresolves the edge)
		// and recreate the route with a fresh id.
		if err := st.DeleteASTUnitsByFile(ctx, "repoB", "b/routes.go"); err != nil {
			t.Fatal(err)
		}
		storeRoute()
		run()

		got := httpEdgeBySrc(t, st, caller.ID)
		if got.DstID == "" {
			t.Fatalf("reindex %d: edge left unresolved", i)
		}
		if got.Confidence != first.Confidence {
			t.Fatalf("reindex %d: confidence drifted to %v, want stable %v", i, got.Confidence, first.Confidence)
		}
	}
}
