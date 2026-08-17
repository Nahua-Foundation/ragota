package promote

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/graph"
	"github.com/Nahua-Foundation/ragota/internal/indexing"
	"github.com/Nahua-Foundation/ragota/internal/storage"
	"github.com/Nahua-Foundation/ragota/internal/storage/sqlite"
)

// calleeFixture seeds one callee that lives in a test file, which is all the
// rank gate needs to be exercised.
func calleeFixture(t *testing.T) *sqlite.SQLite {
	t.Helper()
	st, err := sqlite.Open(&sqlite.Config{Path: filepath.Join(t.TempDir(), "callee.db"), PoolSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	u := &storage.ASTUnit{
		RepoID: "r1", FilePath: "shipping/service_test.go", Language: "go",
		Kind: "function", Name: "TestShipOrder", Qualified: "shipping.TestShipOrder",
		StartLine: 5, EndLine: 30,
	}
	if err := st.StoreASTUnit(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return st
}

// TestCalleeUnitsRankGateSkipsTestFiles: a top hit that lands in a test file is
// accepted as the callee only when the question describes its name. Rank alone
// is a vocabulary coincidence, and promoting it answers a question nobody asked.
func TestCalleeUnitsRankGateSkipsTestFiles(t *testing.T) {
	st := calleeFixture(t)
	p := New(st, graph.New(st), "")

	hits := []*indexing.Hit{{RepoID: "r1", FilePath: "shipping/service_test.go", Line: 6, EndLine: 20}}

	units := p.calleeUnits(context.Background(), &indexing.SearchQuery{Repos: []string{"r1"}},
		"payment gateway timeout", hits)
	if len(units) != 0 {
		t.Fatalf("calleeUnits accepted a test-file unit on rank alone: %+v", units)
	}

	units = p.calleeUnits(context.Background(), &indexing.SearchQuery{Repos: []string{"r1"}},
		"the test ship order helper", hits)
	if len(units) != 1 || units[0].repr.Name != "TestShipOrder" {
		t.Fatalf("calleeUnits by described name = %+v, want TestShipOrder", units)
	}
}
