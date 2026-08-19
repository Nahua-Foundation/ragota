package ast

import (
	"context"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/index"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// Following a local helper one level. The shapes are n8n's: a helper that
// forwards its own method and path parameters into an options object, and one
// that hard-codes the route it calls.

func parseFactsOrFail(t *testing.T, lang, path, src string) *fileFacts {
	t.Helper()
	p, ok := GetParserForLanguage(lang).(factsParser)
	if !ok {
		t.Fatalf("%s parser reports no facts", lang)
	}
	f, err := p.ParseFacts(path, src)
	if err != nil {
		t.Fatalf("ParseFacts(%s): %v", lang, err)
	}
	return f
}

func TestWrapperFromParameterizedHelper(t *testing.T) {
	src := `export async function hunterApiRequest(
	this: IHookFunctions | IExecuteFunctions,
	method: IHttpRequestMethods,
	resource: string,
	body: any = {},
	qs: IDataObject = {},
	uri?: string,
): Promise<any> {
	const options: IRequestOptions = {
		method,
		qs,
		body,
		uri: uri || ` + "`https://api.hunter.io/v2${resource}`" + `,
		json: true,
	};
	return await this.helpers.request(options);
}

export async function search(this: IExecuteFunctions) {
	return await hunterApiRequest.call(this, 'GET', '/domain-search', {}, qs);
}
`
	facts := parseFactsOrFail(t, "typescript", "nodes/Hunter/GenericFunctions.ts", src)

	if len(facts.Wrappers) != 1 {
		t.Fatalf("wrappers = %+v, want one", facts.Wrappers)
	}
	w := facts.Wrappers[0]
	want := wrapper{
		Name: "hunterApiRequest", Dir: "nodes/Hunter",
		Path: "/v2", Host: "api.hunter.io", MethodParam: 1, URLParam: 2,
	}
	if w != want {
		t.Errorf("wrapper = %+v, want %+v", w, want)
	}

	// The call site's own arguments complete the route the helper prefixes.
	e := findEdge(facts.Edges, store.EdgeHTTPCall, "http:GET /v2/domain-search")
	if e == nil {
		t.Fatalf("missing http_call through the helper; edges: %+v", edgeNames(facts.Edges))
	}
	if e.Confidence != contract.ConfWeak {
		t.Errorf("confidence = %v, want ConfWeak", e.Confidence)
	}
	meta := store.DecodeEdgeMeta(e.Meta)
	if meta.Host != "api.hunter.io" || meta.Source != "wrapper:hunterApiRequest" {
		t.Errorf("meta = %+v", meta)
	}
}

func TestWrapperFromFixedRouteHelper(t *testing.T) {
	src := `package client

func postCharge(payload []byte) error {
	_, err := http.Post("http://billing/api/billing/charge", "application/json", payload)
	return err
}

func Charge(order *Order) error {
	return postCharge(order.Payload)
}
`
	facts := parseFactsOrFail(t, "go", "billing/client.go", src)

	if len(facts.Wrappers) != 1 || facts.Wrappers[0].Path != "/api/billing/charge" {
		t.Fatalf("wrappers = %+v", facts.Wrappers)
	}
	var through *domain.Edge
	for _, e := range facts.Edges {
		if e.Kind == store.EdgeHTTPCall && strings.Contains(e.Meta, "wrapper:") {
			through = e
		}
	}
	if through == nil {
		t.Fatalf("no http_call attributed to the call site; edges: %+v", edgeNames(facts.Edges))
	}
	if through.DstName != "http:POST /api/billing/charge" {
		t.Errorf("edge = %q", through.DstName)
	}
}

// The helper is usually a file of its own next to its callers, so the link is
// made once the package has been parsed.
func TestWrapperLinkedAcrossFilesInADirectory(t *testing.T) {
	generic := `export async function apiRequest(
	this: IExecuteFunctions,
	method: IHttpRequestMethods,
	resource: string,
) {
	return await this.helpers.httpRequest({
		method,
		url: ` + "`https://api.example.com/v1${resource}`" + `,
	});
}
`
	node := `import { apiRequest } from './GenericFunctions';

export class Example implements INodeType {
	async execute(this: IExecuteFunctions) {
		const created = await apiRequest.call(this, 'POST', '/orders');
		const listed = await apiRequest.call(this, 'GET', '/orders');
		return [created, listed];
	}
}
`
	other := `export async function unrelated(this: IExecuteFunctions) {
	return await apiRequest.call(this, 'DELETE', '/orders/1');
}
`
	files := []*index.FileToIndex{
		{Path: "nodes/Example/GenericFunctions.ts", Language: "typescript", Content: []byte(generic)},
		{Path: "nodes/Example/Example.node.ts", Language: "typescript", Content: []byte(node)},
		{Path: "nodes/Other/Other.node.ts", Language: "typescript", Content: []byte(other)},
	}

	mem := &memStorage{}
	idx := New(&Config{Storage: mem})
	RegisterDefaultParsers(idx)
	if _, err := idx.Index(context.Background(), &index.IndexRequest{RepoID: "r", Files: files}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	got := map[string]string{}
	for _, e := range mem.edges {
		if e.Kind == store.EdgeHTTPCall {
			got[e.DstName] = e.FilePath
		}
	}
	for _, want := range []string{"http:POST /v1/orders", "http:GET /v1/orders"} {
		if got[want] != "nodes/Example/Example.node.ts" {
			t.Errorf("edge %q attributed to %q, want the calling file; edges: %v", want, got[want], got)
		}
	}
	// A directory away, the helper is not in scope: the join is by name, and a
	// name is not enough evidence to cross a package.
	if file, ok := got["http:DELETE /v1/orders/1"]; ok {
		t.Errorf("helper followed into %q, want the link kept inside its directory", file)
	}
}

// Following stops after one hop: a helper that calls a helper is not resolved
// through both, and the intermediate call site keeps only its call edge.
func TestWrapperFollowingIsOneLevel(t *testing.T) {
	src := `export async function apiRequest(this: IExecuteFunctions, method: string, resource: string) {
	return await this.helpers.httpRequest({ method, url: ` + "`https://api.example.com/v1${resource}`" + ` });
}

export async function apiRequestAllItems(this: IExecuteFunctions, method: string, resource: string) {
	return await apiRequest.call(this, method, resource);
}

export async function listOrders(this: IExecuteFunctions) {
	return await apiRequestAllItems.call(this, 'GET', '/orders');
}
`
	facts := parseFactsOrFail(t, "typescript", "nodes/Example/GenericFunctions.ts", src)
	for _, w := range facts.Wrappers {
		if w.Name == "apiRequestAllItems" {
			t.Errorf("wrapper %+v: an attributed call must not make its own caller a wrapper", w)
		}
	}
	for _, e := range facts.Edges {
		if e.Kind == store.EdgeHTTPCall && strings.Contains(e.DstName, "/orders") {
			t.Errorf("unexpected second-level http_call %q", e.DstName)
		}
	}
}

func TestWrapperTarget(t *testing.T) {
	tests := []struct {
		name    string
		wrapper wrapper
		args    []string
		method  string
		path    string
		host    string
		ok      bool
	}{
		{
			name:    "fixed route",
			wrapper: wrapper{Method: "POST", Path: "/api/charge", Host: "billing", MethodParam: -1, URLParam: -1},
			args:    []string{"payload"},
			method:  "POST", path: "/api/charge", host: "billing", ok: true,
		},
		{
			name:    "route and method from the call site",
			wrapper: wrapper{Path: "/v2", Host: "api.hunter.io", MethodParam: 1, URLParam: 2},
			args:    []string{"this", `'DELETE'`, `'/leads/1'`},
			method:  "DELETE", path: "/v2/leads/1", host: "api.hunter.io", ok: true,
		},
		{
			name:    "unknown method falls back to ANY",
			wrapper: wrapper{MethodParam: -1, URLParam: 1},
			args:    []string{"this", `'/leads'`},
			method:  "ANY", path: "/leads", ok: true,
		},
		{
			name:    "a method argument that is not a method is ignored",
			wrapper: wrapper{Method: "GET", MethodParam: 1, URLParam: 2},
			args:    []string{"this", "verb", `'/leads'`},
			method:  "GET", path: "/leads", ok: true,
		},
		{
			name:    "the call site does not pass a literal route",
			wrapper: wrapper{MethodParam: 1, URLParam: 2},
			args:    []string{"this", `'GET'`, "endpoint"},
		},
		{
			name:    "too few arguments",
			wrapper: wrapper{MethodParam: 1, URLParam: 2},
			args:    []string{"this"},
		},
		{
			name:    "a fixed-route wrapper without a route",
			wrapper: wrapper{MethodParam: -1, URLParam: -1},
			args:    []string{"x"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			method, path, host, ok := tt.wrapper.target(tt.args)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if method != tt.method || path != tt.path || host != tt.host {
				t.Errorf("target = %s %s (host %q), want %s %s (host %q)",
					method, path, host, tt.method, tt.path, tt.host)
			}
		})
	}
}

func TestURLTemplatePrefix(t *testing.T) {
	tests := []struct {
		expr, host, prefix string
	}{
		{expr: "uri || `https://api.hunter.io/v2${resource}`", host: "api.hunter.io", prefix: "/v2"},
		{expr: "`https://api.example.com${endpoint}`", host: "api.example.com"},
		{expr: "'/api/v1' + path", prefix: "/api/v1"},
		{expr: "endpoint"},
	}
	for _, tt := range tests {
		host, prefix := urlTemplatePrefix(tt.expr)
		if host != tt.host || prefix != tt.prefix {
			t.Errorf("urlTemplatePrefix(%q) = %q, %q; want %q, %q", tt.expr, host, prefix, tt.host, tt.prefix)
		}
	}
}
