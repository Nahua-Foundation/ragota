package hybrid

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Source constants
// ---------------------------------------------------------------------------

func TestSourceConstants(t *testing.T) {
	assert.Equal(t, Source("vector"), SrcVector)
	assert.Equal(t, Source("bm25"), SrcBM25)
}

// ---------------------------------------------------------------------------
// maxScore helper
// ---------------------------------------------------------------------------

func TestMaxScore_Empty(t *testing.T) {
	assert.Equal(t, 0.0, maxScore(nil))
}

func TestMaxScore_SingleElement(t *testing.T) {
	assert.Equal(t, 5.0, maxScore([]Candidate{{Score: 5.0}}))
}

func TestMaxScore_MultipleElements(t *testing.T) {
	cs := []Candidate{{Score: 0.1}, {Score: 0.9}, {Score: 0.5}}
	assert.InDelta(t, 0.9, maxScore(cs), 1e-9)
}

func TestMaxScore_AllZeros(t *testing.T) {
	cs := []Candidate{{Score: 0}, {Score: 0}}
	assert.Equal(t, 0.0, maxScore(cs))
}

func TestMaxScore_NegativeScores(t *testing.T) {
	cs := []Candidate{{Score: -1}, {Score: -0.5}}
	// All negative, maxScore starts at 0 so returns 0.
	assert.Equal(t, 0.0, maxScore(cs))
}

// ---------------------------------------------------------------------------
// Merge — additional edge cases
// ---------------------------------------------------------------------------

func TestMerge_OnlyVector(t *testing.T) {
	vec := []Candidate{
		{ID: "a", Content: "A", Rank: 1},
		{ID: "b", Content: "B", Rank: 2},
	}
	got := Merge(vec, nil, Options{RRFK: 60})
	require.Len(t, got, 2)
	assert.Equal(t, "a", got[0].ID) // rank 1 > rank 2
}

func TestMerge_OnlyLex(t *testing.T) {
	lex := []Candidate{
		{ID: "x", Content: "X", Rank: 1},
	}
	got := Merge(nil, lex, Options{RRFK: 60})
	require.Len(t, got, 1)
	assert.Equal(t, "x", got[0].ID)
}

func TestMerge_BothEmpty(t *testing.T) {
	got := Merge(nil, nil, Options{})
	assert.Empty(t, got)
}

func TestMerge_EmptySlices(t *testing.T) {
	got := Merge([]Candidate{}, []Candidate{}, Options{})
	assert.Empty(t, got)
}

func TestMerge_DuplicateIDContentFilledFromSecondSource(t *testing.T) {
	// vec has empty content, lex has content.
	vec := []Candidate{{ID: "a", Content: "", Rank: 1}}
	lex := []Candidate{{ID: "a", Content: "full content", Rank: 2}}
	got := Merge(vec, lex, Options{})
	require.Len(t, got, 1)
	assert.Equal(t, "full content", got[0].Content)
}

func TestMerge_DuplicateIDBothHaveContent_FirstWins(t *testing.T) {
	vec := []Candidate{{ID: "a", Content: "vec content", Rank: 1}}
	lex := []Candidate{{ID: "a", Content: "lex content", Rank: 2}}
	got := Merge(vec, lex, Options{})
	require.Len(t, got, 1)
	// First source (vec) sets content; second won't overwrite non-empty.
	assert.Equal(t, "vec content", got[0].Content)
}

func TestMerge_WeightedSum_ZeroMaxScore(t *testing.T) {
	// All scores are 0 → normMax = 0 → no score added.
	vec := []Candidate{{ID: "a", Score: 0}}
	lex := []Candidate{{ID: "b", Score: 0}}
	got := Merge(vec, lex, Options{VectorWeight: 1, BM25Weight: 1})
	require.Len(t, got, 2)
	for _, r := range got {
		assert.Equal(t, 0.0, r.HybridScore)
	}
}

func TestMerge_WeightedSum_OnlyVectorWeight(t *testing.T) {
	// Only VectorWeight > 0, BM25Weight = 0 → NOT weighted mode (both must be > 0).
	vec := []Candidate{{ID: "a", Score: 0.8, Rank: 1}}
	got := Merge(vec, nil, Options{VectorWeight: 1, BM25Weight: 0, RRFK: 60})
	require.Len(t, got, 1)
	// Should use RRF since both weights aren't > 0.
	assert.InDelta(t, 1.0/61.0, got[0].HybridScore, 1e-9)
}

func TestMerge_RRFK_ZeroDefaultsTo60ViaNew(t *testing.T) {
	// Direct Merge call with RRFK=0 — the formula uses RRFK+rank.
	// With RRFK=0 and rank=1: 1/1 = 1.0.
	vec := []Candidate{{ID: "a", Rank: 1}}
	got := Merge(vec, nil, Options{RRFK: 0})
	require.Len(t, got, 1)
	assert.InDelta(t, 1.0, got[0].HybridScore, 1e-9)
}

func TestMerge_LargeNumberOfCandidates(t *testing.T) {
	var vec, lex []Candidate
	for i := 0; i < 50; i++ {
		id := "item-" + string(rune('A'+i/26)) + string(rune('a'+i%26))
		vec = append(vec, Candidate{ID: id, Content: "v", Rank: i + 1})
		lex = append(lex, Candidate{ID: id + "-lex", Content: "l", Rank: i + 1})
	}
	got := Merge(vec, lex, Options{RRFK: 60})
	assert.Len(t, got, 100) // All unique IDs (50 vec + 50 lex).
	// Verify sorted descending.
	for i := 1; i < len(got); i++ {
		assert.GreaterOrEqual(t, got[i-1].HybridScore, got[i].HybridScore)
	}
}

// ---------------------------------------------------------------------------
// New — constructor
// ---------------------------------------------------------------------------

func TestNew_DefaultRRFKApplied(t *testing.T) {
	e := New(nil, nil, Options{})
	assert.Equal(t, 60, e.Opts.RRFK)
}

func TestNew_NegativeRRFKDefaultsTo60(t *testing.T) {
	e := New(nil, nil, Options{RRFK: -5})
	assert.Equal(t, 60, e.Opts.RRFK)
}

func TestNew_PreservesCustomRRFK(t *testing.T) {
	e := New(nil, nil, Options{RRFK: 30})
	assert.Equal(t, 30, e.Opts.RRFK)
}

func TestNew_StoresRetrievers(t *testing.T) {
	vec := &stubVec{}
	lex := &stubLex{}
	e := New(vec, lex, Options{})
	assert.Same(t, vec, e.Vec)
	assert.Same(t, lex, e.Lex)
}

// ---------------------------------------------------------------------------
// Engine.Search — additional cases
// ---------------------------------------------------------------------------

func TestSearch_CustomCandidatesPerSource(t *testing.T) {
	vec := &stubVec{cands: []Candidate{{ID: "a", Rank: 1}}}
	lex := &stubLex{cands: []Candidate{{ID: "b", Rank: 1}}}
	e := New(vec, lex, Options{})

	_, err := e.Search(context.Background(), "q", 25, 0, nil)
	require.NoError(t, err)
	assert.Equal(t, 25, vec.lastLimit)
	assert.Equal(t, 25, lex.lastLimit)
}

func TestSearch_LimitZeroReturnsAll(t *testing.T) {
	vec := &stubVec{cands: []Candidate{
		{ID: "a", Rank: 1},
		{ID: "b", Rank: 2},
		{ID: "c", Rank: 3},
	}}
	e := New(vec, nil, Options{})
	got, err := e.Search(context.Background(), "q", 10, 0, nil)
	require.NoError(t, err)
	assert.Len(t, got, 3)
}

func TestSearch_LimitGreaterThanResults(t *testing.T) {
	vec := &stubVec{cands: []Candidate{{ID: "a", Rank: 1}}}
	e := New(vec, nil, Options{})
	got, err := e.Search(context.Background(), "q", 10, 100, nil)
	require.NoError(t, err)
	assert.Len(t, got, 1)
}

func TestSearch_NilEngine(t *testing.T) {
	var e *Engine
	_, err := e.Search(context.Background(), "q", 5, 5, nil)
	assert.ErrorIs(t, err, ErrNotImplemented)
}

func TestSearch_BothRetrieversNil(t *testing.T) {
	e := New(nil, nil, Options{})
	_, err := e.Search(context.Background(), "q", 5, 5, nil)
	assert.ErrorIs(t, err, ErrNotImplemented)
}

func TestSearch_VectorErrorPropagates(t *testing.T) {
	wantErr := errors.New("vector down")
	e := New(&stubVec{err: wantErr}, &stubLex{}, Options{})
	_, err := e.Search(context.Background(), "q", 5, 5, nil)
	assert.ErrorIs(t, err, wantErr)
}

func TestSearch_LexErrorPropagates(t *testing.T) {
	wantErr := errors.New("bm25 down")
	e := New(&stubVec{}, &stubLex{err: wantErr}, Options{})
	_, err := e.Search(context.Background(), "q", 5, 5, nil)
	assert.ErrorIs(t, err, wantErr)
}

func TestSearch_OnlyLexRetriever(t *testing.T) {
	lex := &stubLex{cands: []Candidate{
		{ID: "x", Rank: 1, Content: "X"},
		{ID: "y", Rank: 2, Content: "Y"},
	}}
	e := New(nil, lex, Options{})
	got, err := e.Search(context.Background(), "q", 10, 0, nil)
	require.NoError(t, err)
	assert.Len(t, got, 2)
	assert.Equal(t, "x", got[0].ID)
}

func TestSearch_FilterPropagatedToVector(t *testing.T) {
	vec := &stubVec{cands: []Candidate{{ID: "a", Rank: 1}}}
	e := New(vec, nil, Options{})
	filter := map[string]any{"lang": "go", "repo": "main"}
	_, err := e.Search(context.Background(), "q", 10, 10, filter)
	require.NoError(t, err)
	assert.Equal(t, "go", vec.lastFilter["lang"])
	assert.Equal(t, "main", vec.lastFilter["repo"])
}

func TestSearch_EmptyResults(t *testing.T) {
	vec := &stubVec{cands: nil}
	lex := &stubLex{cands: nil}
	e := New(vec, lex, Options{})
	got, err := e.Search(context.Background(), "q", 10, 10, nil)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// ---------------------------------------------------------------------------
// ErrNotImplemented
// ---------------------------------------------------------------------------

func TestErrNotImplemented_Message(t *testing.T) {
	assert.Contains(t, ErrNotImplemented.Error(), "not implemented")
}
