package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/index"
)

// coverageIndexer is an indexer that reports contract coverage, i.e. what the
// AST indexer becomes once it counts candidate call sites.
type coverageIndexer struct {
	mockIndexer
	counts map[string]domain.CoverageCounts
}

func (c *coverageIndexer) ContractCoverage() map[string]domain.CoverageCounts { return c.counts }

func TestDoIndexPersistsCoverage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	idx := &coverageIndexer{
		mockIndexer: mockIndexer{name: "ast", indexType: index.IndexTypeAST},
		counts: map[string]domain.CoverageCounts{
			domain.ContractKindHTTP: {Candidates: 3000, Edges: 104},
			domain.ContractKindDB:   {Candidates: 12, Edges: 12},
		},
	}
	st := &mockStorage{}
	svc := newLocalTestService(t, st, map[index.IndexType]index.Indexer{index.IndexTypeAST: idx})
	repo := &domain.Repo{ID: "r1", Name: "t", Source: domain.SourceTypeLocal, Path: dir}

	if err := svc.doIndex(ctx, repo, false); err != nil {
		t.Fatalf("doIndex() error = %v", err)
	}

	report, err := svc.RepoCoverage(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if !report.Reported {
		t.Fatal("report.Reported = false, want the pass to have recorded coverage")
	}
	byKind := map[string]CoverageKind{}
	for _, k := range report.Kinds {
		byKind[k.Kind] = k
	}
	// Every known kind is listed, so "no messaging here" is distinguishable
	// from "messaging was never looked at".
	for _, kind := range domain.ContractKinds {
		if _, ok := byKind[kind]; !ok {
			t.Errorf("kind %s missing from the report", kind)
		}
	}
	if got := byKind[domain.ContractKindHTTP]; got.Candidates != 3000 || got.Edges != 104 {
		t.Errorf("http coverage = %+v, want 104 of 3000", got)
	}
	if got := byKind[domain.ContractKindDB].Ratio; got != 1 {
		t.Errorf("db ratio = %v, want 1 (every candidate produced an edge)", got)
	}
	if got := byKind[domain.ContractKindMessaging].Ratio; got != 1 {
		t.Errorf("messaging ratio = %v, want 1 (nothing to find is fully covered)", got)
	}
	if report.Totals.Candidates != 3012 || report.Totals.Edges != 116 {
		t.Errorf("totals = %+v, want 116 of 3012", report.Totals)
	}
	if report.UpdatedAt == 0 {
		t.Error("report.UpdatedAt = 0, want the time the summary was written")
	}
}

// TestRepoCoverageUnreported: a repo indexed by indexers that cannot count
// candidates must not read as "zero contracts found" — that is the exact
// confusion the endpoint exists to remove.
func TestRepoCoverageUnreported(t *testing.T) {
	ctx := context.Background()
	st := &mockStorage{repo: &domain.Repo{ID: "r1", Name: "t", IndexedAt: 42}}
	svc := newTestService(st, nil)

	report, err := svc.RepoCoverage(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if report.Reported {
		t.Error("report.Reported = true, want false when no pass recorded coverage")
	}
	if len(report.Kinds) != 0 {
		t.Errorf("report.Kinds = %v, want no counters at all", report.Kinds)
	}
	if report.IndexedAt != 42 {
		t.Errorf("report.IndexedAt = %d, want the repo's index time", report.IndexedAt)
	}
}

// TestCoverageAccumulatorSumsBatches: a full pass indexes in batches and runs
// every indexer over each of them, so the repo-wide summary only exists once
// the per-run counters are added up.
func TestCoverageAccumulatorSumsBatches(t *testing.T) {
	var acc coverageAccumulator
	if acc.summary("r1", time.Now()) != nil {
		t.Fatal("summary() on an untouched accumulator must be nil, not an empty tally")
	}

	idx := &coverageIndexer{counts: map[string]domain.CoverageCounts{
		domain.ContractKindHTTP: {Candidates: 10, Edges: 4},
	}}
	acc.collect(idx, &index.IndexResult{})
	acc.collect(idx, &index.IndexResult{})

	summary := acc.summary("r1", time.Now())
	if summary == nil {
		t.Fatal("summary() = nil after two batches")
	}
	if got := summary.Kinds[domain.ContractKindHTTP]; got.Candidates != 20 || got.Edges != 8 {
		t.Errorf("http counts = %+v, want 8 of 20", got)
	}
}

// TestCoveragePartialPassKeepsPreviousSummary: a pass that skipped unchanged
// files saw only part of the repository, and its counters must not replace a
// summary that describes all of it.
func TestCoveragePartialPassKeepsPreviousSummary(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	idx := &coverageIndexer{
		mockIndexer: mockIndexer{name: "ast", indexType: index.IndexTypeAST},
		counts:      map[string]domain.CoverageCounts{domain.ContractKindHTTP: {Candidates: 40, Edges: 40}},
	}
	st := &mockStorage{}
	svc := newLocalTestService(t, st, map[index.IndexType]index.Indexer{index.IndexTypeAST: idx})
	repo := &domain.Repo{ID: "r1", Name: "t", Source: domain.SourceTypeLocal, Path: dir}

	if err := svc.doIndex(ctx, repo, false); err != nil {
		t.Fatal(err)
	}

	// Second pass: the file is unchanged, so it is skipped and the indexer
	// sees nothing.
	st.mu.Lock()
	st.serveStoredFiles = true
	st.mu.Unlock()
	idx.counts = map[string]domain.CoverageCounts{domain.ContractKindHTTP: {}}

	if err := svc.doIndex(ctx, repo, false); err != nil {
		t.Fatal(err)
	}
	report, err := svc.RepoCoverage(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range report.Kinds {
		if k.Kind == domain.ContractKindHTTP && k.Candidates != 40 {
			t.Fatalf("http coverage after a partial pass = %+v, want the full pass's 40", k)
		}
	}
}
