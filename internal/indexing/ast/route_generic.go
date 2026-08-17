package ast

import (
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/storage"

	sitter "github.com/smacker/go-tree-sitter"
)

// Route registration through a project's own machinery.
//
// The framework rules in the lang_*.go files recognize a registration by the
// router it is made on (r.Get, app.post, @GetMapping). A service that owns its
// routing table registers through a name only it knows, and the router never
// appears: Consul builds a table of endpoint -> handler
// (registerEndpoint("/v1/acl/login", []string{"POST"}, (*HTTPHandlers).ACLLogin))
// and Elasticsearch returns Route records from RestHandler.routes()
// (new Route(GET, "/_cat/nodes")). Measured, that is 3 http_route units across
// Consul's 5087 files and 0 across Elasticsearch's 40001.
//
// The generic rule below reads the shape instead of the receiver: a call or a
// struct/record literal that pairs a rooted path literal with a handler
// reference or an HTTP method. It is deliberately gated on the callee (or
// constructed type) naming a routing concept — without that gate the shape
// also matches filepath.Walk("/tmp", walkFn) and client.Request("POST", "/x",
// body) — and its handled_by edge is emitted at ConfHeuristic, below the
// ConfHigh of every framework-specific rule, so a precise match always wins.

// routeNameHints are the callee/type name fragments that name a routing
// concept. Matched case-insensitively against the last component only, so the
// receiver's name is irrelevant — which is the point.
var routeNameHints = []string{"route", "endpoint", "handler", "handle", "register", "mapping"}

// routeAntiHints are fragments that name the description of an outgoing call
// rather than a registration. Grafana's generated clients build
// `resource.CustomRouteRequestOptions{Path: "/something", Verb: "GET"}`, which
// carries a routing word and the exact shape of a registration while being the
// opposite of one.
var routeAntiHints = []string{"request", "response", "options", "config", "client", "param"}

// genericRouteDetector is the name recorded on the units this rule produces.
const genericRouteDetector = "generic-route"

// genericRoute reads a route registration out of a call or literal: callee is
// the called function or the constructed type, args its argument (or element)
// expressions. It returns one entry per HTTP method named at the site, or a
// single "ANY" when the site names a handler but no method.
func genericRoute(callee string, args []string, res resolver, list listResolver) (methods []string, path, handler string, ok bool) {
	if len(args) < 2 || !routeNameHint(callee) {
		return nil, "", "", false
	}
	pathIdx := -1
	for i, a := range args {
		v, resolved := res(a)
		if !resolved || !strings.HasPrefix(v, "/") || !isURLShaped(v) {
			continue
		}
		path, pathIdx = v, i
		break
	}
	if pathIdx < 0 {
		return nil, "", "", false
	}
	for i, a := range args {
		if i == pathIdx {
			continue
		}
		if ms := httpMethodsIn(a, res, list); len(ms) > 0 {
			methods = append(methods, ms...)
			continue
		}
		if handler == "" && isFuncRefExpr(a) {
			handler = a
		}
	}
	if handler == "" && len(methods) == 0 {
		return nil, "", "", false
	}
	if len(methods) == 0 {
		methods = []string{"ANY"}
	}
	return methods, path, handler, true
}

// routeNameHint reports whether a callee or constructed type names a routing
// concept: registerEndpoint, Route, addHandler, RouteMapping.
func routeNameHint(callee string) bool {
	name := strings.ToLower(lastComponent(strings.TrimSpace(callee)))
	for _, anti := range routeAntiHints {
		if strings.Contains(name, anti) {
			return false
		}
	}
	for _, hint := range routeNameHints {
		if strings.Contains(name, hint) {
			return true
		}
	}
	return false
}

// httpMethodsIn reads the HTTP method(s) an argument names: a string literal
// ("POST"), a list of them ([]string{"GET", "PUT"}), or a constant such as
// GET, Method.Post or http.MethodGet. Returns nil for anything else.
func httpMethodsIn(expr string, res resolver, list listResolver) []string {
	if v, ok := res(expr); ok {
		if m := strings.ToUpper(strings.TrimSpace(v)); httpMethodNames[m] {
			return []string{m}
		}
		return nil // a resolved literal that is not a method is not one
	}
	var out []string
	if list != nil {
		for _, v := range list(expr) {
			if m := strings.ToUpper(strings.TrimSpace(v)); httpMethodNames[m] {
				out = append(out, m)
			}
		}
	}
	if len(out) > 0 {
		return out
	}
	if m := strings.ToUpper(strings.TrimPrefix(lastComponent(strings.TrimSpace(expr)), "Method")); httpMethodNames[m] {
		return []string{m}
	}
	return nil
}

// isFuncRefExpr reports whether an argument text references a function or
// method rather than passing a value: an identifier or member chain
// (ACLLogin, h.handleLogin), a Go method expression ((*HTTPHandlers).ACLLogin)
// or a JVM method reference (Handlers::login). Calls, literals, lambdas and
// composite literals are not references.
func isFuncRefExpr(expr string) bool {
	s := strings.TrimSpace(expr)
	// Method expression: a parenthesized receiver type followed by the method.
	if strings.HasPrefix(s, "(") {
		if i := strings.Index(s, ")."); i > 0 {
			s = s[i+2:]
		} else {
			return false
		}
	}
	if s == "" || strings.ContainsAny(s, " \t\n\"'`(){}[]") {
		return false
	}
	letters := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case isWordByte(c):
			if c < '0' || c > '9' {
				letters = true
			}
		case c == '.' || c == ':':
		default:
			return false
		}
	}
	return letters && (s[0] == '_' || s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z')
}

// applyGenericRoute publishes the route units for a registration the framework
// rules did not recognize, and reports whether it matched. prefix is the
// router prefix in effect at the site, if the language tracks one.
func applyGenericRoute(fc *fileCtx, n *sitter.Node, callee string, args []string, res resolver, list listResolver, prefix string) bool {
	methods, path, handler, ok := genericRoute(callee, args, res, list)
	if !ok {
		return false
	}
	line := int(n.StartPoint().Row) + 1
	path = joinPath(prefix, path)
	for _, m := range methods {
		if fc.hasRoute(m, path) {
			continue
		}
		fc.addRoute(n, m, path, handler, line, contract.ConfHeuristic, genericRouteDetector)
	}
	return true
}

// literalElements returns the element expressions of a struct/record literal
// node: the values of keyed elements (Path: "/x") and the plain elements of a
// positional one ({"GET", "/x", handler}).
func literalElements(fc *fileCtx, body *sitter.Node, keyedType string) []string {
	if body == nil {
		return nil
	}
	var out []string
	for i := 0; i < int(body.NamedChildCount()); i++ {
		el := body.NamedChild(i)
		if el.Type() == keyedType && el.NamedChildCount() >= 2 {
			out = append(out, fc.text(el.NamedChild(1)))
			continue
		}
		out = append(out, fc.text(el))
	}
	return out
}

// hasRoute reports whether a route with this join key was already published
// for the file, so a generic detection never duplicates a route a framework
// rule already found precisely.
func (fc *fileCtx) hasRoute(method, path string) bool {
	key := RouteKey(method, path)
	for _, u := range fc.units {
		if u.Kind == storage.KindHTTPRoute && u.Qualified == key {
			return true
		}
	}
	return false
}
