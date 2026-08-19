package ast

import (
	"regexp"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"

	sitter "github.com/smacker/go-tree-sitter"
)

// tsExtractor extracts units and edges from TypeScript/JavaScript source.
//
// Framework heuristics:
//   - express/koa-style routes: app.get('/p', handler), router.post(...)
//   - HTTP clients: axios.get/post/..., fetch(url, {method})
//   - kafkajs: producer.send({topic, messages}), consumer.subscribe({topic(s)})
//   - gRPC clients (grpc-js/nice-grpc/connect style): new XxxServiceClient(...)
type tsExtractor struct {
	consts   constResolver
	aliases  aliasTable        // local aliases (const x = userId), scoped per function
	clients  map[string]string // var name -> gRPC service name (client bindings), file scope
	types    map[string]string // property/parameter name -> declared type
	prefixes map[string]string // router var -> mount prefix (app.use('/api', router))
	queues   map[string]string // bullmq queue/worker var -> queue name
	// objects maps a variable to the object literal it was initialized with,
	// so a request described by an options object still has its fields at the
	// call site that passes the variable on (const options = {...}; request(options)).
	objects map[string]map[string]string
}

var tsHTTPMethods = map[string]string{
	"get": "GET", "post": "POST", "put": "PUT", "delete": "DELETE",
	"patch": "PATCH", "head": "HEAD", "options": "OPTIONS", "all": "ANY",
}

// tsDetectors are the declarative framework rules for TS/JS (see frameworks.go).
var tsDetectors = detectorSet{
	HTTP: []httpClientRule{
		// axios.post(url, body) / httpClient.get(url)
		{
			Object:  objectMatch{Contains: []string{"axios", "http"}},
			Methods: tsHTTPMethods,
			URLArg:  0,
			Conf:    contract.ConfCrossFile,
		},
		// ky.post(url) / got.post(url) / superagent.post(url)
		{
			Object:  objectMatch{Exact: []string{"ky", "got", "superagent"}},
			Methods: tsHTTPMethods,
			URLArg:  0,
			Conf:    contract.ConfCrossFile,
		},
		// fetch(url, {method: 'POST', body: ...}) — defaults to GET
		{
			Methods:         map[string]string{"fetch": "GET"},
			URLArg:          0,
			MethodFromField: "method",
			Conf:            contract.ConfCrossFile,
		},
		// The request described by an options object rather than by argument
		// positions: axios({url, method}), this.helpers.httpRequest({method,
		// url, baseURL}), request({uri}). n8n makes thousands of calls this
		// way — every one of them invisible to a rule that reads a position.
		{
			Object: objectMatch{
				Exact:    []string{""},
				Contains: []string{"axios", "http", "helpers", "client"},
				Fold:     true,
			},
			Methods: map[string]string{
				"axios": "GET", "request": "GET", "httpRequest": "GET",
				"requestWithAuthentication": "GET", "httpRequestWithAuthentication": "GET",
				"requestOAuth1": "GET", "requestOAuth2": "GET",
			},
			URLArg:          0,
			URLFromFields:   true,
			MethodFromField: "method",
			Conf:            contract.ConfCrossFile,
		},
	},
	Produce: append([]kafkaProduceRule{
		// kafkajs: producer.send({topic: 'x', messages: [...]})
		{Methods: []string{"send"}, TopicFromField: "topic", OmitArgs: true, Loose: true, Conf: contract.ConfHigh},
	}, tsBrokerProduce...),
	Consume: append([]kafkaConsumeRule{
		// kafkajs: consumer.subscribe({topic: 'x'} / {topics: ['x', 'y']})
		{Methods: []string{"subscribe"}, TopicsFromFields: []string{"topic", "topics"}, Loose: true, Conf: contract.ConfHigh},
	}, tsBrokerConsume...),
	Broker: true,
}

func (t *tsExtractor) extract(fc *fileCtx, root *sitter.Node) {
	t.consts = constResolver{}
	t.aliases = aliasTable{}
	t.clients = map[string]string{}
	t.types = map[string]string{}
	t.prefixes = map[string]string{}
	t.queues = map[string]string{}
	t.objects = map[string]map[string]string{}
	t.collectConsts(fc, root)
	t.collectMounts(fc, root)
	t.walkDecls(fc, root, "")
	t.walkCalls(fc, root)
}

// collectMounts records express sub-router mounts, app.use('/api', router), so
// routes registered on the sub-router are stored with the full path.
func (t *tsExtractor) collectMounts(fc *fileCtx, n *sitter.Node) {
	if n == nil {
		return
	}
	if n.Type() == "call_expression" {
		fn := n.ChildByFieldName("function")
		if fn != nil && lastComponent(fc.text(fn)) == "use" {
			nodes := argNodes(n, "arguments")
			if len(nodes) >= 2 {
				if prefix, ok := t.resolveString(fc.text(nodes[0])); ok && strings.HasPrefix(prefix, "/") {
					for _, a := range nodes[1:] {
						if a.Type() == "identifier" {
							name := fc.text(a)
							t.prefixes[name] = joinPath(prefix, t.prefixes[name])
						}
					}
				}
			}
		}
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		t.collectMounts(fc, n.NamedChild(i))
	}
}

func (t *tsExtractor) collectConsts(fc *fileCtx, n *sitter.Node) {
	if n == nil {
		return
	}
	if n.Type() == "variable_declarator" {
		name := fc.text(n.ChildByFieldName("name"))
		value := n.ChildByFieldName("value")
		if v, ok := unquote(fc.text(value)); ok && name != "" {
			t.consts[name] = v
		} else if name != "" && value != nil {
			// gRPC client binding: const client = new OrderServiceClient(...).
			// bullmq binding: new Queue('orders') / new Worker('orders', fn).
			if value.Type() == "new_expression" {
				ctor := value.ChildByFieldName("constructor")
				if ctor == nil {
					ctor = value.NamedChild(0)
				}
				cn := lastComponent(fc.text(ctor))
				if svc := grpcServiceFromType(cn); svc != "" {
					t.clients[name] = svc
				}
				if cn == "Queue" || cn == "Worker" || cn == "QueueScheduler" {
					if a := argNodes(value, "arguments"); len(a) > 0 {
						if q, ok := t.resolveString(fc.text(a[0])); ok {
							t.queues[name] = q
						}
					}
				}
			}
			// Local alias: const x = userId; / const x = body.userId;
			// A call RHS (const x = extractUserId(req)) is recorded with its
			// full text so token matching links x to the call's arguments.
			switch value.Type() {
			case "identifier", "member_expression", "call_expression":
				t.aliases.record(n, name, fc.text(value))
				// ORM repository binding: const repo = ds.getRepository(Order).
				// Recorded as the declared type the injected form carries, so
				// both spellings resolve through one lookup.
				if e := tsRepositoryEntity(fc.text(value), ""); e != "" {
					t.types[name] = "Repository<" + e + ">"
				}
			case "object":
				if f := tsObjectFields(fc, value); f != nil {
					t.objects[name] = f
				}
			}
		}
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		t.collectConsts(fc, n.NamedChild(i))
	}
}

func (t *tsExtractor) walkDecls(fc *fileCtx, n *sitter.Node, scope string) {
	t.walkDeclsIn(fc, n, scope, "")
}

func (t *tsExtractor) walkDeclsIn(fc *fileCtx, n *sitter.Node, scope, basePath string) {
	if n == nil {
		return
	}
	switch n.Type() {
	case "function_declaration", "generator_function_declaration":
		name := fc.text(n.ChildByFieldName("name"))
		if name != "" {
			sig := fc.text(n.ChildByFieldName("parameters"))
			doc := extractLineComments(string(fc.src), n, "//")
			fc.addUnit(n, "function", name, t.qualify(scope, name), sig, doc)
		}
	case "class_declaration":
		name := fc.text(n.ChildByFieldName("name"))
		if name != "" {
			doc := extractLineComments(string(fc.src), n, "//")
			fc.addUnit(n, "class", name, t.qualify(scope, name), "", doc)
			// NestJS @Controller('orders') and TypeORM @Entity('orders').
			classBase := basePath
			for _, d := range tsDecorators(fc, n) {
				switch d.name {
				case "Controller":
					classBase = joinPath(basePath, d.arg(0))
				case "Entity":
					// The declared name is published against the entity, so a
					// data-access edge keyed on the entity's derived name can
					// still reach it (see tableCandidates in internal/graph).
					if tbl := tsEntityTable(d, name); tbl != "" {
						fc.addUnit(n, store.KindDBTable, tbl, contract.DB(tbl), "entity:"+name, doc)
					}
				}
			}
			if body := n.ChildByFieldName("body"); body != nil {
				for i := 0; i < int(body.NamedChildCount()); i++ {
					t.walkDeclsIn(fc, body.NamedChild(i), t.qualify(scope, name), classBase)
				}
			}
			return
		}
	case "method_definition":
		name := fc.text(n.ChildByFieldName("name"))
		if name != "" && name != "constructor" {
			sig := fc.text(n.ChildByFieldName("parameters"))
			idx := fc.addUnit(n, "method", name, t.qualify(scope, name), sig, "")
			t.applyMethodDecorators(fc, n, idx, basePath)
		}
		if name == "constructor" {
			// Constructor-injected dependencies carry their type in the
			// parameter list: constructor(private orders: OrderServiceClient).
			t.recordParamTypes(fc, n.ChildByFieldName("parameters"))
		}
	case "public_field_definition":
		t.recordFieldType(fc, n)
	case "variable_declarator":
		// const f = (a, b) => {...} / const f = async function() {...}
		name := fc.text(n.ChildByFieldName("name"))
		value := n.ChildByFieldName("value")
		if name != "" && value != nil {
			vt := value.Type()
			if vt == "arrow_function" || vt == "function" || vt == "function_expression" {
				sig := fc.text(value.ChildByFieldName("parameters"))
				doc := extractLineComments(string(fc.src), n, "//")
				fc.addUnit(n, "function", name, t.qualify(scope, name), sig, doc)
			}
		}
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		t.walkDeclsIn(fc, n.NamedChild(i), scope, basePath)
	}
}

// tsDecorator is a parsed decorator with its literal string arguments.
type tsDecorator struct {
	name string
	args []string
	// raws are the argument texts as written, which is the only form an
	// options object survives in: unquoting one yields "".
	raws []string
}

// arg returns the i-th decorator argument as a string literal, or "".
func (d tsDecorator) arg(i int) string {
	if i < len(d.args) {
		return d.args[i]
	}
	return ""
}

// rawArg returns the i-th decorator argument as written, or "".
func (d tsDecorator) rawArg(i int) string {
	if i < len(d.raws) {
		return d.raws[i]
	}
	return ""
}

// reTSEntityTable extracts the table an ORM entity decorator names in its
// options object. TypeORM spells the key `name`, MikroORM `tableName`, and
// medusa's own entities are written the MikroORM way.
var reTSEntityTable = regexp.MustCompile("\\b(?:tableName|name)\\s*:\\s*[\"'`]([^\"'`]+)")

// tsEntityTable returns the table an @Entity decorator declares — positional
// as in TypeORM's @Entity('orders'), or in the options object of either ORM —
// falling back to the name derived from the class for a decorator that names
// no table.
func tsEntityTable(d tsDecorator, class string) string {
	name := d.arg(0)
	if name == "" {
		if m := reTSEntityTable.FindStringSubmatch(d.rawArg(0)); m != nil {
			name = m[1]
		}
	}
	if name == "" {
		return tableName(class)
	}
	return sqlTableName(name)
}

// tsDecoratorSkip are the tokens that may sit between a decorator and the
// declaration it annotates ("@Controller('x') export class C").
var tsDecoratorSkip = map[string]bool{
	"export": true, "default": true, "abstract": true, "declare": true,
	"comment": true, "async": true, "static": true, "readonly": true,
	"accessibility_modifier": true, "override_modifier": true,
}

// tsDecorators returns the decorators attached to a declaration, with their
// string-literal arguments already unquoted. In tree-sitter-typescript a
// decorator is a preceding sibling of the declaration, not a child of it.
func tsDecorators(fc *fileCtx, decl *sitter.Node) []tsDecorator {
	var out []tsDecorator
	add := func(d *sitter.Node) {
		body := d.NamedChild(0)
		if body == nil {
			return
		}
		dec := tsDecorator{name: lastComponent(fc.text(body))}
		if body.Type() == "call_expression" {
			dec.name = lastComponent(fc.text(body.ChildByFieldName("function")))
			for _, a := range argNodes(body, "arguments") {
				raw := fc.text(a)
				v, _ := unquote(raw)
				dec.args = append(dec.args, v)
				dec.raws = append(dec.raws, raw)
			}
		}
		out = append(out, dec)
	}
	for i := 0; i < int(decl.ChildCount()); i++ {
		if d := decl.Child(i); d != nil && d.Type() == "decorator" {
			add(d)
		}
	}
	for p := decl.PrevSibling(); p != nil; p = p.PrevSibling() {
		if p.Type() == "decorator" {
			add(p)
			continue
		}
		if !tsDecoratorSkip[p.Type()] {
			break
		}
	}
	return out
}

// applyMethodDecorators turns NestJS routing and messaging decorators into
// http_route units and consumes edges for the method at unit index idx.
func (t *tsExtractor) applyMethodDecorators(fc *fileCtx, n *sitter.Node, idx int, basePath string) {
	u := fc.units[idx]
	for _, d := range tsDecorators(fc, n) {
		if m, ok := tsHTTPMethods[strings.ToLower(d.name)]; ok && d.name != strings.ToLower(d.name) {
			// @Get(), @Post('bulk'), @Get(':id') — the sub-path may be empty.
			path := joinPath(basePath, d.arg(0))
			ridx := fc.addUnit(n, store.KindHTTPRoute, m+" "+path, routeKey(m, path), "path:"+path, "")
			fc.addEdge(ridx, store.EdgeHandledBy, u.Name, u.StartLine, contract.ConfHigh, nil)
			continue
		}
		switch d.name {
		case "EventPattern", "MessagePattern", "OnEvent", "Process", "SubscribeMessage":
			topic := d.arg(0)
			if topic == "" {
				continue
			}
			key, name := topicEdgeKey(topic)
			fc.addEdge(idx, store.EdgeConsumes, key, u.StartLine, contract.ConfHigh,
				&store.EdgeMeta{Topic: name})
		}
	}
}

// recordParamTypes maps constructor parameter names to their declared types.
func (t *tsExtractor) recordParamTypes(fc *fileCtx, params *sitter.Node) {
	if params == nil {
		return
	}
	for i := 0; i < int(params.NamedChildCount()); i++ {
		p := params.NamedChild(i)
		pat := p.ChildByFieldName("pattern")
		typ := p.ChildByFieldName("type")
		if pat == nil || typ == nil {
			// required_parameter nested inside an accessibility modifier
			pat, typ = tsFirstOfType(p, "identifier"), tsFirstOfType(p, "type_annotation")
		}
		if pat == nil || typ == nil {
			continue
		}
		t.types[fc.text(pat)] = strings.TrimPrefix(strings.TrimSpace(fc.text(typ)), ":")
	}
}

// recordFieldType maps a class property name to its declared type.
func (t *tsExtractor) recordFieldType(fc *fileCtx, n *sitter.Node) {
	name := fc.text(n.ChildByFieldName("name"))
	typ := fc.text(n.ChildByFieldName("type"))
	if name != "" && typ != "" {
		t.types[name] = strings.TrimPrefix(strings.TrimSpace(typ), ":")
	}
}

// tsFirstOfType returns the first named descendant of the given type.
func tsFirstOfType(n *sitter.Node, typ string) *sitter.Node {
	if n == nil {
		return nil
	}
	if n.Type() == typ {
		return n
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		if r := tsFirstOfType(n.NamedChild(i), typ); r != nil {
			return r
		}
	}
	return nil
}

func (t *tsExtractor) walkCalls(fc *fileCtx, n *sitter.Node) {
	if n == nil {
		return
	}
	switch n.Type() {
	case "call_expression":
		t.handleCall(fc, n)
	case "new_expression":
		t.handleNew(fc, n)
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		t.walkCalls(fc, n.NamedChild(i))
	}
}

// handleNew emits the consumer side of bullmq: new Worker('orders', handler)
// subscribes the enclosing unit to the queue.
func (t *tsExtractor) handleNew(fc *fileCtx, n *sitter.Node) {
	ctor := n.ChildByFieldName("constructor")
	if ctor == nil {
		ctor = n.NamedChild(0)
	}
	if lastComponent(fc.text(ctor)) != "Worker" {
		return
	}
	nodes := argNodes(n, "arguments")
	if len(nodes) == 0 {
		return
	}
	q, ok := t.resolveString(fc.text(nodes[0]))
	if !ok {
		return
	}
	src := fc.enclosingUnit(int(n.StartByte()), callableKinds...)
	if src < 0 {
		return
	}
	key, topic := topicEdgeKey(q)
	fc.addEdge(src, store.EdgeConsumes, key, int(n.StartPoint().Row)+1, contract.ConfHigh,
		&store.EdgeMeta{Topic: topic})
}

func (t *tsExtractor) handleCall(fc *fileCtx, call *sitter.Node) {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return
	}
	fnText := fc.text(fn)
	name := lastComponent(fnText)
	object := ""
	if i := strings.LastIndex(fnText, "."); i > 0 {
		object = fnText[:i]
	}
	args := namedChildTexts(fc, call.ChildByFieldName("arguments"))
	nodes := argNodes(call, "arguments")
	line := int(call.StartPoint().Row) + 1
	pos := int(call.StartByte())

	// helper.call(this, 'GET', '/x') is how a module-level helper is invoked
	// with the node's context; it is the dominant call shape in n8n. Unwrap it
	// to the helper, keeping the `this` argument, so argument positions still
	// line up with the helper's declared parameter list.
	if name == "call" && object != "" && len(args) > 0 {
		fnText, object = object, ""
		name = lastComponent(fnText)
		if i := strings.LastIndex(fnText, "."); i > 0 {
			object = fnText[:i]
		}
	}

	// Route registration: app.get('/path', handler). HTTP client objects also
	// expose get/post, and `this.http.get('/api/users', {headers})` has the
	// same arity as a route — so the trailing argument must look like a
	// handler (a function, a reference to one, or a middleware factory call)
	// and the receiver must not be a known client.
	if m, ok := tsHTTPMethods[name]; ok && len(nodes) >= 2 && object != "" &&
		!tsClientObjects.matches(object) && tsHandlerLike(nodes[len(nodes)-1]) {
		if path, isStr := t.resolveString(args[0]); isStr && strings.HasPrefix(path, "/") {
			path = joinPath(t.prefixes[lastComponent(object)], path)
			idx := fc.addUnit(call, store.KindHTTPRoute, m+" "+path, routeKey(m, path), "path:"+path, "")
			handler := args[len(args)-1]
			if isTSIdentifier(handler) {
				fc.addEdge(idx, store.EdgeHandledBy, handler, line, contract.ConfHigh, nil)
			}
			return
		}
	}

	// gRPC server registration: server.addService(proto.CurrencyService.service,
	// {getSupportedCurrencies, convert}). Handled before the enclosing-unit
	// check because the edges it emits come from the handler functions, which
	// are module-level, not from the function holding the registration.
	if name == "addService" && len(nodes) >= 2 {
		if t.markRPCHandlers(fc, args[0], nodes[1], line) {
			return
		}
	}

	src := fc.enclosingUnit(pos, callableKinds...)
	if src < 0 {
		return
	}

	// gRPC client call on a tracked client: client.createOrder({...}, cb), or
	// on one constructed in the call itself. Stub method names are lowerCamel;
	// capitalize to match the PascalCase proto method the linker compares
	// against. The request object literal (first argument) supplies the
	// payload fields.
	svc, tracked := t.clients[object]
	if !tracked {
		svc = inlineStubService(object, fc.hasGRPC() || len(t.clients) > 0)
	}
	if svc != "" && name != "" {
		var fields map[string]string
		if len(nodes) >= 1 {
			fields = tsObjectFields(fc, nodes[0])
		}
		fc.addEdge(src, store.EdgeRPCCall, grpcKey(svc, capitalizeFirst(name)), line, contract.ConfHigh,
			&store.EdgeMeta{Args: args, Fields: fields, Aliases: t.aliases.relevant(pos, args, fields)})
		fc.contractSite(domain.ContractKindRPC, true)
		return
	}

	// Object-literal fields for the detectors: kafkajs carries topic and
	// payload in an object argument, HTTP clients their options — either
	// written at the call site or built into a variable first.
	fields := t.optionsFields(fc, nodes)

	// Declarative framework detection: axios/fetch HTTP calls, kafkajs, bullmq.
	aliases := t.aliases.relevant(pos, args, fields)
	cs := &callSite{Callee: fnText, Name: name, Object: object, RecvType: t.types[lastComponent(object)],
		Args: args, Fields: fields, Aliases: aliases, Line: line, Src: src}
	if runDetectors(fc, &tsDetectors, cs, t.resolveString, t.stringsInExpr) {
		return
	}

	// Injected gRPC client: constructor(private orders: OrderServiceClient).
	if name != "" {
		if svc := grpcStubService(t.types[lastComponent(object)], fc.hasGRPC() || len(t.clients) > 0); svc != "" {
			fc.addEdge(src, store.EdgeRPCCall, grpcKey(svc, capitalizeFirst(name)), line, contract.ConfCrossFile,
				&store.EdgeMeta{Args: args, Fields: fields, Aliases: aliases})
			fc.contractSite(domain.ContractKindRPC, true)
			return
		}
	}

	// bullmq: queue.add('job', data) on a tracked Queue binding.
	if name == "add" || name == "addBulk" {
		if q, ok := t.queues[lastComponent(object)]; ok {
			key, topic := topicEdgeKey(q)
			fc.addEdge(src, store.EdgeProduces, key, line, contract.ConfHigh,
				&store.EdgeMeta{Topic: topic, Args: args, Fields: fields, Aliases: aliases})
			fc.contractSite(domain.ContractKindMessaging, true)
			return
		}
	}

	// Prisma: prisma.order.findMany() / this.prisma.order.create({data}).
	if tbl, kind, ok := prismaAccess(object, name); ok {
		fc.contractSite(domain.ContractKindDB, true)
		fc.addEdge(src, kind, contract.DB(tbl), line, contract.ConfCrossFile,
			&store.EdgeMeta{Args: args, Fields: fields, Aliases: aliases})
	} else if tbl, kind, conf, ok := t.ormAccess(object, name, args); ok {
		// TypeORM / MikroORM: repo.save(o), manager.find(Order, {...}).
		fc.contractSite(domain.ContractKindDB, true)
		fc.addEdge(src, kind, contract.DB(tbl), line, conf,
			&store.EdgeMeta{Args: args, Fields: fields, Aliases: aliases})
	}

	sqlEdgesFromArgs(fc, src, line, args)
	if name != "" {
		fc.addEdge(src, store.EdgeCall, name, line, contract.ConfHeuristic,
			&store.EdgeMeta{Args: args, Aliases: aliases, Receiver: object, RecvType: t.types[lastComponent(object)]})
	}
}

// tsORMWriteMethods and tsORMReadMethods are the TypeORM repository and
// MikroORM entity-manager operations. The two ORMs share their vocabulary
// almost exactly, so one pair of tables serves both.
var tsORMWriteMethods = map[string]bool{
	"save": true, "insert": true, "update": true, "upsert": true,
	"delete": true, "remove": true, "softDelete": true, "softRemove": true,
	"restore": true, "increment": true, "decrement": true,
	"persist": true, "persistAndFlush": true, "removeAndFlush": true,
	"nativeInsert": true, "nativeUpdate": true, "nativeDelete": true,
	"upsertMany": true, "insertMany": true, "nativeUpdateMany": true,
}

var tsORMReadMethods = map[string]bool{
	"find": true, "findAll": true, "findOne": true, "findOneBy": true, "findBy": true,
	"findAndCount": true, "findOneOrFail": true, "findOneByOrFail": true,
	"findByIds": true, "count": true, "countBy": true, "exist": true, "exists": true,
	"createQueryBuilder": true, "qb": true,
}

// tsORMRecv matches an ORM entity manager or repository receiver. Both ORMs
// route everything through one of the two, and their method names ("find",
// "save", "count") are too ordinary to stand without the receiver.
var tsORMRecv = objectMatch{
	Exact:    []string{"em", "this.em", "orm.em"},
	Contains: []string{"repository", "repo", "manager", "datasource", "entitymanager"},
	Fold:     true,
}

// reGetRepository captures the entity a repository was obtained for, from
// anywhere in the receiver chain: dataSource.getRepository(Order).find(...).
var reGetRepository = regexp.MustCompile(`getRepository\s*\(\s*([^),]*)\)`)

// ormAccess resolves the table a TypeORM or MikroORM call touches, from the
// entity the receiver was bound to or from the entity class the call names.
//
//	this.orderRepository.save(order)              // Repository<Order> field
//	dataSource.getRepository(Order).findOneBy(..) // entity in the chain
//	manager.find(Team, { id })                    // entity as first argument
//	manager.nativeDelete(toMikroORMEntity(X), ..) // entity behind a helper
func (t *tsExtractor) ormAccess(object, name string, args []string) (table, kind string, conf float32, ok bool) {
	write := tsORMWriteMethods[name]
	if !write && !tsORMReadMethods[name] {
		return "", "", 0, false
	}
	kind = store.EdgeReadsFrom
	if write {
		kind = store.EdgeWritesTo
	}
	if entity := tsRepositoryEntity(object, t.types[lastComponent(object)]); entity != "" {
		if tbl := tableName(entity); tbl != "" {
			return tbl, kind, contract.ConfCrossFile, true
		}
	}
	if len(args) > 0 && tsORMRecv.matches(object) {
		if entity := tsEntityArg(args[0]); entity != "" {
			if tbl := tableName(entity); tbl != "" {
				return tbl, kind, contract.ConfHeuristic, true
			}
		}
	}
	return "", "", 0, false
}

// tsRepositoryEntity returns the entity a repository receiver serves: the type
// argument of its declared Repository<T>, or the class named by a
// getRepository() call in the receiver chain.
func tsRepositoryEntity(object, recvType string) string {
	if hasSuffixFold(trimGenericArgs(recvType), "repository") {
		if a := genericArgs(recvType); len(a) > 0 {
			if e := tsEntityArg(a[0]); e != "" {
				return e
			}
		}
	}
	if m := reGetRepository.FindStringSubmatch(object); m != nil {
		return tsEntityArg(m[1])
	}
	return ""
}

// tsEntityArg returns the entity class an ORM call names, unwrapping the
// single-argument helper the entity is often passed through (MikroORM's
// toMikroORMEntity(IndexData)). Returns "" when the argument is anything but a
// plain class reference — an options object, a string, an id.
func tsEntityArg(expr string) string {
	e := strings.TrimSpace(expr)
	for i := 0; i < 2; i++ {
		head, isCall := callHead(e)
		if !isCall || head == "" {
			break
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(e[len(head):]), "("), ")")
		if strings.ContainsAny(inner, ",{}[]\"'") {
			return ""
		}
		e = strings.TrimSpace(inner)
	}
	name := lastComponent(trimGenericArgs(e))
	if name == "" || name[0] < 'A' || name[0] > 'Z' || !isIdentLike(name) {
		return ""
	}
	return name
}

// prismaWriteMethods are the Prisma delegate methods that mutate rows; the
// remaining delegate methods in prismaReadMethods are queries.
var prismaWriteMethods = map[string]bool{
	"create": true, "createMany": true, "update": true, "updateMany": true,
	"upsert": true, "delete": true, "deleteMany": true,
}

var prismaReadMethods = map[string]bool{
	"findMany": true, "findUnique": true, "findFirst": true,
	"findUniqueOrThrow": true, "findFirstOrThrow": true, "count": true, "aggregate": true,
}

// prismaAccess recognizes a Prisma delegate call: the receiver is
// "<something>prisma.<model>" and the method is a delegate operation. The
// model name is the table Prisma maps it to.
func prismaAccess(object, method string) (table, kind string, ok bool) {
	write := prismaWriteMethods[method]
	if !write && !prismaReadMethods[method] {
		return "", "", false
	}
	i := strings.LastIndex(object, ".")
	if i < 0 {
		return "", "", false
	}
	model, receiver := object[i+1:], strings.ToLower(object[:i])
	if !strings.HasSuffix(receiver, "prisma") && !strings.HasSuffix(receiver, "db") {
		return "", "", false
	}
	if model == "" || !isIdentLike(model) {
		return "", "", false
	}
	kind = store.EdgeReadsFrom
	if write {
		kind = store.EdgeWritesTo
	}
	return snakeCase(model), kind, true
}

// tsClientObjects are the receiver shapes the HTTP client rules claim; they
// expose get/post/... as outgoing requests, never as route registrations.
var tsClientObjects = objectMatch{
	Contains: []string{"axios", "http"},
	Exact:    []string{"ky", "got", "superagent"},
}

// tsHandlerLike reports whether a trailing route argument looks like a request
// handler: a function, a reference to one, or a middleware factory call.
// Object literals and literals are option bags — the mark of a client call.
func tsHandlerLike(n *sitter.Node) bool {
	if n == nil {
		return false
	}
	switch n.Type() {
	case "arrow_function", "function", "function_expression", "generator_function",
		"identifier", "member_expression", "call_expression", "array":
		return true
	}
	return false
}

func (t *tsExtractor) resolveString(expr string) (string, bool) {
	expr = strings.TrimSpace(expr)
	// Template literals are checked first: unquote() treats a backtick like any
	// other delimiter, so a literal with substitutions would otherwise resolve
	// to its raw "${...}" text instead of falling through to route templating.
	if strings.HasPrefix(expr, "`") && strings.HasSuffix(expr, "`") && len(expr) >= 2 {
		if strings.Contains(expr, "${") {
			return "", false
		}
		return expr[1 : len(expr)-1], true
	}
	return t.consts.resolve(expr)
}

// stringsInExpr extracts string literals (and known consts) from an expression
// like "'a'" or "['a', 'b']".
func (t *tsExtractor) stringsInExpr(expr string) []string {
	expr = strings.Trim(strings.TrimSpace(expr), "[]")
	var out []string
	for _, part := range strings.Split(expr, ",") {
		if v, ok := t.resolveString(part); ok {
			out = append(out, v)
		}
	}
	return out
}

func (t *tsExtractor) qualify(scope, name string) string {
	if scope != "" {
		return scope + "." + name
	}
	return name
}

func isTSIdentifier(s string) bool {
	for _, r := range s {
		if !(r == '_' || r == '$' || r == '.' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return s != ""
}

// optionsFields returns the fields of the first argument that describes an
// object: a literal written at the call site, or a variable the file bound to
// one. Which position that is differs per library — kafkajs puts it first,
// fetch second, n8n's helpers pass a variable — so the shape decides rather
// than the position.
func (t *tsExtractor) optionsFields(fc *fileCtx, nodes []*sitter.Node) map[string]string {
	for _, n := range nodes {
		if f := tsObjectFields(fc, n); f != nil {
			return f
		}
		if n.Type() == "identifier" {
			if f, ok := t.objects[fc.text(n)]; ok {
				return f
			}
		}
	}
	return nil
}

// tsObjectFields extracts key -> value-expression pairs from an object literal.
// markRPCHandlers turns a grpc-js service registration into implements_rpc
// edges: the service definition names the proto service, and the handler map's
// keys are the rpc methods, spelled lowerCamel.
//
//	server.addService(shopProto.CurrencyService.service, {getSupportedCurrencies, convert})
//	this.server.addService(pkg.PaymentService.service, {charge: Handler.bind(this)})
//
// The edge is attributed to the handler function itself when this file
// declares one by that name — the map value's identifier first, since the key
// is the rpc name and the function is as likely to be called something else.
func (t *tsExtractor) markRPCHandlers(fc *fileCtx, def string, handlers *sitter.Node, line int) bool {
	svc := grpcServiceFromDefinition(def)
	if svc == "" {
		return false
	}
	fields := tsObjectFields(fc, handlers)
	if len(fields) == 0 {
		return false
	}
	for key, value := range fields {
		src := t.unitNamed(fc, lastComponent(strings.TrimSuffix(trimCallSuffix(value), "()")))
		if src < 0 {
			src = t.unitNamed(fc, key)
		}
		if src < 0 {
			continue
		}
		fc.addEdge(src, store.EdgeImplementsRPC, grpcKey(svc, capitalizeFirst(key)),
			line, contract.ConfCrossFile, nil)
	}
	return true
}

// grpcServiceFromDefinition reads the proto service out of a grpc-js service
// definition expression: "pkg.CurrencyService.service" -> "CurrencyService".
func grpcServiceFromDefinition(expr string) string {
	rest, ok := strings.CutSuffix(strings.TrimSpace(expr), ".service")
	if !ok {
		return ""
	}
	svc := lastComponent(rest)
	if svc == "" || svc[0] < 'A' || svc[0] > 'Z' {
		return ""
	}
	return svc
}

// trimCallSuffix drops a trailing call from a handler expression, so
// "Server.ChargeHandler.bind(this)" is looked up as "Server.ChargeHandler".
func trimCallSuffix(expr string) string {
	head, ok := callHead(strings.TrimSpace(expr))
	if !ok {
		return strings.TrimSpace(expr)
	}
	if trimmed, cut := strings.CutSuffix(head, ".bind"); cut {
		return trimmed
	}
	return head
}

// unitNamed returns the index of a function or method unit declared in this
// file under the given name, or -1.
func (t *tsExtractor) unitNamed(fc *fileCtx, name string) int {
	if name == "" {
		return -1
	}
	for i, u := range fc.units {
		if u.Name == name && (u.Kind == "function" || u.Kind == "method") {
			return i
		}
	}
	return -1
}

func tsObjectFields(fc *fileCtx, n *sitter.Node) map[string]string {
	if n == nil || n.Type() != "object" {
		return nil
	}
	fields := map[string]string{}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		pair := n.NamedChild(i)
		switch pair.Type() {
		case "pair":
			key := fc.text(pair.ChildByFieldName("key"))
			if v, ok := unquote(key); ok {
				key = v
			}
			fields[key] = fc.text(pair.ChildByFieldName("value"))
		case "shorthand_property_identifier":
			key := fc.text(pair)
			fields[key] = key
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}
