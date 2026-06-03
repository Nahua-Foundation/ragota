package hybrid

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sort"
	"testing"
)

// ---------------------------------------------------------------------------
// Тестовые моки ретриверов.
//
// VectorRetriever скрывает Qdrant, BM25Retriever — Bleve. В юнит-тестах мы
// никогда не поднимаем эти сервисы: проверяется только логика Engine/Merge.
// ---------------------------------------------------------------------------

type stubVec struct {
	cands []Candidate
	err   error
	// последние аргументы — для проверки проброса.
	lastQuery  string
	lastLimit  int
	lastFilter map[string]any
}

func (s *stubVec) HybridCandidates(_ context.Context, q string, limit int, filter map[string]any) ([]Candidate, error) {
	s.lastQuery, s.lastLimit, s.lastFilter = q, limit, filter
	return s.cands, s.err
}

type stubLex struct {
	cands     []Candidate
	err       error
	lastQuery string
	lastLimit int
}

func (s *stubLex) HybridCandidates(_ context.Context, q string, limit int) ([]Candidate, error) {
	s.lastQuery, s.lastLimit = q, limit
	return s.cands, s.err
}

// ---------------------------------------------------------------------------
// Merge: проверяем RRF, weighted-sum, дедуп и упорядочивание.
// ---------------------------------------------------------------------------

func TestMerge_RRFDeduplicatesAndOrders(t *testing.T) {
	// Кандидат "a" есть в обеих выдачах — должен подняться выше суммой RRF;
	// "b" только в vec, "c" только в lex.
	vec := []Candidate{
		{ID: "a", Content: "A", Rank: 1, Source: SrcVector},
		{ID: "b", Content: "B", Rank: 2, Source: SrcVector},
	}
	lex := []Candidate{
		{ID: "a", Rank: 1, Source: SrcBM25}, // Content пуст — должен подцепить из vec
		{ID: "c", Content: "C", Rank: 2, Source: SrcBM25},
	}

	got := Merge(vec, lex, Options{})
	if len(got) != 3 {
		t.Fatalf("expected 3 unique results, got %d: %+v", len(got), got)
	}
	if got[0].ID != "a" {
		t.Errorf("expected 'a' to be top after RRF fusion, got %q", got[0].ID)
	}
	// Сохранили контент даже когда дубликат пришёл без него.
	for _, r := range got {
		if r.ID == "a" && r.Content != "A" {
			t.Errorf("expected merged 'a' to keep content 'A', got %q", r.Content)
		}
	}

	// Сортировка должна быть строго невозрастающей по HybridScore.
	for i := 1; i < len(got); i++ {
		if got[i-1].HybridScore < got[i].HybridScore {
			t.Errorf("results not sorted desc by HybridScore: %+v", got)
		}
	}
}

func TestMerge_RRFScoreFormula(t *testing.T) {
	const k = 60
	vec := []Candidate{{ID: "x", Rank: 1}}
	lex := []Candidate{{ID: "x", Rank: 3}}
	got := Merge(vec, lex, Options{RRFK: k})
	if len(got) != 1 {
		t.Fatalf("expected single merged result, got %d", len(got))
	}
	want := 1.0/float64(k+1) + 1.0/float64(k+3)
	if math.Abs(got[0].HybridScore-want) > 1e-9 {
		t.Errorf("RRF score mismatch: got %v, want %v", got[0].HybridScore, want)
	}
}

func TestMerge_RRFUsesPositionWhenRankZero(t *testing.T) {
	// Когда Rank==0, Merge должен использовать индекс (1-based) как ранг.
	vec := []Candidate{
		{ID: "a"}, // pos=1 → rank=1
		{ID: "b"}, // pos=2 → rank=2
	}
	got := Merge(vec, nil, Options{RRFK: 10})
	scores := map[string]float64{}
	for _, r := range got {
		scores[r.ID] = r.HybridScore
	}
	want := map[string]float64{
		"a": 1.0 / 11.0,
		"b": 1.0 / 12.0,
	}
	for id, w := range want {
		if math.Abs(scores[id]-w) > 1e-9 {
			t.Errorf("score for %q: got %v, want %v", id, scores[id], w)
		}
	}
}

func TestMerge_WeightedSumNormalizesByMax(t *testing.T) {
	vec := []Candidate{
		{ID: "a", Score: 1.0},
		{ID: "b", Score: 0.5},
	}
	lex := []Candidate{
		{ID: "a", Score: 4.0},
		{ID: "c", Score: 2.0},
	}
	got := Merge(vec, lex, Options{VectorWeight: 1, BM25Weight: 0.5})

	// Ожидаемые скоры:
	// a: 1*(1.0/1.0) + 0.5*(4.0/4.0) = 1.5
	// b: 1*(0.5/1.0)                  = 0.5
	// c:                 0.5*(2.0/4.0) = 0.25
	want := map[string]float64{"a": 1.5, "b": 0.5, "c": 0.25}
	for _, r := range got {
		if math.Abs(r.HybridScore-want[r.ID]) > 1e-9 {
			t.Errorf("weighted score for %q: got %v, want %v", r.ID, r.HybridScore, want[r.ID])
		}
	}
	if got[0].ID != "a" {
		t.Errorf("expected 'a' to be top in weighted mode, got %q", got[0].ID)
	}
}

func TestMerge_EmptyInputs(t *testing.T) {
	if got := Merge(nil, nil, Options{}); len(got) != 0 {
		t.Errorf("expected empty merge for empty inputs, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Engine.Search.
// ---------------------------------------------------------------------------

func TestNew_DefaultRRFK(t *testing.T) {
	e := New(nil, nil, Options{})
	if e.Opts.RRFK != 60 {
		t.Errorf("expected default RRFK=60, got %d", e.Opts.RRFK)
	}
	e2 := New(nil, nil, Options{RRFK: 7})
	if e2.Opts.RRFK != 7 {
		t.Errorf("expected RRFK to be preserved, got %d", e2.Opts.RRFK)
	}
}

func TestEngine_SearchMergesAndLimits(t *testing.T) {
	vec := &stubVec{cands: []Candidate{
		{ID: "1", Rank: 1, Content: "one"},
		{ID: "2", Rank: 2, Content: "two"},
	}}
	lex := &stubLex{cands: []Candidate{
		{ID: "2", Rank: 1, Content: "two-lex"},
		{ID: "3", Rank: 2, Content: "three"},
	}}
	e := New(vec, lex, Options{})

	got, err := e.Search(context.Background(), "q", 0 /* default */, 2, map[string]any{"lang": "go"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected limit=2 results, got %d", len(got))
	}
	if got[0].ID != "2" {
		t.Errorf("expected '2' (present in both) to win RRF, got %q", got[0].ID)
	}

	// Проверяем, что параметры реально прокинулись в моки.
	if vec.lastQuery != "q" || lex.lastQuery != "q" {
		t.Errorf("query not propagated: vec=%q lex=%q", vec.lastQuery, lex.lastQuery)
	}
	if vec.lastLimit != 50 || lex.lastLimit != 50 {
		t.Errorf("default candidatesPerSource should be 50, got vec=%d lex=%d", vec.lastLimit, lex.lastLimit)
	}
	if !reflect.DeepEqual(vec.lastFilter, map[string]any{"lang": "go"}) {
		t.Errorf("filter not propagated to vector retriever: %+v", vec.lastFilter)
	}
}

func TestEngine_SearchPropagatesVectorError(t *testing.T) {
	wantErr := errors.New("qdrant down")
	e := New(&stubVec{err: wantErr}, &stubLex{}, Options{})
	if _, err := e.Search(context.Background(), "q", 5, 5, nil); !errors.Is(err, wantErr) {
		t.Errorf("expected vector error to be propagated, got %v", err)
	}
}

func TestEngine_SearchPropagatesLexError(t *testing.T) {
	wantErr := errors.New("bleve down")
	e := New(&stubVec{}, &stubLex{err: wantErr}, Options{})
	if _, err := e.Search(context.Background(), "q", 5, 5, nil); !errors.Is(err, wantErr) {
		t.Errorf("expected lex error to be propagated, got %v", err)
	}
}

func TestEngine_SearchNilEngineOrNoRetrievers(t *testing.T) {
	var e *Engine
	if _, err := e.Search(context.Background(), "q", 5, 5, nil); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("expected ErrNotImplemented for nil engine, got %v", err)
	}
	e2 := New(nil, nil, Options{})
	if _, err := e2.Search(context.Background(), "q", 5, 5, nil); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("expected ErrNotImplemented when no retrievers, got %v", err)
	}
}

func TestEngine_SearchOnlyOneRetrieverPresent(t *testing.T) {
	// Только vector — Search должен отработать без ошибок и вернуть отсортированную выдачу.
	vec := &stubVec{cands: []Candidate{
		{ID: "a", Rank: 2},
		{ID: "b", Rank: 1},
	}}
	e := New(vec, nil, Options{})
	got, err := e.Search(context.Background(), "q", 10, 0 /* no limit */, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d", len(got))
	}
	// 'b' имеет ранг 1 → должен быть сверху.
	if got[0].ID != "b" {
		t.Errorf("expected 'b' on top (rank=1), got %q", got[0].ID)
	}
	// Проверим, что результат отсортирован.
	sorted := make([]float64, len(got))
	for i, r := range got {
		sorted[i] = r.HybridScore
	}
	if !sort.SliceIsSorted(sorted, func(i, j int) bool { return sorted[i] > sorted[j] }) {
		t.Errorf("results not sorted desc: %+v", sorted)
	}
}
