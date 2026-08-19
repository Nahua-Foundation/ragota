package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

func TestPostgresRepoCoverage(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	repoID := fmt.Sprintf("cov-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = st.DeleteRepoCoverage(ctx, repoID) })

	if _, err := st.GetRepoCoverage(ctx, repoID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRepoCoverage() before any pass = %v, want ErrNotFound", err)
	}

	if err := st.StoreRepoCoverage(ctx, &domain.RepoCoverage{
		RepoID:    repoID,
		UpdatedAt: 100,
		Kinds: map[string]domain.CoverageCounts{
			domain.ContractKindHTTP: {Candidates: 3000, Edges: 104},
			domain.ContractKindRPC:  {Candidates: 7, Edges: 7},
		},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetRepoCoverage(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if c := got.Kinds[domain.ContractKindHTTP]; c.Candidates != 3000 || c.Edges != 104 {
		t.Errorf("http counts = %+v, want 104 of 3000", c)
	}

	// A re-index replaces the whole summary rather than merging into it.
	if err := st.StoreRepoCoverage(ctx, &domain.RepoCoverage{
		RepoID:    repoID,
		UpdatedAt: 200,
		Kinds:     map[string]domain.CoverageCounts{domain.ContractKindHTTP: {Candidates: 42, Edges: 42}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetRepoCoverage(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Kinds) != 1 || got.UpdatedAt != 200 {
		t.Fatalf("coverage after re-index = %+v, want only the new pass's counters", got)
	}

	if err := st.DeleteRepoCoverage(ctx, repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetRepoCoverage(ctx, repoID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRepoCoverage() after delete = %v, want ErrNotFound", err)
	}
}
