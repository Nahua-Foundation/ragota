package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/app/promote"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/index"
	"github.com/Nahua-Foundation/ragota/internal/store"
	"github.com/Nahua-Foundation/ragota/internal/store/sqlite"
)

// contractUseFixture reproduces the shapes the corpus put in front of this
// lookup: a gRPC contract whose caller and implementation are in different
// services and different languages, a topic with a publisher at one end and a
// subscriber at the other, and the four kinds of edge that match a question's
// words without answering it — a sibling rpc of the same service, a one-word
// method that every service has, a route called only by a load generator, and
// a call from a test.
func contractUseFixture(t *testing.T) *sqlite.SQLite {
	t.Helper()
	st, err := sqlite.Open(&sqlite.Config{Path: filepath.Join(t.TempDir(), "uses.db"), PoolSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	units := map[string]*domain.ASTUnit{
		"chargeCard": {
			RepoID: "r1", FilePath: "src/checkoutservice/main.go", Language: "go",
			Kind: "method", Name: "chargeCard", Qualified: "main.checkoutService.chargeCard",
			StartLine: 369, EndLine: 395,
		},
		// The implementation is not named after the rpc it serves, which is why
		// resolving the callee by name finds nothing for the question below.
		"ChargeServiceHandler": {
			RepoID: "r1", FilePath: "src/paymentservice/server.js", Language: "javascript",
			Kind: "function", Name: "ChargeServiceHandler", Qualified: "ChargeServiceHandler",
			StartLine: 41, EndLine: 60,
		},
		"getShippingQuote": {
			RepoID: "r1", FilePath: "src/frontend/rpc.go", Language: "go",
			Kind: "function", Name: "getShippingQuote", Qualified: "main.getShippingQuote",
			StartLine: 85, EndLine: 95,
		},
		"PriceHandler": {
			RepoID: "r1", FilePath: "src/Webhooks.API/IntegrationEvents/ProductPriceChangedIntegrationEventHandler.cs",
			Language: "csharp", Kind: "method", Name: "Handle", Qualified: "Webhooks.ProductPriceChangedIntegrationEventHandler.Handle",
			StartLine: 5, EndLine: 12,
		},
		"CatalogApi": {
			RepoID: "r1", FilePath: "src/Catalog.API/Apis/CatalogApi.cs", Language: "csharp",
			Kind: "method", Name: "UpdateItem", Qualified: "Catalog.API.CatalogApi.UpdateItem",
			StartLine: 340, EndLine: 360,
		},
		"snapshotStore": {
			RepoID: "r1", FilePath: "pkg/registry/annotation/grpc_store.go", Language: "go",
			Kind: "method", Name: "Create", Qualified: "annotation.store.Create",
			StartLine: 80, EndLine: 95,
		},
		"loadgen": {
			RepoID: "r1", FilePath: "load-gen/robot-shop.py", Language: "python",
			Kind: "function", Name: "load", Qualified: "load", StartLine: 50, EndLine: 80,
		},
		"cartTest": {
			RepoID: "r1", FilePath: "src/cartservice/tests/CartServiceTests.cs", Language: "csharp",
			Kind: "method", Name: "AddItem_Works", Qualified: "CartServiceTests.AddItem_Works",
			StartLine: 90, EndLine: 105,
		},
	}
	for _, u := range units {
		if err := st.StoreASTUnit(ctx, u); err != nil {
			t.Fatal(err)
		}
	}

	for _, e := range []*domain.Edge{
		{RepoID: "r1", SrcID: units["chargeCard"].ID, Kind: store.EdgeRPCCall,
			DstName: "grpc:PaymentService/Charge", FilePath: "src/checkoutservice/main.go", Line: 370, Confidence: 0.63},
		{RepoID: "r1", SrcID: units["ChargeServiceHandler"].ID, Kind: store.EdgeImplementsRPC,
			DstName: "grpc:PaymentService/Charge", FilePath: "src/paymentservice/server.js", Line: 88, Confidence: 0.56},
		{RepoID: "r1", SrcID: units["getShippingQuote"].ID, Kind: store.EdgeRPCCall,
			DstName: "grpc:ShippingService/GetQuote", FilePath: "src/frontend/rpc.go", Line: 88, Confidence: 0.63},
		{RepoID: "r1", SrcID: units["snapshotStore"].ID, Kind: store.EdgeRPCCall,
			DstName: "grpc:AnnotationStore/Create", FilePath: "pkg/registry/annotation/grpc_store.go", Line: 87, Confidence: 0.63},
		{RepoID: "r1", SrcID: units["PriceHandler"].ID, Kind: store.EdgeConsumes,
			DstName:  "topic:ProductPriceChangedIntegrationEvent",
			FilePath: "src/Webhooks.API/IntegrationEvents/ProductPriceChangedIntegrationEventHandler.cs", Line: 5, Confidence: 0.9},
		{RepoID: "r1", SrcID: units["CatalogApi"].ID, Kind: store.EdgeProduces,
			DstName:  "topic:ProductPriceChangedIntegrationEvent",
			FilePath: "src/Catalog.API/Apis/CatalogApi.cs", Line: 356, Confidence: 0.9},
		{RepoID: "r1", SrcID: units["loadgen"].ID, Kind: store.EdgeHTTPCall,
			DstName: "http:GET /api/catalogue/products", FilePath: "load-gen/robot-shop.py", Line: 56, Confidence: 0.8},
		{RepoID: "r1", SrcID: units["cartTest"].ID, Kind: store.EdgeRPCCall,
			DstName: "grpc:CartService/AddItem", FilePath: "src/cartservice/tests/CartServiceTests.cs", Line: 97, Confidence: 0.63},
	} {
		if err := st.StoreEdge(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func firstPaths(hits []*index.Hit, n int) []string {
	var out []string
	for i, h := range hits {
		if i >= n {
			break
		}
		out = append(out, h.FilePath)
	}
	return out
}

// The question the graph could always answer and the lookup could not: the
// caller is on the expected line, carrying the key, while the implementation
// that would resolve the callee by name is called something else entirely.
func TestSearchPromotesContractCaller(t *testing.T) {
	st := contractUseFixture(t)
	// Retrieval returns the vendored proto copies, as it does on the corpus.
	srch := &stubSearcher{hits: hitsForFiles("protos/demo.proto", "src/currencyservice/proto/demo.proto")}
	svc := serviceWithSearcher(srch, st)
	defer svc.Close(context.Background())

	res, err := svc.Search(context.Background(), &index.SearchQuery{
		Query: "which service calls the payment service Charge rpc", Repos: []string{"r1"}, Limit: 10,
	}, "keyword")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("no hits")
	}
	first := res.Hits[0]
	if first.FilePath != "src/checkoutservice/main.go" || first.Line != 370 {
		t.Fatalf("first hit = %s:%d, want src/checkoutservice/main.go:370 (hits: %v)",
			first.FilePath, first.Line, firstPaths(res.Hits, 5))
	}
	if first.Symbol != "chargeCard" || first.Reason != "rpc call to grpc:PaymentService/Charge" {
		t.Errorf("first hit symbol/reason = %q/%q, want chargeCard / rpc call to grpc:PaymentService/Charge",
			first.Symbol, first.Reason)
	}
	// The far side of the same contract answers the opposite question.
	for _, h := range res.Hits {
		if h.FilePath == "src/paymentservice/server.js" {
			t.Errorf("promoted the implementation for a callers question: %+v", h)
		}
	}
	// A sibling rpc of a service the question did not name is not an answer.
	for _, h := range res.Hits {
		if h.FilePath == "src/frontend/rpc.go" {
			t.Errorf("promoted an unrelated rpc of another service: %+v", h)
		}
	}
	if keys, _ := res.Metadata["contract_use_keys"].([]string); len(keys) != 1 || keys[0] != "grpc:PaymentService/Charge" {
		t.Errorf("contract_use_keys = %v, want [grpc:PaymentService/Charge]", res.Metadata["contract_use_keys"])
	}
}

// A topic question describes its contract in prose ("catalog product price
// changes" for ProductPriceChangedIntegrationEvent) and says by its verb which
// end of it is being asked for.
func TestSearchPromotesTopicSideByVerb(t *testing.T) {
	st := contractUseFixture(t)
	consumer := "src/Webhooks.API/IntegrationEvents/ProductPriceChangedIntegrationEventHandler.cs"
	producer := "src/Catalog.API/Apis/CatalogApi.cs"

	tests := []struct {
		query string
		want  string
		avoid string
	}{
		{"which service subscribes to catalog product price changes", consumer, producer},
		{"which service publishes the catalog product price change event", producer, consumer},
	}
	svc := serviceWithSearcher(&stubSearcher{hits: hitsForFiles("README.md")}, st)
	defer svc.Close(context.Background())

	for _, tt := range tests {
		res, err := svc.Search(context.Background(), &index.SearchQuery{
			Query: tt.query, Repos: []string{"r1"}, Limit: 10,
		}, "keyword")
		if err != nil {
			t.Fatalf("Search(%q) error = %v", tt.query, err)
		}
		if len(res.Hits) == 0 || res.Hits[0].FilePath != tt.want {
			t.Errorf("Search(%q) first hit = %v, want %s", tt.query, firstPaths(res.Hits, 3), tt.want)
			continue
		}
		for _, h := range res.Hits {
			if h.FilePath == tt.avoid {
				t.Errorf("Search(%q) promoted the other end of the topic: %s", tt.query, tt.avoid)
			}
		}
	}
}

// Every one of these matched a question's words on the corpus and answered a
// different question. None may be promoted.
func TestSearchLeavesUndescribedContractsAlone(t *testing.T) {
	st := contractUseFixture(t)
	tests := []struct {
		name  string
		query string
	}{
		// "Create" is one word every service has; the question names no app.
		{"one-word rpc method", "what calls the created-snapshot handler in the http server"},
		// A route is only described when two of its segments are.
		{"one-segment route", "which service reads the whole product catalog"},
		// The only caller of this route in the graph is a load generator.
		{"load generator", "how does the ratings service check that a product exists in the catalogue"},
		// The only caller of this rpc is a test.
		{"test caller", "which code calls the cart service AddItem rpc"},
		// A callers question about a contract nothing in the index carries.
		{"unknown contract", "what calls the billing service Refund rpc"},
	}
	svc := serviceWithSearcher(&stubSearcher{hits: hitsForFiles("some/text/hit.go")}, st)
	defer svc.Close(context.Background())

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := svc.Search(context.Background(), &index.SearchQuery{
				Query: tt.query, Repos: []string{"r1"}, Limit: 10,
			}, "keyword")
			if err != nil {
				t.Fatalf("Search() error = %v", err)
			}
			if _, ok := res.Metadata["contract_uses"]; ok {
				t.Errorf("promoted %v for %q", firstPaths(res.Hits, 3), tt.query)
			}
		})
	}
}

func TestSearchContractUsesIntentNoneLeavesResultsAlone(t *testing.T) {
	st := contractUseFixture(t)
	svc := serviceWithSearcher(&stubSearcher{hits: hitsForFiles("docs/events.md")}, st)
	defer svc.Close(context.Background())

	res, err := svc.Search(context.Background(), &index.SearchQuery{
		Query: "which service subscribes to catalog product price changes",
		Repos: []string{"r1"}, Limit: 10, Intent: promote.IntentNone,
	}, "keyword")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(res.Hits) != 1 || res.Hits[0].FilePath != "docs/events.md" {
		t.Fatalf("hits = %v, want the untouched text hit", firstPaths(res.Hits, 3))
	}
}
