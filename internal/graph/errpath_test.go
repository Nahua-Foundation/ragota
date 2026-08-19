package graph

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// failEdgesStore wraps a real store but fails every edge query, standing in for
// a database that breaks mid-trace.
type failEdgesStore struct {
	store.Storage
	err error
}

func (f failEdgesStore) GetEdges(context.Context, domain.QueryOpts) ([]*domain.Edge, error) {
	return nil, f.err
}

// TestTraceSurfacesStorageError: a storage failure during a trace must be
// returned, not swallowed into an empty "no edges" result that reads as a
// complete walk ending at a real sink.
func TestTraceSurfacesStorageError(t *testing.T) {
	st := openTestStore(t)
	storeFunc(t, st, "r1", "A", "(userID string)")

	g := New(failEdgesStore{Storage: st, err: errors.New("boom")})
	_, err := g.Trace(context.Background(), &TraceRequest{RepoID: "r1", Symbol: "A", Param: "userID"})
	if err == nil {
		t.Fatal("Trace should surface the storage error, got nil")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want it to wrap the storage error", err)
	}
}
