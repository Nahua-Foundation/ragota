package render

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/client"
)

func TestSearchLocationsCarryTheRangeAndTheRepo(t *testing.T) {
	out := Search(&client.SearchResponse{
		Query: "q", Mode: "hybrid", Total: 2,
		Hits: []*client.SearchHit{
			{RepoID: "orders-aaa", FilePath: "a.go", Line: 42, EndLine: 88, Symbol: "F", Kind: "function", Language: "go", Reason: "keyword"},
			// A hit with no end line must not render as "a.go:7-0".
			{RepoID: "orders-aaa", FilePath: "b.go", Line: 7},
		},
	})
	if !strings.Contains(out, "orders-aaa a.go:42-88 F (function, go) [keyword]") {
		t.Errorf("hit line is not the expected shape:\n%s", out)
	}
	if !strings.Contains(out, "orders-aaa b.go:7\n") {
		t.Errorf("a hit without an end line rendered badly:\n%s", out)
	}
}

func TestSearchIndentsACodeBodyUniformly(t *testing.T) {
	out := Search(&client.SearchResponse{
		Query: "q", Mode: "hybrid", Total: 1,
		Hits: []*client.SearchHit{{
			RepoID: "r", FilePath: "a.py", Line: 1,
			Snippet: "def f():\n    return 1\n",
		}},
	})
	// Every line is shifted by the same amount, so indentation-significant code
	// pasted back out is still valid.
	if !strings.Contains(out, "   def f():\n       return 1\n") {
		t.Errorf("snippet indentation changed the code's own shape:\n%q", out)
	}
}

func TestDiagnosticsSpeakOnlyWhenItChangesTheAnswer(t *testing.T) {
	whole := &client.SearchDiagnostics{Degraded: false, Searchers: []string{"bm25", "vector"}}

	if got := Diagnostics(whole, false); got != "" {
		t.Errorf("a healthy answer with hits should get no commentary, got %q", got)
	}
	if got := Diagnostics(whole, true); !strings.Contains(got, "not degraded") {
		t.Errorf("an empty answer must be labelled as real: %q", got)
	}

	down := &client.SearchDiagnostics{
		Degraded:        true,
		FailedSearchers: []string{"vector"},
		SearcherErrors:  map[string]string{"vector": "connection refused"},
	}
	got := Diagnostics(down, false)
	for _, want := range []string{"DEGRADED", "vector", "connection refused", "not evidence"} {
		if !strings.Contains(got, want) {
			t.Errorf("degraded note is missing %q: %q", want, got)
		}
	}

	// Diagnostics absent from the response is itself a fact worth stating on an
	// empty answer: it means nobody asked, so nothing is known.
	if got := Diagnostics(nil, true); !strings.Contains(got, "cannot be told apart") {
		t.Errorf("an unexplained empty answer should say so: %q", got)
	}
	if got := Diagnostics(nil, false); got != "" {
		t.Errorf("nothing to say about a healthy answer, got %q", got)
	}
}

func TestContextCapsTheExpansionAndSaysWhereTheRestIs(t *testing.T) {
	related := make([]*client.RelatedUnit, 0, 24)
	for i := range 24 {
		related = append(related, &client.RelatedUnit{
			Unit:      &client.Unit{ID: fmt.Sprint(i), RepoID: "r", FilePath: "x.go", Name: fmt.Sprintf("N%d", i), Kind: "function"},
			Via:       "call",
			Direction: "out",
			Distance:  1,
		})
	}
	out := Context(&client.ContextResponse{
		Query: "q", Mode: "keyword",
		Items: []*client.ContextItem{{
			Hit:     &client.SearchHit{RepoID: "r", FilePath: "a.go", Line: 1},
			Unit:    &client.Unit{ID: "9021", RepoID: "r", FilePath: "a.go", Name: "F", Kind: "function"},
			Related: related,
		}},
	})
	if strings.Count(out, "-> d1 call") != relatedPerItem {
		t.Errorf("expected %d related lines, got %d:\n%s", relatedPerItem, strings.Count(out, "-> d1 call"), out)
	}
	// The expansion is capped here, not on the server, so the answer has to say
	// where the rest of it can be had.
	if !strings.Contains(out, "ragota_neighbors on unit 9021") {
		t.Errorf("a truncated expansion must name the way to the rest:\n%s", out)
	}
}

func TestContextKeepsAHitThatResolvedToNoUnit(t *testing.T) {
	// A hit the graph could not place still answers "where is this code";
	// dropping it would lose a result to make the rendering tidier.
	out := Context(&client.ContextResponse{
		Query: "q", Mode: "keyword",
		Items: []*client.ContextItem{{Hit: &client.SearchHit{RepoID: "r", FilePath: "vendor/x.go", Line: 3}}},
	})
	if !strings.Contains(out, "vendor/x.go:3") {
		t.Errorf("an unresolved hit was dropped:\n%s", out)
	}
}

func TestNeighborsCapsEachDirection(t *testing.T) {
	var out []*client.EdgeHop
	for i := range edgesPerDirection + 5 {
		out = append(out, &client.EdgeHop{
			Edge: &client.GraphEdge{Kind: "call", Confidence: 1, RepoID: "r", Line: i},
			Unit: &client.Unit{ID: fmt.Sprint(i), RepoID: "r", FilePath: "x.go", Name: "N", Kind: "function"},
		})
	}
	rendered := Neighbors(&client.NeighborsResponse{
		Center: &client.Unit{ID: "1", RepoID: "r", FilePath: "a.go", Name: "F", Kind: "function"},
		Out:    out,
	})
	// The endpoint caps neither list on purpose, so a unit everything calls comes
	// back with every edge it has and something has to stop it here.
	if strings.Count(rendered, "-> call") != edgesPerDirection {
		t.Errorf("expected %d out lines, got %d", edgesPerDirection, strings.Count(rendered, "-> call"))
	}
	if !strings.Contains(rendered, "5 more out edges") {
		t.Errorf("the cap is silent:\n%s", rendered)
	}
	if !strings.Contains(rendered, "in: none.") {
		t.Errorf("an empty direction must be stated:\n%s", rendered)
	}
}

func TestNeighborsNamesAnUnresolvedContract(t *testing.T) {
	for _, tc := range []struct {
		name string
		edge *client.GraphEdge
		want string
	}{
		{"topic", &client.GraphEdge{Kind: "produces", Topic: "orders.created", RepoID: "r"}, `"topic:orders.created"`},
		{"route", &client.GraphEdge{Kind: "http_call", Method: "POST", Path: "/charges", RepoID: "r"}, `"POST /charges"`},
		{"callee", &client.GraphEdge{Kind: "call", DstName: "thirdparty.Do", RepoID: "r"}, `"thirdparty.Do"`},
		{"nothing", &client.GraphEdge{Kind: "call", RepoID: "r"}, "unnamed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := Neighbors(&client.NeighborsResponse{
				Center: &client.Unit{ID: "1", RepoID: "r", FilePath: "a.go", Name: "F", Kind: "function"},
				Out:    []*client.EdgeHop{{Edge: tc.edge}},
			})
			if !strings.Contains(out, tc.want) || !strings.Contains(out, "not indexed here") {
				t.Errorf("want %s in:\n%s", tc.want, out)
			}
		})
	}
}

func TestTraceWithholdsAlternativesByDefault(t *testing.T) {
	res := &client.TraceResponse{
		Param: "user_id", Chains: 4,
		Steps: []*client.TraceStep{{
			Unit:    &client.Unit{ID: "1", RepoID: "r", FilePath: "a.go", Name: "F", Kind: "function", StartLine: 3},
			Tracked: []string{"userID"}, Confidence: 1,
		}},
		Alternatives: [][]*client.TraceStep{
			{{Unit: &client.Unit{ID: "2", RepoID: "r", FilePath: "b.go", Name: "G", Kind: "function"}, Tracked: []string{"uid"}}},
		},
	}
	brief := Trace(res, "F", false)
	if !strings.Contains(brief, "1 alternative chains withheld") || strings.Contains(brief, "G (function)") {
		t.Errorf("alternatives leaked into the cheap answer:\n%s", brief)
	}
	if !strings.Contains(brief, "Confidence is cumulative") {
		t.Errorf("a chain without its confidence caveat invites over-reporting:\n%s", brief)
	}
	full := Trace(res, "F", true)
	if !strings.Contains(full, "alternative 1:") || !strings.Contains(full, "G (function)") {
		t.Errorf("include_alternatives did not add them:\n%s", full)
	}
}

func TestTraceEmptyExplainsTheMatchingRule(t *testing.T) {
	out := Trace(&client.TraceResponse{Param: "user"}, "CreateOrder", false)
	if !strings.Contains(out, "word boundaries") {
		t.Errorf("an empty trace should explain why a name may not have matched:\n%s", out)
	}
}

func TestStatusDistinguishesNeverIndexedFromNineteenSeventy(t *testing.T) {
	out := Status(
		&client.HealthResponse{Status: "ok", Version: "v1", APIVersion: "0.2.0"},
		"http://localhost:8080",
		[]*client.Repo{
			{ID: "a-1", Name: "a", Status: "idle", IndexedAt: 1_700_000_000},
			{ID: "b-2", Name: "b", Status: "error", LastError: "clone failed\nmore"},
		},
		&client.StatsResponse{Indexers: map[string]client.IndexerStats{
			"vector": {Documents: 10, SizeBytes: 2048, Repos: 3},
			"bm25":   {Documents: 20},
		}},
		nil,
	)
	if !strings.Contains(out, "indexed never") {
		t.Errorf("a repository that has never been indexed must not read as 1970:\n%s", out)
	}
	if !strings.Contains(out, "error clone failed") || strings.Contains(out, "more") {
		t.Errorf("only the first line of a failure belongs in a listing:\n%s", out)
	}
	// Only the vector indexer fills all three counters; "0 repos" beside a
	// document count reads as a fault rather than as "not reported".
	if !strings.Contains(out, "vector 10 documents, 3 repos, 2.0 KB") {
		t.Errorf("vector stats line is wrong:\n%s", out)
	}
	if !strings.Contains(out, "bm25 20 documents\n") {
		t.Errorf("bm25 should report documents alone:\n%s", out)
	}
	// Map order must not change between two identical calls.
	if strings.Index(out, "bm25") > strings.Index(out, "vector") {
		t.Errorf("indexers are not rendered in a stable order:\n%s", out)
	}
}

func TestCoverageWarnsOnlyWhenItMatters(t *testing.T) {
	low := Status(nil, "u", nil, nil, &client.Coverage{
		RepoID: "a-1", Reported: true, UpdatedAt: 100, IndexedAt: 90,
		Totals: client.CoverageKind{Kind: "all", Candidates: 500, Edges: 100, Ratio: 0.2},
		Kinds: []client.CoverageKind{
			{Kind: "http", Candidates: 400, Edges: 80, Ratio: 0.2},
			// A kind with no candidates is fully covered by definition; printing
			// it would be a line that says nothing.
			{Kind: "messaging", Candidates: 0, Edges: 0, Ratio: 1},
		},
	})
	if !strings.Contains(low, "not evidence") {
		t.Errorf("a low ratio must be spelled out:\n%s", low)
	}
	if strings.Contains(low, "messaging") {
		t.Errorf("a kind with no candidates should not be listed:\n%s", low)
	}

	high := Status(nil, "u", nil, nil, &client.Coverage{
		RepoID: "a-1", Reported: true, UpdatedAt: 100, IndexedAt: 90,
		Totals: client.CoverageKind{Kind: "all", Candidates: 100, Edges: 100, Ratio: 1},
	})
	if strings.Contains(high, "not evidence") {
		t.Errorf("full coverage needs no caveat:\n%s", high)
	}

	stale := Status(nil, "u", nil, nil, &client.Coverage{
		RepoID: "a-1", Reported: true, UpdatedAt: 90, IndexedAt: 100,
		Totals: client.CoverageKind{Kind: "all", Candidates: 100, Edges: 100, Ratio: 1},
	})
	if !strings.Contains(stale, "predates") {
		t.Errorf("a summary older than the index describes another pass:\n%s", stale)
	}

	never := Status(nil, "u", nil, nil, &client.Coverage{RepoID: "a-1"})
	if !strings.Contains(never, "meaningless rather than zero") {
		t.Errorf("unreported coverage must not read as zero coverage:\n%s", never)
	}
}

func TestServicesWithNoLinksPointsAtCoverage(t *testing.T) {
	out := Services(&client.ServicesResponse{
		Services: []*client.ServiceInfo{{RepoID: "a-1", Name: "api", Root: "", DetectedBy: "root", UnitID: "5"}},
	})
	// No resolved cross-service call is either the estate or the indexer, and
	// coverage is the only thing that tells them apart.
	if !strings.Contains(out, "ragota_status") {
		t.Errorf("an empty link list should say how to check whether it is real:\n%s", out)
	}
	if !strings.Contains(out, "root . (root)") {
		t.Errorf("a service at the repository root should render as \".\":\n%s", out)
	}
}

// The point of rendering text rather than returning the API's JSON is cost. This
// records what a full-size answer actually costs, so that a change to the
// renderer which quietly doubles it fails here instead of in a context window.
func TestRenderedSearchStaysSmallerThanTheWireJSON(t *testing.T) {
	hits := make([]*client.SearchHit, 0, 10)
	for i := range 10 {
		hits = append(hits, &client.SearchHit{
			RepoID: "orders-aaaaaaaaaaaa", FilePath: fmt.Sprintf("internal/http/handler_%d.go", i),
			Line: 40 + i, EndLine: 88 + i, Symbol: fmt.Sprintf("CheckoutHandler%d", i),
			Kind: "function", Language: "go", Score: 1.5, Reason: "semantic+keyword",
			Snippet: "func (h *CartHandler) Checkout(w http.ResponseWriter, r *http.Request) error {",
		})
	}
	res := &client.SearchResponse{Query: "where does checkout go", Mode: "hybrid", Total: 10, Hits: hits}
	out := Search(res)

	wire, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) >= len(wire) {
		t.Errorf("rendering to text cost %d bytes against the wire JSON's %d; the whole point of this package is that it is cheaper", len(out), len(wire))
	}

	// Ten hits with a line of code each: on the order of 1.5 KB, a few hundred
	// tokens. The ceiling is generous; it exists to catch an order of magnitude,
	// not a rewording.
	const ceiling = 3 << 10
	if len(out) > ceiling {
		t.Errorf("ten hits rendered to %d bytes, over the %d-byte ceiling:\n%s", len(out), ceiling, out)
	}
	t.Logf("ragota_search, 10 hits, snippet=line: %d bytes rendered, %d bytes as wire JSON", len(out), len(wire))
}

// The same record for the expensive tool. A default /context call against a small
// corpus has measured over ten thousand tokens as JSON; this is what the same
// answer costs once the expansion is capped and rendered.
func TestRenderedContextStaysWithinItsCeiling(t *testing.T) {
	items := make([]*client.ContextItem, 0, 5)
	for i := range 5 {
		related := make([]*client.RelatedUnit, 0, 24)
		for j := range 24 {
			related = append(related, &client.RelatedUnit{
				Unit: &client.Unit{
					ID: fmt.Sprint(j), RepoID: "billing-bbbbbbbbbbbb", FilePath: "internal/consume/order.go",
					Name: fmt.Sprintf("ConsumeOrderCreated%d", j), Kind: "function", StartLine: 20, EndLine: 44,
				},
				Service: "billing", Via: "consumes", Direction: "in", Distance: 1,
			})
		}
		items = append(items, &client.ContextItem{
			Hit: &client.SearchHit{
				RepoID: "orders-aaaaaaaaaaaa", FilePath: fmt.Sprintf("internal/kafka/publish_%d.go", i),
				Line: 31, EndLine: 60, Symbol: "PublishOrderCreated", Kind: "function", Reason: "keyword",
				Snippet: "func PublishOrderCreated(ctx context.Context, o Order) error {",
			},
			Unit: &client.Unit{
				ID: fmt.Sprint(9000 + i), RepoID: "orders-aaaaaaaaaaaa", FilePath: "internal/kafka/publish.go",
				Name: "PublishOrderCreated", Kind: "function", StartLine: 31, EndLine: 60,
				Signature: "func PublishOrderCreated(ctx context.Context, o Order) error",
			},
			Service: "orders",
			Related: related,
		})
	}
	out := Context(&client.ContextResponse{Query: "who consumes order.created", Mode: "keyword", Items: items})

	const ceiling = 6 << 10
	if len(out) > ceiling {
		t.Errorf("five expanded items rendered to %d bytes, over the %d-byte ceiling", len(out), ceiling)
	}
	t.Logf("ragota_context, 5 items each with 24 related units, snippet=line: %d bytes", len(out))
}

func TestEmptyAnswersAreStatedRatherThanBlank(t *testing.T) {
	for name, got := range map[string]string{
		"symbols":    Symbols(&client.SymbolResponse{}, "Foo"),
		"references": References(&client.ReferencesResponse{}),
		"path":       Path(&client.GraphPathResponse{}),
		"services":   Services(&client.ServicesResponse{}),
		"topics":     Topics(&client.TopicsResponse{}),
		"neighbors":  Neighbors(&client.NeighborsResponse{}),
	} {
		if strings.TrimSpace(got) == "" {
			t.Errorf("%s rendered nothing at all; a blank tool result tells a model nothing", name)
		}
	}
}
