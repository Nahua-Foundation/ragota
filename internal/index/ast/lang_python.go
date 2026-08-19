package ast

import (
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"

	sitter "github.com/smacker/go-tree-sitter"
)

// pyExtractor extracts units and edges from Python source.
//
// Framework heuristics:
//   - FastAPI/Flask routes: @app.get("/p"), @router.post("/p"),
//     @app.route("/p", methods=["POST"])
//   - HTTP clients: requests.get/post/..., httpx.get/post/..., session/client calls
//   - Kafka: producer.send/produce("topic", ...), consumer.subscribe([...]),
//     KafkaConsumer("topic", ...)
type pyExtractor struct {
	consts   constResolver
	aliases  aliasTable        // local aliases (x = user_id), scoped per function
	prefixes map[string]string // router/blueprint var -> url prefix
	topics   map[string]string // faust topic var -> topic name
	models   map[string]string // SQLAlchemy model class -> table name

	// servicers maps a class to the proto service it serves, following
	// intermediate base classes declared in the same file; rpcSvc is the entry
	// for the class currently being walked.
	servicers map[string]string
	rpcSvc    string
}

var pyHTTPMethods = map[string]string{
	"get": "GET", "post": "POST", "put": "PUT", "delete": "DELETE",
	"patch": "PATCH", "head": "HEAD", "options": "OPTIONS",
}

// pyProduceMethods are the publish call names across the supported brokers;
// used to decide whether to collect payload dict fields for the call.
var pyProduceMethods = map[string]bool{
	"send": true, "produce": true, "send_and_wait": true, "publish": true,
}

// pyHTTPClientObject is the receiver filter shared by the HTTP client rules
// and the method-in-argument requests.request() form.
var pyHTTPClientObject = objectMatch{
	Exact:    []string{"requests", "httpx"},
	Contains: []string{"client", "session"},
	Fold:     true,
}

// pyHTTPClientNoSession is pyHTTPClientObject with the session receiver
// dropped, used in files that import SQLAlchemy (see pyDetectorsORM).
var pyHTTPClientNoSession = objectMatch{
	Exact:    []string{"requests", "httpx"},
	Contains: []string{"client"},
	Fold:     true,
}

// pyDetectors are the declarative framework rules for Python (see frameworks.go).
var pyDetectors = detectorSet{
	Produce: append([]kafkaProduceRule{
		// kafka-python / confluent-kafka / aiokafka:
		// producer.send("topic", value=...), producer.produce("topic", ...),
		// await producer.send_and_wait("topic", payload)
		//
		// NeedsBroker because "send" in python is the generator protocol
		// (gen.send(x)), the requests adapter (session.send(prepared)) and
		// every IPC channel ever written: airflow's task supervisor alone
		// accounted for 117 of these, none of them a broker.
		{Methods: []string{"send", "produce", "send_and_wait"}, TopicArg: 0, NeedsBroker: true, Conf: contract.ConfHigh},
	}, pyBrokerProduce...),
	Consume: append([]kafkaConsumeRule{
		// consumer.subscribe(["topic-a", "topic-b"])
		{Methods: []string{"subscribe"}, TopicArg: 0, TopicArgList: true, NeedsBroker: true, Conf: contract.ConfHigh},
		// KafkaConsumer("topic", ...) / AIOKafkaConsumer("topic", ...)
		{Methods: []string{"KafkaConsumer", "AIOKafkaConsumer"}, TopicArg: 0, Conf: contract.ConfHigh},
	}, pyBrokerConsume...),
	Broker: true,
	HTTP: []httpClientRule{
		// requests.post(url, json=...), httpx.get(url), client/session calls.
		//
		// The target is written against the client's base_url as often as it is
		// written whole — httpx.Client(base_url=…).get("variables/keys") — so a
		// base-relative route counts, which is all the URL argument of these
		// calls can be.
		{
			Object:      pyHTTPClientObject,
			Methods:     pyHTTPMethods,
			URLArg:      0,
			RelativeURL: true,
			Conf:        contract.ConfCrossFile,
		},
	},
}

// pyDetectorsORM is pyDetectors for a file that imports SQLAlchemy, where a
// session is a database session.
//
// SQLAlchemy's Session and requests' Session share the method names the HTTP
// rule matches on, and only the file's imports tell them apart:
// session.get(DagModel, dag_id) reads a row and session.get(url) makes a
// request. Reading the first as a request cost nothing in edges and a great
// deal in candidates — over the corpus the session receiver produced 3 http
// edges, none of them in a file that imports SQLAlchemy, against 334 row reads
// counted as outbound requests whose target could not be resolved.
var pyDetectorsORM = detectorSet{
	Produce: pyDetectors.Produce,
	Consume: pyDetectors.Consume,
	Broker:  pyDetectors.Broker,
	HTTP:    []httpClientRule{pyHTTPNoSessionRule},
}

// pyHTTPNoSessionRule is the HTTP rule of pyDetectors with the session
// receiver dropped.
var pyHTTPNoSessionRule = httpClientRule{
	Object:      pyHTTPClientNoSession,
	Methods:     pyHTTPMethods,
	URLArg:      0,
	RelativeURL: true,
	Conf:        contract.ConfCrossFile,
}

// pyDetectorsFor returns the rule set that applies to this file, and the
// receiver filter its HTTP rule carries — which the requests.request() form
// below has to match on too.
func pyDetectorsFor(fc *fileCtx) (*detectorSet, objectMatch) {
	if fc.hasSQLAlchemy() {
		return &pyDetectorsORM, pyHTTPClientNoSession
	}
	return &pyDetectors, pyHTTPClientObject
}

func (p *pyExtractor) extract(fc *fileCtx, root *sitter.Node) {
	p.consts = constResolver{}
	p.aliases = aliasTable{}
	p.prefixes = map[string]string{}
	p.topics = map[string]string{}
	p.models = map[string]string{}
	p.servicers = map[string]string{}
	p.rpcSvc = ""
	p.collectConsts(fc, root)
	p.collectRouters(fc, root)
	p.walkDecls(fc, root, "")
	p.walkCalls(fc, root)
}

// pyPrefixFactories are the constructors that carry a mount prefix for the
// routes registered on the object they return.
var pyPrefixFactories = map[string]bool{"APIRouter": true, "Blueprint": true}

// collectRouters records the url prefix of routers and blueprints so decorated
// routes are stored with the path clients actually call:
//
//	router = APIRouter(prefix="/api/v1")     -> @router.get("/orders")
//	bp = Blueprint("orders", __name__, url_prefix="/api")
//	app.include_router(router, prefix="/api/v1")
//
// It also records faust topic bindings (orders = app.topic("orders")).
func (p *pyExtractor) collectRouters(fc *fileCtx, n *sitter.Node) {
	if n == nil {
		return
	}
	switch n.Type() {
	case "assignment":
		left, right := n.ChildByFieldName("left"), n.ChildByFieldName("right")
		if left != nil && right != nil && right.Type() == "call" && left.Type() == "identifier" {
			name := fc.text(left)
			callee := lastComponent(fc.text(right.ChildByFieldName("function")))
			_, kwargs := pyCallArgs(fc, right)
			if pyPrefixFactories[callee] {
				for _, key := range []string{"prefix", "url_prefix"} {
					if v, ok := p.resolveString(kwargs[key]); ok && strings.HasPrefix(v, "/") {
						p.prefixes[name] = v
					}
				}
			}
			// faust: orders_topic = app.topic("orders")
			if callee == "topic" {
				if args, _ := pyCallArgs(fc, right); len(args) > 0 {
					if v, ok := p.resolveString(args[0]); ok {
						p.topics[name] = v
					}
				}
			}
		}
	case "call":
		if lastComponent(fc.text(n.ChildByFieldName("function"))) == "include_router" {
			args, kwargs := pyCallArgs(fc, n)
			if v, ok := p.resolveString(kwargs["prefix"]); ok && len(args) > 0 && strings.HasPrefix(v, "/") {
				router := lastComponent(strings.TrimSpace(args[0]))
				p.prefixes[router] = joinPath(v, p.prefixes[router])
			}
		}
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		p.collectRouters(fc, n.NamedChild(i))
	}
}

func (p *pyExtractor) collectConsts(fc *fileCtx, n *sitter.Node) {
	if n == nil {
		return
	}
	if n.Type() == "assignment" {
		left := n.ChildByFieldName("left")
		right := n.ChildByFieldName("right")
		if left != nil && right != nil && left.Type() == "identifier" {
			if v, ok := p.resolveString(fc.text(right)); ok {
				p.consts[fc.text(left)] = v
			} else {
				// Local alias: x = user_id / x = body.user_id. A call RHS
				// (x = extract_user_id(req)) is recorded with its full text so
				// token matching links x to the call's argument identifiers.
				switch right.Type() {
				case "identifier", "attribute", "call":
					p.aliases.record(n, fc.text(left), fc.text(right))
				}
			}
		}
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		p.collectConsts(fc, n.NamedChild(i))
	}
}

// walkDecls collects functions, classes and route decorators.
func (p *pyExtractor) walkDecls(fc *fileCtx, n *sitter.Node, scope string) {
	if n == nil {
		return
	}
	switch n.Type() {
	case "decorated_definition":
		def := n.ChildByFieldName("definition")
		if def == nil {
			for i := 0; i < int(n.NamedChildCount()); i++ {
				c := n.NamedChild(i)
				if c.Type() == "function_definition" || c.Type() == "class_definition" {
					def = c
				}
			}
		}
		if def != nil {
			idx := p.addDefinition(fc, def, scope)
			if idx >= 0 && def.Type() == "function_definition" {
				p.applyDecorators(fc, n, idx)
			}
			if idx >= 0 && def.Type() == "class_definition" {
				p.recordModel(fc, def, idx)
			}
			p.walkInto(fc, def, scope)
		}
		return
	case "function_definition", "class_definition":
		idx := p.addDefinition(fc, n, scope)
		if idx >= 0 && n.Type() == "class_definition" {
			p.recordModel(fc, n, idx)
		}
		p.walkInto(fc, n, scope)
		return
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		p.walkDecls(fc, n.NamedChild(i), scope)
	}
}

// walkInto descends into a definition body with an extended scope.
func (p *pyExtractor) walkInto(fc *fileCtx, def *sitter.Node, scope string) {
	name := fc.text(def.ChildByFieldName("name"))
	inner := scope
	svc := p.rpcSvc
	if def.Type() == "class_definition" && name != "" {
		inner = p.qualify(scope, name)
		p.rpcSvc = p.recordServicer(fc, def, name)
	}
	if body := def.ChildByFieldName("body"); body != nil {
		for i := 0; i < int(body.NamedChildCount()); i++ {
			p.walkDecls(fc, body.NamedChild(i), inner)
		}
	}
	p.rpcSvc = svc
}

// recordServicer returns the proto service a class serves, from a
// `<pb2_grpc>.<X>Servicer` base or from a base class in this file that is
// itself a servicer. Boutique's email service puts the RPC methods two levels
// down — DummyEmailService(BaseEmailService(demo_pb2_grpc.EmailServiceServicer))
// — so a rule reading only the direct bases finds no implementation at all.
func (p *pyExtractor) recordServicer(fc *fileCtx, def *sitter.Node, name string) string {
	bases := firstChildOfType(def, "argument_list")
	if bases == nil {
		return ""
	}
	svc := ""
	for i := 0; i < int(bases.NamedChildCount()); i++ {
		base := lastComponent(strings.TrimSpace(fc.text(bases.NamedChild(i))))
		if stem, ok := strings.CutSuffix(base, "Servicer"); ok && stem != "" {
			svc = stem
			break
		}
		if inherited, ok := p.servicers[base]; ok {
			svc = inherited
			break
		}
	}
	if svc != "" {
		p.servicers[name] = svc
	}
	return svc
}

// pyIsRPCImpl reports whether a servicer method is one of the generated rpc
// handlers: protoc declares every one of them taking the request and the call
// context, and nothing else in a servicer class has that shape.
func pyIsRPCImpl(name, sig string) bool {
	if name == "" || name[0] < 'A' || name[0] > 'Z' {
		return false
	}
	return strings.Contains(sig, "request") && strings.Contains(sig, "context")
}

func (p *pyExtractor) addDefinition(fc *fileCtx, def *sitter.Node, scope string) int {
	name := fc.text(def.ChildByFieldName("name"))
	if name == "" {
		return -1
	}
	switch def.Type() {
	case "function_definition":
		kind := "function"
		if scope != "" {
			kind = "method"
		}
		sig := fc.text(def.ChildByFieldName("parameters"))
		doc := extractLineComments(string(fc.src), def, "#")
		idx := fc.addUnit(def, kind, name, p.qualify(scope, name), sig, doc)
		if p.rpcSvc != "" && pyIsRPCImpl(name, sig) {
			fc.addEdge(idx, store.EdgeImplementsRPC, grpcKey(p.rpcSvc, name),
				fc.units[idx].StartLine, contract.ConfCrossFile, nil)
		}
		return idx
	case "class_definition":
		doc := extractLineComments(string(fc.src), def, "#")
		return fc.addUnit(def, "class", name, p.qualify(scope, name), "", doc)
	}
	return -1
}

// recordModel maps a SQLAlchemy declarative model to its table: the explicit
// __tablename__ when present, otherwise the default naming of the class name
// for classes that declare Column attributes.
func (p *pyExtractor) recordModel(fc *fileCtx, def *sitter.Node, idx int) {
	name := fc.text(def.ChildByFieldName("name"))
	body := def.ChildByFieldName("body")
	if name == "" || body == nil {
		return
	}
	text := fc.text(body)
	tbl := ""
	if i := strings.Index(text, "__tablename__"); i >= 0 {
		rest := text[i+len("__tablename__"):]
		if j := strings.IndexByte(rest, '='); j >= 0 {
			line := rest[j+1:]
			if k := strings.IndexByte(line, '\n'); k >= 0 {
				line = line[:k]
			}
			if v, ok := unquote(strings.TrimSpace(line)); ok {
				tbl = v
			}
		}
	}
	if tbl == "" {
		if !strings.Contains(text, "Column(") && !strings.Contains(text, "mapped_column(") {
			return
		}
		tbl = tableName(name)
	}
	if tbl == "" {
		return
	}
	p.models[name] = tbl
	fc.addUnit(def, store.KindDBTable, tbl, contract.DB(tbl), "entity:"+name, fc.units[idx].Doc)
}

// pyNonTaskReceivers are the receivers a celery-style dispatch is never
// called on: a pronoun, or the worker pool that shares apply_async's name
// with celery. The task's name is the receiver's, so these name nothing.
var pyNonTaskReceivers = map[string]bool{
	"self": true, "cls": true, "super": true, "this": true,
	"pool": true, "executor": true, "loop": true,
}

// pyConsumerDecorators are decorators that bind a function to a message
// stream. The topic/queue is the decorator's first string argument, or the
// faust topic object it names.
var pyConsumerDecorators = map[string]bool{
	"agent": true, "on_event": true, "subscribe": true, "task": true, "shared_task": true,
}

// applyDecorators turns route decorators into http_route units and consumer
// decorators into consumes edges for the function at unit index fnIdx.
func (p *pyExtractor) applyDecorators(fc *fileCtx, decorated *sitter.Node, fnIdx int) {
	fn := fc.units[fnIdx]
	for i := 0; i < int(decorated.NamedChildCount()); i++ {
		d := decorated.NamedChild(i)
		if d.Type() != "decorator" {
			continue
		}
		call := firstNamedOfType(d, "call")
		if call == nil {
			// Bare decorator: @shared_task / @app.task — the celery task name
			// defaults to the function name and is the join key producers use.
			//
			// Only where there is a celery to dispatch through, though. Airflow
			// spells its DAG nodes `@task` too, and reading those as queue
			// consumers put 1093 consumes edges into one repository — keyed on
			// the function's own name, joined to no producer anywhere, one per
			// example DAG. A bare decorator names nothing but the function; the
			// broker is the only thing that makes it a destination.
			if bare := lastComponent(strings.TrimPrefix(fc.text(d), "@")); pyConsumerDecorators[bare] && fc.hasBroker() {
				fc.addEdge(fnIdx, store.EdgeConsumes, topicKey(fn.Name), fn.StartLine, contract.ConfHeuristic,
					&store.EdgeMeta{Topic: fn.Name})
			}
			continue
		}
		callee := fc.text(call.ChildByFieldName("function"))
		name := lastComponent(callee)
		object := ""
		if k := strings.LastIndex(callee, "."); k > 0 {
			object = callee[:k]
		}
		args, kwargs := pyCallArgs(fc, call)

		// @app.get("/p") / @router.post("/p"), prefixed by the router's mount.
		if m, ok := pyHTTPMethods[name]; ok && len(args) > 0 {
			if path, isStr := p.resolveString(args[0]); isStr && strings.HasPrefix(path, "/") {
				path = joinPath(p.prefixes[lastComponent(object)], path)
				ridx := fc.addUnit(call, store.KindHTTPRoute, m+" "+path, routeKey(m, path), "path:"+path, "")
				fc.addEdge(ridx, store.EdgeHandledBy, fn.Name, fn.StartLine, contract.ConfHigh, nil)
			}
			continue
		}
		// @app.route("/p", methods=["POST"]) / @bp.route("/p")
		if name == "route" && len(args) > 0 {
			if path, isStr := p.resolveString(args[0]); isStr && strings.HasPrefix(path, "/") {
				method := "ANY"
				if mv, ok := kwargs["methods"]; ok {
					for _, s := range pyStringsIn(mv, p.consts) {
						method = strings.ToUpper(s)
						break
					}
				}
				path = joinPath(p.prefixes[lastComponent(object)], path)
				ridx := fc.addUnit(call, store.KindHTTPRoute, method+" "+path, routeKey(method, path), "path:"+path, "")
				fc.addEdge(ridx, store.EdgeHandledBy, fn.Name, fn.StartLine, contract.ConfHigh, nil)
			}
			continue
		}
		// Stream consumers: faust @app.agent(orders_topic), celery
		// @app.task(name="orders.process"), @consumer.subscribe("topic").
		if pyConsumerDecorators[name] {
			topic := ""
			if len(args) > 0 {
				if v, ok := p.resolveString(args[0]); ok {
					topic = v
				} else if v, ok := p.topics[lastComponent(strings.TrimSpace(args[0]))]; ok {
					topic = v
				}
			}
			if v, ok := p.resolveString(kwargs["name"]); ok {
				topic = v
			}
			if topic == "" {
				// The decorator named no destination, so the only name left is
				// the function's own — and that is a queue key only where
				// there is a queue. @task(retries=3) is a DAG node in every
				// airflow file that has no celery in it (473 edges), and
				// @task in a locust file is a load-generator step (3).
				if !fc.hasBroker() {
					continue
				}
				topic = fn.Name
			}
			key, tn := topicEdgeKey(topic)
			fc.addEdge(fnIdx, store.EdgeConsumes, key, fn.StartLine, contract.ConfHeuristic,
				&store.EdgeMeta{Topic: tn})
			continue
		}
	}
}

func (p *pyExtractor) walkCalls(fc *fileCtx, n *sitter.Node) {
	if n == nil {
		return
	}
	if n.Type() == "call" {
		p.handleCall(fc, n)
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		p.walkCalls(fc, n.NamedChild(i))
	}
}

func (p *pyExtractor) handleCall(fc *fileCtx, call *sitter.Node) {
	callee := fc.text(call.ChildByFieldName("function"))
	name := lastComponent(callee)
	object := ""
	if i := strings.LastIndex(callee, "."); i > 0 {
		object = callee[:i]
	}
	args, kwargs := pyCallArgs(fc, call)
	line := int(call.StartPoint().Row) + 1
	pos := int(call.StartByte())
	src := fc.enclosingUnit(pos, callableKinds...)
	if src < 0 {
		return
	}

	// Body/dict fields for the detectors: producers carry the payload as a
	// dict literal in the call, HTTP clients as json=/data=/params= kwargs.
	var fields map[string]string
	if pyProduceMethods[name] {
		fields = pyDictFields(fc, call, p.consts)
	} else if _, isHTTP := pyHTTPMethods[name]; isHTTP {
		fields = pyHTTPKwargFields(kwargs, p.consts)
	}

	// Declarative framework detection: kafka produce/consume, HTTP clients.
	aliases := p.aliases.relevant(pos, args, fields)

	// gRPC stub constructed in the call itself:
	// pb2_grpc.OrderServiceStub(channel).CreateOrder(req).
	if svc := inlineStubService(object, fc.hasGRPC()); svc != "" && name != "" {
		fc.addEdge(src, store.EdgeRPCCall, grpcKey(svc, capitalizeFirst(name)), line, contract.ConfHigh,
			&store.EdgeMeta{Args: args, Fields: fields, Aliases: aliases})
		fc.contractSite(domain.ContractKindRPC, true)
		return
	}

	cs := &callSite{Callee: callee, Name: name, Object: object, Args: args,
		Kwargs: kwargs, Fields: fields, Aliases: aliases, Line: line, Src: src}
	list := func(expr string) []string { return pyStringsIn(expr, p.consts) }
	detectors, clientObject := pyDetectorsFor(fc)
	if runDetectors(fc, detectors, cs, p.resolveString, list) {
		return
	}

	// requests.request("POST", url, ...) — the method is the first argument.
	// Handled outside the rule table because URLArg moves to position 1.
	if name == "request" && len(args) >= 2 && clientObject.matches(strings.ToLower(object)) {
		u, conf, ok := resolveRequestURL(args[1], p.resolveString, contract.ConfCrossFile, true)
		fc.contractSite(domain.ContractKindHTTP, ok)
		if ok {
			m := httpMethodFromArg(args[0], p.resolveString)
			host, path := splitURL(u)
			fc.addEdge(src, store.EdgeHTTPCall, routeKey(m, path), line, conf,
				&store.EdgeMeta{Method: m, Path: path, Host: host, Args: args,
					Fields: pyHTTPKwargFields(kwargs, p.consts), Aliases: aliases})
			return
		}
		fc.httpCandidate(src, args[1], args[0])
	}

	// Celery task dispatch: process_order.delay(x) / .apply_async(...). The
	// task name is the join key its @task-decorated definition consumes on,
	// and it is the receiver's own name — so a receiver that cannot be a task
	// names no destination. `pool.apply_async(...)` is a multiprocessing pool
	// (20 sites in airflow's dev tooling, keyed topic:pool) and
	// `self.delay(seconds)` is a sleep (6, keyed topic:self).
	if (name == "delay" || name == "apply_async") && !pyNonTaskReceivers[lastComponent(object)] {
		task := lastComponent(object)
		fc.contractSite(domain.ContractKindMessaging, task != "")
		if task != "" {
			fc.addEdge(src, store.EdgeProduces, topicKey(task), line, contract.ConfHeuristic,
				&store.EdgeMeta{Topic: task, Args: args, Aliases: aliases})
			return
		}
	}

	// SQLAlchemy session access: session.query(Order) / session.add(order).
	if tbl := p.sqlAlchemyTable(name, args); tbl != "" {
		fc.contractSite(domain.ContractKindDB, true)
		kind := store.EdgeReadsFrom
		if pySessionWriteMethods[name] {
			kind = store.EdgeWritesTo
		}
		fc.addEdge(src, kind, contract.DB(tbl), line, contract.ConfHeuristic,
			&store.EdgeMeta{Args: args, Aliases: aliases})
	}

	sqlEdgesFromArgs(fc, src, line, args)
	if name != "" {
		fc.addEdge(src, store.EdgeCall, name, line, contract.ConfHeuristic,
			&store.EdgeMeta{Args: args, Aliases: aliases, Receiver: object})
	}
}

// pySessionWriteMethods are the SQLAlchemy session methods that stage row
// mutations; query/get are reads.
var pySessionWriteMethods = map[string]bool{
	"add": true, "add_all": true, "merge": true, "delete": true, "bulk_save_objects": true,
}

var pySessionReadMethods = map[string]bool{"query": true, "get": true}

// sqlAlchemyTable resolves the table a session call touches from the model
// class or instance it is given. Only models declared in this file resolve.
func (p *pyExtractor) sqlAlchemyTable(name string, args []string) string {
	if len(args) == 0 || (!pySessionWriteMethods[name] && !pySessionReadMethods[name]) {
		return ""
	}
	arg := strings.TrimSpace(args[0])
	if i := strings.IndexAny(arg, "(."); i > 0 {
		arg = arg[:i]
	}
	if tbl, ok := p.models[arg]; ok {
		return tbl
	}
	return ""
}

// pyHTTPKwargFields merges dict pairs from the json=/data=/params= keyword
// arguments of an HTTP client call into a single wire-field map.
func pyHTTPKwargFields(kwargs map[string]string, consts map[string]string) map[string]string {
	fields := map[string]string{}
	for _, key := range []string{"json", "data", "params"} {
		if v, ok := kwargs[key]; ok {
			for fk, fv := range pyDictPairs(v, consts) {
				fields[fk] = fv
			}
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func (p *pyExtractor) resolveString(expr string) (string, bool) {
	expr = strings.TrimSpace(expr)
	// String literal (f-prefix stripped): reject only literals with
	// substitutions. Const values are returned as-is — they may legitimately
	// contain "${REF}" config references for the linker.
	if v, ok := unquote(strings.TrimPrefix(expr, "f")); ok {
		if strings.Contains(v, "{") {
			return "", false // f-string substitution — not a constant
		}
		return v, true
	}
	if v, ok := p.consts[expr]; ok {
		return v, true
	}
	// self.X / cls.X: a class attribute holding a literal is collected under
	// its bare name, which is how it is spelled at its assignment. Robot
	// Shop's publisher keeps its exchange and routing key this way, so the
	// call site names nothing a bare lookup can find.
	for _, prefix := range []string{"self.", "cls."} {
		if attr, found := strings.CutPrefix(expr, prefix); found {
			if v, ok := p.consts[attr]; ok {
				return v, true
			}
		}
	}
	// os.getenv("X") / os.environ["X"] -> "${X}" config reference for the linker
	for _, prefix := range []string{"os.getenv(", "os.environ.get(", "os.environ["} {
		if rest, found := strings.CutPrefix(expr, prefix); found {
			end := strings.IndexAny(rest, ")],")
			if end > 0 {
				if lit, ok := unquote(rest[:end]); ok {
					return "${" + lit + "}", true
				}
			}
		}
	}
	return "", false
}

func (p *pyExtractor) qualify(scope, name string) string {
	if scope != "" {
		return scope + "." + name
	}
	return name
}

// pyCallArgs returns positional argument texts and keyword arguments of a call.
// Kept language-specific because of kwargs; positional/keyword nodes come from
// the shared argNodes helper.
func pyCallArgs(fc *fileCtx, call *sitter.Node) ([]string, map[string]string) {
	nodes := argNodes(call, "arguments")
	if nodes == nil {
		return nil, nil
	}
	var args []string
	kwargs := map[string]string{}
	for _, c := range nodes {
		if c.Type() == "keyword_argument" {
			kwargs[fc.text(c.ChildByFieldName("name"))] = fc.text(c.ChildByFieldName("value"))
			continue
		}
		args = append(args, fc.text(c))
	}
	return args, kwargs
}

// pyDictFields collects dict-literal pairs from any argument of a call.
func pyDictFields(fc *fileCtx, call *sitter.Node, consts map[string]string) map[string]string {
	fields := map[string]string{}
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if n.Type() == "pair" {
			key := fc.text(n.ChildByFieldName("key"))
			if v, ok := unquote(key); ok {
				key = v
			}
			fields[key] = fc.text(n.ChildByFieldName("value"))
		}
		for i := 0; i < int(n.NamedChildCount()); i++ {
			walk(n.NamedChild(i))
		}
	}
	if argsNode := call.ChildByFieldName("arguments"); argsNode != nil {
		walk(argsNode)
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// pyDictPairs parses a dict-literal expression text into key -> value pairs.
// Best effort: works for flat literals like {"user_id": user_id, "amount": amount}.
func pyDictPairs(expr string, consts map[string]string) map[string]string {
	expr = strings.TrimSpace(expr)
	if !strings.HasPrefix(expr, "{") || !strings.HasSuffix(expr, "}") {
		return nil
	}
	out := map[string]string{}
	for _, part := range contract.SplitTopLevel(expr[1:len(expr)-1], ',') {
		kv := contract.SplitTopLevel(part, ':')
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		if v, ok := unquote(key); ok {
			key = v
		}
		out[key] = strings.TrimSpace(kv[1])
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// pyStringsIn extracts string literals (or known consts) from an expression
// like '"a"' or '["a", "b"]' or '("a",)'.
func pyStringsIn(expr string, consts map[string]string) []string {
	expr = strings.Trim(strings.TrimSpace(expr), "[]()")
	var out []string
	for _, part := range strings.Split(expr, ",") {
		part = strings.TrimSpace(part)
		if v, ok := unquote(part); ok {
			out = append(out, v)
		} else if v, ok := consts[part]; ok {
			out = append(out, v)
		}
	}
	return out
}

func firstNamedOfType(n *sitter.Node, typ string) *sitter.Node {
	for i := 0; i < int(n.NamedChildCount()); i++ {
		if c := n.NamedChild(i); c != nil && c.Type() == typ {
			return c
		}
	}
	return nil
}
