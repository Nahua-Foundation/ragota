package graph

import (
	"context"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"
	"github.com/Nahua-Foundation/ragota/internal/store/sqlite"
)

func storeService(t *testing.T, st *sqlite.SQLite, repoID, name, root string) *domain.ASTUnit {
	t.Helper()
	return storeUnit(t, st, &domain.ASTUnit{
		RepoID: repoID, FilePath: root, Kind: store.KindService, Name: name,
		Qualified: name, Meta: store.EncodeUnitMeta(&store.UnitMeta{Root: root, DetectedBy: "test"}),
	})
}

func storeFuncAt(t *testing.T, st *sqlite.SQLite, repoID, name, filePath string) *domain.ASTUnit {
	t.Helper()
	return storeUnit(t, st, &domain.ASTUnit{
		RepoID: repoID, FilePath: filePath, Language: "go",
		Kind: "function", Name: name, Qualified: "pkg." + name,
	})
}

// TestServicesGraphPicksConfidentImplementation: a contract may collect
// several implements_rpc edges; the weakest guess must not decide which
// service the call is attributed to just because it was stored first.
func TestServicesGraphPicksConfidentImplementation(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	storeService(t, st, "repoA", "gateway", "gateway")
	storeService(t, st, "repoB", "legacy", "legacy")
	storeService(t, st, "repoB", "orders", "orders")

	caller := storeFuncAt(t, st, "repoA", "PlaceOrder", "gateway/main.go")
	rpcMethod := storeUnit(t, st, &domain.ASTUnit{
		RepoID: "repoB", FilePath: "proto/orders.proto", Language: "proto",
		Kind: store.KindRPCMethod, Name: "CreateOrder",
		Qualified: "grpc:orders.OrderService/CreateOrder",
	})
	guess := storeFuncAt(t, st, "repoB", "CreateOrder", "legacy/server.go")
	real := storeFuncAt(t, st, "repoB", "CreateOrder", "orders/server.go")

	// The weak guess is stored first, so it also has the lower edge id.
	storeEdge(t, st, &domain.Edge{
		RepoID: "repoB", SrcID: guess.ID, DstID: rpcMethod.ID, DstRepoID: "repoB",
		Kind: store.EdgeImplementsRPC, DstName: rpcMethod.Qualified,
		Confidence: contract.ConfWeak,
	})
	storeEdge(t, st, &domain.Edge{
		RepoID: "repoB", SrcID: real.ID, DstID: rpcMethod.ID, DstRepoID: "repoB",
		Kind: store.EdgeImplementsRPC, DstName: rpcMethod.Qualified,
		Confidence: contract.ConfExact,
	})
	storeEdge(t, st, &domain.Edge{
		RepoID: "repoA", SrcID: caller.ID, DstID: rpcMethod.ID, DstRepoID: "repoB",
		Kind: store.EdgeRPCCall, DstName: rpcMethod.Qualified, Confidence: contract.ConfExact,
	})

	_, links, err := New(st).ServicesGraph(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 {
		t.Fatalf("links = %d, want 1: %+v", len(links), links)
	}
	if links[0].DstService != "orders" {
		t.Errorf("DstService = %q, want orders (the confident implementation)", links[0].DstService)
	}
}

// TestServicesGraphMergesRouteKeyCase: route matching is case-insensitive, so
// two spellings of the same route are one link, not two.
func TestServicesGraphMergesRouteKeyCase(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	storeService(t, st, "repoA", "gateway", "gateway")
	storeService(t, st, "repoB", "users", "users")

	caller := storeFuncAt(t, st, "repoA", "FetchUsers", "gateway/main.go")
	route := storeUnit(t, st, &domain.ASTUnit{
		RepoID: "repoB", FilePath: "users/routes.go", Language: "go",
		Kind: store.KindHTTPRoute, Name: "GET /api/users", Qualified: "http:GET /api/users",
	})
	for _, key := range []string{"http:GET /api/users", "http:GET /API/Users"} {
		storeEdge(t, st, &domain.Edge{
			RepoID: "repoA", SrcID: caller.ID, DstID: route.ID, DstRepoID: "repoB",
			Kind: store.EdgeHTTPCall, DstName: key, Confidence: contract.ConfExact,
		})
	}

	_, links, err := New(st).ServicesGraph(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 {
		t.Fatalf("links = %d, want 1 (route keys differ only in case): %+v", len(links), links)
	}
	if links[0].Count != 2 {
		t.Errorf("Count = %d, want 2 calls aggregated into one link", links[0].Count)
	}
	if links[0].Via != "http:GET /api/users" {
		t.Errorf("Via = %q, want the canonical key", links[0].Via)
	}
}

func TestCanonicalVia(t *testing.T) {
	tests := []struct{ in, want string }{
		{"http:GET /API/Users", "http:GET /api/users"},
		{"http:get /api/users/", "http:GET /api/users"},
		{"http:GET /api/users?page=2", "http:GET /api/users"},
		{"grpc:orders.OrderService/CreateOrder", "grpc:orders.OrderService/CreateOrder"},
		{"topic:Orders.Created", "topic:Orders.Created"},
	}
	for _, tt := range tests {
		if got := canonicalVia(tt.in); got != tt.want {
			t.Errorf("canonicalVia(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
