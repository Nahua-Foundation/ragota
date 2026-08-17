package service

import (
	"context"
	"strconv"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// refStorage answers the two lookups References makes and records the limit
// each of them was given, which is the thing under test: the endpoint used to
// hand the caller's limit to both.
type refStorage struct {
	*mockStorage
	unit       *storage.ASTUnit
	resolved   []*storage.Edge
	unresolved []*storage.Edge
	// limits are the Limit of every GetEdges call, resolved lookup first.
	limits []int
}

func (r *refStorage) GetASTUnits(context.Context, storage.QueryOpts) ([]*storage.ASTUnit, error) {
	if r.unit == nil {
		return nil, nil
	}
	return []*storage.ASTUnit{r.unit}, nil
}

func (r *refStorage) GetEdges(_ context.Context, opts storage.QueryOpts) ([]*storage.Edge, error) {
	r.limits = append(r.limits, opts.Limit)
	src := r.resolved
	if opts.Unresolved {
		src = r.unresolved
	}
	if opts.Limit > 0 && len(src) > opts.Limit {
		src = src[:opts.Limit]
	}
	return src, nil
}

// edges builds n edges tagged with the given prefix, so a test can tell which
// lookup a reference came from.
func edges(prefix string, n int) []*storage.Edge {
	out := make([]*storage.Edge, n)
	for i := range out {
		out[i] = &storage.Edge{ID: prefix + strconv.Itoa(i), RepoID: "r1", Kind: "call", DstName: "Handle"}
	}
	return out
}

func refService(resolved, unresolved int) (*Service, *refStorage) {
	st := &refStorage{
		mockStorage: &mockStorage{},
		unit:        &storage.ASTUnit{ID: "u1", RepoID: "r1", Name: "Handle", StartLine: 10, EndLine: 20},
		resolved:    edges("resolved-", resolved),
		unresolved:  edges("unresolved-", unresolved),
	}
	return &Service{storage: st}, st
}

// TestReferencesLimitIsTheWholeAnswer: limit used to bound each of the two
// lookups, so a request for ten could be answered with nineteen — nine resolved
// plus ten unresolved.
func TestReferencesLimitIsTheWholeAnswer(t *testing.T) {
	svc, st := refService(9, 40)

	refs, err := svc.References(context.Background(), "r1", "src/handler.go", 12, 10)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	if len(refs) != 10 {
		t.Fatalf("references = %d, want the 10 that were asked for", len(refs))
	}
	// The resolved side is taken whole first: it names this unit, where an
	// unresolved edge merely shares its name.
	for i := 0; i < 9; i++ {
		if refs[i].ID != "resolved-"+strconv.Itoa(i) {
			t.Fatalf("reference %d is %q, want the resolved edges first", i, refs[i].ID)
		}
	}
	if refs[9].ID != "unresolved-0" {
		t.Errorf("last reference is %q, want the unresolved lookup to fill the one remaining slot", refs[9].ID)
	}
	if len(st.limits) != 2 || st.limits[0] != 10 || st.limits[1] != 1 {
		t.Errorf("lookup limits = %v, want the full 10 then the 1 left over", st.limits)
	}
}

// TestReferencesSkipsTheSecondLookupWhenFull: a budget the resolved edges fill
// on their own buys nothing from a second query.
func TestReferencesSkipsTheSecondLookupWhenFull(t *testing.T) {
	svc, st := refService(20, 20)

	refs, err := svc.References(context.Background(), "r1", "src/handler.go", 12, 5)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	if len(refs) != 5 {
		t.Errorf("references = %d, want 5", len(refs))
	}
	if len(st.limits) != 1 {
		t.Errorf("made %d lookups, want only the resolved one", len(st.limits))
	}
}

// TestReferencesDefaultsAndClamps: an absent limit used to mean "every edge in
// the table", twice over, which is the one answer the endpoint must never give
// — /nav/references is in the same API whose responses are budgeted.
func TestReferencesDefaultsAndClamps(t *testing.T) {
	tests := []struct {
		name  string
		limit int
		want  int
	}{
		{"absent", 0, defaultReferenceLimit},
		{"negative", -1, defaultReferenceLimit},
		{"over the ceiling", maxReferenceLimit * 10, maxReferenceLimit},
		{"under the ceiling", 7, 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, st := refService(0, maxReferenceLimit*20)

			refs, err := svc.References(context.Background(), "r1", "src/handler.go", 12, tt.limit)
			if err != nil {
				t.Fatalf("References: %v", err)
			}
			if len(refs) != tt.want {
				t.Errorf("references = %d, want %d", len(refs), tt.want)
			}
			for _, got := range st.limits {
				if got > tt.want {
					t.Errorf("a lookup was given limit %d, over the %d the answer may hold", got, tt.want)
				}
			}
		})
	}
}

// TestReferencesNoDefinition: nothing at that line is an answer, and it must
// not turn into an unfiltered edge query.
func TestReferencesNoDefinition(t *testing.T) {
	st := &refStorage{mockStorage: &mockStorage{}, unresolved: edges("unresolved-", 5)}
	svc := &Service{storage: st}

	refs, err := svc.References(context.Background(), "r1", "src/handler.go", 12, 0)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	if len(refs) != 0 || len(st.limits) != 0 {
		t.Errorf("references = %d after %d lookups, want an empty answer and no query",
			len(refs), len(st.limits))
	}
}
