package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/app/promote"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/index"
	"github.com/Nahua-Foundation/ragota/internal/store"
	"github.com/Nahua-Foundation/ragota/internal/store/sqlite"
)

// callersFixture seeds a store with a callee, three kinds of caller and one
// structural edge, and returns the store. The layout:
//
//	shipping/app.go        ShipOrder (the callee, lines 40-80)
//	checkout/main.go:25        resolved call edge (the answer)
//	shipping/service_test.go   resolved call edge from a test (ranked after)
//	legacy/old.go:7            unresolved call edge by name
//	checkout/main.go           import edge (structural, never promoted)
func callersFixture(t *testing.T) (*sqlite.SQLite, *domain.ASTUnit) {
	t.Helper()
	st, err := sqlite.Open(&sqlite.Config{Path: filepath.Join(t.TempDir(), "callers.db"), PoolSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	callee := &domain.ASTUnit{
		RepoID: "r1", FilePath: "shipping/app.go", Language: "go",
		Kind: "method", Name: "ShipOrder", Qualified: "shipping.ShipOrder",
		StartLine: 40, EndLine: 80,
	}
	caller := &domain.ASTUnit{
		RepoID: "r1", FilePath: "checkout/main.go", Language: "go",
		Kind: "function", Name: "placeOrder", Qualified: "checkout.placeOrder",
		StartLine: 10, EndLine: 50,
	}
	testCaller := &domain.ASTUnit{
		RepoID: "r1", FilePath: "shipping/service_test.go", Language: "go",
		Kind: "function", Name: "TestShipOrder", Qualified: "shipping.TestShipOrder",
		StartLine: 5, EndLine: 30,
	}
	for _, u := range []*domain.ASTUnit{callee, caller, testCaller} {
		if err := st.StoreASTUnit(ctx, u); err != nil {
			t.Fatal(err)
		}
	}

	edges := []*domain.Edge{
		{RepoID: "r1", SrcID: caller.ID, DstID: callee.ID, Kind: store.EdgeCall,
			DstName: "ShipOrder", FilePath: "checkout/main.go", Line: 25, Confidence: 0.9},
		{RepoID: "r1", SrcID: testCaller.ID, DstID: callee.ID, Kind: store.EdgeCall,
			DstName: "ShipOrder", FilePath: "shipping/service_test.go", Line: 12, Confidence: 0.9},
		{RepoID: "r1", SrcID: caller.ID, Kind: store.EdgeCall,
			DstName: "ShipOrder", FilePath: "legacy/old.go", Line: 7, Confidence: 0.7},
		{RepoID: "r1", SrcID: caller.ID, DstID: callee.ID, Kind: store.EdgeImport,
			DstName: "shipping", FilePath: "checkout/main.go", Line: 3, Confidence: 1.0},
	}
	for _, e := range edges {
		if err := st.StoreEdge(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	return st, callee
}

func TestSearchPromotesCallers(t *testing.T) {
	st, callee := callersFixture(t)

	// Retrieval finds the callee's own body for the stripped description —
	// the promotion has to turn that into the call sites.
	srch := &stubSearcher{byQuery: map[string][]*index.Hit{
		"shipping service ShipOrder rpc": {{
			RepoID: "r1", FilePath: callee.FilePath, Path: callee.FilePath,
			Line: 45, EndLine: 60, Score: 0.5, Reason: "keyword",
		}},
	}}
	svc := serviceWithSearcher(srch, st)
	defer svc.Close(context.Background())

	res, err := svc.Search(context.Background(), &index.SearchQuery{
		Query: "what calls the shipping service ShipOrder rpc",
		Repos: []string{"r1"},
		Limit: 10,
	}, "keyword")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(res.Hits) < 4 {
		t.Fatalf("got %d hits, want the three call sites plus the text hit:\n%+v", len(res.Hits), res.Hits)
	}

	// The resolved production call site is the first answer; the unresolved
	// production one still beats the confirmed call from a test, because
	// tests are almost never the answer; the import edge is never promoted.
	first := res.Hits[0]
	if first.FilePath != "checkout/main.go" || first.Line != 25 {
		t.Fatalf("first hit = %s:%d, want checkout/main.go:25", first.FilePath, first.Line)
	}
	if first.Symbol != "placeOrder" || first.Reason != "calls ShipOrder" {
		t.Errorf("first hit symbol/reason = %q/%q, want placeOrder/\"calls ShipOrder\"", first.Symbol, first.Reason)
	}
	// Without a snippet the reranker (and any LLM handed this as context)
	// judges a call site by its file path alone.
	for _, want := range []string{"placeOrder", "ShipOrder", "checkout/main.go:25"} {
		if !strings.Contains(first.Snippet, want) {
			t.Errorf("snippet does not mention %q:\n%s", want, first.Snippet)
		}
	}
	if res.Hits[1].FilePath != "legacy/old.go" || res.Hits[1].Line != 7 {
		t.Errorf("second hit = %s:%d, want the unresolved call site legacy/old.go:7", res.Hits[1].FilePath, res.Hits[1].Line)
	}
	if res.Hits[2].FilePath != "shipping/service_test.go" {
		t.Errorf("third hit = %s, want the test call site last", res.Hits[2].FilePath)
	}
	for _, h := range res.Hits {
		if h.Line == 3 && h.FilePath == "checkout/main.go" {
			t.Errorf("import edge was promoted: %+v", h)
		}
	}
	// The text hit survives after the promoted block.
	last := res.Hits[len(res.Hits)-1]
	if last.FilePath != callee.FilePath {
		t.Errorf("text hit missing from the tail, got %s", last.FilePath)
	}

	// Scores must reproduce the served order for clients that sort.
	for i := 1; i < len(res.Hits); i++ {
		if res.Hits[i-1].Score < res.Hits[i].Score {
			t.Errorf("scores out of order at %d: %f < %f", i, res.Hits[i-1].Score, res.Hits[i].Score)
		}
	}

	if res.Metadata["intent"] != promote.IntentCallers {
		t.Errorf("metadata intent = %v, want callers", res.Metadata["intent"])
	}
	callees, _ := res.Metadata["intent_callees"].([]string)
	if len(callees) == 0 || callees[0] != "ShipOrder" {
		t.Errorf("metadata intent_callees = %v, want [ShipOrder ...]", res.Metadata["intent_callees"])
	}
	if res.Metadata["intent_promoted"] != 3 {
		t.Errorf("metadata intent_promoted = %v, want 3", res.Metadata["intent_promoted"])
	}
	if res.Query != "what calls the shipping service ShipOrder rpc" {
		t.Errorf("result query = %q, want the original phrasing", res.Query)
	}
}

func TestSearchExplicitCallersIntentOnBareSymbol(t *testing.T) {
	st, _ := callersFixture(t)
	srch := &stubSearcher{byQuery: map[string][]*index.Hit{}} // retrieval finds nothing
	svc := serviceWithSearcher(srch, st)
	defer svc.Close(context.Background())

	res, err := svc.Search(context.Background(), &index.SearchQuery{
		Query: "ShipOrder", Intent: promote.IntentCallers, Repos: []string{"r1"}, Limit: 10,
	}, "keyword")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	// The symbol-name lookup alone resolves the callee: the call sites come
	// back even though text retrieval had nothing.
	if len(res.Hits) != 3 {
		t.Fatalf("got %d hits, want 3 call sites:\n%+v", len(res.Hits), res.Hits)
	}
	if res.Hits[0].FilePath != "checkout/main.go" || res.Hits[0].Line != 25 {
		t.Fatalf("first hit = %s:%d, want checkout/main.go:25", res.Hits[0].FilePath, res.Hits[0].Line)
	}
}

func TestSearchCallersIntentNoGraphFallsBack(t *testing.T) {
	st, err := sqlite.Open(&sqlite.Config{Path: filepath.Join(t.TempDir(), "empty.db"), PoolSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srch := &stubSearcher{byQuery: map[string][]*index.Hit{
		"payment gateway": {{RepoID: "r1", FilePath: "docs/pay.md", Path: "docs/pay.md", Line: 1, EndLine: 5, Score: 0.4}},
	}}
	svc := serviceWithSearcher(srch, st)
	defer svc.Close(context.Background())

	res, err := svc.Search(context.Background(), &index.SearchQuery{
		Query: "who calls the payment gateway", Repos: []string{"r1"}, Limit: 10,
	}, "keyword")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	// No units, no edges: the text results come back untouched, annotated
	// with the intent that was tried.
	if len(res.Hits) != 1 || res.Hits[0].FilePath != "docs/pay.md" {
		t.Fatalf("hits = %+v, want the single text hit", res.Hits)
	}
	if res.Metadata["intent"] != promote.IntentCallers {
		t.Errorf("metadata intent = %v, want callers", res.Metadata["intent"])
	}
	if _, ok := res.Metadata["intent_promoted"]; ok {
		t.Errorf("intent_promoted set with nothing promoted")
	}
}

func TestSearchIntentNoneLeavesResultsAlone(t *testing.T) {
	st, _ := callersFixture(t)
	srch := &stubSearcher{byQuery: map[string][]*index.Hit{
		"what calls the shipping service ShipOrder rpc": {{
			RepoID: "r1", FilePath: "shipping/app.go", Path: "shipping/app.go",
			Line: 45, EndLine: 60, Score: 0.5,
		}},
	}}
	svc := serviceWithSearcher(srch, st)
	defer svc.Close(context.Background())

	res, err := svc.Search(context.Background(), &index.SearchQuery{
		Query: "what calls the shipping service ShipOrder rpc",
		Repos: []string{"r1"}, Limit: 10, Intent: promote.IntentNone,
	}, "keyword")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(res.Hits) != 1 || res.Hits[0].FilePath != "shipping/app.go" {
		t.Fatalf("hits = %+v, want the untouched text hit", res.Hits)
	}
	if _, ok := res.Metadata["intent"]; ok {
		t.Errorf("intent metadata set with intent none")
	}
}

// TestCalleeGroupsUnitsSharingAQualifiedName pins the defect that made
// "what calls the shipping service ShipOrder rpc" unanswerable on the real
// corpus: a vendored .proto puts the same rpc method in every service's
// generated package (17 units named ShipOrder in the boutique repository),
// the linker resolves the call edge to exactly one of them, and a lookup that
// kept the first few units by name asked the wrong ones for their callers.
func TestCalleeGroupsUnitsSharingAQualifiedName(t *testing.T) {
	st, err := sqlite.Open(&sqlite.Config{Path: filepath.Join(t.TempDir(), "dup.db"), PoolSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	// Twelve copies of one rpc method, as a vendored proto produces.
	var copies []*domain.ASTUnit
	for i := 0; i < 12; i++ {
		u := &domain.ASTUnit{
			RepoID: "r1", FilePath: fmt.Sprintf("src/svc%d/genproto/demo.pb.go", i),
			Language: "go", Kind: store.KindRPCMethod, Name: "ShipOrder",
			Qualified: "grpc:shop.ShippingService/ShipOrder", StartLine: 10, EndLine: 12,
		}
		if err := st.StoreASTUnit(ctx, u); err != nil {
			t.Fatal(err)
		}
		copies = append(copies, u)
	}
	caller := &domain.ASTUnit{
		RepoID: "r1", FilePath: "src/checkoutservice/main.go", Language: "go",
		Kind: "function", Name: "shipOrder", Qualified: "main.shipOrder",
		StartLine: 380, EndLine: 400,
	}
	if err := st.StoreASTUnit(ctx, caller); err != nil {
		t.Fatal(err)
	}
	// The linker resolved the call to the last copy, not the first.
	if err := st.StoreEdge(ctx, &domain.Edge{
		RepoID: "r1", SrcID: caller.ID, DstID: copies[len(copies)-1].ID, Kind: store.EdgeRPCCall,
		DstName: "grpc:ShippingService/ShipOrder", FilePath: "src/checkoutservice/main.go",
		Line: 387, Confidence: 0.9,
	}); err != nil {
		t.Fatal(err)
	}

	svc := serviceWithSearcher(&stubSearcher{}, st)
	defer svc.Close(ctx)

	res, err := svc.Search(ctx, &index.SearchQuery{
		Query: "what calls the shipping service ShipOrder rpc", Repos: []string{"r1"}, Limit: 10,
	}, "keyword")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("no hits: the call site was not found through any of the copies")
	}
	if got := res.Hits[0]; got.FilePath != "src/checkoutservice/main.go" || got.Line != 387 {
		t.Fatalf("first hit = %s:%d, want src/checkoutservice/main.go:387", got.FilePath, got.Line)
	}
}
