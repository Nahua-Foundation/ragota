package graph

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"
	"github.com/Nahua-Foundation/ragota/internal/store/sqlite"
)

// setupAmbiguousRoute stores one http_call edge from repoA and two identical
// route candidates in repoB and repoC — an ambiguous match (equal scores).
func setupAmbiguousRoute(t *testing.T) (st *sqlite.SQLite, caller, routeB, routeC *domain.ASTUnit, edge *domain.Edge) {
	t.Helper()
	s := openTestStore(t)
	caller = storeFunc(t, s, "repoA", "CallThings", "(id string)")
	routeB = storeUnit(t, s, &domain.ASTUnit{
		RepoID: "repoB", FilePath: "b/routes.go", Language: "go",
		Kind: store.KindHTTPRoute, Name: "GET /api/things",
		Qualified: "http:GET /api/things",
	})
	routeC = storeUnit(t, s, &domain.ASTUnit{
		RepoID: "repoC", FilePath: "c/routes.go", Language: "go",
		Kind: store.KindHTTPRoute, Name: "GET /api/things",
		Qualified: "http:GET /api/things",
	})
	edge = storeEdge(t, s, &domain.Edge{
		RepoID: "repoA", SrcID: caller.ID, Kind: store.EdgeHTTPCall,
		DstName: "http:GET /api/things", FilePath: "src/CallThings.go", Line: 10,
	})
	return s, caller, routeB, routeC, edge
}

// httpEdgeBySrc fetches the single http_call edge originating from srcID.
func httpEdgeBySrc(t *testing.T, st store.Storage, srcID string) *domain.Edge {
	t.Helper()
	edges, err := st.GetEdges(context.Background(), domain.QueryOpts{Kind: store.EdgeHTTPCall, SrcID: srcID})
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 {
		t.Fatalf("http_call edges from %s = %d, want 1", srcID, len(edges))
	}
	return edges[0]
}

func TestLinkerDisambiguatesAmbiguousHTTPRoute(t *testing.T) {
	ctx := context.Background()
	st, caller, _, routeC, _ := setupAmbiguousRoute(t)

	l := NewLinker(st)
	calls := 0
	l.SetDisambiguator(func(_ context.Context, prompt string) (int, bool) {
		calls++
		// Pick the repoC candidate by its index in the prompt's candidate
		// list (lines look like "1) http:GET /api/things (repo=repoC, ...)").
		for _, line := range strings.Split(prompt, "\n") {
			if strings.Contains(line, "repo=repoC") {
				n, err := strconv.Atoi(strings.SplitN(line, ")", 2)[0])
				if err != nil {
					t.Fatalf("unparsable candidate line %q", line)
				}
				return n, true
			}
		}
		t.Fatalf("prompt lists no repoC candidate:\n%s", prompt)
		return -1, false
	})

	if err := l.Run(ctx, "repoA"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("disambiguator calls = %d, want 1", calls)
	}

	got := httpEdgeBySrc(t, st, caller.ID)
	if got.DstID != routeC.ID || got.DstRepoID != "repoC" {
		t.Errorf("edge dst = %s@%s, want %s@repoC", got.DstID, got.DstRepoID, routeC.ID)
	}
	if !strings.Contains(got.Meta, `"source":"llm"`) {
		t.Errorf("edge meta = %q, want it to contain \"source\":\"llm\"", got.Meta)
	}
	if got.Confidence <= 0 {
		t.Errorf("confidence = %v, want > 0", got.Confidence)
	}

	// A second run must keep the LLM resolution (edge is validly resolved)
	// and must not consult the disambiguator again.
	if err := l.Run(ctx, "repoA"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("disambiguator calls after rerun = %d, want 1", calls)
	}
	again := httpEdgeBySrc(t, st, caller.ID)
	if again.DstID != routeC.ID {
		t.Errorf("rerun edge dst = %s, want %s", again.DstID, routeC.ID)
	}
}

// TestLinkerAnnotatesLLMEdgeInPlace: marking an edge as LLM-resolved changes
// only its meta, so the edge keeps its stored identity — and so does every
// other edge sharing its (kind, dst_name) group, which the old rewrite deleted
// and re-inserted along with it.
func TestLinkerAnnotatesLLMEdgeInPlace(t *testing.T) {
	ctx := context.Background()
	st, caller, _, routeC, edge := setupAmbiguousRoute(t)

	other := storeFunc(t, st, "repoA", "CallThingsAgain", "(id string)")
	sibling := storeEdge(t, st, &domain.Edge{
		RepoID: "repoA", SrcID: other.ID, Kind: store.EdgeHTTPCall,
		DstName: "http:GET /api/things", FilePath: "src/CallThingsAgain.go", Line: 4,
	})

	l := NewLinker(st)
	l.SetDisambiguator(func(_ context.Context, prompt string) (int, bool) {
		for _, line := range strings.Split(prompt, "\n") {
			if strings.Contains(line, "repo=repoC") {
				n, err := strconv.Atoi(strings.SplitN(line, ")", 2)[0])
				if err != nil {
					t.Fatalf("unparsable candidate line %q", line)
				}
				return n, true
			}
		}
		t.Fatalf("prompt lists no repoC candidate:\n%s", prompt)
		return -1, false
	})
	if err := l.Run(ctx, "repoA"); err != nil {
		t.Fatal(err)
	}

	got := httpEdgeBySrc(t, st, caller.ID)
	if got.ID != edge.ID {
		t.Errorf("edge id = %s, want %s (annotation must not re-insert the edge)", got.ID, edge.ID)
	}
	if got.DstID != routeC.ID || !strings.Contains(got.Meta, `"source":"llm"`) {
		t.Errorf("edge dst = %s meta = %q, want %s marked as LLM-resolved", got.DstID, got.Meta, routeC.ID)
	}
	if again := httpEdgeBySrc(t, st, other.ID); again.ID != sibling.ID {
		t.Errorf("sibling edge id = %s, want %s (its group must not be rewritten)", again.ID, sibling.ID)
	}
}

func TestLinkerDisambiguatorDeclineKeepsHeuristic(t *testing.T) {
	ctx := context.Background()
	st, caller, routeB, routeC, _ := setupAmbiguousRoute(t)

	l := NewLinker(st)
	l.SetDisambiguator(func(context.Context, string) (int, bool) { return 0, false })

	if err := l.Run(ctx, "repoA"); err != nil {
		t.Fatal(err)
	}

	got := httpEdgeBySrc(t, st, caller.ID)
	if got.DstID != routeB.ID && got.DstID != routeC.ID {
		t.Errorf("edge dst = %q, want one of the heuristic candidates", got.DstID)
	}
	if strings.Contains(got.Meta, "llm") {
		t.Errorf("edge meta = %q, must not be marked as LLM-resolved", got.Meta)
	}
}

// TestLinkerDisambiguatesVersionedRPCService: a client key without the proto
// package suffix-matches both service versions, so the linker must ask rather
// than take the lower-id one at ConfExact.
func TestLinkerDisambiguatesVersionedRPCService(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	caller := storeFunc(t, st, "repoA", "PlaceOrder", "(userID string)")
	for _, qual := range []string{
		"grpc:orders.v1.OrderService/CreateOrder",
		"grpc:orders.v2.OrderService/CreateOrder",
	} {
		storeUnit(t, st, &domain.ASTUnit{
			RepoID: "repoB", FilePath: qual + ".proto", Language: "proto",
			Kind: store.KindRPCMethod, Name: "CreateOrder", Qualified: qual,
		})
	}
	storeEdge(t, st, &domain.Edge{
		RepoID: "repoA", SrcID: caller.ID, Kind: store.EdgeRPCCall,
		DstName: "grpc:OrderService/CreateOrder", FilePath: "src/PlaceOrder.go", Line: 8,
	})

	l := NewLinker(st)
	calls := 0
	var prompt string
	l.SetDisambiguator(func(_ context.Context, p string) (int, bool) {
		calls++
		prompt = p
		for _, line := range strings.Split(p, "\n") {
			if strings.Contains(line, "orders.v2.OrderService") {
				n, err := strconv.Atoi(strings.SplitN(line, ")", 2)[0])
				if err != nil {
					t.Fatalf("unparsable candidate line %q", line)
				}
				return n, true
			}
		}
		return -1, false
	})

	if err := l.Run(ctx, "repoA"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("disambiguator calls = %d, want 1; prompt was:\n%s", calls, prompt)
	}

	edges, err := st.GetEdges(ctx, domain.QueryOpts{Kind: store.EdgeRPCCall, SrcID: caller.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 {
		t.Fatalf("rpc_call edges = %d, want 1", len(edges))
	}
	units, err := st.GetASTUnits(ctx, domain.QueryOpts{Qualified: "grpc:orders.v2.OrderService/CreateOrder"})
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 1 || edges[0].DstID != units[0].ID {
		t.Errorf("edge dst = %q, want the v2 service method", edges[0].DstID)
	}
}

func TestLinkerNoDisambiguatorUnchanged(t *testing.T) {
	ctx := context.Background()
	st, caller, routeB, routeC, _ := setupAmbiguousRoute(t)

	if err := NewLinker(st).Run(ctx, "repoA"); err != nil {
		t.Fatal(err)
	}
	got := httpEdgeBySrc(t, st, caller.ID)
	if got.DstID != routeB.ID && got.DstID != routeC.ID {
		t.Errorf("edge dst = %q, want a heuristic candidate", got.DstID)
	}
	if got.Meta != "" {
		t.Errorf("edge meta = %q, want empty without a disambiguator", got.Meta)
	}
}
