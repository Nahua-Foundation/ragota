package api

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Nahua-Foundation/ragota/internal/service/promote"
	wire "github.com/Nahua-Foundation/ragota/pkg/client"
)

// The embedded spec is the contract an external repository generates its client
// from, so it is the one artifact where drift from the code is not a
// documentation bug but a broken client. These tests check the parts a compiler
// cannot.

func specDoc(t *testing.T) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(openAPISpec, &doc); err != nil {
		t.Fatalf("openapi.yaml does not parse: %v", err)
	}
	return doc
}

// TestSchemaVersionMatchesSpec: /health serves SchemaVersion as `api_version`,
// and it is worth nothing if it is not the version of the document a client
// generated itself from.
func TestSchemaVersionMatchesSpec(t *testing.T) {
	info, ok := specDoc(t)["info"].(map[string]any)
	if !ok {
		t.Fatal("openapi.yaml has no info block")
	}
	got := fmt.Sprint(info["version"])
	if got != SchemaVersion {
		t.Errorf("openapi.yaml info.version = %q, api.SchemaVersion = %q; bump both together", got, SchemaVersion)
	}
}

// TestSpecErrorCodesAreDocumented: a client branches on `code`, so a code the
// server can send and the spec does not list is one a generated client has no
// case for.
func TestSpecErrorCodesAreDocumented(t *testing.T) {
	served := []string{
		CodeRepoBusy, CodeCommitGap, CodePayloadTooLarge, CodeInvalidPath,
		CodeNotFound, CodeValidationFailed, CodeRateLimited, CodeUnauthorized,
		CodeForbidden, CodeInternal, CodeNotReady, CodeIndexDamaged,
	}

	documented := specErrorCodes(t)
	for _, code := range served {
		if !documented[code] {
			t.Errorf("error code %q is served but missing from the Error.code enum", code)
		}
	}
	known := make(map[string]bool, len(served))
	for _, code := range served {
		known[code] = true
	}
	for code := range documented {
		if !known[code] {
			t.Errorf("error code %q is documented but nothing serves it", code)
		}
	}
}

// specErrorCodes reads the `code` enum of the Error schema — the set a client
// generated from this document would have cases for.
func specErrorCodes(t *testing.T) map[string]bool {
	t.Helper()
	components, _ := specDoc(t)["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	errSchema, _ := schemas["Error"].(map[string]any)
	props, _ := errSchema["properties"].(map[string]any)
	codeProp, _ := props["code"].(map[string]any)
	rawEnum, _ := codeProp["enum"].([]any)
	if len(rawEnum) == 0 {
		t.Fatal("the Error schema documents no code enum")
	}
	out := make(map[string]bool, len(rawEnum))
	for _, v := range rawEnum {
		out[fmt.Sprint(v)] = true
	}
	return out
}

// TestEveryServedCodeHasAClientSentinel: the public client promises callers
// they can write errors.Is(err, client.ErrRepoBusy) instead of matching on a
// message. That promise is only kept for the codes it has a value for, and a
// code added to the server without one leaves the caller back on the default
// branch. TestSpecErrorCodesAreDocumented ties the served codes to the spec;
// this ties them to the client.
func TestEveryServedCodeHasAClientSentinel(t *testing.T) {
	sentinels := map[string]error{
		CodeRepoBusy:         wire.ErrRepoBusy,
		CodeCommitGap:        wire.ErrCommitGap,
		CodePayloadTooLarge:  wire.ErrPayloadTooLarge,
		CodeInvalidPath:      wire.ErrInvalidPath,
		CodeNotFound:         wire.ErrNotFound,
		CodeValidationFailed: wire.ErrValidationFailed,
		CodeRateLimited:      wire.ErrRateLimited,
		CodeUnauthorized:     wire.ErrUnauthorized,
		CodeForbidden:        wire.ErrForbidden,
		CodeInternal:         wire.ErrInternal,
		CodeNotReady:         wire.ErrNotReady,
		CodeIndexDamaged:     wire.ErrIndexDamaged,
	}

	documented := specErrorCodes(t)
	for code := range documented {
		sentinel, ok := sentinels[code]
		if !ok {
			t.Errorf("error code %q is served but pkg/client has no sentinel for it", code)
			continue
		}
		// The value must actually match an error carrying that code, which is
		// the whole mechanism: a sentinel declared with the wrong string is
		// worse than a missing one.
		if !errors.Is(&wire.Error{StatusCode: 500, Code: code}, sentinel) {
			t.Errorf("errors.Is does not match %q against its sentinel", code)
		}
	}
	for code := range sentinels {
		if !documented[code] {
			t.Errorf("pkg/client has a sentinel for %q, which nothing serves", code)
		}
	}
}

// TestSpecRefsResolve: a $ref that names nothing fails at code-generation time
// in the consuming repository, which is a long way from where it was written.
func TestSpecRefsResolve(t *testing.T) {
	doc := specDoc(t)
	var missing []string
	var walk func(node any)
	walk = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			for key, val := range v {
				if key == "$ref" {
					if ref, ok := val.(string); ok && !resolveRef(doc, ref) {
						missing = append(missing, ref)
					}
					continue
				}
				walk(val)
			}
		case []any:
			for _, item := range v {
				walk(item)
			}
		}
	}
	walk(doc)

	sort.Strings(missing)
	for _, ref := range missing {
		t.Errorf("unresolvable $ref: %s", ref)
	}
}

// resolveRef follows a local JSON pointer ("#/components/schemas/Unit") through
// the parsed document. Only local refs are used and only they are understood.
func resolveRef(doc map[string]any, ref string) bool {
	if !strings.HasPrefix(ref, "#/") {
		return false
	}
	var node any = doc
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		m, ok := node.(map[string]any)
		if !ok {
			return false
		}
		if node, ok = m[part]; !ok {
			return false
		}
	}
	return node != nil
}

// TestSpecDocumentsTheRetrievalBounds pins the three additions a client cannot
// discover by trying: two request fields and the flag that admits a budget was
// applied.
func TestSpecDocumentsTheRetrievalBounds(t *testing.T) {
	components, _ := specDoc(t)["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)

	for _, req := range []string{"SearchRequest", "ContextRequest"} {
		schema, _ := schemas[req].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		for _, field := range []string{"max_bytes", "snippet"} {
			if _, ok := props[field]; !ok {
				t.Errorf("%s does not document %q", req, field)
			}
		}
	}
	for _, resp := range []string{"SearchResponse", "ContextResponse", "ServicesResponse"} {
		schema, _ := schemas[resp].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if _, ok := props["truncated"]; !ok {
			t.Errorf("%s does not document `truncated`", resp)
		}
	}

	mode, _ := schemas["SnippetMode"].(map[string]any)
	enum, _ := mode["enum"].([]any)
	got := make([]string, 0, len(enum))
	for _, v := range enum {
		got = append(got, fmt.Sprint(v))
	}
	want := []string{SnippetChunk, SnippetLine, SnippetNone}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("SnippetMode enum = %v, want %v", got, want)
	}
}

// specSchema returns one component schema.
func specSchema(t *testing.T, schema string) map[string]any {
	t.Helper()
	components, _ := specDoc(t)["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	target, ok := schemas[schema].(map[string]any)
	if !ok {
		t.Fatalf("openapi.yaml documents no %s schema", schema)
	}
	return target
}

// specProperties returns the property names one component schema documents.
func specProperties(t *testing.T, schema string) map[string]bool {
	t.Helper()
	props, _ := specSchema(t, schema)["properties"].(map[string]any)
	if len(props) == 0 {
		t.Fatalf("the %s schema documents no properties", schema)
	}
	out := make(map[string]bool, len(props))
	for name := range props {
		out[name] = true
	}
	return out
}

// TestSpecDocumentsTheOptInDiagnostics: `diagnostics` is the one field a caller
// has to know exists before it can ask for it — nothing in a default response
// hints at it — so an undocumented request field is an unreachable feature.
func TestSpecDocumentsTheOptInDiagnostics(t *testing.T) {
	if !specProperties(t, "SearchRequest")["diagnostics"] {
		t.Error("SearchRequest does not document `diagnostics`, so no client can ask for it")
	}
	if !specProperties(t, "SearchResponse")["diagnostics"] {
		t.Error("SearchResponse does not document `diagnostics`")
	}

	// The two booleans are the ones a client branches on, and a generated
	// client only treats a property as always-present if the schema says so.
	components, _ := specDoc(t)["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	diag, _ := schemas["SearchDiagnostics"].(map[string]any)
	required := map[string]bool{}
	for _, v := range diag["required"].([]any) {
		required[fmt.Sprint(v)] = true
	}
	for _, field := range []string{"degraded", "reranked"} {
		if !required[field] {
			t.Errorf("SearchDiagnostics does not require %q, so a client cannot read it as a definite boolean", field)
		}
	}
}

// TestSpecMatchesTheDiagnosticsType: the diagnostics are read out of a
// free-form metadata map, which is exactly the kind of source that grows a key
// without anyone noticing. This ties the served struct to the document field
// for field, in both directions.
func TestSpecMatchesTheDiagnosticsType(t *testing.T) {
	documented := specProperties(t, "SearchDiagnostics")
	typ := reflect.TypeOf(wire.SearchDiagnostics{})
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		if !documented[name] {
			t.Errorf("SearchDiagnostics serves %q, which the spec does not document", name)
		}
		delete(documented, name)
	}
	for name := range documented {
		t.Errorf("the SearchDiagnostics schema documents %q, which nothing serves", name)
	}
}

// TestSpecDocumentsTheReferencesBudget: /nav/references used to bound each of
// its two lookups separately and have no default at all, so `limit: 10` could
// answer with nineteen and an omitted limit returned the whole edge table. The
// numbers here mirror service.defaultReferenceLimit and maxReferenceLimit,
// which are unexported; TestReferencesDefaultsAndClamps pins the other side.
func TestSpecDocumentsTheReferencesBudget(t *testing.T) {
	components, _ := specDoc(t)["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	req, _ := schemas["ReferencesRequest"].(map[string]any)
	props, _ := req["properties"].(map[string]any)
	limit, ok := props["limit"].(map[string]any)
	if !ok {
		t.Fatal("ReferencesRequest documents no `limit`")
	}
	if limit["default"] != 50 {
		t.Errorf("ReferencesRequest.limit default = %v, want 50", limit["default"])
	}
	if limit["maximum"] != 500 {
		t.Errorf("ReferencesRequest.limit maximum = %v, want 500", limit["maximum"])
	}
}

// TestSpecSendsProseAndIdentifiersToDifferentEndpoints: /search and
// /nav/symbol measure at opposite strengths — 0.524 recall@1 for a
// natural-language question against 0.667 for an identifier the caller already
// holds — and an MCP server generates its tool descriptions from this document.
// If either endpoint stops naming the other, the agent reading those
// descriptions has nothing telling it which one to reach for.
func TestSpecSendsProseAndIdentifiersToDifferentEndpoints(t *testing.T) {
	for path, mustMention := range map[string]string{
		"/api/v1/search":     "/nav/symbol",
		"/api/v1/nav/symbol": "/search",
	} {
		desc := pathDescription(t, path, "post")
		if !strings.Contains(desc, mustMention) {
			t.Errorf("the %s description does not send the caller to %s for the queries it answers worse", path, mustMention)
		}
		if !strings.Contains(desc, "recall@1") {
			t.Errorf("the %s description drops the measurement the choice rests on", path)
		}
	}
}

// TestSpecDocumentsIntent: the retrieval endpoints reject an unknown intent
// with 400, so a client that cannot see the accepted values from the spec finds
// them out by failing.
func TestSpecDocumentsIntent(t *testing.T) {
	components, _ := specDoc(t)["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)

	for _, req := range []string{"SearchRequest", "ContextRequest"} {
		schema, _ := schemas[req].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if _, ok := props["intent"]; !ok {
			t.Errorf("%s does not document `intent`, which the handler validates", req)
		}
	}

	intent, _ := schemas["Intent"].(map[string]any)
	enum, _ := intent["enum"].([]any)
	got := make([]string, 0, len(enum))
	for _, v := range enum {
		got = append(got, fmt.Sprint(v))
	}
	want := []string{promote.IntentAuto, promote.IntentCallers, promote.IntentNone}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Intent enum = %v, want %v", got, want)
	}
}

// pathDescription returns the description of one operation, failing when the
// operation has none: an endpoint an agent picks between needs prose, and an
// empty string would otherwise pass every assertion made about it.
func pathDescription(t *testing.T, path, method string) string {
	t.Helper()
	paths, _ := specDoc(t)["paths"].(map[string]any)
	item, ok := paths[path].(map[string]any)
	if !ok {
		t.Fatalf("openapi.yaml documents no %s", path)
	}
	op, ok := item[method].(map[string]any)
	if !ok {
		t.Fatalf("openapi.yaml documents no %s %s", strings.ToUpper(method), path)
	}
	desc := fmt.Sprint(op["description"])
	if op["description"] == nil || strings.TrimSpace(desc) == "" {
		t.Fatalf("%s %s has no description", strings.ToUpper(method), path)
	}
	return desc
}

// TestSpecMatchesTheRepoType: /repos is where a client learns what a repository
// is, and the type it reads back is pkg/client.Repo. A field served and not
// documented is one a generated client drops — which is how `active` could be
// served while every scoping rule that depends on it stayed invisible.
func TestSpecMatchesTheRepoType(t *testing.T) {
	documented := specProperties(t, "Repo")
	typ := reflect.TypeOf(wire.Repo{})
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		if !documented[name] {
			t.Errorf("Repo serves %q, which the spec does not document", name)
		}
		delete(documented, name)
	}
	for name := range documented {
		t.Errorf("the Repo schema documents %q, which nothing serves", name)
	}
}

// TestSpecScopesRetrievalAndNotTheGraph: an unscoped /search answers from the
// active repositories only, and the model reading these descriptions as MCP
// tools has to be able to tell "the index does not hold this" from "the
// repository holding it is out of the way" — the first is the corpus's answer
// and the second is fixed by naming that repository. The rule lives in one
// place, RepoScope, so the two retrieval requests cannot drift apart; the graph
// endpoints each have to say they are exempt, because a tool description is
// read alone.
func TestSpecScopesRetrievalAndNotTheGraph(t *testing.T) {
	const scopeRef = "#/components/schemas/RepoScope"
	for _, req := range []string{"SearchRequest", "ContextRequest"} {
		props, _ := specSchema(t, req)["properties"].(map[string]any)
		field, _ := props["repos"].(map[string]any)
		if fmt.Sprint(field["$ref"]) != scopeRef {
			t.Errorf("%s.repos does not point at %s, so the two selectors can be documented differently", req, scopeRef)
		}
	}

	scope := fmt.Sprint(specSchema(t, "RepoScope")["description"])
	for _, must := range []string{"active", "/repos", "/graph/neighbors", "/services"} {
		if !strings.Contains(scope, must) {
			t.Errorf("the RepoScope description never mentions %q", must)
		}
	}

	// The endpoints that narrow, and the ones that never do. Each has to carry
	// the fact itself: an agent reads one tool description, not the document.
	for path, method := range map[string]string{
		"/api/v1/search":          "post",
		"/api/v1/context":         "post",
		"/api/v1/graph/neighbors": "post",
		"/api/v1/graph/path":      "post",
		"/api/v1/graph/trace":     "post",
		"/api/v1/nav/symbol":      "post",
		"/api/v1/services":        "get",
		"/api/v1/topics":          "get",
	} {
		desc := pathDescription(t, path, method)
		if !strings.Contains(desc, "active") {
			t.Errorf("the %s description says nothing about the active set", path)
		}
		if !strings.Contains(desc, "/repos") && !strings.Contains(desc, "/search") {
			t.Errorf("the %s description names neither /repos nor /search, so a caller reading it alone has no next call", path)
		}
	}
}

// TestSpecScopesTheSymbolLookup guards the half of the scoping rule that is
// easiest to document away. /nav/symbol lives under /nav/ with two endpoints
// the active set genuinely does not touch, and it reads the code graph's
// storage, so it was written down as part of the graph — while it is in fact
// /search asked by a caller holding an identifier instead of a sentence. The
// spec said so out loud, and a model reading these as MCP tool descriptions was
// told that the endpoint it should prefer for identifiers answers from
// repositories the other one will not.
func TestSpecScopesTheSymbolLookup(t *testing.T) {
	desc := pathDescription(t, "/api/v1/nav/symbol", "post")
	if strings.Contains(desc, "every indexed repository") {
		t.Error("the /nav/symbol description still claims it reads every indexed repository")
	}
	if !strings.Contains(desc, "/repos") {
		t.Error("the /nav/symbol description does not name /repos, which is what separates " +
			"'nothing carries this name' from 'the repository holding it is out of the way'")
	}
	// The graph exemption is real, and this is the list it is spelled out in.
	// "/nav/*" put all three navigation endpoints in it at once.
	if scope := fmt.Sprint(specSchema(t, "RepoScope")["description"]); strings.Contains(scope, "/nav/*") {
		t.Error("RepoScope still exempts /nav/* as a group from the active set")
	}
}

// TestSpecDropsTheDuplicateHitPath: file_path and path carried one value, and
// the spec is where a client would learn to read the wrong one.
func TestSpecDropsTheDuplicateHitPath(t *testing.T) {
	components, _ := specDoc(t)["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	hit, _ := schemas["SearchHit"].(map[string]any)
	props, _ := hit["properties"].(map[string]any)

	if _, ok := props["path"]; ok {
		t.Error("SearchHit still documents the duplicate `path` field")
	}
	if _, ok := props["file_path"]; !ok {
		t.Error("SearchHit does not document `file_path`")
	}
}
