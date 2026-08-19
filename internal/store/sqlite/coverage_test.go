package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

func TestRepoCoverageRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTestDB(t)

	if _, err := s.GetRepoCoverage(ctx, "repo-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRepoCoverage() before any pass = %v, want ErrNotFound", err)
	}

	first := &domain.RepoCoverage{
		RepoID:    "repo-1",
		UpdatedAt: 100,
		Kinds: map[string]domain.CoverageCounts{
			domain.ContractKindHTTP:      {Candidates: 3000, Edges: 104},
			domain.ContractKindMessaging: {Candidates: 8, Edges: 8},
		},
	}
	if err := s.StoreRepoCoverage(ctx, first); err != nil {
		t.Fatalf("StoreRepoCoverage() error = %v", err)
	}
	got, err := s.GetRepoCoverage(ctx, "repo-1")
	if err != nil {
		t.Fatalf("GetRepoCoverage() error = %v", err)
	}
	if got.UpdatedAt != 100 || len(got.Kinds) != 2 {
		t.Fatalf("coverage = %+v, want 2 kinds written at 100", got)
	}
	if c := got.Kinds[domain.ContractKindHTTP]; c.Candidates != 3000 || c.Edges != 104 {
		t.Errorf("http counts = %+v, want 104 of 3000", c)
	}

	// A re-index replaces the summary: a kind the new pass no longer reports
	// must not survive as a counter from the previous one.
	second := &domain.RepoCoverage{
		RepoID:    "repo-1",
		UpdatedAt: 200,
		Kinds:     map[string]domain.CoverageCounts{domain.ContractKindHTTP: {Candidates: 42, Edges: 42}},
	}
	if err := s.StoreRepoCoverage(ctx, second); err != nil {
		t.Fatalf("StoreRepoCoverage() error = %v", err)
	}
	got, err = s.GetRepoCoverage(ctx, "repo-1")
	if err != nil {
		t.Fatalf("GetRepoCoverage() error = %v", err)
	}
	if len(got.Kinds) != 1 || got.Kinds[domain.ContractKindHTTP].Edges != 42 || got.UpdatedAt != 200 {
		t.Fatalf("coverage after re-index = %+v, want only the new pass's http counters", got)
	}

	if err := s.DeleteRepoCoverage(ctx, "repo-1"); err != nil {
		t.Fatalf("DeleteRepoCoverage() error = %v", err)
	}
	if _, err := s.GetRepoCoverage(ctx, "repo-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRepoCoverage() after delete = %v, want ErrNotFound", err)
	}
}
