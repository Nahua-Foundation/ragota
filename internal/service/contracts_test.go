package service

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/indexing"
	"github.com/Nahua-Foundation/ragota/internal/service/promote"
	"github.com/Nahua-Foundation/ragota/internal/storage"
	"github.com/Nahua-Foundation/ragota/internal/storage/sqlite"
)

// contractFixture seeds a route with its handler, a table with a writer and a
// reader, and a topic with a producer.
func contractFixture(t *testing.T) *sqlite.SQLite {
	t.Helper()
	st, err := sqlite.Open(&sqlite.Config{Path: filepath.Join(t.TempDir(), "contracts.db"), PoolSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	route := &storage.ASTUnit{
		RepoID: "r1", FilePath: "api/routes.go", Language: "go",
		Kind: storage.KindHTTPRoute, Name: "POST /orders/cancel",
		Qualified: "http:POST /orders/cancel", StartLine: 12, EndLine: 12,
	}
	handler := &storage.ASTUnit{
		RepoID: "r1", FilePath: "api/orders.go", Language: "go",
		Kind: "function", Name: "CancelOrder", Qualified: "api.CancelOrder",
		StartLine: 80, EndLine: 120,
	}
	table := &storage.ASTUnit{
		RepoID: "r1", FilePath: "db/migrations/003_orders.sql", Language: "sql",
		Kind: storage.KindDBTable, Name: "orders", Qualified: "db:orders",
		StartLine: 5, EndLine: 20,
	}
	writer := &storage.ASTUnit{
		RepoID: "r1", FilePath: "store/orders.go", Language: "go",
		Kind: "function", Name: "InsertOrder", Qualified: "store.InsertOrder",
		StartLine: 30, EndLine: 60,
	}
	producer := &storage.ASTUnit{
		RepoID: "r1", FilePath: "events/publish.go", Language: "go",
		Kind: "function", Name: "PublishOrder", Qualified: "events.PublishOrder",
		StartLine: 10, EndLine: 25,
	}
	for _, u := range []*storage.ASTUnit{route, handler, table, writer, producer} {
		if err := st.StoreASTUnit(ctx, u); err != nil {
			t.Fatal(err)
		}
	}

	for _, e := range []*storage.Edge{
		{RepoID: "r1", SrcID: route.ID, DstID: handler.ID, Kind: storage.EdgeHandledBy,
			DstName: "CancelOrder", FilePath: "api/routes.go", Line: 12, Confidence: 0.95},
		{RepoID: "r1", SrcID: writer.ID, Kind: storage.EdgeWritesTo,
			DstName: "db:orders", FilePath: "store/orders.go", Line: 42, Confidence: 0.9},
		{RepoID: "r1", SrcID: handler.ID, Kind: storage.EdgeReadsFrom,
			DstName: "db:orders", FilePath: "api/orders.go", Line: 95, Confidence: 0.9},
		{RepoID: "r1", SrcID: producer.ID, Kind: storage.EdgeProduces,
			DstName: "topic:orders", FilePath: "events/publish.go", Line: 18, Confidence: 0.9},
	} {
		if err := st.StoreEdge(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func TestSearchPromotesRouteHandler(t *testing.T) {
	st := contractFixture(t)
	// Retrieval returns the generated API documentation, as it does on the
	// real corpus: the same words, none of the answer.
	srch := &stubSearcher{hits: []*indexing.Hit{{
		RepoID: "r1", FilePath: "docs/openapi.json", Path: "docs/openapi.json",
		Line: 1, EndLine: 40, Score: 0.9, Reason: "keyword",
	}}}
	svc := serviceWithSearcher(srch, st)
	defer svc.Close(context.Background())

	res, err := svc.Search(context.Background(), &indexing.SearchQuery{
		Query: "where does POST /orders/cancel go", Repos: []string{"r1"}, Limit: 10,
	}, "keyword")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(res.Hits) < 2 {
		t.Fatalf("got %d hits: %+v", len(res.Hits), res.Hits)
	}
	// The handler answers the question; the registration line is context.
	if got := res.Hits[0]; got.FilePath != "api/orders.go" || got.Symbol != "CancelOrder" {
		t.Fatalf("first hit = %s (%s), want api/orders.go (CancelOrder)", got.FilePath, got.Symbol)
	}
	if got := res.Hits[1]; got.FilePath != "api/routes.go" {
		t.Errorf("second hit = %s, want the route declaration api/routes.go", got.FilePath)
	}
	if keys, _ := res.Metadata["contract_keys"].([]string); len(keys) != 1 || keys[0] != "http:POST /orders/cancel" {
		t.Errorf("contract_keys = %v, want [http:POST /orders/cancel]", res.Metadata["contract_keys"])
	}
	if !strings.Contains(res.Hits[0].Snippet, "CancelOrder") {
		t.Errorf("handler hit has no usable snippet: %q", res.Hits[0].Snippet)
	}
}

func TestSearchPromotesTableWritersBeforeReaders(t *testing.T) {
	st := contractFixture(t)
	svc := serviceWithSearcher(&stubSearcher{}, st)
	defer svc.Close(context.Background())

	res, err := svc.Search(context.Background(), &indexing.SearchQuery{
		Query: "which code writes rows into the orders table", Repos: []string{"r1"}, Limit: 10,
	}, "keyword")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	var paths []string
	for _, h := range res.Hits {
		paths = append(paths, h.FilePath)
	}
	// The declaration first (it is what "the orders table" names), then the
	// writer, then the reader.
	want := []string{"db/migrations/003_orders.sql", "store/orders.go", "api/orders.go"}
	if len(paths) < len(want) {
		t.Fatalf("got %v, want at least %v", paths, want)
	}
	for i, w := range want {
		if paths[i] != w {
			t.Errorf("hit %d = %s, want %s (full order: %v)", i, paths[i], w, paths)
		}
	}
}

func TestSearchPromotesTopicProducer(t *testing.T) {
	st := contractFixture(t)
	svc := serviceWithSearcher(&stubSearcher{}, st)
	defer svc.Close(context.Background())

	res, err := svc.Search(context.Background(), &indexing.SearchQuery{
		Query: "which service publishes to the orders queue", Repos: []string{"r1"}, Limit: 10,
	}, "keyword")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(res.Hits) == 0 || res.Hits[0].FilePath != "events/publish.go" || res.Hits[0].Line != 18 {
		t.Fatalf("first hit = %+v, want the producer at events/publish.go:18", res.Hits)
	}
}

func TestSearchContractIntentNoneLeavesResultsAlone(t *testing.T) {
	st := contractFixture(t)
	srch := &stubSearcher{hits: []*indexing.Hit{{
		RepoID: "r1", FilePath: "docs/openapi.json", Path: "docs/openapi.json", Line: 1, EndLine: 40, Score: 0.9,
	}}}
	svc := serviceWithSearcher(srch, st)
	defer svc.Close(context.Background())

	res, err := svc.Search(context.Background(), &indexing.SearchQuery{
		Query: "where does POST /orders/cancel go", Repos: []string{"r1"}, Limit: 10, Intent: promote.IntentNone,
	}, "keyword")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(res.Hits) != 1 || res.Hits[0].FilePath != "docs/openapi.json" {
		t.Fatalf("hits = %+v, want the untouched text hit", res.Hits)
	}
	if _, ok := res.Metadata["contract_keys"]; ok {
		t.Error("contract metadata set with intent none")
	}
}

// A key that nothing in the index declares must change nothing.
func TestSearchUnknownContractKeyIsInert(t *testing.T) {
	st := contractFixture(t)
	srch := &stubSearcher{hits: hitsForFiles("some/file.go")}
	svc := serviceWithSearcher(srch, st)
	defer svc.Close(context.Background())

	res, err := svc.Search(context.Background(), &indexing.SearchQuery{
		Query: "where does POST /nothing/here go", Repos: []string{"r1"}, Limit: 10,
	}, "keyword")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(res.Hits) != 1 || res.Hits[0].FilePath != "some/file.go" {
		t.Fatalf("hits = %+v, want the text hit untouched", res.Hits)
	}
}

// TestSearchPromotesRPCImplementation covers the three shapes a gRPC question
// takes on the corpus, none of which a key pattern extracts: the method named
// beside its service ("the ApplicationService Sync grpc method"), the method
// alone ("the ShipOrder grpc method"), and the service alone ("the grpc
// basket service"). The match therefore runs from the edges' keys back to the
// question rather than the other way round.
func TestSearchPromotesRPCImplementation(t *testing.T) {
	st, err := sqlite.Open(&sqlite.Config{Path: filepath.Join(t.TempDir(), "rpc.db"), PoolSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	impl := &storage.ASTUnit{
		RepoID: "r1", FilePath: "src/shippingservice/main.go", Language: "go",
		Kind: "method", Name: "ShipOrder", Qualified: "main.server.ShipOrder",
		StartLine: 142, EndLine: 160,
	}
	stub := &storage.ASTUnit{
		RepoID: "r1", FilePath: "src/checkoutservice/genproto/demo_grpc.pb.go", Language: "go",
		Kind: "method", Name: "ShipOrder", Qualified: "genproto.ShipOrder",
		StartLine: 524, EndLine: 526,
	}
	testImpl := &storage.ASTUnit{
		RepoID: "r1", FilePath: "src/shippingservice/main_test.go", Language: "go",
		Kind: "method", Name: "ShipOrder", Qualified: "main.mock.ShipOrder",
		StartLine: 20, EndLine: 24,
	}
	for _, u := range []*storage.ASTUnit{impl, stub, testImpl} {
		if err := st.StoreASTUnit(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []*storage.Edge{
		{RepoID: "r1", SrcID: impl.ID, Kind: storage.EdgeImplementsRPC,
			DstName: "grpc:ShippingService/ShipOrder", FilePath: "src/shippingservice/main.go", Line: 142, Confidence: 0.9},
		// A generated stub and a test carry the same key and must not win.
		{RepoID: "r1", SrcID: stub.ID, Kind: storage.EdgeImplementsRPC, DstName: "grpc:/ShipOrder",
			FilePath: "src/checkoutservice/genproto/demo_grpc.pb.go", Line: 524, Confidence: 0.6},
		{RepoID: "r1", SrcID: testImpl.ID, Kind: storage.EdgeImplementsRPC, DstName: "grpc:ShippingService/ShipOrder",
			FilePath: "src/shippingservice/main_test.go", Line: 20, Confidence: 0.9},
	} {
		if err := st.StoreEdge(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	svc := serviceWithSearcher(&stubSearcher{}, st)
	defer svc.Close(ctx)

	for _, query := range []string{
		"where is the ShipOrder grpc method implemented",
		"where is the grpc shipping service implemented",
	} {
		res, err := svc.Search(ctx, &indexing.SearchQuery{
			Query: query, Repos: []string{"r1"}, Limit: 10,
		}, "keyword")
		if err != nil {
			t.Fatalf("Search(%q) error = %v", query, err)
		}
		if len(res.Hits) == 0 {
			t.Fatalf("Search(%q): no hits", query)
		}
		if got := res.Hits[0]; got.FilePath != "src/shippingservice/main.go" || got.Line != 142 {
			t.Errorf("Search(%q) first hit = %s:%d, want src/shippingservice/main.go:142",
				query, got.FilePath, got.Line)
		}
		for _, h := range res.Hits {
			if strings.Contains(h.FilePath, "_test.go") {
				t.Errorf("Search(%q) promoted a test implementation: %s", query, h.FilePath)
			}
		}
	}

	// A callers question about the same contract must not be answered with
	// implementations.
	res, err := svc.Search(ctx, &indexing.SearchQuery{
		Query: "what calls the shipping service ShipOrder rpc", Repos: []string{"r1"}, Limit: 10,
	}, "keyword")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if _, ok := res.Metadata["rpc_implementations"]; ok {
		t.Error("implementation lookup fired for a callers question")
	}
}
