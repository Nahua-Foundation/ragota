package search

import (
	"context"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/index"
)

func convexService(t *testing.T, vectorWeight, keywordWeight float32, vector, keyword []*index.Hit) *Service {
	t.Helper()
	return New(map[index.IndexType]index.Searcher{
		index.IndexTypeVector: &mockSearcher{name: index.IndexTypeVector, hits: vector},
		index.IndexTypeBM25:   &mockSearcher{name: index.IndexTypeBM25, hits: keyword},
	}, &Config{
		Method: FusionConvex,
		Weights: map[index.IndexType]float32{
			index.IndexTypeVector: vectorWeight,
			index.IndexTypeBM25:   keywordWeight,
		},
	})
}

// TestConvexNormalisesEachLegBeforeAdding: BM25 scores are unbounded and cosine
// similarities sit near one, so the point of this fusion is that the keyword
// leg's units cannot decide the ranking. Both legs here rank their own list the
// same way; only the raw magnitudes differ, and they must not matter.
func TestConvexNormalisesEachLegBeforeAdding(t *testing.T) {
	vector := []*index.Hit{
		{RepoID: "r1", FilePath: "a.go", Line: 1, EndLine: 10, Score: 0.81, Reason: "semantic"},
		{RepoID: "r1", FilePath: "b.go", Line: 1, EndLine: 10, Score: 0.79, Reason: "semantic"},
	}
	keyword := []*index.Hit{
		{RepoID: "r1", FilePath: "b.go", Line: 1, EndLine: 10, Score: 42.0, Reason: "keyword"},
		{RepoID: "r1", FilePath: "c.go", Line: 1, EndLine: 10, Score: 11.0, Reason: "keyword"},
	}

	svc := convexService(t, 0.5, 0.5, vector, keyword)
	result, err := svc.Search(context.Background(), &index.SearchQuery{Query: "q", Limit: 10}, true)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(result.Hits) != 3 {
		t.Fatalf("got %d hits, want 3", len(result.Hits))
	}
	// b.go is the only file both legs found: top of the keyword list, bottom of
	// the vector one. Agreement wins, as it does under RRF.
	if got := result.Hits[0].FilePath; got != "b.go" {
		t.Errorf("first hit = %q, want b.go — the file both legs returned", got)
	}
	if result.Metadata["fusion"] != string(FusionConvex) {
		t.Errorf("metadata fusion = %v, want convex", result.Metadata["fusion"])
	}
}

// TestConvexWeightsAreProportions is the difference from RRF that motivates
// this function, and it is measured here against RRF on the same input.
//
// A weight under RRF compares positions, so a leg weighted at a tenth of the
// other has its entire score range below the other's last rank: the heavy leg's
// second hit wins even when the heavy leg itself scored it as near-worthless,
// and the light leg can then only confirm results, never contribute them.
// Weighted score fusion asks instead how good each hit was, so a tenth of the
// total still beats a hit that earned a twentieth of its own leg's.
func TestConvexWeightsAreProportions(t *testing.T) {
	vector := func() []*index.Hit {
		return []*index.Hit{
			{RepoID: "r1", FilePath: "v-strong.go", Line: 1, EndLine: 10, Score: 0.9, Reason: "semantic"},
			{RepoID: "r1", FilePath: "v-weak.go", Line: 1, EndLine: 10, Score: 0.05, Reason: "semantic"},
		}
	}
	keyword := func() []*index.Hit {
		return []*index.Hit{
			{RepoID: "r1", FilePath: "k-strong.go", Line: 1, EndLine: 10, Score: 9.0, Reason: "keyword"},
		}
	}
	order := func(svc *Service) []string {
		result, err := svc.Search(context.Background(), &index.SearchQuery{Query: "q", Limit: 10}, true)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		paths := make([]string, len(result.Hits))
		for i, h := range result.Hits {
			paths[i] = h.FilePath
		}
		return paths
	}

	convex := order(convexService(t, 0.9, 0.1, vector(), keyword()))
	want := []string{"v-strong.go", "k-strong.go", "v-weak.go"}
	for i, w := range want {
		if i >= len(convex) || convex[i] != w {
			t.Fatalf("convex order = %v, want %v", convex, want)
		}
	}

	rrf := order(New(map[index.IndexType]index.Searcher{
		index.IndexTypeVector: &mockSearcher{name: index.IndexTypeVector, hits: vector()},
		index.IndexTypeBM25:   &mockSearcher{name: index.IndexTypeBM25, hits: keyword()},
	}, &Config{
		Method:  FusionRRF,
		RRFK:    60,
		Weights: map[index.IndexType]float32{index.IndexTypeVector: 0.9, index.IndexTypeBM25: 0.1},
	}))
	if rrf[len(rrf)-1] != "k-strong.go" {
		t.Errorf("rrf order = %v; the weighted-down leg was expected to fall to the bottom — "+
			"if it no longer does, the reason convex fusion exists has changed", rrf)
	}
}

// TestConvexHandlesATiedLeg: a source whose hits all carry the same score says
// nothing about their order. Normalising that would divide by zero; the rule is
// that such a leg gives every one of its hits its full weight.
func TestConvexHandlesATiedLeg(t *testing.T) {
	tied := []*index.Hit{
		{RepoID: "r1", FilePath: "a.go", Line: 1, EndLine: 10, Score: 1.0, Reason: "keyword"},
		{RepoID: "r1", FilePath: "b.go", Line: 1, EndLine: 10, Score: 1.0, Reason: "keyword"},
	}
	svc := New(map[index.IndexType]index.Searcher{
		index.IndexTypeBM25: &mockSearcher{name: index.IndexTypeBM25, hits: tied},
	}, &Config{Method: FusionConvex})

	result, err := svc.Search(context.Background(), &index.SearchQuery{Query: "q", Limit: 10}, true)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(result.Hits) != 2 {
		t.Fatalf("got %d hits, want both", len(result.Hits))
	}
	for _, h := range result.Hits {
		if h.Score != 1.0 {
			t.Errorf("%s scored %v, want the full weight 1.0", h.FilePath, h.Score)
		}
	}
}

// TestDefaultFusionStaysRRF: convex is an experiment until it is measured, and
// the default has to stay the configuration every recorded number was produced
// under.
func TestDefaultFusionStaysRRF(t *testing.T) {
	if got := DefaultConfig().Method; got != FusionRRF {
		t.Errorf("default fusion = %q, want %q", got, FusionRRF)
	}
	svc := New(map[index.IndexType]index.Searcher{
		index.IndexTypeBM25: &mockSearcher{name: index.IndexTypeBM25, hits: rerankHits()},
	}, nil)
	if got := svc.method(); got != FusionRRF {
		t.Errorf("a nil config fuses with %q, want %q", got, FusionRRF)
	}
}
