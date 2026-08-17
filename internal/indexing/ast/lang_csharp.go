package ast

import (
	"regexp"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/storage"

	sitter "github.com/smacker/go-tree-sitter"
)

// csharpExtractor extracts units and edges from C# source.
//
// Framework heuristics:
//   - ASP.NET Core attribute routing: [Route], [HttpGet]/[HttpPost]/... on controllers
//   - HTTP clients: HttpClient Get/Post/Put/DeleteAsync, PostAsJsonAsync
//   - Kafka (Confluent-style): consumer.Subscribe(topic), producer.Produce/ProduceAsync(topic, ...)
//   - gRPC clients via generated new XxxService.XxxServiceClient(channel)
//   - RestSharp requests: new RestRequest("/api/x", Method.Post)
type csharpExtractor struct {
	ns      string
	consts  constResolver
	aliases aliasTable // local aliases (var x = userId), scoped per method
	// locals holds the request targets assembled into a variable (var uri =
	// $"{base}items/{id}"), scoped per method rather than per file: half the
	// methods of a .NET service client call theirs "uri", and a file-scoped
	// value would answer every one of them with the last one's route.
	locals  aliasTable
	clients map[string]string // var name -> gRPC service name (client bindings), file scope
	types   map[string]string // field/property/parameter name -> declared type
	dbSets  map[string]string // DbContext property name -> table (EF Core DbSet<T>)

	// rpcSvc is the proto service the class being walked implements, taken
	// from its generated base class; eventTopic is the message a handler class
	// declares it handles. Both are set on entering a class body and restored
	// on leaving it.
	rpcSvc     string
	eventTopic string
}

var csharpHTTPAttrs = map[string]string{
	"HttpGet": "GET", "HttpPost": "POST", "HttpPut": "PUT",
	"HttpDelete": "DELETE", "HttpPatch": "PATCH", "HttpHead": "HEAD",
}

// csharpDetectors are the declarative framework rules for C# (see frameworks.go).
var csharpDetectors = detectorSet{
	Consume: append([]kafkaConsumeRule{
		// Confluent.Kafka: consumer.Subscribe("topic"). Loose because "Subscribe"
		// is also what in-process eventing is called — eShop's app host writes
		// eventing.Subscribe<BeforeStartEvent>(handler), which names no topic
		// because there is no broker in it.
		{Methods: []string{"Subscribe"}, TopicArg: 0, Loose: true, Conf: contract.ConfHigh},
	}, csBrokerConsume...),
	Produce: append([]kafkaProduceRule{
		// Confluent.Kafka: producer.Produce/ProduceAsync("topic", message)
		{Methods: []string{"Produce", "ProduceAsync"}, TopicArg: 0, Conf: contract.ConfHigh},
	}, csBrokerProduce...),
	Broker: true,
	HTTP: []httpClientRule{
		// HttpClient: httpClient.PostAsync(url, content), client.GetFromJsonAsync(url)
		{
			Object: objectMatch{Contains: []string{"client", "http"}, Fold: true},
			Methods: map[string]string{
				"GetAsync": "GET", "GetStringAsync": "GET", "GetFromJsonAsync": "GET",
				"GetByteArrayAsync": "GET", "GetStreamAsync": "GET",
				"PostAsync": "POST", "PostAsJsonAsync": "POST",
				"PutAsync": "PUT", "PutAsJsonAsync": "PUT",
				"DeleteAsync": "DELETE", "DeleteFromJsonAsync": "DELETE",
				"PatchAsync": "PATCH", "PatchAsJsonAsync": "PATCH",
			},
			URLArg: 0,
			// An HttpClient is configured with a BaseAddress and its requests
			// are then written against it, which is how eShop spells all but a
			// handful of them: "api/orders", not "/api/orders".
			RelativeURL: true,
			Conf:        contract.ConfCrossFile,
		},
	},
}

func (c *csharpExtractor) extract(fc *fileCtx, root *sitter.Node) {
	c.consts = constResolver{}
	c.aliases = aliasTable{}
	c.locals = aliasTable{}
	c.clients = map[string]string{}
	c.types = map[string]string{}
	c.dbSets = map[string]string{}
	c.rpcSvc, c.eventTopic = "", ""
	c.collectConsts(fc, root)
	c.walk(fc, root, "", "")
	c.collectEFTables(fc, root, "")
	c.walkCalls(fc, root)
}

func (c *csharpExtractor) collectConsts(fc *fileCtx, n *sitter.Node) {
	if n == nil {
		return
	}
	if n.Type() == "variable_declarator" {
		var name, value string
		var valueNode *sitter.Node
		for i := 0; i < int(n.NamedChildCount()); i++ {
			child := n.NamedChild(i)
			if child.Type() == "identifier" && name == "" {
				name = fc.text(child)
			}
		}
		if eq := firstChildOfType(n, "="); eq != nil {
			// value follows '='; take the last named child text
			if n.NamedChildCount() > 0 {
				valueNode = n.NamedChild(int(n.NamedChildCount()) - 1)
				value = fc.text(valueNode)
			}
		}
		if v, ok := unquote(value); ok && name != "" {
			c.consts[name] = v
		} else if name != "" && valueNode != nil && valueNode.Type() == "interpolated_string_expression" {
			// The route a request is built into, kept as the expression it was
			// written as: it is expanded per call site, where the constants it
			// names are known (see resolveAt).
			c.locals.record(n, name, strings.TrimSpace(value))
		} else if name != "" && isAliasExpr(value) {
			// Local alias: var x = userId; / var x = body.UserId;
			// The value is extracted textually, so a textual heuristic decides:
			// identifier characters and dots only.
			c.aliases.record(n, name, strings.TrimSpace(value))
		} else if name != "" && valueNode != nil && valueNode.Type() == "invocation_expression" {
			// Call-result alias: var x = ExtractUserId(req); the full call text
			// becomes the alias so token matching links x to the call's arguments.
			c.aliases.record(n, name, strings.TrimSpace(value))
		} else if name != "" && valueNode != nil && valueNode.Type() == "object_creation_expression" {
			// gRPC client binding: var client = new OrderService.OrderServiceClient(ch).
			// Generated clients are nested classes named <Service>Client inside
			// the service class; requiring last == parent+"Client" keeps plain
			// HttpClient/RestClient constructions out.
			if tn := valueNode.NamedChild(0); tn != nil {
				typ := fc.text(tn)
				parts := strings.Split(typ, ".")
				last := parts[len(parts)-1]
				if len(parts) >= 2 && last == parts[len(parts)-2]+"Client" {
					c.clients[name] = strings.TrimSuffix(last, "Client")
				}
				// The constructed type is this local's declared type, which is
				// how a published message names its topic: the event is built
				// into a variable one statement before it is handed to the bus.
				if _, known := c.types[name]; !known {
					c.types[name] = typ
				}
			}
		}
	}
	// Declared types of fields, properties and parameters: an injected gRPC
	// client and an EF Core DbSet are only recognizable by their type.
	switch n.Type() {
	case "field_declaration", "property_declaration", "parameter":
		c.recordDeclaredType(fc, n)
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		c.collectConsts(fc, n.NamedChild(i))
	}
}

// resolve is the C# expression resolver: string literals and tracked constants
// first, then an interpolated string rendered as a route template.
func (c *csharpExtractor) resolve(expr string) (string, bool) {
	if v, ok := c.consts.resolve(expr); ok {
		return v, true
	}
	return c.interpolatedRoute(expr)
}

// resolveAt is resolve extended with the locals a request target was assembled
// into, as visible at byte offset pos. Two methods of the same class routinely
// build different routes into the same variable name, so the offset is what
// decides which one this call site means.
func (c *csharpExtractor) resolveAt(pos int) resolver {
	locals := c.locals.at(pos)
	if len(locals) == 0 {
		return c.resolve
	}
	return func(expr string) (string, bool) {
		if v, ok := c.resolve(expr); ok {
			return v, true
		}
		if v, ok := locals[strings.TrimSpace(expr)]; ok {
			return c.interpolatedRoute(v)
		}
		return "", false
	}
}

// interpolatedRoute renders an interpolated string that reads as a request
// path, with the constants it names substituted and every other hole reduced to
// the "{}" the linker's path matcher treats as a path parameter:
//
//	private readonly string remoteServiceBaseUrl = "api/catalog/";
//	var uri = $"{remoteServiceBaseUrl}items/{id}";   ->  api/catalog/items/{}
//
// This is the shape a .NET service builds its requests in — the target is
// assembled into a local one statement before the client is called, so the call
// site itself carries no route at all.
//
// Only a route-shaped result is returned, because what this produces is a
// template rather than the string the program computes: as a route it is
// exactly what the path matcher wants, and as a topic or a table name it would
// be a name nothing has. That gate is what lets this be the extractor's one
// resolver instead of a second one wired to the HTTP rules alone.
func (c *csharpExtractor) interpolatedRoute(expr string) (string, bool) {
	body, ok := csharpInterpolatedBody(expr)
	if !ok {
		return "", false
	}
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '{':
			if i+1 < len(body) && body[i+1] == '{' { // "{{" is a literal brace
				b.WriteByte('{')
				i++
				continue
			}
			shut := strings.IndexByte(body[i:], '}')
			if shut < 0 {
				return "", false
			}
			hole, _, _ := strings.Cut(body[i+1:i+shut], ":") // drop a format clause
			i += shut
			// A constant substitutes itself; anything else is a runtime value,
			// and a constant that is itself a template cannot be nested into
			// one without losing which hole is which.
			if v, ok := c.consts.resolve(hole); ok && !strings.ContainsAny(v, "{} \t") {
				b.WriteString(v)
				continue
			}
			b.WriteString("{}")
		case '}':
			if i+1 < len(body) && body[i+1] == '}' {
				i++
			}
			b.WriteByte('}')
		default:
			b.WriteByte(body[i])
		}
	}
	s := b.String()
	if !isRelativeRoute(s) && !isURLShaped(s) || !hasLiteralSegment(s) {
		return "", false
	}
	return s, true
}

// csharpInterpolatedBody returns the text between the quotes of an interpolated
// string expression ($"...", $@"...", @$"..."), or ok=false when the expression
// is not one.
func csharpInterpolatedBody(expr string) (string, bool) {
	s := strings.TrimSpace(expr)
	for _, prefix := range []string{"$@\"", "@$\"", "$\"\"\"", "$\""} {
		if rest, found := strings.CutPrefix(s, prefix); found {
			body, _, ok := strings.Cut(rest, "\"")
			return body, ok
		}
	}
	return "", false
}

// reDbSet extracts the entity type of an EF Core DbSet<T> declaration.
var reDbSet = regexp.MustCompile(`\bDbSet\s*<\s*([A-Za-z_][A-Za-z0-9_.]*)`)

// reEFCoreSet matches a trailing Set<T>() accessor, i.e. a receiver that *is*
// the set rather than one that merely contains the accessor somewhere in a
// longer chain.
var reEFCoreSet = regexp.MustCompile(`\bSet\s*<\s*([A-Za-z_][A-Za-z0-9_.]*)\s*>\s*\(\s*\)\s*$`)

// recordDeclaredType maps a field/property/parameter name to its declared type
// and, for EF Core DbSet<T> properties, to the table T maps to.
func (c *csharpExtractor) recordDeclaredType(fc *fileCtx, n *sitter.Node) {
	typ := fc.text(n.ChildByFieldName("type"))
	var names []string
	if name := fc.text(n.ChildByFieldName("name")); name != "" {
		names = append(names, name)
	}
	// A field wraps its type and names in a variable_declaration.
	for i := 0; i < int(n.NamedChildCount()); i++ {
		d := n.NamedChild(i)
		if d.Type() != "variable_declaration" {
			continue
		}
		if typ == "" {
			typ = fc.text(d.ChildByFieldName("type"))
		}
		for k := 0; k < int(d.NamedChildCount()); k++ {
			if v := d.NamedChild(k); v.Type() == "variable_declarator" {
				names = append(names, fc.text(v.NamedChild(0)))
			}
		}
	}
	if typ == "" || len(names) == 0 {
		return
	}
	m := reDbSet.FindStringSubmatch(typ)
	for _, name := range names {
		if name == "" {
			continue
		}
		c.types[name] = typ
		if m != nil {
			if tbl := tableName(m[1]); tbl != "" {
				c.dbSets[name] = tbl
			}
		}
	}
}

// walk collects namespaces, classes, methods and route attributes.
func (c *csharpExtractor) walk(fc *fileCtx, n *sitter.Node, scope, basePath string) {
	if n == nil {
		return
	}
	switch n.Type() {
	case "namespace_declaration", "file_scoped_namespace_declaration":
		if name := fc.text(n.ChildByFieldName("name")); name != "" {
			c.ns = name
		}
	case "class_declaration", "interface_declaration", "record_declaration", "struct_declaration":
		name := fc.text(n.ChildByFieldName("name"))
		if name != "" {
			kind := "class"
			if n.Type() == "interface_declaration" {
				kind = "interface"
			}
			qualified := c.qualify(scope, name)
			doc := extractLineComments(string(fc.src), n, "//")
			fc.addUnit(n, kind, name, qualified, "", doc)

			classBase := c.attrRoutePath(fc, n, name)
			// The class's base list says what contracts it implements: a
			// generated gRPC server base names the proto service, a generic
			// handler interface names the message. Both apply to the methods
			// inside, so they are set for the body walk and restored after.
			svc, topic := c.rpcSvc, c.eventTopic
			c.rpcSvc, c.eventTopic = c.baseContracts(fc, n)
			if body := n.ChildByFieldName("body"); body != nil {
				for i := 0; i < int(body.NamedChildCount()); i++ {
					c.walk(fc, body.NamedChild(i), qualified, joinPath(basePath, classBase))
				}
			}
			c.rpcSvc, c.eventTopic = svc, topic
			return
		}
	case "method_declaration", "constructor_declaration", "local_function_statement":
		c.handleMethod(fc, n, scope, basePath)
		return
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		c.walk(fc, n.NamedChild(i), scope, basePath)
	}
}

// baseContracts reads a class's base list: the proto service its generated
// gRPC base class implements, and the message its handler interface handles.
//
// The generated C# server base is a nested class named after its own outer
// class — `service Basket` yields `Basket.BasketBase` — a shape nothing but
// protoc's C# plugin produces, which is why it needs no other corroboration.
// The bare `HealthBase` spelling a `using static` leaves behind is admitted
// only when the file otherwise names gRPC, since "…Base" on its own is what
// half of ASP.NET's own types are called.
func (c *csharpExtractor) baseContracts(fc *fileCtx, decl *sitter.Node) (svc, topic string) {
	bases := firstChildOfType(decl, "base_list")
	if bases == nil {
		return "", ""
	}
	for i := 0; i < int(bases.NamedChildCount()); i++ {
		base := strings.TrimSpace(fc.text(bases.NamedChild(i)))
		if svc == "" {
			svc = csharpGRPCServiceFromBase(base, fc.hasGRPC())
		}
		if topic == "" && eventHandlerInterfaces[lastComponent(trimGenericArgs(base))] {
			if args := genericArgs(base); len(args) == 1 {
				topic = eventTopicName(args[0])
			}
		}
	}
	return svc, topic
}

// csharpGRPCServiceFromBase returns the proto service a generated server base
// class implements, or "" when the base is not one.
func csharpGRPCServiceFromBase(base string, fileHasGRPC bool) string {
	name := lastComponent(trimGenericArgs(base))
	stem, ok := strings.CutSuffix(name, "Base")
	if !ok || stem == "" {
		return ""
	}
	// X.XBase: the nested form protoc generates, evidence in itself.
	if qualifier := strings.TrimSuffix(base, "."+name); qualifier != base &&
		lastComponent(qualifier) == stem {
		return stem
	}
	if fileHasGRPC {
		return stem
	}
	return ""
}

// csharpIsRPCImpl reports whether a method implements one of its generated
// base's rpcs. A generated server base is implemented by overriding its
// methods, and every override carries a ServerCallContext; both are required,
// because a class can override plenty that is not an rpc.
func csharpIsRPCImpl(fc *fileCtx, n *sitter.Node, sig string) bool {
	if !strings.Contains(sig, "ServerCallContext") {
		return false
	}
	for i := 0; i < int(n.ChildCount()); i++ {
		if ch := n.Child(i); ch != nil && ch.Type() == "modifier" && fc.text(ch) == "override" {
			return true
		}
	}
	return false
}

// csharpEventHandlerMethods are the method names the handler interfaces
// declare; the message they receive is the class's own type argument.
var csharpEventHandlerMethods = map[string]bool{
	"Handle": true, "HandleAsync": true, "Consume": true, "ConsumeAsync": true,
}

func (c *csharpExtractor) handleMethod(fc *fileCtx, n *sitter.Node, scope, basePath string) {
	name := fc.text(n.ChildByFieldName("name"))
	if name == "" {
		return
	}
	sig := fc.text(n.ChildByFieldName("parameters"))
	doc := extractLineComments(string(fc.src), n, "//")
	idx := fc.addUnit(n, "method", name, c.qualify(scope, name), sig, doc)
	line := fc.units[idx].StartLine

	if c.rpcSvc != "" && csharpIsRPCImpl(fc, n, sig) {
		fc.addEdge(idx, storage.EdgeImplementsRPC, GrpcKey(c.rpcSvc, name), line, contract.ConfCrossFile, nil)
	}
	if c.eventTopic != "" && csharpEventHandlerMethods[name] {
		key, topic := topicEdgeKey(c.eventTopic)
		fc.addEdge(idx, storage.EdgeConsumes, key, line, contract.ConfCrossFile, &storage.EdgeMeta{Topic: topic})
		fc.contractSite(storage.ContractKindMessaging, true)
	}

	for _, attr := range csharpAttributes(fc, n) {
		if method, ok := csharpHTTPAttrs[attr.name]; ok {
			sub := c.attrValue(fc, attr.node)
			path := joinPath(basePath, sub)
			ridx := fc.addUnit(n, storage.KindHTTPRoute, method+" "+path, RouteKey(method, path), "path:"+path, "")
			fc.addEdge(ridx, storage.EdgeHandledBy, name, line, contract.ConfHigh, nil)
		}
	}
}

func (c *csharpExtractor) walkCalls(fc *fileCtx, n *sitter.Node) {
	if n == nil {
		return
	}
	if n.Type() == "invocation_expression" {
		c.handleInvocation(fc, n)
	}
	if n.Type() == "object_creation_expression" {
		c.handleObjectCreation(fc, n)
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		c.walkCalls(fc, n.NamedChild(i))
	}
}

// csharpCallee splits a call's function expression into the receiver text and
// the method name.
//
// Taken from the syntax rather than from the callee text, because C# writes a
// call's type arguments into it: cutting "httpClient.GetFromJsonAsync<Models.
// Orders.Order>" at the last dot names the type argument, not the method, and
// System.Net.Http.Json's helpers — the ones an HttpClient's JSON requests
// actually go through — are all generic.
func csharpCallee(fc *fileCtx, fn *sitter.Node) (object, name string) {
	if fn.Type() == "member_access_expression" {
		return strings.TrimSpace(fc.text(fn.ChildByFieldName("expression"))),
			csharpMemberName(fc, fn.ChildByFieldName("name"))
	}
	if fn.Type() == "generic_name" {
		return "", csharpMemberName(fc, fn)
	}
	text := fc.text(fn)
	if i := strings.LastIndex(text, "."); i > 0 {
		object = text[:i]
	}
	return object, lastComponent(text)
}

// csharpMemberName returns the plain name of a member reference, dropping the
// type arguments of a generic one.
func csharpMemberName(fc *fileCtx, n *sitter.Node) string {
	if n == nil {
		return ""
	}
	if n.Type() == "generic_name" {
		return strings.TrimSpace(fc.text(n.NamedChild(0)))
	}
	return lastComponent(fc.text(n))
}

// csharpTypeArgs returns the type arguments of a generic call, in source
// order, or nil when the callee is not generic. It is the other half of what
// csharpMemberName drops: `AddSubscription<TEvent, THandler>()` is the name
// and the two types, and for a generic registration the types are the whole
// contract (see genericTopicContract).
func csharpTypeArgs(fc *fileCtx, fn *sitter.Node) []string {
	if fn == nil {
		return nil
	}
	name := fn
	if fn.Type() == "member_access_expression" {
		name = fn.ChildByFieldName("name")
	}
	if name == nil || name.Type() != "generic_name" {
		return nil
	}
	list := firstChildOfType(name, "type_argument_list")
	if list == nil {
		return nil
	}
	var out []string
	for i := 0; i < int(list.NamedChildCount()); i++ {
		if arg := strings.TrimSpace(fc.text(list.NamedChild(i))); arg != "" {
			out = append(out, arg)
		}
	}
	return out
}

// csharpRequestCtors are the request objects whose construction carries the
// target: RestSharp's new RestRequest("/api/x", Method.Post) and
// System.Net.Http's new HttpRequestMessage(HttpMethod.Post, url), which put
// the URL and the method at opposite positions.
var csharpRequestCtors = map[string]struct {
	urlArg    int
	methodArg int
}{
	"RestRequest":        {urlArg: 0, methodArg: 1},
	"HttpRequestMessage": {urlArg: 1, methodArg: 0},
}

// handleObjectCreation detects a request built as an object, and routes
// declared as records. A request whose method argument is absent or is not a
// method constant defaults to GET, which is what both libraries do.
func (c *csharpExtractor) handleObjectCreation(fc *fileCtx, n *sitter.Node) {
	tn := n.NamedChild(0)
	if tn == nil {
		return
	}
	typ := fc.text(tn)
	args := namedChildTexts(fc, firstChildOfType(n, "argument_list"))
	ctor, isRequest := csharpRequestCtors[lastComponent(typ)]
	if !isRequest {
		applyGenericRoute(fc, n, typ, args, c.resolve, nil, "")
		return
	}
	if len(args) <= ctor.urlArg {
		return
	}
	src := fc.enclosingUnit(int(n.StartByte()), callableKinds...)
	if src < 0 {
		return
	}
	// Both request objects carry a target that is resolved against the client
	// that sends them, so a relative resource is the normal spelling here too.
	u, conf, ok := resolveRequestURL(args[ctor.urlArg], c.resolveAt(int(n.StartByte())), contract.ConfCrossFile, true)
	fc.contractSite(storage.ContractKindHTTP, ok)
	if !ok {
		fc.httpCandidate(src, args[ctor.urlArg], "")
		return
	}
	method := "GET"
	if len(args) > ctor.methodArg {
		if ms := httpMethodsIn(args[ctor.methodArg], c.consts.resolve, nil); len(ms) == 1 {
			method = ms[0]
		}
	}
	line := int(n.StartPoint().Row) + 1
	host, path := splitURL(u)
	fc.addEdge(src, storage.EdgeHTTPCall, RouteKey(method, path), line, conf,
		&storage.EdgeMeta{Method: method, Path: path, Host: host, Args: args,
			Aliases: c.aliases.relevant(int(n.StartByte()), args, nil)})
}

func (c *csharpExtractor) handleInvocation(fc *fileCtx, n *sitter.Node) {
	fn := n.ChildByFieldName("function")
	if fn == nil {
		return
	}
	fnText := fc.text(fn)
	object, name := csharpCallee(fc, fn)
	args, named := csharpCallArgs(fc, n.ChildByFieldName("arguments"))
	line := int(n.StartPoint().Row) + 1
	pos := int(n.StartByte())
	src := fc.enclosingUnit(pos, callableKinds...)
	if src < 0 {
		return
	}

	// Declarative framework detection: Confluent Kafka, HttpClient.
	aliases := c.aliases.relevant(pos, args, nil)

	// Route registered through the service's own machinery, when the routing
	// attributes are absent.
	if applyGenericRoute(fc, n, fnText, args, c.resolve, nil, "") {
		return
	}

	// gRPC client call on a tracked client: client.CreateOrder(req) /
	// client.CreateOrderAsync(req), or on a client constructed in the call
	// itself (new Greeter.GreeterClient(channel).SayHello(req)). The Async
	// suffix is transport sugar; the proto method name is the trimmed
	// PascalCase name.
	svc, tracked := c.clients[object]
	if !tracked {
		svc = inlineStubService(object, fc.hasGRPC())
	}
	if svc != "" && name != "" {
		method := strings.TrimSuffix(name, "Async")
		if method != "" {
			fc.addEdge(src, storage.EdgeRPCCall, GrpcKey(svc, capitalizeFirst(method)), line, contract.ConfHigh,
				&storage.EdgeMeta{Args: args, Aliases: aliases})
			fc.contractSite(storage.ContractKindRPC, true)
			return
		}
	}

	// Event-typed publish: the bus API takes no destination, so the topic is
	// the message's own type (see eventPublishVerbs).
	if eventPublishVerbs[name] && len(args) > 0 &&
		eventPublishReceiverOK(name, object, c.types[lastComponent(object)]) {
		if topic := eventTopicName(c.argType(args[0])); topic != "" {
			key, t := topicEdgeKey(topic)
			fc.addEdge(src, storage.EdgeProduces, key, line, contract.ConfCrossFile,
				&storage.EdgeMeta{Topic: t, Args: args, Aliases: aliases})
			fc.contractSite(storage.ContractKindMessaging, true)
			return
		}
	}

	cs := &callSite{Callee: fnText, Name: name, Object: object, RecvType: c.types[lastComponent(object)],
		Args: args, Kwargs: named, Line: line, Src: src, Aliases: aliases}

	// Contract named in a type parameter rather than in an argument:
	// eventBus.AddSubscription<TEvent, THandler>(), bus.Publish<TEvent>(). The
	// broker test is what keeps the same shape from claiming the in-process
	// eventing every .NET application also has — eShop's own app host writes
	// eventing.Subscribe<BeforeStartEvent>(handler) two projects away.
	if kind, topic, handler, ok := genericTopicContract(name, csharpTypeArgs(fc, fn)); ok && brokerHandle(fc, cs) {
		key, t := topicEdgeKey(topic)
		meta := &storage.EdgeMeta{Topic: t, Args: args, Aliases: aliases, Receiver: object}
		if handler != "" {
			// The handler is the consuming class, and this call is the only
			// place that says so: the registration names it, the handler names
			// the event, and neither names the other's file.
			meta.Fields = map[string]string{"handler": handler}
		}
		fc.addEdge(src, kind, key, line, contract.ConfCrossFile, meta)
		fc.contractSite(storage.ContractKindMessaging, true)
		return
	}

	if runDetectors(fc, &csharpDetectors, cs, c.resolveAt(pos), nil) {
		return
	}

	// Injected gRPC client: `private readonly Orders.OrdersClient _client;`
	if name != "" {
		if svc := grpcStubService(c.types[lastComponent(object)], fc.hasGRPC() || len(c.clients) > 0); svc != "" {
			method := strings.TrimSuffix(name, "Async")
			if method != "" {
				fc.addEdge(src, storage.EdgeRPCCall, GrpcKey(svc, capitalizeFirst(method)), line, contract.ConfCrossFile,
					&storage.EdgeMeta{Args: args, Aliases: aliases})
				fc.contractSite(storage.ContractKindRPC, true)
				return
			}
		}
	}

	// EF Core: _context.Orders.Add(order) / services.Context.CatalogItems.Where(...).
	if tbl, conf := c.efCoreTable(object, name, fnText); tbl != "" {
		fc.contractSite(storage.ContractKindDB, true)
		kind := storage.EdgeReadsFrom
		if efCoreWriteMethods[name] {
			kind = storage.EdgeWritesTo
		}
		fc.addEdge(src, kind, contract.DB(tbl), line, conf,
			&storage.EdgeMeta{Args: args, Aliases: aliases})
	}

	sqlEdgesFromArgs(fc, src, line, args)
	fc.addEdge(src, storage.EdgeCall, name, line, contract.ConfHeuristic,
		&storage.EdgeMeta{Args: args, Aliases: aliases, Receiver: object, RecvType: c.types[lastComponent(object)]})
}

// efCoreTable resolves the table an EF Core call touches.
//
// The DbSet<T> declarations live in the DbContext and the queries live in the
// repositories and the endpoint files, so the file-scoped dbSets table — the
// only lookup this had — resolved nothing outside the context itself: eShop's
// four contexts declare nine DbSets and not one query sits beside them. The
// shape the queries do share is a receiver `<context>.<Property>` where the
// context is known to be one, either by its declared type or by being named
// `Context`, plus an EF method. That is what carries the cross-file case.
func (c *csharpExtractor) efCoreTable(object, method, callee string) (string, float32) {
	prop := lastComponent(object)
	if tbl := c.dbSets[prop]; tbl != "" {
		return tbl, contract.ConfCrossFile
	}
	// _context.Set<Order>().Add(x): the entity is the type argument, and the
	// entity has no DbSet property at all. Matched on the receiver rather than
	// on the accessor call itself, so a chain yields the one edge its terminal
	// operation stands for instead of one per link.
	if m := reEFCoreSet.FindStringSubmatch(object); m != nil {
		if tbl := tableName(m[1]); tbl != "" {
			return tbl, contract.ConfCrossFile
		}
	}
	if !efCoreWriteMethods[method] && !efCoreReadMethods[method] {
		return "", 0
	}
	// Cutting at the last dot rather than trimming the property text off:
	// a chained query splits the receiver across lines, so the object ends in
	// the indentation that follows the property name.
	dot := strings.LastIndex(object, ".")
	if dot <= 0 {
		return "", 0
	}
	owner := lastComponent(object[:dot])
	if !isDbContextName(owner) && !isDbContextName(lastComponent(c.types[owner])) {
		return "", 0
	}
	return dbSetTable(prop), contract.ConfHeuristic
}

// nonEFContexts are the framework types whose name ends in "Context" but whose
// members are not DbSets. Without them `httpContext.Items.Add(x)` is read as a
// write to a table called "items". A bare "Context" is deliberately absent: it
// is the name of the accessor eShop reaches its DbSets through.
var nonEFContexts = map[string]bool{
	"httpcontext": true, "testcontext": true, "servercallcontext": true,
	"actioncontext": true, "validationcontext": true, "modelbindingcontext": true,
	"authorizationhandlercontext": true, "synchronizationcontext": true,
	"executioncontext": true, "activitycontext": true, "operationcontext": true,
}

// isDbContextName reports whether an identifier or type name is an EF Core
// context: "Context" and "DbContext" suffixes are what every context, every
// field holding one and every accessor exposing one is called.
func isDbContextName(s string) bool {
	if s == "" || !hasSuffixFold(s, "context") {
		return false
	}
	return !nonEFContexts[strings.ToLower(s)]
}

// dbSetTable derives the table a DbSet property serves from the property name.
// The names are already plural (CatalogItems), so the entity pluralization
// tableName applies would double it into catalog_itemses; the DbSet<T> path
// pluralizes the singular entity and lands on the same key.
func dbSetTable(prop string) string {
	s := snakeCase(prop)
	if s == "" {
		return ""
	}
	if strings.HasSuffix(s, "s") {
		return s
	}
	return tableName(s)
}

// reEFEntity extracts the entity type argument of the EF Core generics that
// name what a mapping configures: the IEntityTypeConfiguration<T> a class
// implements, the EntityTypeBuilder<T> its Configure takes, and the
// modelBuilder.Entity<T>() a model builder opens. Only a ToTable call turns
// one into a unit, so the set can afford to be generous.
var reEFEntity = regexp.MustCompile(
	`\b(?:IEntityTypeConfiguration|EntityTypeBuilder|OwnedNavigationBuilder|Entity)\s*<\s*([A-Za-z_][A-Za-z0-9_.]*)\s*>`)

// collectEFTables publishes the table an EF Core mapping declares, paired with
// the entity it maps: a db_table unit keyed on the declared name whose
// signature carries "entity:<T>". The data-access edges are keyed on the name
// derived from the entity (db:catalog_items), which is not the name the
// project declared (db:catalog); the pairing is what lets the linker join the
// two (see tableCandidates in internal/graph).
//
// entity is the type the enclosing scope configures, taken from whichever of
// the three generics above the scope names. It is inherited by the subtree, so
// a Configure method reaches the ToTable calls in its body and an
// Entity<T>(b => ...) block reaches the ones in its lambda.
func (c *csharpExtractor) collectEFTables(fc *fileCtx, n *sitter.Node, entity string) {
	if n == nil {
		return
	}
	switch n.Type() {
	case "class_declaration", "record_declaration", "struct_declaration":
		if e := efEntityArg(fc.text(firstChildOfType(n, "base_list"))); e != "" {
			entity = e
		}
	case "method_declaration":
		if e := efEntityArg(fc.text(n.ChildByFieldName("parameters"))); e != "" {
			entity = e
		}
	case "invocation_expression":
		fn := fc.text(n.ChildByFieldName("function"))
		// The receiver carries the entity for the chained spelling
		// modelBuilder.Entity<T>().ToTable("x"), the arguments for the block
		// spelling modelBuilder.Entity<T>(b => b.ToTable("x")).
		if e := efEntityArg(fn); e != "" {
			entity = e
		}
		if lastComponent(fn) == "ToTable" {
			c.addEFTable(fc, n, entity)
		}
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		c.collectEFTables(fc, n.NamedChild(i), entity)
	}
}

// efEntityArg returns the entity type named by an EF Core generic in text, or
// "" when it names none.
func efEntityArg(text string) string {
	m := reEFEntity.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return lastComponent(m[1])
}

// addEFTable records the table a ToTable call declares for entity.
//
// ToTable also has overloads that name no table — ToTable(t => t.HasTrigger())
// configures the default one — and a table may be qualified by a schema, given
// positionally or as schema:. The qualifier stays part of the key, which is
// how the SQL parser keys a schema-qualified CREATE TABLE too.
func (c *csharpExtractor) addEFTable(fc *fileCtx, n *sitter.Node, entity string) {
	if entity == "" {
		return
	}
	args, _ := csharpCallArgs(fc, n.ChildByFieldName("arguments"))
	if len(args) == 0 {
		return
	}
	name, ok := c.consts.resolve(args[0])
	if !ok || name == "" {
		return
	}
	if len(args) > 1 {
		if schema, ok := c.consts.resolve(args[1]); ok && schema != "" {
			name = schema + "." + name
		}
	}
	tbl := sqlTableName(name)
	if tbl == "" {
		return
	}
	fc.addUnit(n, storage.KindDBTable, tbl, contract.DB(tbl), "entity:"+entity,
		extractLineComments(string(fc.src), n, "//"))
}

// csharpCallArgs splits an argument list into positional expression texts and
// the named arguments. C# names arguments at the call site far more than the
// other languages do — the RabbitMQ client's own API is written that way —
// and a rule reading position 1 of BasicPublishAsync(exchange: x, routingKey:
// y, ...) gets the text "routingKey: y", which resolves to nothing.
func csharpCallArgs(fc *fileCtx, argList *sitter.Node) ([]string, map[string]string) {
	if argList == nil {
		return nil, nil
	}
	var args []string
	var named map[string]string
	for i := 0; i < int(argList.NamedChildCount()); i++ {
		a := argList.NamedChild(i)
		text := strings.TrimSpace(fc.text(a))
		if key := fc.text(a.ChildByFieldName("name")); key != "" {
			if _, expr, ok := strings.Cut(text, ":"); ok {
				text = strings.TrimSpace(expr)
			}
			if named == nil {
				named = map[string]string{}
			}
			named[key] = text
		}
		args = append(args, text)
	}
	return args, named
}

// argType returns the declared or constructed type of an argument expression:
// a `new X(...)` written at the call site, or the type recorded for the local
// or field it names.
func (c *csharpExtractor) argType(expr string) string {
	expr = strings.TrimSpace(expr)
	if rest, ok := strings.CutPrefix(expr, "new "); ok {
		if head, ok := callHead("new " + rest); ok {
			return strings.TrimSpace(strings.TrimPrefix(head, "new"))
		}
		if i := strings.IndexAny(rest, "({ "); i > 0 {
			return strings.TrimSpace(rest[:i])
		}
		return strings.TrimSpace(rest)
	}
	return c.types[lastComponent(expr)]
}

// efCoreWriteMethods are the DbSet<T> methods that stage row mutations; other
// DbSet calls are LINQ reads.
var efCoreWriteMethods = map[string]bool{
	"Add": true, "AddAsync": true, "AddRange": true, "AddRangeAsync": true,
	"Update": true, "UpdateRange": true, "Remove": true, "RemoveRange": true,
	"ExecuteDelete": true, "ExecuteUpdate": true,
	"ExecuteDeleteAsync": true, "ExecuteUpdateAsync": true,
}

// efCoreReadMethods are the LINQ and EF operators that end or compose a query.
// The DbSet<T> path needs no such list — anything called on a known DbSet is a
// query — but the receiver-shape path does: without it every `.Response`,
// `.WriteAsync` and `.Dispose` on something ending in "Context" would be read
// as a table access.
var efCoreReadMethods = map[string]bool{
	"Where": true, "Select": true, "SelectMany": true, "Include": true, "ThenInclude": true,
	"Find": true, "FindAsync": true, "First": true, "FirstAsync": true,
	"FirstOrDefault": true, "FirstOrDefaultAsync": true,
	"Single": true, "SingleAsync": true, "SingleOrDefault": true, "SingleOrDefaultAsync": true,
	"Any": true, "AnyAsync": true, "All": true, "AllAsync": true,
	"Count": true, "CountAsync": true, "LongCount": true, "LongCountAsync": true,
	"ToList": true, "ToListAsync": true, "ToArray": true, "ToArrayAsync": true,
	"ToDictionary": true, "ToDictionaryAsync": true,
	"OrderBy": true, "OrderByDescending": true, "GroupBy": true, "Skip": true, "Take": true,
	"AsNoTracking": true, "AsQueryable": true, "AsEnumerable": true,
	"Sum": true, "SumAsync": true, "Max": true, "MaxAsync": true, "Min": true, "MinAsync": true,
	"FromSqlRaw": true, "FromSqlInterpolated": true, "FromSql": true,
}

// csharpAttribute is a parsed C# attribute.
type csharpAttribute struct {
	name string
	node *sitter.Node
}

// csharpAttributes returns attributes attached to a declaration.
func csharpAttributes(fc *fileCtx, decl *sitter.Node) []csharpAttribute {
	var out []csharpAttribute
	for i := 0; i < int(decl.ChildCount()); i++ {
		list := decl.Child(i)
		if list == nil || list.Type() != "attribute_list" {
			continue
		}
		for k := 0; k < int(list.NamedChildCount()); k++ {
			a := list.NamedChild(k)
			if a.Type() == "attribute" {
				out = append(out, csharpAttribute{name: fc.text(a.ChildByFieldName("name")), node: a})
			}
		}
	}
	return out
}

// attrRoutePath returns the [Route("...")] path on a class, expanding the
// [controller] token to the class name without the Controller suffix.
func (c *csharpExtractor) attrRoutePath(fc *fileCtx, decl *sitter.Node, className string) string {
	for _, attr := range csharpAttributes(fc, decl) {
		if attr.name == "Route" {
			path := c.attrValue(fc, attr.node)
			token := strings.ToLower(strings.TrimSuffix(className, "Controller"))
			path = strings.ReplaceAll(path, "[controller]", token)
			return path
		}
	}
	return ""
}

// attrValue extracts the first string argument of an attribute.
func (c *csharpExtractor) attrValue(fc *fileCtx, attr *sitter.Node) string {
	argList := firstChildOfType(attr, "attribute_argument_list")
	if argList == nil {
		return ""
	}
	for i := 0; i < int(argList.NamedChildCount()); i++ {
		if v, ok := c.resolve(fc.text(argList.NamedChild(i))); ok {
			return v
		}
	}
	return ""
}

func (c *csharpExtractor) qualify(scope, name string) string {
	if scope != "" {
		return scope + "." + name
	}
	if c.ns != "" {
		return c.ns + "." + name
	}
	return name
}
