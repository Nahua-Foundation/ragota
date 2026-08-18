package server

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/client"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Nahua-Foundation/ragota/internal/mcp/config"
)

const searchPath = "/api/v1/search"

func TestToolListMatchesToolNames(t *testing.T) {
	cs := connect(t, newStub(t))

	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var got []string
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
	}
	slices.Sort(got)
	want := ToolNames()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("registered tools %v, ToolNames says %v", got, want)
	}
}

// The tool descriptions are the only place a model is told which of the two
// retrieval tools to reach for, and the split is worth 0.714 against 0.587 in
// MRR. A description that lost that sentence would be a silent quality
// regression, so the pairing is asserted rather than trusted.
func TestSearchAndSymbolDescriptionsPointAtEachOther(t *testing.T) {
	cs := connect(t, newStub(t))

	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		byName[tool.Name] = tool
	}

	for name, mustMention := range map[string]string{
		"ragota_search": "ragota_symbol",
		"ragota_symbol": "ragota_search",
	} {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if !strings.Contains(tool.Description, mustMention) {
			t.Errorf("%s does not tell the model when to use %s instead:\n%s", name, mustMention, tool.Description)
		}
		if !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is not marked read-only", name)
		}
	}
}

// A value ragota does not recognise is a 400, never a quiet downgrade, so
// the legal set has to be in the schema the model reads.
func TestEnumsReachTheSchema(t *testing.T) {
	cs := connect(t, newStub(t))

	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	want := map[string]map[string][]string{
		"ragota_search":   {"snippet": snippetModes, "intent": intents},
		"ragota_context":  {"snippet": snippetModes, "intent": intents},
		"ragota_services": {"format": serviceFormats},
	}
	for _, tool := range res.Tools {
		fields, ok := want[tool.Name]
		if !ok {
			continue
		}
		schema, ok := tool.InputSchema.(map[string]any)
		if !ok {
			t.Fatalf("%s input schema is %T", tool.Name, tool.InputSchema)
		}
		props, _ := schema["properties"].(map[string]any)
		for field, values := range fields {
			prop, _ := props[field].(map[string]any)
			enum, _ := prop["enum"].([]any)
			if len(enum) != len(values) {
				t.Errorf("%s.%s enum is %v, wanted %v", tool.Name, field, enum, values)
			}
		}
	}
}

// Required-versus-optional is inferred from the json tags, so a missing
// `omitempty` silently makes an argument mandatory for every caller.
func TestRequiredArgumentsAreOnlyTheUnavoidableOnes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema func(*jsonschema.ForOptions) (*jsonschema.Schema, error)
		want   []string
	}{
		{"search", jsonschema.For[searchInput], []string{"query"}},
		{"context", jsonschema.For[contextInput], []string{"query"}},
		{"symbol", jsonschema.For[symbolInput], nil},
		{"references", jsonschema.For[referencesInput], []string{"repo", "file_path", "line"}},
		{"neighbors", jsonschema.For[neighborsInput], []string{"unit_id"}},
		{"path", jsonschema.For[pathInput], []string{"from_unit_id", "to_unit_id"}},
		{"trace", jsonschema.For[traceInput], []string{"symbol", "param"}},
		{"services", jsonschema.For[servicesInput], nil},
		{"topics", jsonschema.For[topicsInput], nil},
		{"status", jsonschema.For[statusInput], nil},
	} {
		schema, err := tc.schema(nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got := slices.Clone(schema.Required)
		slices.Sort(got)
		want := slices.Clone(tc.want)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s required %v, wanted %v", tc.name, got, want)
		}
	}
}

// --- startup ---

func TestStartupCheckRejectsAnOlderContract(t *testing.T) {
	s := newStub(t)
	// A server predating the fields this build reads would serve them as absent,
	// which a caller reads as "no results" rather than as "unsupported". That is
	// the failure that does not look like one, so it has to be refused loudly.
	s.reply("/health", client.HealthResponse{Status: "ok", Version: "old", APIVersion: "0.0.1"})

	_, err := newServer(s).StartupCheck(t.Context())
	if err == nil {
		t.Fatal("startup check accepted a server speaking 0.0.1")
	}
	for _, want := range []string{"0.0.1", client.SchemaVersion} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
}

func TestStartupCheckReportsAnUnreachableServer(t *testing.T) {
	s := newStub(t)
	srv := newServer(s, func(c *config.Config) {
		// A port nothing listens on: the failure must name the address, because
		// that is the thing the operator has to fix.
		c.BaseURL = "http://127.0.0.1:1"
	})
	_, err := srv.StartupCheck(t.Context())
	if err == nil {
		t.Fatal("startup check accepted an unreachable server")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") || !strings.Contains(err.Error(), "RAGOTA_URL") {
		t.Errorf("error does not point at the address or the variable that sets it: %v", err)
	}
}

func TestStartupCheckRejectsABadKeyBeforeTheFirstQuestion(t *testing.T) {
	s := newStub(t)
	s.fail("/api/v1/repos", http.StatusUnauthorized, client.CodeUnauthorized, "unknown api key")

	_, err := newServer(s).StartupCheck(t.Context())
	if err == nil {
		t.Fatal("startup check accepted a rejected key")
	}
	if !strings.Contains(err.Error(), "RAGOTA_MCP_KEY") {
		t.Errorf("error does not name the variable an operator fixes: %v", err)
	}
}

func TestStartupCheckRejectsAnUnknownDefaultScope(t *testing.T) {
	s := newStub(t)
	srv := newServer(s, func(c *config.Config) { c.Repos = []string{"nope"} })

	_, err := srv.StartupCheck(t.Context())
	if err == nil {
		t.Fatal("startup check accepted a scope naming no repository")
	}
	if !strings.Contains(err.Error(), "RAGOTA_REPOS") || !strings.Contains(err.Error(), ordersID) {
		t.Errorf("error should name the variable and the repositories that do exist: %v", err)
	}
}

func TestStartupCheckSendsTheKey(t *testing.T) {
	s := newStub(t)
	if _, err := newServer(s).StartupCheck(t.Context()); err != nil {
		t.Fatalf("startup check: %v", err)
	}
	if got := s.last(t, "/api/v1/repos").header.Get("X-API-Key"); got != "test-key" {
		t.Errorf("X-API-Key is %q", got)
	}
	// /health takes no credential, which is what makes it able to separate a
	// network failure from a key failure.
	if got := s.last(t, "/health").header.Get("X-API-Key"); got != "test-key" {
		t.Logf("the client sends the key to /health as well (harmless): %q", got)
	}
}

// --- search ---

func TestSearchBudgetsTheAnswerAndAsksForDiagnostics(t *testing.T) {
	s := newStub(t)
	s.reply(searchPath, client.SearchResponse{
		Query: "where is checkout handled", Mode: "hybrid", Total: 1,
		Hits: []*client.SearchHit{{
			RepoID: ordersID, FilePath: "internal/http/cart.go", Line: 42, EndLine: 88,
			Symbol: "CheckoutHandler", Kind: "function", Language: "go",
			Reason: "semantic+keyword", Snippet: "func (h *CartHandler) Checkout(w, r) {",
		}},
	})
	cs := connect(t, s)

	out := call(t, cs, "ragota_search", map[string]any{"query": "where is checkout handled"})
	for _, want := range []string{ordersID, "internal/http/cart.go:42-88", "CheckoutHandler", "semantic+keyword"} {
		if !strings.Contains(out, want) {
			t.Errorf("answer does not carry %q:\n%s", want, out)
		}
	}

	body := s.lastBody(t, searchPath)
	if got := body["snippet"]; got != client.SnippetLine {
		t.Errorf("snippet default is %v, wanted %q — the whole chunk is the largest thing in a response", got, client.SnippetLine)
	}
	if got := body["max_bytes"]; got != float64(config.DefaultMaxBytes) {
		t.Errorf("max_bytes is %v, wanted the configured default %d", got, config.DefaultMaxBytes)
	}
	if got := body["diagnostics"]; got != true {
		t.Errorf("diagnostics is %v: without it a degraded answer cannot be told from a thin one", got)
	}
}

func TestSearchSurfacesDegradation(t *testing.T) {
	s := newStub(t)
	s.reply(searchPath, client.SearchResponse{
		Query: "who charges the card", Mode: "hybrid",
		Hits: nil,
		Diagnostics: &client.SearchDiagnostics{
			Degraded:        true,
			Searchers:       []string{"bm25"},
			FailedSearchers: []string{"vector"},
			SearcherErrors:  map[string]string{"vector": "dial tcp 127.0.0.1:6333: connection refused"},
		},
	})
	cs := connect(t, s)

	out := call(t, cs, "ragota_search", map[string]any{"query": "who charges the card"})
	for _, want := range []string{"DEGRADED", "vector", "connection refused", "not evidence"} {
		if !strings.Contains(out, want) {
			t.Errorf("degraded answer does not say %q:\n%s", want, out)
		}
	}
}

func TestSearchSaysWhenAnEmptyAnswerIsRealAndDoesNotChatterOtherwise(t *testing.T) {
	s := newStub(t)
	s.reply(searchPath, client.SearchResponse{
		Query: "nothing here", Mode: "hybrid",
		Diagnostics: &client.SearchDiagnostics{Degraded: false, Searchers: []string{"bm25", "vector"}},
	})
	cs := connect(t, s)

	empty := call(t, cs, "ragota_search", map[string]any{"query": "nothing here"})
	if !strings.Contains(empty, "not degraded") {
		t.Errorf("an empty answer must say retrieval was whole:\n%s", empty)
	}

	s.reply(searchPath, client.SearchResponse{
		Query: "found", Mode: "hybrid", Total: 1,
		Hits:        []*client.SearchHit{{RepoID: ordersID, FilePath: "a.go", Line: 1}},
		Diagnostics: &client.SearchDiagnostics{Degraded: false, Searchers: []string{"bm25", "vector"}},
	})
	// A healthy answer with hits gets no commentary at all: commentary is bytes,
	// and there is nothing here for the caller to decide.
	full := call(t, cs, "ragota_search", map[string]any{"query": "found"})
	if strings.Contains(full, "degraded") || strings.Contains(full, "DEGRADED") {
		t.Errorf("a healthy answer should carry no diagnostics prose:\n%s", full)
	}
}

func TestSearchReportsTruncation(t *testing.T) {
	s := newStub(t)
	s.reply(searchPath, client.SearchResponse{
		Query: "big", Mode: "hybrid", Total: 10, Truncated: true,
		Hits: []*client.SearchHit{{RepoID: ordersID, FilePath: "a.go", Line: 1}},
	})
	cs := connect(t, s)

	out := call(t, cs, "ragota_search", map[string]any{"query": "big"})
	for _, want := range []string{"9 of the 10", "max_bytes"} {
		if !strings.Contains(out, want) {
			t.Errorf("truncated answer does not say %q:\n%s", want, out)
		}
	}
}

func TestSearchFlattensTheFilter(t *testing.T) {
	s := newStub(t)
	s.reply(searchPath, client.SearchResponse{Query: "q", Mode: "hybrid"})
	cs := connect(t, s)

	call(t, cs, "ragota_search", map[string]any{
		"query":       "q",
		"languages":   []string{"go"},
		"kinds":       []string{"function"},
		"path_prefix": "services/orders",
	})
	filter, ok := s.lastBody(t, searchPath)["filter"].(map[string]any)
	if !ok {
		t.Fatalf("no filter reached the server: %v", s.lastBody(t, searchPath))
	}
	if filter["path_prefix"] != "services/orders" {
		t.Errorf("filter is %v", filter)
	}
	if _, ok := filter["languages"]; !ok {
		t.Errorf("filter is %v", filter)
	}
}

func TestSearchRejectsAnEmptyQuery(t *testing.T) {
	cs := connect(t, newStub(t))
	msg := callError(t, cs, "ragota_search", map[string]any{"query": "   "})
	if !strings.Contains(msg, "question") {
		t.Errorf("message does not say what the argument should be: %s", msg)
	}
}

func TestSearchRejectsAnInventedEnumValue(t *testing.T) {
	s := newStub(t)
	cs := connect(t, s)

	// Schema validation runs before the handler, so this never reaches
	// ragota to come back as a 400 the model has to interpret.
	msg := callError(t, cs, "ragota_search", map[string]any{"query": "q", "snippet": "full"})
	if !strings.Contains(msg, "snippet") {
		t.Errorf("message does not name the offending field: %s", msg)
	}
	if n := s.calls(searchPath); n != 0 {
		t.Errorf("an invalid argument reached the server %d times", n)
	}
}

// --- repository scoping ---

func TestScopeAcceptsANameAndSendsTheID(t *testing.T) {
	s := newStub(t)
	s.reply(searchPath, client.SearchResponse{Query: "q", Mode: "hybrid"})
	cs := connect(t, s)

	call(t, cs, "ragota_search", map[string]any{"query": "q", "repos": []string{"orders"}})
	repos, _ := s.lastBody(t, searchPath)["repos"].([]any)
	if len(repos) != 1 || repos[0] != ordersID {
		t.Fatalf("repos reached the server as %v, wanted [%s]", repos, ordersID)
	}
}

func TestScopeRefusesAnUnknownRepositoryInsteadOfAnsweringEmpty(t *testing.T) {
	s := newStub(t)
	s.reply(searchPath, client.SearchResponse{Query: "q", Mode: "hybrid"})
	cs := connect(t, s)

	// ragota filters on ids and answers an unknown one with zero hits, so a
	// scoping typo would otherwise read as "there is no such code".
	msg := callError(t, cs, "ragota_search", map[string]any{"query": "q", "repos": []string{"odrers"}})
	for _, want := range []string{"odrers", ordersID, billingID} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not carry %q: %s", want, msg)
		}
	}
	if n := s.calls(searchPath); n != 0 {
		t.Errorf("a bad scope still reached /search %d times", n)
	}
}

func TestConfiguredScopeAppliesWhenTheCallNamesNone(t *testing.T) {
	s := newStub(t)
	s.reply(searchPath, client.SearchResponse{Query: "q", Mode: "hybrid"})
	cs := connect(t, s, func(c *config.Config) { c.Repos = []string{"billing"} })

	call(t, cs, "ragota_search", map[string]any{"query": "q"})
	repos, _ := s.lastBody(t, searchPath)["repos"].([]any)
	if len(repos) != 1 || repos[0] != billingID {
		t.Fatalf("configured scope did not apply: %v", repos)
	}

	call(t, cs, "ragota_search", map[string]any{"query": "q", "repos": []string{"orders"}})
	repos, _ = s.lastBody(t, searchPath)["repos"].([]any)
	if len(repos) != 1 || repos[0] != ordersID {
		t.Fatalf("an explicit scope must win over the configured one: %v", repos)
	}
}

// --- context ---

func TestContextRendersTheGraphAroundEachHit(t *testing.T) {
	s := newStub(t)
	s.reply("/api/v1/context", client.ContextResponse{
		Query: "who consumes order.created", Mode: "keyword",
		Items: []*client.ContextItem{{
			Hit: &client.SearchHit{
				RepoID: ordersID, FilePath: "internal/kafka/publish.go", Line: 31, EndLine: 60,
				Symbol: "PublishOrderCreated", Kind: "function", Reason: "keyword",
			},
			Unit:    unit("9021", ordersID, "internal/kafka/publish.go", "PublishOrderCreated", "function", 31, 60),
			Service: "orders",
			Related: []*client.RelatedUnit{{
				Unit:      unit("7714", billingID, "internal/consume/order.go", "ConsumeOrderCreated", "function", 20, 44),
				Service:   "billing",
				Via:       "consumes",
				Direction: "in",
				Distance:  1,
			}},
		}},
	})
	cs := connect(t, s)

	out := call(t, cs, "ragota_context", map[string]any{"query": "who consumes order.created"})
	for _, want := range []string{"PublishOrderCreated", "unit 9021", "<- d1 consumes", "ConsumeOrderCreated", "billing"} {
		if !strings.Contains(out, want) {
			t.Errorf("answer does not carry %q:\n%s", want, out)
		}
	}
	body := s.lastBody(t, "/api/v1/context")
	if body["max_bytes"] != float64(config.DefaultMaxBytes) || body["snippet"] != client.SnippetLine {
		t.Errorf("context call was not budgeted: %v", body)
	}
}

// /context carries no diagnostics of its own, and its empty answer is the one an
// agent is most likely to report as "nothing calls it".
func TestContextChecksRetrievalHealthOnlyWhenEmpty(t *testing.T) {
	s := newStub(t)
	s.reply("/api/v1/context", client.ContextResponse{Query: "q", Mode: "keyword"})
	s.reply(searchPath, client.SearchResponse{
		Query: "q", Mode: "hybrid",
		Diagnostics: &client.SearchDiagnostics{
			Degraded:        true,
			FailedSearchers: []string{"bm25"},
			SearcherErrors:  map[string]string{"bm25": "index closed"},
		},
	})
	cs := connect(t, s)

	out := call(t, cs, "ragota_context", map[string]any{"query": "q"})
	if !strings.Contains(out, "DEGRADED") || !strings.Contains(out, "bm25") {
		t.Errorf("an empty context answer must report degradation:\n%s", out)
	}
	if n := s.calls(searchPath); n != 1 {
		t.Fatalf("expected exactly one probe, got %d", n)
	}

	s.reply("/api/v1/context", client.ContextResponse{
		Query: "q", Mode: "keyword",
		Items: []*client.ContextItem{{Hit: &client.SearchHit{RepoID: ordersID, FilePath: "a.go", Line: 1}}},
	})
	call(t, cs, "ragota_context", map[string]any{"query": "q"})
	if n := s.calls(searchPath); n != 1 {
		t.Errorf("a non-empty context answer should not pay for a probe: %d probes", n)
	}
}

// --- symbol ---

func TestSymbolLooksUpAnIdentifier(t *testing.T) {
	s := newStub(t)
	s.reply("/api/v1/nav/symbol", client.SymbolResponse{
		Total: 1,
		Symbols: []*client.ASTSymbol{{
			RepoID: ordersID, FilePath: "internal/app/order.go", Language: "go",
			Kind: "function", Name: "ShipOrder", Qualified: "app.ShipOrder",
			StartLine: 12, EndLine: 40, Doc: "ShipOrder hands the order to the carrier.\nMore prose.",
		}},
	})
	cs := connect(t, s)

	out := call(t, cs, "ragota_symbol", map[string]any{"symbol": "ShipOrder", "repo": "orders"})
	for _, want := range []string{"ShipOrder", "app.ShipOrder", "internal/app/order.go:12-40"} {
		if !strings.Contains(out, want) {
			t.Errorf("answer does not carry %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "More prose") {
		t.Errorf("only the first doc line belongs in a listing:\n%s", out)
	}
	if got := mustStringField(t, s.lastBody(t, "/api/v1/nav/symbol"), "symbol"); got != "ShipOrder" {
		t.Errorf("symbol reached the server as %q", got)
	}
	if got := mustStringField(t, s.lastBody(t, "/api/v1/nav/symbol"), "repo_id"); got != ordersID {
		t.Errorf("repo reached the server as %q", got)
	}
}

func TestSymbolRefusesAnUnfilteredLookup(t *testing.T) {
	cs := connect(t, newStub(t))
	msg := callError(t, cs, "ragota_symbol", map[string]any{})
	if !strings.Contains(msg, "symbol") || !strings.Contains(msg, "kind") {
		t.Errorf("message does not name the selectors this tool has: %s", msg)
	}
}

func TestSymbolEmptyAnswerPointsAtSearch(t *testing.T) {
	s := newStub(t)
	s.reply("/api/v1/nav/symbol", client.SymbolResponse{})
	cs := connect(t, s)

	out := call(t, cs, "ragota_symbol", map[string]any{"symbol": "NoSuchThing"})
	if !strings.Contains(out, "ragota_search") {
		t.Errorf("an empty name lookup should send a prose question elsewhere:\n%s", out)
	}
}

// --- graph ---

func TestReferencesRendersResolvedAndUnresolvedSites(t *testing.T) {
	s := newStub(t)
	s.reply("/api/v1/nav/references", client.ReferencesResponse{
		Total: 2,
		References: []*client.ASTReference{
			{RepoID: ordersID, FilePath: "internal/app/order.go", Line: 88, Kind: "call", Word: "ShipOrder", Target: "app.ShipOrder"},
			{RepoID: billingID, FilePath: "internal/x.go", Line: 4, Kind: "reference", Word: "ShipOrder"},
		},
	})
	cs := connect(t, s)

	out := call(t, cs, "ragota_references", map[string]any{
		"repo": "orders", "file_path": "internal/app/order.go", "line": 12,
	})
	if !strings.Contains(out, "-> app.ShipOrder") || !strings.Contains(out, "(unresolved)") {
		t.Errorf("a resolved and an unresolved reference must read differently:\n%s", out)
	}
	body := s.lastBody(t, "/api/v1/nav/references")
	pos, _ := body["position"].(map[string]any)
	if pos["line"] != float64(12) {
		t.Errorf("position did not reach the server: %v", body)
	}
}

func TestReferencesRejectsLineZero(t *testing.T) {
	cs := connect(t, newStub(t))
	// Line 0 always resolves to nothing, so a 0-based caller would get a
	// confident empty answer instead of a correction.
	msg := callError(t, cs, "ragota_references", map[string]any{
		"repo": "orders", "file_path": "a.go", "line": 0,
	})
	if !strings.Contains(msg, "1-based") {
		t.Errorf("message does not explain the convention: %s", msg)
	}
}

func TestNeighborsRendersBothDirectionsAndUnindexedFarSides(t *testing.T) {
	s := newStub(t)
	s.reply("/api/v1/graph/neighbors", client.NeighborsResponse{
		Center: unit("9021", ordersID, "internal/http/cart.go", "CheckoutHandler", "function", 42, 88),
		Out: []*client.EdgeHop{
			{
				Edge: &client.GraphEdge{Kind: "call", Confidence: 0.95, RepoID: ordersID, FilePath: "internal/http/cart.go", Line: 50},
				Unit: unit("7714", ordersID, "internal/app/order.go", "CreateOrder", "function", 12, 40),
			},
			{Edge: &client.GraphEdge{
				Kind: "http_call", Confidence: 0.8, RepoID: ordersID,
				FilePath: "internal/http/cart.go", Line: 61, Method: "POST", Path: "/charges",
			}},
		},
		In: []*client.EdgeHop{{
			Edge: &client.GraphEdge{Kind: "handles", Confidence: 1, RepoID: ordersID},
			Unit: unit("3311", ordersID, "routes.go", "http:POST /cart/checkout", "http_route", 9, 9),
		}},
	})
	cs := connect(t, s)

	out := call(t, cs, "ragota_neighbors", map[string]any{"unit_id": "9021"})
	for _, want := range []string{"center", "out (2)", "in (1)", "CreateOrder", "POST /charges", "not indexed here"} {
		if !strings.Contains(out, want) {
			t.Errorf("answer does not carry %q:\n%s", want, out)
		}
	}
}

func TestNeighborsExplainsAStaleUnitID(t *testing.T) {
	s := newStub(t)
	s.fail("/api/v1/graph/neighbors", http.StatusNotFound, client.CodeNotFound, "unit not found")
	cs := connect(t, s)

	// A bare "not found" invites a retry of the same id. The likely cause is a
	// reindex, and the fix is to fetch a fresh id rather than to try again.
	msg := callError(t, cs, "ragota_neighbors", map[string]any{"unit_id": "999"})
	for _, want := range []string{`"999"`, "do not survive a reindex", "ragota_context"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not carry %q: %s", want, msg)
		}
	}
}

func TestPathReportsNoPathAsAnAnswer(t *testing.T) {
	s := newStub(t)
	s.reply("/api/v1/graph/path", client.GraphPathResponse{})
	cs := connect(t, s)

	// An empty result is a 200 here, and covers both "unreachable" and "no such
	// unit". Reporting it as an error would invent a distinction the API refuses.
	out := call(t, cs, "ragota_path", map[string]any{"from_unit_id": "1", "to_unit_id": "2"})
	if !strings.Contains(out, "No path") || !strings.Contains(out, "does not distinguish") {
		t.Errorf("answer does not explain what an empty path means:\n%s", out)
	}
}

func TestPathRendersSteps(t *testing.T) {
	s := newStub(t)
	s.reply("/api/v1/graph/path", client.GraphPathResponse{
		Length: 2,
		Steps: []*client.PathStep{
			{Unit: unit("1", ordersID, "a.go", "Handler", "function", 1, 9)},
			{Unit: unit("2", billingID, "b.go", "Charge", "rpc_method", 4, 8), Via: "rpc_call"},
		},
	})
	cs := connect(t, s)

	out := call(t, cs, "ragota_path", map[string]any{"from_unit_id": "1", "to_unit_id": "2"})
	if !strings.Contains(out, "via rpc_call") || !strings.Contains(out, "Charge") {
		t.Errorf("answer does not carry the hops:\n%s", out)
	}
}

func TestTraceClampsMaxDepthSoThatDeeperMeansDeeper(t *testing.T) {
	s := newStub(t)
	s.reply("/api/v1/graph/trace", client.TraceResponse{
		Param: "user_id", Chains: 3,
		Steps: []*client.TraceStep{
			{Unit: unit("1", ordersID, "a.go", "CreateOrder", "function", 1, 9), Tracked: []string{"userID"}, Confidence: 1},
			{Unit: unit("2", billingID, "b.go", "Charge", "function", 4, 8), Tracked: []string{"user_id"}, Via: "rpc_call", Confidence: 0.7, Note: "sent in gRPC request"},
		},
		Alternatives: [][]*client.TraceStep{{{Unit: unit("3", ordersID, "c.go", "Audit", "function", 1, 2), Tracked: []string{"uid"}}}},
	})
	cs := connect(t, s)

	out := call(t, cs, "ragota_trace", map[string]any{"symbol": "CreateOrder", "param": "user_id", "max_depth": 100})
	// The endpoint resets anything over 24 to 16, so 100 would search *less*
	// deeply than 24. Clamping makes the number mean what it reads as.
	if got := mustNumberField(t, s.lastBody(t, "/api/v1/graph/trace"), "max_depth"); got != traceMaxDepth {
		t.Errorf("max_depth reached the server as %v, wanted %d", got, traceMaxDepth)
	}
	if !strings.Contains(out, "1 alternative chains withheld") {
		t.Errorf("alternatives should be counted, not printed, by default:\n%s", out)
	}
	if strings.Contains(out, "Audit") {
		t.Errorf("an alternative chain leaked into the default answer:\n%s", out)
	}

	with := call(t, cs, "ragota_trace", map[string]any{
		"symbol": "CreateOrder", "param": "user_id", "include_alternatives": true,
	})
	if !strings.Contains(with, "Audit") {
		t.Errorf("include_alternatives did not add them:\n%s", with)
	}
}

func TestTraceRequiresBothArguments(t *testing.T) {
	cs := connect(t, newStub(t))
	msg := callError(t, cs, "ragota_trace", map[string]any{"symbol": "CreateOrder", "param": " "})
	if !strings.Contains(msg, "param") {
		t.Errorf("message does not name the missing argument: %s", msg)
	}
}

// --- estate ---

func TestServicesRendersTheMapAndBoundsIt(t *testing.T) {
	s := newStub(t)
	s.reply("/api/v1/services", client.ServicesResponse{
		Services: []*client.ServiceInfo{
			{RepoID: ordersID, Name: "orders", Root: "cmd/orders", DetectedBy: "cmd", UnitID: "9021"},
			{RepoID: billingID, Name: "billing", Root: "", DetectedBy: "root", UnitID: "7714"},
		},
		Links: []*client.ServiceLink{{
			SrcRepo: ordersID, SrcService: "orders", DstRepo: billingID, DstService: "billing",
			Kind: "http_call", Via: "http:POST /charges", Count: 3, Confidence: 0.9,
		}},
	})
	cs := connect(t, s)

	out := call(t, cs, "ragota_services", nil)
	for _, want := range []string{"2 services, 1 links", "unit 9021", "http:POST /charges x3"} {
		if !strings.Contains(out, want) {
			t.Errorf("answer does not carry %q:\n%s", want, out)
		}
	}
	if got := s.last(t, "/api/v1/services").query.Get("limit"); got != "60" {
		t.Errorf("default limit reached the server as %q, wanted 60 — the graph grows with every repository", got)
	}
}

func TestServicesExportSaysWhenSelectorsWereIgnored(t *testing.T) {
	s := newStub(t)
	s.text("/api/v1/services/export", "flowchart LR\n  orders --> billing\n")
	cs := connect(t, s)

	out := call(t, cs, "ragota_services", map[string]any{"format": "mermaid", "repos": []string{"orders"}})
	if !strings.Contains(out, "flowchart LR") {
		t.Errorf("diagram text did not come back:\n%s", out)
	}
	// The export route takes neither selector, so honouring them silently would
	// be a lie about what was rendered.
	if !strings.Contains(out, "ignored") {
		t.Errorf("answer does not admit that repos was ignored:\n%s", out)
	}
	if got := s.last(t, "/api/v1/services/export").query.Get("format"); got != "mermaid" {
		t.Errorf("format reached the server as %q", got)
	}
}

func TestTopicsDistinguishesDeclaredFromObserved(t *testing.T) {
	s := newStub(t)
	s.reply("/api/v1/topics", client.TopicsResponse{Topics: []*client.TopicInfo{
		{
			Topic: "orders.created", Declared: true, Description: "Emitted when an order is placed",
			Producers: []*client.TopicNode{{Unit: unit("1", ordersID, "publish.go", "Publish", "function", 3, 9), Service: "orders"}},
		},
		{Topic: "legacy.ping"},
	}})
	cs := connect(t, s)

	out := call(t, cs, "ragota_topics", nil)
	for _, want := range []string{"orders.created (declared)", "Emitted when an order is placed", "consumed by: nothing indexed here", "legacy.ping"} {
		if !strings.Contains(out, want) {
			t.Errorf("answer does not carry %q:\n%s", want, out)
		}
	}
}

func TestStatusReportsWhatAnEmptyAnswerIsWorth(t *testing.T) {
	s := newStub(t)
	s.reply("/api/v1/stats", client.StatsResponse{Indexers: map[string]client.IndexerStats{
		"bm25": {Documents: 12043},
		"ast":  {Documents: 40213},
	}})
	s.reply("/api/v1/repos/"+ordersID+"/coverage", client.Coverage{
		RepoID: ordersID, Reported: true, UpdatedAt: 1_700_000_100, IndexedAt: 1_700_000_000,
		Kinds:  []client.CoverageKind{{Kind: "http", Candidates: 140, Edges: 40, Ratio: 0.28}},
		Totals: client.CoverageKind{Kind: "all", Candidates: 523, Edges: 200, Ratio: 0.38},
	})
	cs := connect(t, s)

	out := call(t, cs, "ragota_status", map[string]any{"repo": "orders"})
	for _, want := range []string{"api " + client.SchemaVersion, "bm25 12043", ordersID, "coverage for " + ordersID, "not evidence"} {
		if !strings.Contains(out, want) {
			t.Errorf("answer does not carry %q:\n%s", want, out)
		}
	}
}

func TestStatusStillAnswersWhenStatsAreDown(t *testing.T) {
	s := newStub(t)
	s.fail("/api/v1/stats", http.StatusInternalServerError, client.CodeInternal, "boom")
	cs := connect(t, s)

	// This is the tool a caller reaches for *because* something looked wrong, so
	// a repository listing that arrived beats a clean failure.
	out := call(t, cs, "ragota_status", nil)
	if !strings.Contains(out, ordersID) {
		t.Errorf("status gave up on a failing sub-call:\n%s", out)
	}
}

func TestStatusReportsAnEmptyDeployment(t *testing.T) {
	s := newStub(t)
	s.reply("/api/v1/repos", []client.Repo{})
	cs := connect(t, s)

	out := call(t, cs, "ragota_status", nil)
	if !strings.Contains(out, "none registered") {
		t.Errorf("an empty deployment must be stated plainly:\n%s", out)
	}
}

// --- error mapping ---

func TestAPIErrorsBecomeActionableToolErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
		want   []string
	}{
		{"damaged index", http.StatusInternalServerError, client.CodeIndexDamaged,
			[]string{"reindex", "never as", "the code is not there"}},
		{"forbidden", http.StatusForbidden, client.CodeForbidden,
			[]string{"scope", "operator"}},
		{"rate limited", http.StatusTooManyRequests, client.CodeRateLimited,
			[]string{"rate limited"}},
		{"validation", http.StatusBadRequest, client.CodeValidationFailed,
			[]string{"rejected", "mode is not recognised"}},
		{"not ready", http.StatusServiceUnavailable, client.CodeNotReady,
			[]string{"dependency"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t)
			s.fail(searchPath, tc.status, tc.code, "mode is not recognised")
			cs := connect(t, s)

			msg := callError(t, cs, "ragota_search", map[string]any{"query": "q"})
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Errorf("message does not carry %q: %s", want, msg)
				}
			}
		})
	}
}

func TestAServerThatAnswersWithAProxyPageIsStillDiagnosable(t *testing.T) {
	s := newStub(t)
	s.on(searchPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
	})
	cs := connect(t, s)

	msg := callError(t, cs, "ragota_search", map[string]any{"query": "q"})
	if !strings.Contains(msg, "502") {
		t.Errorf("a non-JSON failure lost its status: %s", msg)
	}
}

// --- helpers ---

func mustStringField(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key].(string)
	if !ok {
		t.Fatalf("request carried no string %q: %v", key, m)
	}
	return v
}

func mustNumberField(t *testing.T, m map[string]any, key string) int {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("request carried no number %q: %v", key, m)
	}
	return int(v)
}
