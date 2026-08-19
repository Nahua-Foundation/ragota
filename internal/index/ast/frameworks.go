package ast

import (
	"bytes"
	"slices"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// This file is the declarative framework-detection engine shared by the
// language extractors. The repeating detection classes — "HTTP client call
// with a URL argument", "kafka produce with a topic argument or topic field",
// "kafka consume with topic arg / topic list / topic fields" — are expressed
// as data rules; adding support for a new client framework is one rule in the
// language's detectorSet instead of a new branch in its handleCall.
//
// Language-specific detection that depends on non-local state or syntax
// (route registrations, annotations/attributes/decorators, gRPC stubs,
// Go's writer/reader kafka bindings) stays in the lang_*.go files.

// callSite is a language-neutral description of a single call expression.
// The language extractor fills it from its AST and passes it to runDetectors.
type callSite struct {
	Callee   string            // full callee expression text ("client.pipeline.get")
	Name     string            // last component of the callee ("get")
	Object   string            // receiver expression, "" if none ("client.pipeline")
	RecvType string            // receiver's declared type, "" when the extractor has none
	Args     []string          // positional argument expression texts, in order
	Kwargs   map[string]string // keyword arguments (python only)
	Fields   map[string]string // body/options object or dict fields, if the language extracted them
	Aliases  map[string]string // local aliases relevant to this call (see relevantAliases)
	Line     int               // 1-based source line of the call
	Src      int               // enclosing unit index (may be -1; addEdge drops the edge then)
}

// resolver resolves a single expression to a string value (string literals,
// tracked consts, env-var references). Each language passes its own.
type resolver func(expr string) (string, bool)

// listResolver extracts string values from a list-ish expression such as
// "['a', CONST]". Used for multi-topic subscriptions; nil falls back to
// treating the expression as a single resolvable string.
type listResolver func(expr string) []string

// objectMatch declares which receiver objects a rule applies to. The zero
// value matches any object. Matching is deliberately literal (exact / suffix /
// substring, optionally case-folded) so hand-written conditions port over
// without changing behavior.
type objectMatch struct {
	Exact    []string // object == entry
	Suffix   []string // strings.HasSuffix(object, entry)
	Contains []string // strings.Contains(object, entry)
	Fold     bool     // lowercase the object before comparing
}

// empty reports whether the match declares no criteria, i.e. matches anything.
func (m objectMatch) empty() bool {
	return len(m.Exact) == 0 && len(m.Suffix) == 0 && len(m.Contains) == 0
}

func (m objectMatch) matches(object string) bool {
	if m.empty() {
		return true
	}
	if m.Fold {
		object = strings.ToLower(object)
	}
	for _, e := range m.Exact {
		if object == e {
			return true
		}
	}
	for _, s := range m.Suffix {
		if strings.HasSuffix(object, s) {
			return true
		}
	}
	for _, c := range m.Contains {
		if strings.Contains(object, c) {
			return true
		}
	}
	return false
}

// httpClientRule detects an outgoing HTTP client call and emits an http_call
// edge keyed by routeKey(method, path), with host/path/args in the meta.
type httpClientRule struct {
	Object  objectMatch       // receiver filter; zero value = any receiver
	Methods map[string]string // call name -> HTTP method ("ANY" defers to MethodArg)
	URLArg  int               // positional argument holding the URL

	// RecvType filters on the receiver's declared type instead of its
	// expression text. A rule that sets both is satisfied by either, so a
	// client is recognized whether it was named or only typed as one.
	RecvType objectMatch

	// MethodArg is the argument holding an HttpMethod.X-style constant, used
	// when the Methods table maps the name to "ANY" (RestTemplate.exchange).
	// 0 disables it: current rules always keep the URL at position 0.
	MethodArg int

	// MethodFromField overrides the method from a body/options field holding
	// a string literal (fetch(url, {method: 'POST'})).
	MethodFromField string

	// Loose marks a rule whose call names are not distinctive enough to stand
	// on their own: it emits an edge when a URL literal is there, but a site
	// it cannot resolve is not evidence of a missed contract. "execute" and
	// "exchange" are also the names of half the JVM's APIs — counted as
	// candidates, they buried Elasticsearch's HTTP coverage under 1177
	// phantom ones, and made every enclosing method look like an HTTP helper.
	Loose bool

	// URLFromFields lets the request target come from the named fields of an
	// options object instead of a position: httpRequest({method, url,
	// baseURL}), axios({url}), request({uri}). The positional URLArg is tried
	// first, so a rule can declare both and cover either spelling; the field
	// names are the language-neutral set in httpURLFields.
	URLFromFields bool

	// RelativeURL admits a target written relative to the client's configured
	// base address ("api/orders"), which is how .NET's HttpClient and RestSharp
	// spell most of their requests. Only a rule whose URL argument can be
	// nothing else may set it: without a leading "/" or a scheme the literal
	// has nothing left that marks it as a request target, and contract.HTTP
	// roots whatever it is given, so a wrong one becomes a phantom route.
	RelativeURL bool

	Conf float32
}

// httpURLFields are the options-object fields that carry a request target,
// most specific first: an explicit url/uri/endpoint wins over the base URL it
// is relative to. Matching is case-insensitive, so baseURL and baseUrl are the
// same field.
var httpURLFields = []string{"url", "uri", "endpoint", "baseurl", "base_url"}

// fieldFold looks a field up case-insensitively. Options objects spell the
// same field baseURL, baseUrl and base_url depending on the library.
func fieldFold(fields map[string]string, name string) (string, bool) {
	if v, ok := fields[name]; ok {
		return v, true
	}
	for k, v := range fields {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}

// urlFromFields resolves the request target from an options object: the first
// of httpURLFields that yields a route, with a relative target joined onto an
// absolute base URL so the host survives ({url: '/b/x', baseURL: 'https://s/y'}).
//
// expr is the field expression the target was read from, and is returned even
// when it does not resolve: a helper whose url field is built from its own
// parameters is exactly what the wrapper builder is looking for.
func (r *httpClientRule) urlFromFields(fields map[string]string, res resolver) (u, expr string, conf float32, ok bool) {
	if !r.URLFromFields || len(fields) == 0 {
		return "", "", 0, false
	}
	for _, name := range httpURLFields {
		e, found := fieldFold(fields, name)
		if !found {
			continue
		}
		if expr == "" {
			expr = e
		}
		u, conf, ok = resolveURL(e, res, r.Conf)
		if !ok {
			continue
		}
		if !strings.Contains(u, "://") {
			if base, found := fieldFold(fields, "baseurl"); found {
				if b, _, ok := resolveURL(base, res, r.Conf); ok && strings.Contains(b, "://") {
					u = strings.TrimSuffix(b, "/") + u
				}
			}
		}
		return u, e, conf, true
	}
	return "", expr, 0, false
}

// matchReceiver reports whether the rule's receiver filters accept this call.
// Object and RecvType are alternatives: a rule declaring both matches when
// either does, and a rule declaring neither matches any receiver.
func (r *httpClientRule) matchReceiver(cs *callSite) bool {
	if r.Object.empty() && r.RecvType.empty() {
		return true
	}
	if !r.Object.empty() && r.Object.matches(cs.Object) {
		return true
	}
	return !r.RecvType.empty() && cs.RecvType != "" &&
		r.RecvType.matches(lastComponent(strings.TrimSpace(cs.RecvType)))
}

// apply emits the rule's http_call edge for a matching call site. claimed
// reports that the site is an outbound HTTP request whether or not its target
// could be resolved — the coverage counters and the wrapper builder both need
// the ones that could not; emitted reports that an edge was added, and is what
// the caller uses to skip its fallback handling.
func (r *httpClientRule) apply(fc *fileCtx, cs *callSite, res resolver) (claimed, emitted bool) {
	m, ok := r.Methods[cs.Name]
	if !ok || !r.matchReceiver(cs) {
		return false, false
	}
	// An object literal at the URL position is the options object, not the
	// URL. Left to resolveURL it templates into a route built out of every
	// literal in the object ("/POST{}/b/my-bucket/o{}https://…").
	positional := len(cs.Args) > r.URLArg && !strings.HasPrefix(strings.TrimSpace(cs.Args[r.URLArg]), "{")
	if !positional && !r.URLFromFields {
		return false, false
	}
	// A context where the target belongs is not a request that failed to
	// resolve; it is a different API wearing the same method names.
	if positional && isContextExpr(cs.Args[r.URLArg]) {
		return false, false
	}
	var (
		u        string
		urlExpr  string
		conf     float32
		resolved bool
	)
	if positional {
		urlExpr = cs.Args[r.URLArg]
		u, conf, resolved = resolveRequestURL(urlExpr, res, r.Conf, r.RelativeURL)
	}
	if !resolved {
		var fieldExpr string
		u, fieldExpr, conf, resolved = r.urlFromFields(cs.Fields, res)
		if fieldExpr != "" {
			urlExpr = fieldExpr
		}
	}
	methodExpr := ""
	// MethodArg is disabled when it collides with URLArg, which is the zero
	// value for rules whose method comes from the call name.
	if m == "ANY" && r.MethodArg != r.URLArg && len(cs.Args) > r.MethodArg {
		methodExpr = cs.Args[r.MethodArg]
		m = httpMethodFromArg(methodExpr, res)
	}
	if r.MethodFromField != "" {
		if expr, found := fieldFold(cs.Fields, r.MethodFromField); found {
			methodExpr = expr
			if v, isLit := unquote(expr); isLit {
				m = strings.ToUpper(v)
			}
		}
	}
	if !resolved {
		if r.Loose {
			return false, false
		}
		fc.httpCandidate(cs.Src, urlExpr, methodExpr)
		return true, false
	}
	host, path := splitURL(u)
	fc.addEdge(cs.Src, store.EdgeHTTPCall, routeKey(m, path), cs.Line, conf,
		&store.EdgeMeta{Method: m, Path: path, Host: host, Args: cs.Args, Fields: cs.Fields, Aliases: cs.Aliases})
	return true, true
}

// resolveURL turns a URL argument into a route path. A resolvable literal
// keeps the rule's own confidence; an interpolated expression is reduced to a
// "{}" template and drops to ConfHeuristic, since the concrete path is only
// known at runtime.
//
// A literal that is not URL-shaped is rejected outright, unless it still
// carries an interpolation to template away ("${orders.url}/api/orders", which
// only becomes a route once the placeholder is reduced): a literal with nothing
// left to resolve is definitive evidence that the argument is not a URL.
func resolveURL(expr string, res resolver, conf float32) (string, float32, bool) {
	return resolveRequestURL(expr, res, conf, false)
}

// resolveRequestURL is resolveURL with the base-relative target admitted, for
// the rules whose URL argument can be nothing else (see
// httpClientRule.RelativeURL). Every other caller goes through resolveURL,
// which is this with relative=false.
func resolveRequestURL(expr string, res resolver, conf float32, relative bool) (string, float32, bool) {
	if v, ok := res(expr); ok {
		// A value with a hole left in it is a template rather than the string
		// the program computes, and takes the confidence an interpolated
		// expression takes below, for the same reason.
		if strings.Contains(v, "{}") {
			conf = contract.ConfHeuristic
		}
		if isURLShaped(v) {
			return v, conf, true
		}
		// Rooting it is what makes the two spellings one contract: the server's
		// route is written "/api/orders", contract.HTTP roots that too, so
		// "api/orders" has to arrive at the same key.
		if relative && isRelativeRoute(v) {
			return "/" + strings.TrimSpace(v), conf, true
		}
		if !strings.Contains(v, "${") && !printfVerb.MatchString(v) {
			return "", 0, false
		}
	}
	if v, ok := interpolatedTemplate(expr, relative); ok {
		return v, contract.ConfHeuristic, true
	}
	return "", 0, false
}

// isRelativeRoute reports whether a resolved literal reads as a request path
// written relative to a client's base address: two or more segments of path
// characters, opening on a letter, optionally followed by a query string.
//
// A single segment is deliberately rejected. "api/orders" is a route and
// "items" is as likely to be a cache key, a blob name or a header value, and
// the difference is all the evidence there is once the leading "/" is gone.
func isRelativeRoute(s string) bool {
	s = strings.TrimSpace(s)
	if !startsWithLetter(s) {
		return false
	}
	if strings.Contains(s, "://") || strings.ContainsAny(s, " \t\n\"'\\") {
		return false
	}
	segs := strings.Split(strings.TrimSuffix(contract.TrimQuery(s), "/"), "/")
	if len(segs) < 2 {
		return false
	}
	for _, seg := range segs {
		if !isRouteSegment(seg) {
			return false
		}
	}
	return true
}

// startsWithLetter reports whether s opens on an ASCII letter, which every
// route's first segment does and a version number ("2.0/x") or a host:port
// pair does not.
func startsWithLetter(s string) bool {
	return s != "" && (s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z')
}

// isRouteSegment reports whether one path segment is made of the characters a
// route segment carries, including the "{}" a templated one is reduced to. "."
// and ".." are rejected: a relative file path is not a route.
func isRouteSegment(seg string) bool {
	if seg == "" || seg == "." || seg == ".." {
		return false
	}
	for i := 0; i < len(seg); i++ {
		switch c := seg[i]; {
		case contract.IsWordByte(c):
		case c == '-', c == '.', c == '~', c == '%', c == '{', c == '}', c == '+':
		default:
			return false
		}
	}
	return true
}

// isContextExpr reports whether an expression is a Go context.Context.
//
// It is the one thing that can stand where a request target belongs and prove
// the call is not an HTTP request: Kubernetes' controller-runtime client and
// the grpc-gateway stubs both spell their calls client.Get(ctx, …), and their
// receiver names ("client", anything ending in "Client") are the same ones
// http.Client is given. Over the corpus this covers every such site: ctx (17),
// t.Context() (13), context.Background() (4) and context.TODO() (2), all of
// them in argo-cd, none of them an outbound request.
func isContextExpr(expr string) bool {
	e := strings.TrimSpace(expr)
	if e == "ctx" {
		return true
	}
	// context.Background(), context.TODO()
	if rest, found := strings.CutPrefix(e, "context."); found {
		return strings.HasSuffix(rest, "()") && !strings.ContainsAny(rest, " \t.")
	}
	// t.Context(), r.Context()
	return strings.HasSuffix(e, ".Context()")
}

// isURLShaped reports whether a resolved literal can be the target of an HTTP
// request: an absolute URL ("https://host/x") or a rooted path ("/health").
//
// This is the shape gate every http_call built from a literal passes through.
// Client method names collide with collection and config APIs across all the
// supported languages, and without the gate their arguments — map keys
// ("key.deserializer"), host:port pairs ("LOCALHOST:9092"), CLI flags
// ("--BOOTSTRAP-SERVER") — were keyed as routes: measured, ~20k phantom edges
// over three Java codebases. Segment count is not part of the shape; "/health"
// is a route.
func isURLShaped(s string) bool {
	s = strings.TrimSpace(s)
	if strings.ContainsAny(s, " \t\n") {
		return false
	}
	return strings.HasPrefix(s, "/") || strings.Contains(s, "://")
}

// httpMethodFromArg reads an HTTP method from a call argument: a string
// literal ("POST"), or a constant such as HttpMethod.POST, Method.Post and
// Go's http.MethodPost.
func httpMethodFromArg(expr string, res resolver) string {
	if v, ok := res(expr); ok {
		return strings.ToUpper(strings.TrimSpace(v))
	}
	return strings.ToUpper(strings.TrimPrefix(lastComponent(strings.TrimSpace(expr)), "Method"))
}

// nonGRPCClientTypes are type names ending in "Client" that belong to HTTP and
// infrastructure SDKs, never to generated gRPC stubs. Without this list the
// type-based stub fallback would swallow ordinary HTTP client calls.
var nonGRPCClientTypes = map[string]bool{
	"Client": true, "HttpClient": true, "HTTPClient": true, "WebClient": true,
	"RestClient": true, "ApiClient": true, "APIClient": true, "GraphQLClient": true,
	"RedisClient": true, "MongoClient": true, "S3Client": true, "SqsClient": true,
	"SnsClient": true, "DynamoDbClient": true, "ElasticClient": true,
	"KafkaClient": true, "AdminClient": true, "SchemaRegistryClient": true,
}

// grpcServiceFromType maps a declared stub/client type to its proto service
// name: OrderServiceClient, OrderServiceBlockingStub and OrderServiceStub all
// yield "OrderService". Returns "" when the type is not a generated stub.
//
// This is the fallback for dependency-injected clients, where the generated
// constructor is called in a wiring file rather than at the call site.
func grpcServiceFromType(typ string) string {
	t := lastComponent(strings.TrimRight(strings.TrimSpace(typ), "?*&"))
	t = strings.TrimLeft(t, "*&")
	if nonGRPCClientTypes[t] {
		return ""
	}
	for _, suffix := range []string{"BlockingStub", "FutureStub", "Stub", "Client"} {
		if svc, found := strings.CutSuffix(t, suffix); found && svc != "" {
			return svc
		}
	}
	return ""
}

// grpcStubService is grpcServiceFromType gated on evidence that the receiver
// really is a generated gRPC stub. The type name alone is not evidence: a bare
// "XxxClient" is what every HTTP and infrastructure SDK calls its entry point,
// and taking it at face value produced 1218 rpc_call edges in Elasticsearch and
// 12 in Spring PetClinic — neither of which uses gRPC at all.
//
// fileHasGRPC is the caller's file-level signal (a gRPC import or a generated
// stub constructed in the file); the other accepted evidence is the naming
// shape generated code produces (see isGeneratedStubType).
func grpcStubService(typ string, fileHasGRPC bool) string {
	svc := grpcServiceFromType(typ)
	if svc == "" || (!fileHasGRPC && !isGeneratedStubType(typ)) {
		return ""
	}
	return svc
}

// isGeneratedStubType reports whether a declared type has a shape only
// generated gRPC code produces:
//
//   - a Stub suffix (OrderServiceGrpc.OrderServiceBlockingStub) — nothing but
//     the protoc gRPC plugins names a type that way;
//   - a qualified Client (pb.OrderServiceClient, OrderService.OrderServiceClient),
//     the form generated clients are referenced through, whereas hand-written
//     SDK clients are imported and used bare;
//   - a ServiceClient suffix, from the proto convention of naming services
//     XxxService.
func isGeneratedStubType(typ string) bool {
	t := strings.Trim(strings.TrimSpace(typ), "*&?")
	name := lastComponent(t)
	if strings.HasSuffix(name, "Stub") {
		return true
	}
	if !strings.HasSuffix(name, "Client") {
		return false
	}
	return strings.Contains(t, ".") || strings.HasSuffix(name, "ServiceClient")
}

// inlineConstructedType returns the type a receiver expression constructs when
// the client is built inline instead of being assigned first:
//
//	pb.NewCartServiceClient(fe.cartSvcConn).GetCart(...)  -> pb.CartServiceClient
//	new OrderService.OrderServiceClient(ch).CreateOrder() -> OrderService.OrderServiceClient
//	OrderServiceGrpc.newBlockingStub(ch).createOrder()    -> OrderServiceStub
//	pb2_grpc.OrderServiceStub(channel).CreateOrder(req)   -> pb2_grpc.OrderServiceStub
//	new HttpClient().GetAsync(url)                        -> HttpClient
//
// Returns "" when the receiver is not a constructor call. The bare form (no
// "new", no "New" prefix) requires a capitalized name, which is what keeps
// getClient().post(...) from being read as a constructed type.
func inlineConstructedType(object string) string {
	head, ok := callHead(object)
	if !ok {
		return ""
	}
	if rest, found := strings.CutPrefix(head, "new"); found && rest != "" && !contract.IsWordByte(rest[0]) {
		return strings.TrimSpace(rest)
	}
	name := lastComponent(head)
	qualifier := strings.TrimSuffix(head, name)
	// Java's generated factories: OrderServiceGrpc.newBlockingStub(channel).
	if javaStubFactories[name] {
		if svc, found := strings.CutSuffix(lastComponent(strings.TrimSuffix(qualifier, ".")), "Grpc"); found && svc != "" {
			return svc + "Stub"
		}
		return ""
	}
	// Go's generated constructors: pb.NewOrderServiceClient(conn).
	if rest, found := strings.CutPrefix(name, "New"); found && rest != "" && rest[0] >= 'A' && rest[0] <= 'Z' {
		return qualifier + rest
	}
	// The bare form — no "new", no "New" prefix — is how the dynamic languages
	// construct a generated stub (pb2_grpc.OrderServiceStub(channel)). It is
	// also how every ordinary function is called, so it is admitted only for
	// the two names generated clients carry.
	if name == "" || name[0] < 'A' || name[0] > 'Z' ||
		(!strings.HasSuffix(name, "Client") && !strings.HasSuffix(name, "Stub")) {
		return ""
	}
	return head
}

// callHead returns the callee text of a call expression: everything before its
// argument list. Multi-line chains are normalized, since the constructor and
// the call it feeds are routinely split across lines.
func callHead(expr string) (string, bool) {
	expr = strings.TrimSpace(expr)
	if !strings.HasSuffix(expr, ")") {
		return "", false
	}
	open := strings.IndexByte(expr, '(')
	if open <= 0 {
		return "", false
	}
	head := strings.Join(strings.Fields(expr[:open]), " ")
	if head == "" || strings.ContainsAny(head, "\"'`{}[]") {
		return "", false
	}
	return head, true
}

// inlineStubService returns the gRPC service a receiver constructs inline, or
// "" when the receiver is not a generated stub constructor. The same evidence
// gate as an injected stub applies (see grpcStubService).
func inlineStubService(object string, fileHasGRPC bool) string {
	typ := inlineConstructedType(object)
	if typ == "" {
		return ""
	}
	return grpcStubService(typ, fileHasGRPC)
}

// httpMethodNames are the request methods a route registration or a client
// call can name.
var httpMethodNames = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "DELETE": true, "PATCH": true,
	"HEAD": true, "OPTIONS": true, "TRACE": true, "CONNECT": true,
}

// grpcSourceMarkers are the lowercased import fragments that show a file
// depends on gRPC. Matching the source text rather than each language's import
// syntax keeps this language-neutral: the package name is the signal, and it is
// spelled the same in every import form.
var grpcSourceMarkers = []string{
	"io.grpc",                       // java, kotlin
	"google.golang.org/grpc",        // go
	"@grpc/", "grpc-js", "grpc-web", // typescript, javascript
	"grpc.core", "grpc.net.client", "grpc.aspnetcore", "servercallcontext", // c#
	"grpc.health.v1",           // generated health service, in every language
	"import grpc", "from grpc", // python
	"_pb2_grpc", "_grpc_pb", "grpc_pb", "_grpc.pb.go", // generated stub modules
	"grpc.newblockingstub", "grpc.newstub", "grpc.newfuturestub", // generated java factories
}

// hasGRPCImport reports whether the file text names a gRPC runtime or
// generated stub module.
func hasGRPCImport(src []byte) bool {
	lower := bytes.ToLower(src)
	for _, m := range grpcSourceMarkers {
		if bytes.Contains(lower, []byte(m)) {
			return true
		}
	}
	return false
}

// topicSpec declares where a rule finds the destination a message goes to —
// the kafka topic, the AMQP routing key, the SQS queue, the NATS subject.
//
// The sources are tried named-first (arity-selected position, keyword
// argument, options field, then plain position) and the first that yields a
// non-empty name wins, so one rule covers an API that spells its destination
// several ways: AMQP's basic_publish carries the routing key at position 1 and
// the exchange at 0, names both when it is called with keywords, and a publish
// to the default exchange fills in only one of them.
type topicSpec struct {
	Args    []int       // positional arguments, in preference order
	ByArity map[int]int // positional argument chosen by the call's argument count
	Kwargs  []string    // keyword arguments (python)
	Fields  []string    // options-object / message-struct fields
	List    bool        // the chosen source holds a list of names
	Ref     bool        // the value is a queue URL / ARN / path: keep its trailing name
}

// empty reports whether the spec declares no source at all.
func (s topicSpec) empty() bool {
	return len(s.Args) == 0 && len(s.ByArity) == 0 && len(s.Kwargs) == 0 && len(s.Fields) == 0
}

// exprs returns the destination expressions at a call site, in the order the
// spec wants them tried.
func (s topicSpec) exprs(cs *callSite) []string {
	var out []string
	if i, ok := s.ByArity[len(cs.Args)]; ok && i >= 0 && i < len(cs.Args) {
		out = append(out, cs.Args[i])
	}
	for _, k := range s.Kwargs {
		if v, ok := fieldFold(cs.Kwargs, k); ok {
			out = append(out, v)
		}
	}
	for _, f := range s.Fields {
		if v, ok := fieldFold(cs.Fields, f); ok {
			out = append(out, v)
		}
	}
	for _, i := range s.Args {
		if i >= 0 && i < len(cs.Args) {
			out = append(out, cs.Args[i])
		}
	}
	return out
}

// resolve returns the single destination name the spec points at. found
// reports that the spec located an expression to read — a call the rule owns
// even when the name could not be resolved.
func (s topicSpec) resolve(cs *callSite, res resolver) (name string, found bool) {
	for _, expr := range s.exprs(cs) {
		found = true
		v, ok := res(expr)
		if !ok || v == "" {
			continue
		}
		return brokerName(v, s.Ref), true
	}
	return "", found
}

// resolveList returns every destination name the spec's first present source
// holds. Only the first source is read: a subscription that names its topics
// in one place does not name them in another, and reading on would duplicate
// them.
func (s topicSpec) resolveList(cs *callSite, list listResolver) (names []string, found bool) {
	exprs := s.exprs(cs)
	if len(exprs) == 0 {
		return nil, false
	}
	for _, v := range list(exprs[0]) {
		if v != "" {
			names = append(names, brokerName(v, s.Ref))
		}
	}
	return names, true
}

// brokerName reduces a destination reference to the name both sides of the
// contract share. An SQS queue URL, an SNS/EventBridge ARN and a Pub/Sub
// subscription path all embed the name behind an account-specific prefix that
// only one side of the contract spells, so joining on the raw reference joins
// nothing. Applied only where the rule says the value is such a reference.
func brokerName(v string, ref bool) string {
	if !ref {
		return v
	}
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "${") {
		return v
	}
	if i := strings.LastIndexAny(v, "/:"); i >= 0 && i+1 < len(v) {
		return v[i+1:]
	}
	return v
}

// kafkaProduceRule detects a message publish and emits a produces edge keyed
// by topicKey(topic).
type kafkaProduceRule struct {
	Object   objectMatch
	Methods  []string // call names ("send", "Produce", ...)
	TopicArg int      // positional argument holding the topic (when Topic and TopicFromField are unset)

	// TopicFromField takes the topic from a body/options object field instead
	// of a positional argument (kafkajs producer.send({topic: ...})).
	TopicFromField string

	// Topic is the general form of the two shorthands above, for the brokers
	// that name their destination in more than one place.
	Topic topicSpec

	// RecvType filters on the receiver's declared type instead of its
	// expression text, as in httpClientRule: a rule that sets both is
	// satisfied by either.
	RecvType objectMatch

	// OmitArgs leaves Args out of the edge meta (kafkajs calls take a single
	// object argument, so the raw args text is redundant).
	OmitArgs bool

	// Loose marks a rule whose call names are not distinctive enough to stand
	// on their own, as it does for httpClientRule: the rule still emits when
	// the destination is there, but a site it cannot resolve is not evidence
	// of a missed contract. "send" is the name of every express response
	// (res.send(err)) and "subscribe" of every rxjs observable, and counting
	// those left Robot Shop reporting 72 messaging candidates for a system
	// that has exactly two.
	Loose bool

	// NeedsBroker demands that something in view names a broker before the
	// rule fires at all (see brokerEvidence). Where Loose says "an unresolved
	// site here is not a missed contract", this says "without a broker this is
	// not messaging in the first place" — for the rules whose call names are
	// shared with the rest of the language.
	NeedsBroker bool

	Conf float32
}

// spec folds the single-source shorthands into the general form.
func (r *kafkaProduceRule) spec() topicSpec {
	switch {
	case !r.Topic.empty():
		return r.Topic
	case r.TopicFromField != "":
		return topicSpec{Fields: []string{r.TopicFromField}}
	default:
		return topicSpec{Args: []int{r.TopicArg}}
	}
}

// matchBrokerReceiver reports whether a messaging rule's receiver filters
// accept this call. Object and RecvType are alternatives, as they are for
// httpClientRule: a rule declaring both matches when either does, and a rule
// declaring neither matches any receiver.
func matchBrokerReceiver(obj, recv objectMatch, cs *callSite) bool {
	if obj.empty() && recv.empty() {
		return true
	}
	if !obj.empty() && obj.matches(cs.Object) {
		return true
	}
	return !recv.empty() && cs.RecvType != "" &&
		recv.matches(lastComponent(strings.TrimSpace(cs.RecvType)))
}

// apply emits the rule's produces edge. See httpClientRule.apply for the
// meaning of the two results.
func (r *kafkaProduceRule) apply(fc *fileCtx, cs *callSite, res resolver) (claimed, emitted bool) {
	if !slices.Contains(r.Methods, cs.Name) || !matchBrokerReceiver(r.Object, r.RecvType, cs) {
		return false, false
	}
	if r.NeedsBroker && !brokerHandle(fc, cs) {
		return false, false
	}
	topic, found := r.spec().resolve(cs, res)
	if !found || topic == "" {
		return !r.Loose, false
	}
	key, name := topicEdgeKey(topic)
	meta := &store.EdgeMeta{Topic: name, Fields: cs.Fields, Aliases: cs.Aliases}
	if !r.OmitArgs {
		meta.Args = cs.Args
	}
	fc.addEdge(cs.Src, store.EdgeProduces, key, cs.Line, r.Conf, meta)
	return true, true
}

// topicEdgeKey turns a resolved topic expression into an edge key plus the
// topic name for the edge meta. A "${KEY:default}" placeholder is keyed by the
// bare reference so the linker's config resolution can match it, while the
// default — the value the service runs with when the key is unset — is kept in
// the meta as the fallback.
func topicEdgeKey(topic string) (key, name string) {
	if ref, def, ok := splitPlaceholderDefault(topic); ok {
		return topicKey("${" + ref + "}"), def
	}
	return topicKey(topic), topic
}

// kafkaConsumeRule detects a subscription and emits one consumes edge per
// resolved topic, keyed by topicKey(topic).
type kafkaConsumeRule struct {
	Object   objectMatch
	Methods  []string
	TopicArg int // positional argument holding the topic(s) (when Topic and TopicsFromFields are unset)

	// TopicArgList parses the topic argument as a list of strings
	// (consumer.subscribe(["a", "b"])).
	TopicArgList bool

	// TopicsFromFields takes topics from body/options object fields; the first
	// present key wins (kafkajs subscribe({topic: ...} / {topics: [...]})).
	TopicsFromFields []string

	// Topic is the general form of the two shorthands above.
	Topic topicSpec

	// RecvType filters on the receiver's declared type instead of its
	// expression text; a rule that sets both is satisfied by either.
	RecvType objectMatch

	// Loose and NeedsBroker have the same meaning as on kafkaProduceRule.
	Loose       bool
	NeedsBroker bool

	Conf float32
}

// spec folds the single-source shorthands into the general form.
func (r *kafkaConsumeRule) spec() topicSpec {
	switch {
	case !r.Topic.empty():
		return r.Topic
	case len(r.TopicsFromFields) > 0:
		return topicSpec{Fields: r.TopicsFromFields, List: true}
	default:
		return topicSpec{Args: []int{r.TopicArg}, List: r.TopicArgList}
	}
}

// apply emits one consumes edge per resolved topic. claimed and emitted mean
// what they do for httpClientRule.apply; handled is the caller's signal to
// skip its fallback handling, and differs from emitted for a subscription
// whose topic list is present but unresolvable — the rule owns that call site
// even though it could name no topic.
func (r *kafkaConsumeRule) apply(fc *fileCtx, cs *callSite, res resolver, list listResolver) (claimed, emitted, handled bool) {
	if !slices.Contains(r.Methods, cs.Name) || !matchBrokerReceiver(r.Object, r.RecvType, cs) {
		return false, false, false
	}
	if r.NeedsBroker && !brokerHandle(fc, cs) {
		return false, false, false
	}
	claimed = true
	emit := func(topic string) {
		key, name := topicEdgeKey(topic)
		fc.addEdge(cs.Src, store.EdgeConsumes, key, cs.Line, r.Conf, &store.EdgeMeta{Topic: name})
		emitted = true
	}
	spec := r.spec()
	if spec.List {
		topics, found := spec.resolveList(cs, list)
		if !found {
			return claimed && !r.Loose, false, false
		}
		for _, topic := range topics {
			emit(topic)
		}
		return claimed, emitted, true
	}
	topic, found := spec.resolve(cs, res)
	if !found || topic == "" {
		return claimed && !r.Loose, false, false
	}
	emit(topic)
	return claimed, true, true
}

// fluentHTTPVerbs are the bare verb calls that terminate a fluent HTTP client
// chain (WebClient-style webClient.post().uri(...)).
var fluentHTTPVerbs = []string{"get", "post", "put", "delete", "patch"}

// fluentChainHTTP detects WebClient/Feign-style fluent chains:
// webClient.post().uri("/api/x").bodyValue(b)... The signal is an invocation
// named "uri" with a resolvable string argument whose receiver chain (the
// object text) contains a bare HTTP verb call lower in the chain. Returns the
// HTTP method and the raw URL on a match. Shared by the JVM languages.
func fluentChainHTTP(name, object string, args []string, res resolver) (method, url string, conf float32, ok bool) {
	if name != "uri" || len(args) < 1 || object == "" {
		return "", "", 0, false
	}
	// The argument of .uri() is the request target and can be nothing else,
	// which is what licenses reading it base-relative: petclinic's gateway
	// writes .uri(hostname + "pets/visits?petId={petId}"), and without this the
	// literal's first segment was taken for the base address and dropped,
	// keying the call /visits against a route the corpus spells /pets/visits.
	u, conf, resolved := resolveRequestURL(args[0], res, contract.ConfCrossFile, true)
	if !resolved {
		return "", "", 0, false
	}
	lower := strings.ToLower(object)
	for _, v := range fluentHTTPVerbs {
		if strings.Contains(lower, "."+v+"()") {
			return strings.ToUpper(v), u, conf, true
		}
	}
	return "", "", 0, false
}

// detectorSet is a language's declarative framework rules.
type detectorSet struct {
	HTTP    []httpClientRule
	Produce []kafkaProduceRule
	Consume []kafkaConsumeRule

	// Broker enables the shape-based messaging fallback for this language
	// (see brokerFallback).
	Broker bool
}

// runDetectors runs the rule set against a call site in order (produce,
// consume, then HTTP — call-name tables are disjoint, so ordering across
// classes is cosmetic; within a class the first matching rule wins). On a
// match it emits the corresponding edge(s) and returns true; the caller then
// skips its fallback handling (SQL scan, generic call edge).
//
// A rule that recognizes the framework call but cannot resolve its key still
// counts the site towards contract coverage — that gap is the whole point of
// the counters — while leaving the caller's fallback handling to run.
func runDetectors(fc *fileCtx, ds *detectorSet, cs *callSite, res resolver, list listResolver) bool {
	if list == nil {
		list = func(expr string) []string {
			if v, ok := res(expr); ok {
				return []string{v}
			}
			return nil
		}
	}
	counted := map[string]bool{}
	record := func(kind string, claimed, emitted bool) {
		if claimed && !counted[kind] {
			counted[kind] = true
			fc.contractSite(kind, emitted)
		}
	}
	for i := range ds.Produce {
		claimed, emitted := ds.Produce[i].apply(fc, cs, res)
		record(domain.ContractKindMessaging, claimed, emitted)
		if emitted {
			return true
		}
	}
	for i := range ds.Consume {
		claimed, emitted, handled := ds.Consume[i].apply(fc, cs, res, list)
		record(domain.ContractKindMessaging, claimed, emitted)
		if handled {
			return true
		}
	}
	if ds.Broker && !counted[domain.ContractKindMessaging] {
		if claimed, emitted := brokerFallback(fc, cs, res); claimed {
			record(domain.ContractKindMessaging, true, emitted)
			if emitted {
				return true
			}
		}
	}
	for i := range ds.HTTP {
		claimed, emitted := ds.HTTP[i].apply(fc, cs, res)
		record(domain.ContractKindHTTP, claimed, emitted)
		if emitted {
			return true
		}
	}
	return false
}

// brokerMarkers are the fragments that mark a receiver as a message broker
// handle rather than an ordinary object, matched case-folded against the
// receiver expression and its declared type. They are the vendor and role
// words that appear in the names of broker clients across every SDK; nothing
// here is a word an ordinary collection or service is called.
//
// The two halves are not equally strong, which is why they are separate lists
// (see brokerEvidence): a receiver named after a vendor can be nothing else,
// while the role words are what in-process eventing calls its handles too.
var brokerMarkers = append(append([]string{}, brokerVendorMarkers...), brokerRoleMarkers...)

// brokerVendorMarkers name a broker product or SDK. A receiver carrying one of
// these is a broker handle on its own evidence.
var brokerVendorMarkers = []string{
	"kafka", "rabbit", "amqp", "sqs", "sns", "nats", "jetstream", "pubsub",
	"pub_sub", "servicebus", "service_bus", "eventhub", "event_hub",
	"eventbridge", "kinesis", "pulsar", "mqtt", "stomp", "jmstemplate",
	"streambridge", "redis",
}

// brokerRoleMarkers name the *part* an object plays, not what it is: every
// in-process event stream in the corpus calls its handle a publisher, a
// producer or a bus. Consul's `stream.Publisher.Publish([]stream.Event{...})`
// is the case that matters — a role word, a publish verb, a destination field
// on the message, and no broker within a mile of the repository.
var brokerRoleMarkers = []string{
	"eventbus", "messagebus", "messagebroker", "broker", "publisher",
	"subscriber", "producer", "consumer",
}

// brokerPublishVerbs and brokerSubscribeVerbs are the call names that move a
// message in each direction. They are matched case-folded, and only on a
// receiver brokerMarkers already accepted — on their own "send" and "publish"
// belong to far too many APIs to mean anything.
var brokerPublishVerbs = map[string]bool{
	"publish": true, "publishasync": true, "publishmessage": true, "send": true,
	"sendasync": true, "sendmessage": true, "sendmessageasync": true,
	"sendtoqueue": true, "produce": true, "produceasync": true, "emit": true,
	"enqueue": true, "convertandsend": true, "basicpublish": true,
	"basicpublishasync": true, "basic_publish": true, "publish_message": true,
	"send_message": true,
}

var brokerSubscribeVerbs = map[string]bool{
	"subscribe": true, "subscribeasync": true, "consume": true, "consumeasync": true,
	"receive": true, "receivemessage": true, "receivemessages": true,
	"basicconsume": true, "basicconsumeasync": true, "basic_consume": true,
	"receive_message": true, "receive_messages": true, "listen": true,
	"pull": true, "createprocessor": true, "createreceiver": true,
}

// brokerFallback is the shape-based half of messaging detection: a call whose
// receiver names a broker, whose method moves a message, and which carries a
// destination-shaped literal somewhere in its arguments, keyword arguments or
// options fields. It is what catches the SDK this file's rule table does not
// name yet, and it joins on the same key as every explicit rule, so a producer
// found this way still meets a consumer found by a rule.
//
// The tier is deliberately the weakest one: the evidence is a name and a
// literal, never an API this code recognizes.
func brokerFallback(fc *fileCtx, cs *callSite, res resolver) (claimed, emitted bool) {
	kind := ""
	switch name := strings.ToLower(cs.Name); {
	case brokerPublishVerbs[name]:
		kind = store.EdgeProduces
	case brokerSubscribeVerbs[name]:
		kind = store.EdgeConsumes
	default:
		return false, false
	}
	if !hasBrokerMarker(cs.Object) && !hasBrokerMarker(cs.RecvType) {
		return false, false
	}
	// A destination has to be somewhere in the call for this to be a messaging
	// site at all. Claiming one on the receiver's name alone counted Consul's
	// in-process event stream (stream.Publisher.Publish, no string anywhere) as
	// 71 missed contracts, which is exactly the "we did not find it" reading the
	// coverage report exists to make trustworthy.
	named := false
	for _, expr := range brokerFallbackExprs(cs) {
		v, ok := res(expr)
		if !ok {
			// An unresolved identifier is still a destination-shaped argument
			// when the call names a destination field; a payload literal is not.
			continue
		}
		named = true
		if !queueNameLike(v) {
			continue
		}
		key, topic := topicEdgeKey(brokerName(v, true))
		fc.addEdge(cs.Src, kind, key, cs.Line, contract.ConfWeak,
			&store.EdgeMeta{Topic: topic, Args: cs.Args, Fields: cs.Fields, Aliases: cs.Aliases})
		return true, true
	}
	if !named {
		named = brokerDestNamed(cs)
	}
	// Nothing was read out of this call, so the only thing left to claim is a
	// gap — and a gap needs a broker to be missing from. The 22 candidates the
	// literal gate left in Consul are all this shape: the same in-process event
	// stream, publishing an event whose struct happens to carry a Topic field,
	// in a repository with no broker client anywhere in it.
	return named && brokerEvidence(fc, cs), false
}

// brokerDestNamed reports whether the call names a destination field or keyword
// argument. Such a call is a messaging site whose destination we failed to
// resolve — a genuine gap — even when no literal survives.
func brokerDestNamed(cs *callSite) bool {
	for _, f := range brokerDestFields {
		if _, ok := fieldFold(cs.Fields, f); ok {
			return true
		}
		if _, ok := fieldFold(cs.Kwargs, f); ok {
			return true
		}
	}
	return false
}

// brokerFallbackExprs are the expressions a destination literal could hide in,
// most likely first: the named destination fields, then the keyword arguments,
// then the leading positional arguments (the payload sits after the
// destination in every SDK, so scanning all of them only finds message bodies).
func brokerFallbackExprs(cs *callSite) []string {
	var out []string
	for _, f := range brokerDestFields {
		if v, ok := fieldFold(cs.Fields, f); ok {
			out = append(out, v)
		}
		if v, ok := fieldFold(cs.Kwargs, f); ok {
			out = append(out, v)
		}
	}
	for i, a := range cs.Args {
		if i >= 2 {
			break
		}
		out = append(out, a)
	}
	return out
}

// brokerDestFields are the field and keyword-argument names that carry a
// destination across the SDKs, most specific first.
var brokerDestFields = []string{
	"routing_key", "routingkey", "queue", "queueurl", "queue_url", "queuename",
	"queue_name", "topic", "topicarn", "topic_arn", "topicname", "topic_name",
	"subject", "channel", "stream", "destination", "exchange", "subscription",
}

// hasBrokerMarker reports whether a receiver expression or declared type names
// a broker.
func hasBrokerMarker(s string) bool { return containsAnyFold(s, brokerMarkers) }

// hasBrokerVendor reports whether a receiver expression or declared type names
// a broker product or SDK, as opposed to a role an in-process object plays too.
func hasBrokerVendor(s string) bool { return containsAnyFold(s, brokerVendorMarkers) }

func containsAnyFold(s string, markers []string) bool {
	if s == "" {
		return false
	}
	lower := strings.ToLower(s)
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// brokerEvidence reports whether anything in view says this call site talks to
// a message broker at all: a receiver named after a broker product, or a file
// that imports a broker client library.
//
// It is the gate on every messaging rule whose call name is ambiguous —
// send, subscribe, publish, consume are the names of half the APIs in every
// language — and it is what the *candidate* count means. A site counted
// without it is not "a contract we failed to resolve", it is a call that was
// never messaging: python's `generator.send(x)`, argo-cd's
// `wsConn.ReadMessage()`, consul's in-process `publisher.Publish(events)`.
func brokerEvidence(fc *fileCtx, cs *callSite) bool {
	if hasBrokerVendor(cs.Object) || hasBrokerVendor(cs.RecvType) {
		return true
	}
	return fc.hasBroker()
}

// brokerHandle is the weaker test the rule tables use: their call names are
// API-specific (a kafka producer's send, an SQS client's receive_message), so
// a receiver named after the *role* is a second signal rather than the only
// one. brokerEvidence, which does not accept a role word alone, is what the
// shape-based fallback and the bare-name fallbacks ask for.
func brokerHandle(fc *fileCtx, cs *callSite) bool {
	return hasBrokerMarker(cs.Object) || hasBrokerMarker(cs.RecvType) || fc.hasBroker()
}

// queueNameLike reports whether a literal can be the name of a topic, queue,
// subject or channel: a single word of name characters, possibly dotted,
// dashed or slashed, and not a URL, a path or a sentence.
//
// The gate is what keeps the fallback from keying a contract on the log line,
// the content type or the message body that sits in the same argument list.
func queueNameLike(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 3 || len(s) > 96 {
		return false
	}
	if strings.ContainsAny(s, " \t\n\"'{}()<>,;=") || strings.HasPrefix(s, "/") {
		return false
	}
	if strings.Contains(s, "://") {
		return false
	}
	letters := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			letters++
		case c >= '0' && c <= '9', c == '.', c == '-', c == '_', c == ':', c == '/', c == '*', c == '$':
		default:
			return false
		}
	}
	return letters >= 3
}
