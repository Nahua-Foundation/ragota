package ast

import (
	"regexp"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"

	sitter "github.com/smacker/go-tree-sitter"
)

// goExtractor extracts units and edges from Go source.
//
// Besides plain symbols and calls it detects, heuristically:
//   - HTTP route registrations (net/http, chi, gin, echo style)
//   - outgoing HTTP client calls (http.Get/Post, http.NewRequest*)
//   - gRPC client calls via generated New<Service>Client stubs
//   - gRPC server implementations via Register<Service>Server + method match
//   - Kafka producers/consumers (segmentio/kafka-go, sarama, confluent style)
type goExtractor struct {
	pkg      string
	consts   constResolver     // identifier -> string literal value (file scope)
	aliases  aliasTable        // local aliases (x := userID), scoped per function
	writers  map[string]string // var name -> kafka topic (producer bindings)
	readers  map[string]string // var name -> kafka topic (consumer bindings)
	clients  map[string]string // var name -> gRPC service name
	types    map[string]string // field/param/var name -> declared type
	prefixes map[string]string // router var -> path prefix (gin/chi groups)
	scopes   []routeScope      // lexical router prefixes (chi Route/Mount closures)
	regSvcs  []string          // services registered via Register<X>Server in this file

	// svcTypes maps a struct that embeds an Unimplemented<X>Server to X, and
	// methodRecv a method unit index to its receiver type. Together they bind
	// each method to the service it implements without a registration call,
	// which is what the implementation file usually lacks: the struct is
	// declared next to its methods and registered from main.
	svcTypes   map[string]string
	methodRecv map[int]string
}

// routeScope is a router prefix in effect for route registrations inside a
// byte range, as produced by chi's r.Route("/api", func(r chi.Router){...}).
type routeScope struct {
	start, end int
	prefix     string
}

func (g *goExtractor) extract(fc *fileCtx, root *sitter.Node) {
	g.consts = constResolver{}
	g.aliases = aliasTable{}
	g.writers = map[string]string{}
	g.readers = map[string]string{}
	g.clients = map[string]string{}
	g.types = map[string]string{}
	g.prefixes = map[string]string{}
	g.scopes = nil
	g.svcTypes = map[string]string{}
	g.methodRecv = map[int]string{}

	g.collect(fc, root)
	g.walkCalls(fc, root)
	g.markRPCImplementations(fc)
}

// collect gathers declarations, constants and framework bindings.
func (g *goExtractor) collect(fc *fileCtx, n *sitter.Node) {
	if n == nil {
		return
	}
	switch n.Type() {
	case "package_clause":
		if id := firstChildOfType(n, "package_identifier"); id != nil {
			g.pkg = fc.text(id)
		}
	case "function_declaration":
		name := fc.text(n.ChildByFieldName("name"))
		if name != "" {
			sig := fc.text(n.ChildByFieldName("parameters"))
			doc := extractLineComments(string(fc.src), n, "//")
			fc.addUnit(n, "function", name, g.qualify(name), sig, doc)
		}
	case "method_declaration":
		name := fc.text(n.ChildByFieldName("name"))
		if name != "" {
			recv := receiverType(fc, n)
			sig := fc.text(n.ChildByFieldName("parameters"))
			doc := extractLineComments(string(fc.src), n, "//")
			qualified := g.qualify(name)
			if recv != "" {
				qualified = g.qualify(recv + "." + name)
			}
			idx := fc.addUnit(n, "method", name, qualified, sig, doc)
			if recv != "" {
				g.methodRecv[idx] = recv
			}
		}
	case "type_spec":
		name := fc.text(n.ChildByFieldName("name"))
		if name != "" {
			doc := extractLineComments(string(fc.src), n, "//")
			fc.addUnit(n, "type", name, g.qualify(name), "", doc)
			if svc := goEmbeddedServer(fc.text(n)); svc != "" {
				g.svcTypes[name] = svc
			}
			// A struct carrying gorm tags is a mapped entity: publish the table
			// it maps to so ORM data-access edges have a destination unit.
			if strings.Contains(fc.text(n), `gorm:"`) {
				if tbl := tableName(name); tbl != "" {
					fc.addUnit(n, store.KindDBTable, tbl, contract.DB(tbl), "entity:"+name, doc)
				}
			}
		}
	case "field_declaration", "parameter_declaration":
		g.recordTypedNames(fc, n)
	case "const_spec", "var_spec":
		g.recordStringBinding(fc, n.ChildByFieldName("name"), n.ChildByFieldName("value"))
	case "short_var_declaration", "assignment_statement":
		g.recordAssignment(fc, n)
	case "call_expression":
		g.recordRouterScope(fc, n)
	}
	for i := 0; i < int(n.ChildCount()); i++ {
		g.collect(fc, n.Child(i))
	}
}

// recordStringBinding maps identifier -> string literal for const/var specs.
//
// A store file's bindings are also published to its package: go-memdb keeps
// the table names in a schema file and the queries that use them in the files
// beside it, so the name a query resolves to is one this file has never seen.
func (g *goExtractor) recordStringBinding(fc *fileCtx, nameNode, valueNode *sitter.Node) {
	if nameNode == nil || valueNode == nil {
		return
	}
	if v, ok := unquote(fc.text(valueNode)); ok {
		name := fc.text(nameNode)
		g.consts[name] = v
		if fc.hasMemdb() {
			fc.publishConst(name, v)
		}
	}
}

// recordTypedNames maps the declared names of a struct field or parameter to
// their type text, so a call on an injected dependency can be classified by
// what it was declared as (`orders pb.OrderServiceClient`).
func (g *goExtractor) recordTypedNames(fc *fileCtx, n *sitter.Node) {
	typ := fc.text(n.ChildByFieldName("type"))
	if typ == "" {
		return
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		switch c := n.NamedChild(i); c.Type() {
		case "field_identifier", "identifier":
			g.types[fc.text(c)] = typ
		}
	}
}

// recordRouterScope tracks the lexical prefix of a chi-style sub-router:
// r.Route("/api", func(r chi.Router) { r.Get("/orders", h) }) registers
// "/api/orders". The prefix applies to every registration inside the closure.
func (g *goExtractor) recordRouterScope(fc *fileCtx, call *sitter.Node) {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return
	}
	if name := lastComponent(fc.text(fn)); name != "Route" && name != "Mount" && name != "Group" {
		return
	}
	nodes := argNodes(call, "arguments")
	if len(nodes) < 2 {
		return
	}
	prefix, ok := unquote(fc.text(nodes[0]))
	if !ok || !strings.HasPrefix(prefix, "/") {
		return
	}
	operand := ""
	if fn.Type() == "selector_expression" {
		operand = fc.text(fn.ChildByFieldName("operand"))
	}
	full := joinPath(g.prefixAt(operand, int(call.StartByte())), prefix)
	for _, a := range nodes[1:] {
		if a.Type() == "func_literal" {
			g.scopes = append(g.scopes, routeScope{start: int(a.StartByte()), end: int(a.EndByte()), prefix: full})
		}
	}
}

// prefixAt returns the router prefix in effect for a registration made on
// receiver `object` at byte offset pos: the receiver's own group prefix when it
// has one, otherwise the innermost enclosing Route/Mount closure.
func (g *goExtractor) prefixAt(object string, pos int) string {
	if p, ok := g.prefixes[object]; ok {
		return p
	}
	best, bestSpan := "", 1<<62
	for _, s := range g.scopes {
		if pos >= s.start && pos < s.end && s.end-s.start < bestSpan {
			best, bestSpan = s.prefix, s.end-s.start
		}
	}
	return best
}

// recordAssignment tracks kafka writer/reader and gRPC client bindings plus
// plain string constants from := / = statements.
func (g *goExtractor) recordAssignment(fc *fileCtx, n *sitter.Node) {
	left := n.ChildByFieldName("left")
	right := n.ChildByFieldName("right")
	if left == nil || right == nil || left.NamedChildCount() == 0 || right.NamedChildCount() == 0 {
		return
	}
	name := fc.text(left.NamedChild(0))
	name = strings.TrimPrefix(name, "*")
	val := right.NamedChild(0)

	if v, ok := unquote(fc.text(val)); ok {
		g.consts[name] = v
		return
	}

	// Constant propagation through a plain identifier (topic := ordersTopic),
	// so a value later taken by address still resolves. The alias is recorded
	// as well: the two maps serve different consumers.
	if val.Type() == "identifier" {
		if v, ok := g.consts[fc.text(val)]; ok {
			g.consts[name] = v
		}
	}

	// Local alias: x := userID / x := body.UserID. Only plain identifiers and
	// selector chains count — calls, literals and composites are not aliases.
	switch val.Type() {
	case "identifier", "selector_expression":
		g.aliases.record(n, name, fc.text(val))
	case "composite_literal":
		// order := Order{...} — remember the type for ORM table resolution.
		if t := val.ChildByFieldName("type"); t != nil {
			g.types[name] = fc.text(t)
		}
	case "unary_expression":
		if o := val.ChildByFieldName("operand"); o != nil && o.Type() == "composite_literal" {
			if t := o.ChildByFieldName("type"); t != nil {
				g.types[name] = fc.text(t)
			}
		}
	}

	// Kafka bindings: kafka.Writer{Topic: ...}, kafka.NewWriter(...Topic...),
	// kafka.NewReader(kafka.ReaderConfig{Topic: ...})
	valText := fc.text(val)
	if topic, ok := g.findTopicField(fc, val); ok {
		switch {
		case strings.Contains(valText, "Writer") || strings.Contains(valText, "Producer"):
			g.writers[name] = topic
		case strings.Contains(valText, "Reader") || strings.Contains(valText, "Consumer"):
			g.readers[name] = topic
		}
	}

	// gRPC client: c := pb.NewOrderServiceClient(conn)
	if val.Type() == "call_expression" {
		fn := lastComponent(fc.text(val.ChildByFieldName("function")))
		if strings.HasPrefix(fn, "New") && strings.HasSuffix(fn, "Client") && len(fn) > len("New")+len("Client") {
			g.clients[name] = fn[len("New") : len(fn)-len("Client")]
		}
		// topic := os.Getenv("ORDERS_TOPIC") — record as a config reference
		// that the linker resolves against indexed config/env files.
		if fn == "Getenv" {
			if a := namedChildTexts(fc, val.ChildByFieldName("arguments")); len(a) == 1 {
				if lit, ok := unquote(a[0]); ok {
					g.consts[name] = "${" + lit + "}"
					return // const recorded — no alias for the same lhs
				}
			}
		}
		// Router group: v1 := r.Group("/api/v1") / sub := r.Route("/api", ...).
		// Routes registered on the returned router carry the accumulated prefix.
		if fn == "Group" || fn == "Route" {
			if a := argNodes(val, "arguments"); len(a) > 0 {
				if p, ok := unquote(fc.text(a[0])); ok && strings.HasPrefix(p, "/") {
					operand := ""
					if f := val.ChildByFieldName("function"); f != nil && f.Type() == "selector_expression" {
						operand = fc.text(f.ChildByFieldName("operand"))
					}
					g.prefixes[name] = joinPath(g.prefixAt(operand, int(val.StartByte())), p)
					return
				}
			}
		}
		// Call-result alias: x := extractUserID(req). The full call text becomes
		// the alias value so token matching (plus transitive dereference in the
		// trace) links x to the call's argument identifiers.
		g.aliases.record(n, name, valText)
	}
}

// findTopicField looks for a `Topic: <expr>` keyed element anywhere below n.
// The value may be an address-of expression (confluent-kafka takes *string in
// kafka.TopicPartition{Topic: &t}), so the address/dereference operators are
// stripped before resolving.
func (g *goExtractor) findTopicField(fc *fileCtx, n *sitter.Node) (string, bool) {
	if n == nil {
		return "", false
	}
	if n.Type() == "keyed_element" && n.NamedChildCount() >= 2 {
		key := fc.text(n.NamedChild(0))
		if key == "Topic" {
			return g.consts.resolve(strings.TrimLeft(strings.TrimSpace(fc.text(n.NamedChild(1))), "&*"))
		}
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		if v, ok := g.findTopicField(fc, n.NamedChild(i)); ok {
			return v, true
		}
	}
	return "", false
}

var goRouteMethods = map[string]string{
	"Get": "GET", "Post": "POST", "Put": "PUT", "Delete": "DELETE", "Patch": "PATCH",
	"Head": "HEAD", "Options": "OPTIONS",
	"GET": "GET", "POST": "POST", "PUT": "PUT", "DELETE": "DELETE", "PATCH": "PATCH",
}

// goDetectors are the declarative framework rules for Go (see frameworks.go).
// Kafka stays out of this set on purpose: segmentio-style produce/consume is
// resolved through per-variable writer/reader bindings (g.writers/g.readers),
// which the neutral engine cannot express.
var goDetectors = detectorSet{
	HTTP: []httpClientRule{
		// net/http package-level helpers and *http.Client methods:
		// http.Get(url), client.Post(url, ...), apiClient.PostForm(url, ...).
		{
			Object:  objectMatch{Exact: []string{"http", "client"}, Suffix: []string{"Client"}},
			Methods: map[string]string{"Get": "GET", "Post": "POST", "Head": "HEAD", "PostForm": "POST"},
			URLArg:  0,
			Conf:    contract.ConfCrossFile,
		},
	},
	Consume: append([]kafkaConsumeRule{
		// confluent-kafka-go: consumer.SubscribeTopics([]string{"orders"}, nil)
		{Methods: []string{"SubscribeTopics"}, TopicArg: 0, TopicArgList: true, Conf: contract.ConfHigh},
	}, goBrokerConsume...),
	Produce: goBrokerProduce,
	Broker:  true,
}

var goKafkaProduceMethods = map[string]bool{
	"WriteMessages": true, "SendMessage": true, "SendMessages": true, "Produce": true, "Publish": true,
}

var goKafkaConsumeMethods = map[string]bool{
	"ReadMessage": true, "FetchMessage": true, "Consume": true, "ConsumeClaim": true, "ReadMessages": true,
}

// goGorm{Write,Read}Methods are the GORM finisher methods that end a chain in
// a statement; the table comes from the chain's Model/Table call or from the
// destination struct.
var goGormWriteMethods = map[string]bool{
	"Create": true, "CreateInBatches": true, "Save": true, "Update": true,
	"Updates": true, "Delete": true, "FirstOrCreate": true,
}

var goGormReadMethods = map[string]bool{
	"Find": true, "First": true, "Last": true, "Take": true,
	"Scan": true, "Pluck": true, "Count": true, "FindInBatches": true,
}

var (
	reGormTable = regexp.MustCompile(`\bTable\(\s*"([^"]+)"`)
	reGormModel = regexp.MustCompile(`\bModel\(\s*([&*]?[A-Za-z_][A-Za-z0-9_.\[\]]*)`)
)

// gormTable resolves the table a GORM statement touches: an explicit
// .Table("orders") or .Model(&Order{}) in the receiver chain, otherwise the
// destination value passed to the finisher (db.Create(&order)).
func (g *goExtractor) gormTable(object string, args []string) string {
	if m := reGormTable.FindStringSubmatch(object); m != nil {
		return strings.ToLower(m[1])
	}
	if m := reGormModel.FindStringSubmatch(object); m != nil {
		if t := g.entityTable(m[1]); t != "" {
			return t
		}
	}
	if len(args) > 0 {
		return g.entityTable(args[0])
	}
	return ""
}

// entityTable maps a Go value expression to the table its type maps to:
// &Order{} / order (declared as Order) / []Order all yield "orders".
func (g *goExtractor) entityTable(expr string) string {
	e := strings.TrimLeft(strings.TrimSpace(expr), "&*")
	if i := strings.IndexByte(e, '{'); i > 0 {
		e = e[:i] // composite literal
	} else if t, ok := g.types[e]; ok {
		e = t
	}
	e = lastComponent(strings.TrimLeft(strings.TrimSpace(e), "*&[]"))
	if e == "" || e[0] < 'A' || e[0] > 'Z' || !isIdentLike(e) {
		return ""
	}
	return tableName(e)
}

// walkCalls emits edges for call expressions using bindings collected earlier.
func (g *goExtractor) walkCalls(fc *fileCtx, n *sitter.Node) {
	if n == nil {
		return
	}
	switch n.Type() {
	case "call_expression":
		g.handleCall(fc, n)
	case "composite_literal":
		g.handleRouteLiteral(fc, n)
	}
	for i := 0; i < int(n.ChildCount()); i++ {
		g.walkCalls(fc, n.Child(i))
	}
}

// handleRouteLiteral detects routes declared as a table of struct literals —
// []Route{{"GET", "/orders", listOrders}} or Route{Method: "GET", Path: "/x",
// Handler: h} — which is how a service that owns its routing registers when it
// does not call a router at all.
func (g *goExtractor) handleRouteLiteral(fc *fileCtx, n *sitter.Node) {
	typ := strings.Trim(fc.text(n.ChildByFieldName("type")), "[]*&")
	if !routeNameHint(typ) {
		return
	}
	body := n.ChildByFieldName("body")
	if body == nil {
		return
	}
	list := goStringsIn(g.consts)
	nested := false
	for i := 0; i < int(body.NamedChildCount()); i++ {
		el := body.NamedChild(i)
		if el.Type() == "literal_element" && el.NamedChildCount() == 1 {
			el = el.NamedChild(0)
		}
		if el.Type() != "literal_value" {
			continue
		}
		nested = true
		applyGenericRoute(fc, el, typ, literalElements(fc, el, "keyed_element"), g.consts.resolve, list, "")
	}
	if !nested {
		applyGenericRoute(fc, n, typ, literalElements(fc, body, "keyed_element"), g.consts.resolve, list, "")
	}
}

func (g *goExtractor) handleCall(fc *fileCtx, call *sitter.Node) {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return
	}
	fnText := fc.text(fn)
	name := lastComponent(fnText)
	operand := ""
	if fn.Type() == "selector_expression" {
		operand = fc.text(fn.ChildByFieldName("operand"))
	}
	args := namedChildTexts(fc, call.ChildByFieldName("arguments"))
	line := int(call.StartPoint().Row) + 1
	pos := int(call.StartByte())
	src := fc.enclosingUnit(pos, callableKinds...)

	// 1. HTTP route registration: r.Post("/path", handler), http.HandleFunc("POST /p", h).
	// The router's group/mount prefix is prepended so the stored path is the one
	// callers actually request.
	if method, route, handler, ok := g.matchRoute(name, args); ok {
		route = joinPath(g.prefixAt(operand, pos), route)
		idx := fc.addUnit(call, store.KindHTTPRoute, method+" "+route, routeKey(method, route), "", "")
		fc.units[idx].Signature = "path:" + route
		if handler != "" {
			fc.addEdge(idx, store.EdgeHandledBy, lastComponent(handler), line, contract.ConfHigh, nil)
		}
		return
	}

	// 1b. Route registered through the service's own machinery:
	// registerEndpoint("/v1/acl/login", []string{"POST"}, (*HTTPHandlers).ACLLogin).
	if applyGenericRoute(fc, call, fnText, args, g.consts.resolve, goStringsIn(g.consts), g.prefixAt(operand, pos)) {
		return
	}

	// 2. gRPC client call: client.CreateOrder(ctx, &pb.Req{...}), including the
	// client constructed in the call itself
	// (pb.NewCartServiceClient(fe.cartSvcConn).GetCart(ctx, req)).
	if svc := g.grpcService(fc, operand); svc != "" && name != "" {
		fields := g.compositeFields(fc, call)
		meta := &store.EdgeMeta{Args: args, Fields: fields, Aliases: g.aliases.relevant(pos, args, fields)}
		fc.addEdge(src, store.EdgeRPCCall, grpcKey(svc, name), line, contract.ConfHigh, meta)
		fc.contractSite(domain.ContractKindRPC, true)
		return
	}

	// 3. gRPC server registration: pb.RegisterOrderServiceServer(s, impl)
	if strings.HasPrefix(name, "Register") && strings.HasSuffix(name, "Server") && len(name) > len("Register")+len("Server") {
		g.regSvcs = append(g.regSvcs, name[len("Register"):len(name)-len("Server")])
		return
	}

	// 4. Kafka produce/consume on tracked writer/reader vars. Stays outside
	// the declarative detectors: the topic comes from per-variable bindings
	// collected at assignment sites, not from the call itself.
	//
	// A call this step recognizes but cannot name a topic for falls through to
	// the broker rules below, which spell the same method names for AMQP and
	// NATS. Counting it as a candidate is therefore deferred until they have
	// had their turn, so one call site is not counted twice.
	messagingSite := false
	if goKafkaProduceMethods[name] {
		topic := g.writers[operand]
		conf := contract.ConfHigh
		if topic == "" {
			// sarama and confluent carry the topic in the message struct:
			// SendMessage(&sarama.ProducerMessage{Topic: "orders"}) and
			// Produce(&kafka.Message{TopicPartition: {Topic: &t}}, nil).
			if t, ok := g.findTopicField(fc, call.ChildByFieldName("arguments")); ok {
				topic = t
			}
		}
		if topic == "" {
			topic, conf = g.anyTopic(g.writers), contract.ConfWeak
		}
		if topic != "" {
			fc.contractSite(domain.ContractKindMessaging, true)
			fields := g.enclosingFields(fc, src)
			key, name := topicEdgeKey(topic)
			fc.addEdge(src, store.EdgeProduces, key, line, conf,
				&store.EdgeMeta{Topic: name, Fields: fields, Aliases: g.aliases.relevant(pos, args, fields)})
			return
		}
		messagingSite = true
	}
	if goKafkaConsumeMethods[name] {
		topic := g.readers[operand]
		conf := contract.ConfHigh
		if topic == "" {
			if t, ok := g.findTopicField(fc, call.ChildByFieldName("arguments")); ok {
				topic = t
			}
		}
		if topic == "" {
			topic, conf = g.anyTopic(g.readers), contract.ConfWeak
		}
		if topic != "" {
			fc.contractSite(domain.ContractKindMessaging, true)
			key, name := topicEdgeKey(topic)
			fc.addEdge(src, store.EdgeConsumes, key, line, conf, &store.EdgeMeta{Topic: name})
			return
		}
		messagingSite = true
	}

	// 5. Declarative framework detection (HTTP client calls, SubscribeTopics,
	// AMQP/NATS/SQS/Redis). The broker rules read a destination out of a
	// message struct literal, which is collected only for the calls that can
	// carry one — building it for every call in the file is a second walk of
	// every argument list.
	var msgFields map[string]string
	if goMessageFieldMethods[name] {
		msgFields = g.compositeFields(fc, call)
	}
	msgCandidates := fc.cov[domain.ContractKindMessaging].Candidates
	cs := &callSite{Callee: fnText, Name: name, Object: operand, RecvType: g.types[lastComponent(operand)],
		Args: args, Fields: msgFields, Line: line, Src: src, Aliases: g.aliases.relevant(pos, args, nil)}
	if runDetectors(fc, &goDetectors, cs, g.consts.resolve, goStringsIn(g.consts)) {
		return
	}
	// A kafka call name with no topic anywhere is only a missed contract when
	// something says there is a broker: Publish, Produce, Consume and
	// ReadMessage are the names of every in-process event stream and every
	// websocket in the language. Consul's stream package spells 14 of these
	// and has no broker; argo-cd's five are gorilla/websocket reads.
	if messagingSite && fc.cov[domain.ContractKindMessaging].Candidates == msgCandidates && brokerEvidence(fc, cs) {
		fc.contractSite(domain.ContractKindMessaging, false)
	}

	// 5a. Dependency-injected gRPC stub: the generated constructor runs in a
	// wiring file, so the receiver is classified by its declared type instead.
	if src >= 0 && name != "" && name[0] >= 'A' && name[0] <= 'Z' {
		if svc := grpcStubService(g.types[lastComponent(operand)], fc.hasGRPC()); svc != "" {
			fields := g.compositeFields(fc, call)
			fc.addEdge(src, store.EdgeRPCCall, grpcKey(svc, name), line, contract.ConfCrossFile,
				&store.EdgeMeta{Args: args, Fields: fields, Aliases: g.aliases.relevant(pos, args, fields)})
			fc.contractSite(domain.ContractKindRPC, true)
			return
		}
	}

	// 5b. http.NewRequest("POST", url, body) — Go-specific because the method
	// arrives as an argument, possibly as an http.MethodPost constant.
	if name == "NewRequest" || name == "NewRequestWithContext" {
		mi, ui := 0, 1
		if name == "NewRequestWithContext" {
			mi, ui = 1, 2
		}
		if len(args) > ui {
			m := httpMethodFromArg(args[mi], g.consts.resolve)
			u, conf, ok := resolveURL(args[ui], g.consts.resolve, contract.ConfCrossFile)
			fc.contractSite(domain.ContractKindHTTP, ok)
			if ok {
				host, path := splitURL(u)
				fc.addEdge(src, store.EdgeHTTPCall, routeKey(m, path), line, conf,
					&store.EdgeMeta{Method: m, Path: path, Host: host, Args: args,
						Aliases: g.aliases.relevant(pos, args, nil)})
				return
			}
			fc.httpCandidate(src, args[ui], args[mi])
		}
	}

	// 5c. Data access. go-memdb first: its transaction methods overlap GORM's
	// finishers ("First", "Delete"), and it names its table where GORM does not,
	// so the site it claims is one GORM would count as a table it failed to
	// resolve. Neither consumes the call — the store method is a real call too
	// and stays in the call graph.
	if !g.memdbAccess(fc, src, name, operand, args, line, pos) &&
		src >= 0 && (goGormWriteMethods[name] || goGormReadMethods[name]) {
		// GORM: db.Model(&Order{}).Updates(...), db.Create(&order).
		tbl := g.gormTable(operand, args)
		// "Create", "Find" and "Delete" are ordinary method names as much as
		// they are GORM finishers: an unresolved site only counts as a missed
		// table when the receiver looks like a database handle. Consul, which
		// uses no ORM at all, offered 592 of them otherwise.
		if tbl != "" || isDBReceiver(operand) {
			fc.contractSite(domain.ContractKindDB, tbl != "")
		}
		if tbl != "" {
			kind := store.EdgeReadsFrom
			if goGormWriteMethods[name] {
				kind = store.EdgeWritesTo
			}
			fc.addEdge(src, kind, contract.DB(tbl), line, contract.ConfCrossFile,
				&store.EdgeMeta{Args: args, Aliases: g.aliases.relevant(pos, args, nil)})
		}
	}

	// 6. SQL statements in string arguments (db.Exec, QueryRow, ...).
	sqlEdgesFromArgs(fc, src, line, args)

	// 7. Plain call edge.
	if name != "" && src >= 0 {
		fc.addEdge(src, store.EdgeCall, name, line, contract.ConfHeuristic,
			&store.EdgeMeta{Args: args, Aliases: g.aliases.relevant(pos, args, nil), Receiver: operand, RecvType: g.types[lastComponent(operand)]})
	}
}

// memdbReadMethods and memdbWriteMethods are the hashicorp/go-memdb
// transaction methods that take the table name as their first argument. The
// store has no query language and no mapped entities: the table name at the
// call site is the whole of what a data-access edge can be keyed on.
var memdbReadMethods = map[string]bool{
	"First": true, "FirstWatch": true, "Last": true, "LastWatch": true,
	"Get": true, "GetReverse": true, "LongestPrefix": true,
	"LowerBound": true, "ReverseLowerBound": true,
}

var memdbWriteMethods = map[string]bool{
	"Insert": true, "Delete": true, "DeleteAll": true, "DeletePrefix": true,
}

// memdbAccess records a go-memdb transaction call as data access and emits its
// reads_from / writes_to edge. It reports whether the site was claimed, which
// is what keeps the GORM rules from counting the same call a second time.
//
// A table named by a constant the file does not declare is left for the
// package stage: Consul keeps its names in a <topic>_schema.go beside the
// queries, and 368 of its 437 accesses name their table that way.
func (g *goExtractor) memdbAccess(fc *fileCtx, src int, name, operand string, args []string, line, pos int) bool {
	kind := store.EdgeReadsFrom
	switch {
	case memdbReadMethods[name]:
	case memdbWriteMethods[name]:
		kind = store.EdgeWritesTo
	default:
		return false
	}
	// Both halves are needed. The import alone would take a testify mock's
	// ret.Get(0) in a state-store test for a table read, and the receiver alone
	// would take every transaction in every SQL package for a memdb one.
	if src < 0 || len(args) == 0 || !goTxnReceiver(operand) || !fc.hasMemdb() {
		return false
	}
	if v, ok := g.consts.resolve(args[0]); ok {
		tbl := sqlTableName(v)
		fc.contractSite(domain.ContractKindDB, tbl != "")
		if tbl != "" {
			fc.addEdge(src, kind, contract.DB(tbl), line, contract.ConfHigh,
				&store.EdgeMeta{Args: args, Aliases: g.aliases.relevant(pos, args, nil)})
		}
		return true
	}
	fc.contractSite(domain.ContractKindDB, false)
	if ident := strings.TrimSpace(args[0]); isAliasExpr(ident) {
		fc.tableCandidate(src, ident, kind, line, args)
	}
	return true
}

// goTxnReceiver reports whether a receiver expression names a transaction
// handle: "tx", "txn", "tx2", "s.tx", "readTxn". Matching is case-sensitive on
// the "Tx"/"Txn" word so that "ctx" is not one.
func goTxnReceiver(object string) bool {
	name := strings.TrimRight(lastComponent(object), "0123456789")
	switch name {
	case "tx", "txn", "Tx", "Txn":
		return true
	}
	return strings.HasSuffix(name, "Tx") || strings.HasSuffix(name, "Txn")
}

// dbReceiverHints are the names a database handle is held under.
var dbReceiverHints = []string{"db", "gorm", "tx", "conn", "session", "sql"}

// isDBReceiver reports whether a receiver expression names a database handle.
func isDBReceiver(object string) bool {
	lower := strings.ToLower(lastComponent(object))
	for _, hint := range dbReceiverHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

// grpcService returns the gRPC service a call's receiver stands for: a client
// assigned earlier in the file, or one constructed in the receiver expression
// itself. The generated constructor is routinely chained straight into the
// call — Online Boutique's frontend writes every one of its calls as
// pb.NewCartServiceClient(fe.cartSvcConn).GetCart(ctx, req) — and without the
// second form those calls are invisible.
func (g *goExtractor) grpcService(fc *fileCtx, object string) string {
	if svc, ok := g.clients[object]; ok {
		return svc
	}
	return inlineStubService(object, fc.hasGRPC())
}

// goStringsIn builds a list resolver for Go slice literals such as
// []string{"orders", TOPIC}.
func goStringsIn(consts constResolver) listResolver {
	return func(expr string) []string {
		if i := strings.IndexByte(expr, '{'); i >= 0 {
			expr = strings.TrimSuffix(expr[i+1:], "}")
		}
		var out []string
		for _, part := range contract.SplitTopLevel(expr, ',') {
			if v, ok := consts.resolve(strings.TrimSpace(part)); ok {
				out = append(out, v)
			}
		}
		return out
	}
}

// routePathArg resolves the path argument of a route registration. Real
// services rarely pass a bare literal: nats-server registers
// `mux.HandleFunc(s.basePath(VarzPath), ...)`, where the path is a package
// constant wrapped in a helper. Requiring a literal drops those routes
// entirely, so a constant — including one behind a single-argument call — is
// resolved too.
func (g *goExtractor) routePathArg(arg string) (string, bool) {
	if v, ok := unquote(arg); ok {
		return v, true
	}
	if v, ok := g.consts.resolve(arg); ok {
		return v, true
	}
	if inner, ok := singleCallArg(arg); ok {
		if v, ok := unquote(inner); ok {
			return v, true
		}
		if v, ok := g.consts.resolve(inner); ok {
			return v, true
		}
	}
	return "", false
}

// singleCallArg returns the sole argument of a call expression, so a path
// wrapped in a helper (`s.basePath(VarzPath)`) can still be resolved.
func singleCallArg(expr string) (string, bool) {
	expr = strings.TrimSpace(expr)
	open := strings.IndexByte(expr, '(')
	if open <= 0 || !strings.HasSuffix(expr, ")") {
		return "", false
	}
	inner := strings.TrimSpace(expr[open+1 : len(expr)-1])
	if inner == "" || len(contract.SplitTopLevel(inner, ',')) != 1 {
		return "", false
	}
	return inner, true
}

// matchRoute detects HTTP route registration calls.
func (g *goExtractor) matchRoute(name string, args []string) (method, path, handler string, ok bool) {
	if len(args) < 1 {
		return "", "", "", false
	}
	first, isLit := g.routePathArg(args[0])
	if !isLit {
		return "", "", "", false
	}
	handlerArg := ""
	if len(args) >= 2 {
		handlerArg = args[len(args)-1]
	}

	if m, found := goRouteMethods[name]; found && strings.HasPrefix(first, "/") {
		return m, first, handlerArg, true
	}
	if name == "HandleFunc" || name == "Handle" {
		// Go 1.22 patterns: "POST /path" or plain "/path"
		if strings.HasPrefix(first, "/") {
			return "ANY", first, handlerArg, true
		}
		parts := strings.SplitN(first, " ", 2)
		if len(parts) == 2 && strings.HasPrefix(parts[1], "/") {
			if _, valid := goRouteMethods[parts[0]]; valid || parts[0] == strings.ToUpper(parts[0]) {
				return parts[0], parts[1], handlerArg, true
			}
		}
	}
	return "", "", "", false
}

// compositeFields collects `Field: value` pairs from composite literals in a call.
func (g *goExtractor) compositeFields(fc *fileCtx, n *sitter.Node) map[string]string {
	fields := map[string]string{}
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if n.Type() == "keyed_element" && n.NamedChildCount() >= 2 {
			fields[fc.text(n.NamedChild(0))] = fc.text(n.NamedChild(1))
		}
		for i := 0; i < int(n.NamedChildCount()); i++ {
			walk(n.NamedChild(i))
		}
	}
	if argsNode := n.ChildByFieldName("arguments"); argsNode != nil {
		walk(argsNode)
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// enclosingFields collects keyed struct-literal fields from the body of the
// unit at index idx (used to describe the payload a producer publishes).
func (g *goExtractor) enclosingFields(fc *fileCtx, idx int) map[string]string {
	if idx < 0 || idx >= len(fc.units) {
		return nil
	}
	u := fc.units[idx]
	body := string(fc.src[u.StartByte:u.EndByte])
	// Cheap textual scan for `Name: expr` pairs inside struct literals.
	fields := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ","))
		i := strings.Index(line, ":")
		if i <= 0 || strings.Contains(line[:i], " ") || strings.Contains(line[:i], "\"") {
			continue
		}
		key, val := line[:i], strings.TrimSpace(line[i+1:])
		if key != "" && val != "" && isIdentLike(key) {
			fields[key] = val
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func isIdentLike(s string) bool {
	for _, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return s != ""
}

// anyTopic returns the sole topic if exactly one binding exists.
func (g *goExtractor) anyTopic(m map[string]string) string {
	if len(m) != 1 {
		return ""
	}
	for _, v := range m {
		return v
	}
	return ""
}

// reGoUnimplemented matches the embedded forward-compatibility struct the
// grpc-go plugin generates, with or without its package qualifier.
var reGoUnimplemented = regexp.MustCompile(`(?m)^\s*(?:[A-Za-z_][A-Za-z0-9_]*\.)?Unimplemented([A-Za-z0-9_]+)Server\s*$`)

// goEmbeddedServer returns the service a struct declaration implements by
// embedding its generated Unimplemented<X>Server, or "".
//
// The embedded field is the only local evidence a service implementation file
// carries: the Register<X>Server call that names the service lives in main,
// and the struct is as likely to be called "server" as anything else.
func goEmbeddedServer(decl string) string {
	m := reGoUnimplemented.FindStringSubmatch(decl)
	if m == nil {
		return ""
	}
	return m[1]
}

// markRPCImplementations emits implements_rpc edges for exported methods: from
// the generated server base the receiver embeds, precisely for services
// registered in this file, and liberally for methods whose parameters look
// like generated request types.
func (g *goExtractor) markRPCImplementations(fc *fileCtx) {
	for i, u := range fc.units {
		if u.Kind != "method" || u.Name == "" || u.Name[0] < 'A' || u.Name[0] > 'Z' {
			continue
		}
		if svc := g.svcTypes[g.methodRecv[i]]; svc != "" {
			fc.addEdge(i, store.EdgeImplementsRPC, grpcKey(svc, u.Name), u.StartLine, contract.ConfHigh, nil)
			continue
		}
		emitted := false
		for _, svc := range g.regSvcs {
			fc.addEdge(i, store.EdgeImplementsRPC, grpcKey(svc, u.Name), u.StartLine, contract.ConfCrossFile, nil)
			emitted = true
		}
		if !emitted && strings.Contains(u.Signature, "Request") && strings.Contains(u.Signature, "context.Context") {
			fc.addEdge(i, store.EdgeImplementsRPC, grpcKey("", u.Name), u.StartLine, contract.ConfWeak, nil)
		}
	}
}

func (g *goExtractor) qualify(name string) string {
	if g.pkg == "" {
		return name
	}
	return g.pkg + "." + name
}

// receiverType extracts the receiver type name of a method declaration.
func receiverType(fc *fileCtx, n *sitter.Node) string {
	recv := n.ChildByFieldName("receiver")
	if recv == nil {
		return ""
	}
	t := fc.text(recv)
	t = strings.Trim(t, "()")
	fields := strings.Fields(t)
	if len(fields) == 0 {
		return ""
	}
	return strings.TrimPrefix(fields[len(fields)-1], "*")
}

func firstChildOfType(n *sitter.Node, typ string) *sitter.Node {
	for i := 0; i < int(n.ChildCount()); i++ {
		if c := n.Child(i); c != nil && c.Type() == typ {
			return c
		}
	}
	return nil
}
