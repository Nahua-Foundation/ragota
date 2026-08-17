package ast

import (
	"path"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// Outbound calls made through a local helper.
//
// A service that talks to one API rarely calls the HTTP client directly: it
// writes one helper that builds the options object and forwards its own
// arguments to the client, and every operation calls that. n8n is the extreme
// case — most of its outbound traffic goes through per-node GenericFunctions.ts
// helpers of the shape
//
//	export async function hunterApiRequest(this: ..., method, resource, ...) {
//	  const options = { method, uri: uri || `https://api.hunter.io/v2${resource}` };
//	  return await this.helpers.request(options);
//	}
//
// whose own URL never resolves (every path segment is interpolated), while the
// call sites carry the concrete route:
//
//	await hunterApiRequest.call(this, 'GET', '/domain-search', {}, qs);
//
// Following the helper one level turns those call sites into http_call edges.
// One level only, and at ConfWeak: the helper is evidence about where the
// request goes, not proof, and a chain of helpers would multiply that guess.

// Wrapper is a local function that performs an outbound HTTP call for its
// callers. Either Path is fixed (the helper hard-codes the route) or URLParam
// names the parameter the route arrives in, in which case Path holds the
// constant prefix the helper prepends and Host the host it targets.
type Wrapper struct {
	Name string // function name as call sites spell it
	Dir  string // directory of the defining file: the package scope of the lookup

	Method string // constant HTTP method, "" when the caller supplies it
	Path   string // fixed route, or the constant prefix when URLParam >= 0
	Host   string

	MethodParam int // parameter index carrying the method, -1 when there is none
	URLParam    int // parameter index carrying the URL, -1 when the route is fixed
}

// minWrapperName is the shortest helper name that may be followed. A one- or
// two-letter name collides across a directory far too easily for an edge that
// is attributed by name alone.
const minWrapperName = 3

// wrappers builds the wrapper table for the file from the outbound HTTP call
// sites recorded during extraction.
func (fc *fileCtx) wrappers() []Wrapper {
	if len(fc.https) == 0 {
		return nil
	}
	dir := path.Dir(fc.path)
	seen := map[string]bool{}
	var out []Wrapper
	for _, site := range fc.https {
		if site.unit < 0 || site.unit >= len(fc.units) {
			continue
		}
		u := fc.units[site.unit]
		if u.Kind != "function" && u.Kind != "method" {
			continue
		}
		if len(u.Name) < minWrapperName || seen[u.Name] {
			continue
		}
		w, ok := newWrapper(fc.lang, u, site)
		if !ok || !followable(w, u.Name) {
			continue
		}
		w.Name, w.Dir = u.Name, dir
		seen[u.Name] = true
		out = append(out, w)
	}
	return out
}

// followable reports whether a helper may be attributed to its callers by name.
//
// A helper that takes the route from its caller always may: the route is then
// read off the call site's own argument, so sharing a name with something else
// costs nothing — consul's s.put(t, "/v1/kv/"+key) is exactly that.
//
// A helper whose route is fixed makes the far stronger claim that every call
// of its name in the directory goes to that one route, and an HTTP method name
// cannot carry it: get, post and put are what every mapping, header bag and
// options object in the language is read with. airflow-ctl declares
// `def get(self, asset_id)` in the same package as
// `TYPE_DEFAULTS.get(annotation)`, `os.environ.get("AIRFLOW_HOME")` and
// `kwargs.get("headers", {})`, and following the name gave 25 of those lookups
// the route of an API call. A distinctive name is what the claim rests on, and
// the helpers that carry it — getAllOwners, GetChannels, CreateOrder — are
// unaffected.
func followable(w Wrapper, name string) bool {
	return w.URLParam >= 0 || !httpMethodNames[strings.ToUpper(name)]
}

// newWrapper describes the outbound call a function makes: a fixed route, or
// the parameters the route and method arrive in.
func newWrapper(lang string, u *storage.ASTUnit, site httpSite) (Wrapper, bool) {
	w := Wrapper{MethodParam: -1, URLParam: -1}
	if site.path != "" {
		w.Method, w.Path, w.Host = site.method, site.path, site.host
		return w, true
	}
	params := contract.ParamNames(lang, u.Signature)
	w.URLParam = paramIndex(params, site.urlExpr)
	if w.URLParam < 0 {
		return w, false
	}
	w.MethodParam = paramIndex(params, site.methodExpr)
	if v, ok := unquote(strings.TrimSpace(site.methodExpr)); ok && httpMethodNames[strings.ToUpper(v)] {
		w.Method = strings.ToUpper(v)
	}
	w.Host, w.Path = urlTemplatePrefix(site.urlExpr)
	return w, true
}

// paramIndex returns the index of the parameter an expression forwards, or -1.
//
// The last matching token wins: a helper writes `uri || ${base}${resource}`,
// where the fallback parameter is named first and the one that actually varies
// per call site is inside the template.
func paramIndex(params []string, expr string) int {
	if expr == "" || len(params) == 0 {
		return -1
	}
	found := -1
	for _, tok := range identTokens(expr) {
		for i, p := range params {
			if p == tok {
				found = i
			}
		}
	}
	return found
}

// urlTemplatePrefix returns the host and the constant path prefix of a URL
// expression built around a parameter: "`https://api.hunter.io/v2${resource}`"
// yields ("api.hunter.io", "/v2"), which the call site's own path completes.
func urlTemplatePrefix(expr string) (host, prefix string) {
	for _, lit := range stringLiterals(expr) {
		lit = placeholderize(lit)
		if i := strings.IndexByte(lit, '{'); i >= 0 {
			lit = lit[:i]
		}
		if !isURLShaped(lit) {
			continue
		}
		host, prefix = splitURL(lit)
		if prefix == "/" {
			prefix = ""
		}
		return host, strings.TrimSuffix(prefix, "/")
	}
	return "", ""
}

// stringLiterals returns the contents of the string literals in an expression,
// in order.
func stringLiterals(expr string) []string {
	var out []string
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		if c != '"' && c != '\'' && c != '`' {
			continue
		}
		j := i + 1
		for j < len(expr) && expr[j] != c {
			j++
		}
		if j >= len(expr) {
			break
		}
		out = append(out, expr[i+1:j])
		i = j
	}
	return out
}

// target resolves the route a call site reaches through the wrapper from the
// argument expressions at that site.
func (w Wrapper) target(args []string) (method, path, host string, ok bool) {
	if w.URLParam < 0 {
		if w.Path == "" {
			return "", "", "", false
		}
		return orAnyMethod(w.Method), w.Path, w.Host, true
	}
	if w.URLParam >= len(args) {
		return "", "", "", false
	}
	u, _, ok := resolveURL(args[w.URLParam], literalOnly, contract.ConfWeak)
	if !ok {
		return "", "", "", false
	}
	host, path = splitURL(u)
	if host == "" {
		host = w.Host
	}
	if w.Path != "" {
		path = joinPath(w.Path, path)
	}
	method = w.Method
	if w.MethodParam >= 0 && w.MethodParam < len(args) {
		if m := httpMethodFromArg(args[w.MethodParam], literalOnly); httpMethodNames[m] {
			method = m
		}
	}
	return orAnyMethod(method), path, host, true
}

// literalOnly resolves string literals and nothing else: the wrapper pass runs
// after parsing, where the defining file's constants are no longer in view.
func literalOnly(expr string) (string, bool) { return unquote(expr) }

// orAnyMethod is the method placeholder used when the helper's callers do not
// name one, matching the "ANY" the route detectors publish.
func orAnyMethod(m string) string {
	if m == "" {
		return "ANY"
	}
	return m
}

// linkWrappers attributes the wrappers' outbound calls to the call sites in
// this file. It is the same pass the indexer runs across a directory, applied
// to the file's own helpers.
func (fc *fileCtx) linkWrappers(wrappers []Wrapper) {
	if len(wrappers) == 0 {
		return
	}
	byName := make(map[string]Wrapper, len(wrappers))
	for _, w := range wrappers {
		if _, dup := byName[w.Name]; !dup {
			byName[w.Name] = w
		}
	}
	added, edges := linkWrapperCalls(fc.edges, byName)
	fc.edges = edges
	for i := 0; i < added; i++ {
		fc.contractSite(storage.ContractKindHTTP, true)
	}
}

// linkWrapperCalls appends an http_call edge for every call edge that targets
// one of the wrappers and whose arguments resolve to a route. It returns how
// many edges were added.
//
// A call site that already carries an http_call on the same line is left
// alone, which makes the pass idempotent: the indexer re-runs it with the
// directory-wide table over files whose own helpers were already linked.
func linkWrapperCalls(edges []*storage.Edge, byName map[string]Wrapper) (int, []*storage.Edge) {
	if len(byName) == 0 {
		return 0, edges
	}
	type siteKey struct {
		src  string
		line int
	}
	resolved := map[siteKey]bool{}
	for _, e := range edges {
		if e.Kind == storage.EdgeHTTPCall {
			resolved[siteKey{e.SrcID, e.Line}] = true
		}
	}
	added := 0
	for _, e := range edges {
		if e.Kind != storage.EdgeCall {
			continue
		}
		w, ok := byName[e.DstName]
		if !ok || resolved[siteKey{e.SrcID, e.Line}] {
			continue
		}
		meta := storage.DecodeEdgeMeta(e.Meta)
		method, route, host, ok := w.target(meta.Args)
		if !ok {
			continue
		}
		resolved[siteKey{e.SrcID, e.Line}] = true
		added++
		edges = append(edges, &storage.Edge{
			SrcID:      e.SrcID,
			Kind:       storage.EdgeHTTPCall,
			DstName:    RouteKey(method, route),
			Line:       e.Line,
			Confidence: contract.ConfWeak,
			Meta: storage.EncodeEdgeMeta(&storage.EdgeMeta{
				Method: method, Path: route, Host: host,
				Args: meta.Args, Aliases: meta.Aliases,
				Source:   "wrapper:" + w.Name,
				BaseConf: contract.ConfWeak,
			}),
		})
	}
	return added, edges
}
