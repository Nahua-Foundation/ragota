package ast

import (
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/store"
)

// genericRoute is the rule for registries the framework rules cannot see. The
// shapes below are the ones measured in Consul (a table of endpoint ->
// handler) and Elasticsearch (Route records returned from routes()).

func TestGenericRouteShapes(t *testing.T) {
	tests := []struct {
		name   string
		lang   string
		file   string
		src    string
		routes []string // route unit names that must exist
		absent []string
	}{
		{
			name: "go registry call with method list and method expression handler",
			lang: "go",
			file: "http_register.go",
			src: `package agent

func init() {
	registerEndpoint("/v1/acl/login", []string{"POST"}, (*HTTPHandlers).ACLLogin)
	registerEndpoint("/v1/acl/policy/", []string{"GET", "PUT", "DELETE"}, (*HTTPHandlers).ACLPolicyCRUD)
}
`,
			routes: []string{
				"POST /v1/acl/login",
				// The trailing slash is normalized away, as it is for every
				// other route the extractors publish.
				"GET /v1/acl/policy", "PUT /v1/acl/policy", "DELETE /v1/acl/policy",
			},
		},
		{
			name: "go route table of struct literals",
			lang: "go",
			file: "routes.go",
			src: `package main

var routes = []Route{
	{"GET", "/orders", listOrders},
	{"POST", "/orders", createOrder},
}
`,
			routes: []string{"GET /orders", "POST /orders"},
		},
		{
			name: "go keyed route literal",
			lang: "go",
			file: "routes.go",
			src: `package main

var r = Route{Method: "PUT", Path: "/orders/{id}", Handler: updateOrder}
`,
			routes: []string{"PUT /orders/{id}"},
		},
		{
			name: "java route record returned from routes()",
			lang: "java",
			file: "RestNodesAction.java",
			src: `package org.elasticsearch.rest.action.cat;

public class RestNodesAction extends AbstractCatAction {
    @Override
    public List<Route> routes() {
        return List.of(new Route(GET, "/_cat/nodes"));
    }
}
`,
			routes: []string{"GET /_cat/nodes"},
		},
		{
			name: "a path passed to an unrelated helper is not a route",
			lang: "go",
			file: "walk.go",
			src: `package main

func main() {
	filepath.Walk("/tmp/data", walkFn)
	t.Run("/api/orders", handler)
}
`,
			absent: []string{"ANY /tmp/data", "ANY /api/orders"},
		},
		{
			name: "a client request carrying a method and a path is not a route",
			lang: "go",
			file: "client.go",
			src: `package main

func fetch() {
	c.Request("POST", "/api/orders", body)
}
`,
			absent: []string{"POST /api/orders"},
		},
		{
			name: "a request-options literal is not a registration",
			lang: "go",
			file: "client_gen.go",
			src: `package v1alpha1

func (c *CustomRouteClient) GetSomething() {
	resp, err := c.NamespacedRequest(ctx, resource.CustomRouteRequestOptions{
		Path: "/something",
		Verb: "GET",
	})
}
`,
			absent: []string{"GET /something"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			units, _ := parseOrFail(t, tt.lang, tt.file, tt.src)
			for _, want := range tt.routes {
				u := findUnit(units, store.KindHTTPRoute, want)
				if u == nil {
					t.Fatalf("missing route %q; units: %+v", want, names(units))
				}
				if !strings.Contains(u.Meta, genericRouteDetector) {
					t.Errorf("route %q meta = %q, want the generic detector recorded", want, u.Meta)
				}
			}
			for _, bad := range tt.absent {
				if findUnit(units, store.KindHTTPRoute, bad) != nil {
					t.Errorf("unexpected route %q; units: %+v", bad, names(units))
				}
			}
		})
	}
}

// A framework rule that already recognized the route must keep it: the generic
// rule is a fallback, never a second opinion.
func TestGenericRouteDoesNotDuplicateFrameworkRoute(t *testing.T) {
	src := `package main

func main() {
	r.HandleFunc("/health", healthHandler)
	registerRoute("/health", "GET", healthHandler)
}
`
	units, edges := parseOrFail(t, "go", "main.go", src)
	n := 0
	for _, u := range units {
		if u.Kind == store.KindHTTPRoute && strings.HasSuffix(u.Name, "/health") {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("route units for /health = %d, want 2 (ANY from the router, GET from the registry); units: %+v", n, names(units))
	}
	// The framework match keeps its precise handled_by confidence; the generic
	// one is emitted below it.
	var confs []float32
	for _, e := range edges {
		if e.Kind == store.EdgeHandledBy && e.DstName == "healthHandler" {
			confs = append(confs, e.Confidence)
		}
	}
	if len(confs) != 2 {
		t.Fatalf("handled_by edges = %v, want 2", confs)
	}
	if !(confs[0] > confs[1]) {
		t.Errorf("handled_by confidences = %v, want the framework rule above the generic one", confs)
	}
}

func TestGenericRouteParts(t *testing.T) {
	res := func(expr string) (string, bool) { return unquote(expr) }
	list := goStringsIn(constResolver{})

	tests := []struct {
		name    string
		callee  string
		args    []string
		methods []string
		path    string
		handler string
		ok      bool
	}{
		{
			name: "handler and method list", callee: "registerEndpoint",
			args:    []string{`"/v1/agent/self"`, `[]string{"GET"}`, `(*HTTPHandlers).AgentSelf`},
			methods: []string{"GET"}, path: "/v1/agent/self", handler: "(*HTTPHandlers).AgentSelf", ok: true,
		},
		{
			name: "method constant, no handler", callee: "Route",
			args:    []string{"GET", `"/_cat/nodes"`},
			methods: []string{"GET"}, path: "/_cat/nodes", ok: true,
		},
		{
			name: "qualified method constant", callee: "addRoute",
			args:    []string{`"/x"`, "http.MethodPut", "h"},
			methods: []string{"PUT"}, path: "/x", handler: "h", ok: true,
		},
		{
			name: "handler only defaults to ANY", callee: "mux.registerHandler",
			args:    []string{`"/debug/pprof"`, "pprof.Index"},
			methods: []string{"ANY"}, path: "/debug/pprof", handler: "pprof.Index", ok: true,
		},
		{name: "no routing name", callee: "Walk", args: []string{`"/tmp"`, "walkFn"}},
		{name: "no path", callee: "registerEndpoint", args: []string{`"orders"`, "h"}},
		{name: "path only", callee: "newRoute", args: []string{`"/x"`, "42"}},
		{name: "single argument", callee: "route", args: []string{`"/x"`}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			methods, path, handler, ok := genericRoute(tt.callee, tt.args, res, list)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if strings.Join(methods, ",") != strings.Join(tt.methods, ",") {
				t.Errorf("methods = %v, want %v", methods, tt.methods)
			}
			if path != tt.path || handler != tt.handler {
				t.Errorf("path/handler = %q/%q, want %q/%q", path, handler, tt.path, tt.handler)
			}
		})
	}
}

func TestIsFuncRefExpr(t *testing.T) {
	tests := map[string]bool{
		"ACLLogin":                 true,
		"h.handleLogin":            true,
		"(*HTTPHandlers).ACLLogin": true,
		"Handlers::login":          true,
		"pprof.Index":              true,
		`"/api"`:                   false,
		"handler()":                false,
		"func() {}":                false,
		"[]string{}":               false,
		"42":                       false,
		"":                         false,
	}
	for expr, want := range tests {
		if got := isFuncRefExpr(expr); got != want {
			t.Errorf("isFuncRefExpr(%q) = %v, want %v", expr, got, want)
		}
	}
}
