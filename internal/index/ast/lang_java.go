package ast

import (
	"slices"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"

	sitter "github.com/smacker/go-tree-sitter"
)

// javaExtractor extracts units and edges from Java source.
//
// Framework heuristics:
//   - Spring MVC routes: @RestController/@RequestMapping/@GetMapping/...
//   - Kafka: @KafkaListener(topics=...), kafkaTemplate.send(topic, ...)
//   - HTTP clients: RestTemplate (getForObject/postForObject/exchange/...),
//     WebClient fluent chains (webClient.post().uri("/x")...)
//   - gRPC clients via generated XxxGrpc.newBlockingStub/newStub/newFutureStub
type javaExtractor struct {
	pkg     string
	consts  constResolver
	aliases aliasTable        // local aliases (String x = userId), scoped per method
	clients map[string]string // var name -> gRPC service name (stub bindings), file scope
	types   map[string]string // field/param name -> declared type (injected dependencies)

	// rpcSvc is the proto service the class being walked implements, from its
	// generated XxxGrpc.XxxImplBase superclass.
	rpcSvc string
}

// javaStubFactories are the generated gRPC stub factory method names.
var javaStubFactories = map[string]bool{
	"newBlockingStub": true, "newStub": true, "newFutureStub": true,
}

// javaGRPCServiceFromBase returns the proto service a class implements by
// extending its generated base, or "".
//
// The protoc-gen-grpc-java output nests XxxImplBase inside XxxGrpc, so both
// halves name the service and either spelling is conclusive on its own —
// nothing but generated code produces an "ImplBase" suffix.
func javaGRPCServiceFromBase(superclass string) string {
	base := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(superclass), "extends"))
	base = trimGenericArgs(base)
	name := lastComponent(base)
	stem, ok := strings.CutSuffix(name, "ImplBase")
	if !ok || stem == "" {
		return ""
	}
	if qualifier := lastComponent(strings.TrimSuffix(base, "."+name)); qualifier != base {
		if svc, found := strings.CutSuffix(qualifier, "Grpc"); found && svc != "" {
			return svc
		}
	}
	return stem
}

var javaMappingMethods = map[string]string{
	"GetMapping": "GET", "PostMapping": "POST", "PutMapping": "PUT",
	"DeleteMapping": "DELETE", "PatchMapping": "PATCH", "RequestMapping": "ANY",
}

// javaDetectors are the declarative framework rules for Java (see frameworks.go).
var javaDetectors = detectorSet{
	Produce: append([]kafkaProduceRule{
		// Spring Kafka: kafkaTemplate.send("topic", key, value)
		{
			Object:   objectMatch{Contains: []string{"kafka"}, Fold: true},
			Methods:  []string{"send"},
			TopicArg: 0,
			Conf:     contract.ConfHigh,
		},
	}, jvmBrokerProduce...),
	Consume: jvmBrokerConsume,
	Broker:  true,
	HTTP: []httpClientRule{
		// RestTemplate: getForObject(url, ...), postForEntity(url, ...).
		//
		// The target is admitted base-relative: these names belong to one API
		// whose first argument is always the request target, and a RestTemplate
		// resolves it against its UriTemplateHandler. Conductor's client is
		// written that way throughout — getForEntity("tasks/queue/size").
		{
			Methods: map[string]string{
				"getForObject": "GET", "getForEntity": "GET",
				"postForObject": "POST", "postForEntity": "POST", "postForLocation": "POST",
			},
			URLArg:      0,
			RelativeURL: true,
			Conf:        contract.ConfCrossFile,
		},
		// RestTemplate's method-in-argument forms: exchange(url, HttpMethod.POST,
		// ...). Their names belong to too many unrelated APIs to mean anything
		// on their own, so they are Loose: an edge when the URL is there,
		// nothing when it is not.
		{
			Methods:   map[string]string{"exchange": "ANY", "execute": "ANY"},
			URLArg:    0,
			MethodArg: 1,
			Loose:     true,
			Conf:      contract.ConfCrossFile,
		},
		// RestTemplate put/delete. Their names are also Map and Collection
		// members, so — unlike the methods above — they count only on a
		// receiver that is an HTTP client by name or by declared type. Left
		// unconstrained, this rule turned props.put("key.deserializer", ...)
		// and map.put("test2", 2) into ~20k phantom http_call edges across
		// Kafka, Elasticsearch and Conductor.
		{
			Object:   javaHTTPClientMatch,
			RecvType: javaHTTPClientMatch,
			Methods:  map[string]string{"put": "PUT", "delete": "DELETE"},
			URLArg:   0,
			Conf:     contract.ConfCrossFile,
		},
	},
}

// javaHTTPClientMatch identifies a receiver that is an HTTP client rather than
// a collection, matched case-insensitively against either the receiver
// expression (restTemplate, this.restTemplate) or its declared type
// (org.springframework.web.client.RestTemplate).
var javaHTTPClientMatch = objectMatch{
	Contains: []string{"resttemplate", "restoperations", "restclient", "webclient", "httpclient"},
	Fold:     true,
}

func (j *javaExtractor) extract(fc *fileCtx, root *sitter.Node) {
	j.consts = constResolver{}
	j.aliases = aliasTable{}
	j.clients = map[string]string{}
	j.types = map[string]string{}
	j.rpcSvc = ""
	j.collectConsts(fc, root)
	j.walk(fc, root, "", "")
	j.walkCalls(fc, root)
}

// collectConsts maps identifiers to string literals (fields and locals).
func (j *javaExtractor) collectConsts(fc *fileCtx, n *sitter.Node) {
	if n == nil {
		return
	}
	if n.Type() == "variable_declarator" {
		name := fc.text(n.ChildByFieldName("name"))
		value := n.ChildByFieldName("value")
		if v, ok := unquote(fc.text(value)); ok && name != "" {
			j.consts[name] = v
		} else if name != "" && value != nil {
			// gRPC stub binding: stub = OrderServiceGrpc.newBlockingStub(channel)
			// — the service name is the factory's receiver class minus "Grpc".
			if value.Type() == "method_invocation" {
				m := fc.text(value.ChildByFieldName("name"))
				obj := lastComponent(fc.text(value.ChildByFieldName("object")))
				if javaStubFactories[m] && strings.HasSuffix(obj, "Grpc") && len(obj) > len("Grpc") {
					j.clients[name] = strings.TrimSuffix(obj, "Grpc")
				}
			}
			// Local alias: String x = userId; / var x = body.userId;
			// A call RHS (var x = extractUserId(req)) is recorded with its
			// full text so token matching links x to the call's arguments.
			switch value.Type() {
			case "identifier", "field_access", "method_invocation":
				j.aliases.record(n, name, fc.text(value))
			}
		}
	}
	// Declared types of fields and constructor parameters: an injected gRPC
	// stub or a repository is only recognizable by what it was declared as.
	switch n.Type() {
	case "field_declaration", "formal_parameter":
		j.recordDeclaredType(fc, n)
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		j.collectConsts(fc, n.NamedChild(i))
	}
}

// recordDeclaredType maps a field or parameter name to its declared type.
func (j *javaExtractor) recordDeclaredType(fc *fileCtx, n *sitter.Node) {
	typ := fc.text(n.ChildByFieldName("type"))
	if typ == "" {
		return
	}
	if name := fc.text(n.ChildByFieldName("name")); name != "" {
		j.types[name] = typ
		return
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		if c := n.NamedChild(i); c.Type() == "variable_declarator" {
			if name := fc.text(c.ChildByFieldName("name")); name != "" {
				j.types[name] = typ
			}
		}
	}
}

// walk collects package, classes, methods and route/consumer annotations.
// scope is the enclosing class qualified name; basePath is the class-level
// @RequestMapping path.
func (j *javaExtractor) walk(fc *fileCtx, n *sitter.Node, scope, basePath string) {
	if n == nil {
		return
	}
	switch n.Type() {
	case "package_declaration":
		text := strings.TrimSuffix(strings.TrimSpace(fc.text(n)), ";")
		j.pkg = strings.TrimSpace(strings.TrimPrefix(text, "package"))
	case "class_declaration", "interface_declaration", "enum_declaration", "record_declaration":
		name := fc.text(n.ChildByFieldName("name"))
		if name != "" {
			kind := "class"
			if n.Type() == "interface_declaration" {
				kind = "interface"
			}
			qualified := j.qualify(scope, name)
			doc := extractLineComments(string(fc.src), n, "//")
			fc.addUnit(n, kind, name, qualified, "", doc)

			// JPA entity: @Entity [@Table(name="orders")] publishes the table
			// this class maps to, so repository access has a destination unit.
			if tbl := j.entityTable(fc, n, name); tbl != "" {
				fc.addUnit(n, store.KindDBTable, tbl, contract.DB(tbl), "entity:"+name, doc)
			}

			classBase := j.annotationPath(fc, n, "RequestMapping")
			svc := j.rpcSvc
			j.rpcSvc = javaGRPCServiceFromBase(fc.text(n.ChildByFieldName("superclass")))
			if body := n.ChildByFieldName("body"); body != nil {
				for i := 0; i < int(body.NamedChildCount()); i++ {
					j.walk(fc, body.NamedChild(i), qualified, joinPath(basePath, classBase))
				}
			}
			j.rpcSvc = svc
			return
		}
	case "method_declaration":
		j.handleMethod(fc, n, scope, basePath)
		return
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		j.walk(fc, n.NamedChild(i), scope, basePath)
	}
}

func (j *javaExtractor) handleMethod(fc *fileCtx, n *sitter.Node, scope, basePath string) {
	name := fc.text(n.ChildByFieldName("name"))
	if name == "" {
		return
	}
	sig := fc.text(n.ChildByFieldName("parameters"))
	doc := extractLineComments(string(fc.src), n, "//")
	idx := fc.addUnit(n, "method", name, j.qualify(scope, name), sig, doc)
	line := fc.units[idx].StartLine

	// A generated server base is implemented by overriding its methods, and
	// every one of them takes the response StreamObserver. Java stubs expose
	// lowerCamel names; the proto rpc they answer is PascalCase.
	if j.rpcSvc != "" && strings.Contains(sig, "StreamObserver") {
		fc.addEdge(idx, store.EdgeImplementsRPC, grpcKey(j.rpcSvc, capitalizeFirst(name)),
			line, contract.ConfCrossFile, nil)
	}

	for _, ann := range annotations(fc, n) {
		annName := ann.name
		// HTTP route
		if method, ok := javaMappingMethods[annName]; ok {
			path := joinPath(basePath, j.annotationValue(fc, ann.node))
			ridx := fc.addUnit(n, store.KindHTTPRoute, method+" "+path, routeKey(method, path), "path:"+path, "")
			fc.addEdge(ridx, store.EdgeHandledBy, name, line, contract.ConfHigh, nil)
		}
		// Broker listener: @KafkaListener(topics=...), @RabbitListener(queues=...),
		// @JmsListener(destination=...), @SqsListener("orders").
		if keys, ok := jvmListenerAnnotations[annName]; ok {
			topics := j.annotationTopics(fc, ann.node, keys)
			fc.contractSite(domain.ContractKindMessaging, len(topics) > 0)
			for _, topic := range topics {
				key, name := topicEdgeKey(topic)
				fc.addEdge(idx, store.EdgeConsumes, key, line, contract.ConfHigh, &store.EdgeMeta{Topic: name})
			}
		}
	}
}

// entityTable returns the table a JPA-annotated class maps to: the explicit
// @Table(name = "orders") when present, otherwise the default naming of the
// class name. Returns "" for classes without @Entity/@Table.
func (j *javaExtractor) entityTable(fc *fileCtx, decl *sitter.Node, className string) string {
	entity := false
	for _, ann := range annotations(fc, decl) {
		switch ann.name {
		case "Entity":
			entity = true
		case "Table":
			if v := j.annotationNamed(fc, ann.node, "name"); v != "" {
				return strings.ToLower(v)
			}
			entity = true
		}
	}
	if !entity {
		return ""
	}
	return tableName(className)
}

// annotationNamed returns the value of a named annotation element.
func (j *javaExtractor) annotationNamed(fc *fileCtx, ann *sitter.Node, key string) string {
	argList := ann.ChildByFieldName("arguments")
	if argList == nil {
		return ""
	}
	for i := 0; i < int(argList.NamedChildCount()); i++ {
		c := argList.NamedChild(i)
		if c.Type() != "element_value_pair" {
			continue
		}
		if fc.text(c.ChildByFieldName("key")) == key {
			if v, ok := j.consts.resolve(fc.text(c.ChildByFieldName("value"))); ok {
				return v
			}
		}
	}
	return ""
}

// jpaWriteMethods are the Spring Data repository methods that mutate rows;
// every other repository call is treated as a read.
var jpaWriteMethods = map[string]bool{
	"save": true, "saveAll": true, "saveAndFlush": true, "saveAllAndFlush": true,
	"delete": true, "deleteAll": true, "deleteById": true, "deleteAllById": true,
	"insert": true, "update": true, "persist": true, "merge": true, "remove": true,
}

// repositoryTable maps a Spring Data repository receiver to the table it
// serves. The entity is taken from the repository's declared type
// (OrderRepository -> Order -> orders), which works even when the interface
// itself lives in another file.
func repositoryTable(typ, object string) string {
	name := lastComponent(strings.TrimSpace(typ))
	if name == "" {
		name = lastComponent(object)
	}
	// Generic bounds like JpaRepository<Order, Long> are not the receiver type.
	if i := strings.IndexByte(name, '<'); i > 0 {
		name = name[:i]
	}
	for _, suffix := range []string{"Repository", "Repo", "DAO", "Dao"} {
		if entity, found := strings.CutSuffix(name, suffix); found && entity != "" {
			return tableName(entity)
		}
	}
	return ""
}

// walkCalls emits call/http_call/produces edges from method invocations.
func (j *javaExtractor) walkCalls(fc *fileCtx, n *sitter.Node) {
	if n == nil {
		return
	}
	switch n.Type() {
	case "method_invocation":
		j.handleInvocation(fc, n)
	case "object_creation_expression":
		j.handleObjectCreation(fc, n)
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		j.walkCalls(fc, n.NamedChild(i))
	}
}

// handleObjectCreation detects a route declared as a record: Elasticsearch's
// RestHandler.routes() returns `new Route(GET, "/_cat/nodes")` values, which no
// router ever sees.
func (j *javaExtractor) handleObjectCreation(fc *fileCtx, n *sitter.Node) {
	typ := fc.text(n.ChildByFieldName("type"))
	if typ == "" {
		return
	}
	args := namedChildTexts(fc, n.ChildByFieldName("arguments"))
	applyGenericRoute(fc, n, typ, args, j.consts.resolve, nil, "")
}

func (j *javaExtractor) handleInvocation(fc *fileCtx, n *sitter.Node) {
	name := fc.text(n.ChildByFieldName("name"))
	object := fc.text(n.ChildByFieldName("object"))
	args := namedChildTexts(fc, n.ChildByFieldName("arguments"))
	line := int(n.StartPoint().Row) + 1
	src := fc.enclosingUnit(int(n.StartByte()), callableKinds...)
	if src < 0 {
		return
	}

	// Declarative framework detection: kafkaTemplate.send, RestTemplate calls.
	callee := name
	if object != "" {
		callee = object + "." + name
	}
	pos := int(n.StartByte())
	aliases := j.aliases.relevant(pos, args, nil)
	recvType := j.types[lastComponent(object)]

	// Route registered through the service's own machinery, when the framework
	// annotations are absent.
	if applyGenericRoute(fc, n, callee, args, j.consts.resolve, nil, "") {
		return
	}

	// gRPC client call on a tracked stub: stub.createOrder(req), or on one
	// built in the call itself (OrderServiceGrpc.newBlockingStub(ch).create()).
	// Java stubs expose lowerCamel method names; capitalize to match the
	// PascalCase proto method the linker compares against.
	svc, tracked := j.clients[object]
	if !tracked {
		svc = inlineStubService(object, fc.hasGRPC())
	}
	if svc != "" && name != "" {
		fc.addEdge(src, store.EdgeRPCCall, grpcKey(svc, capitalizeFirst(name)), line, contract.ConfHigh,
			&store.EdgeMeta{Args: args, Aliases: aliases})
		fc.contractSite(domain.ContractKindRPC, true)
		return
	}

	// WebClient fluent chain: webClient.post().uri("/api/x").bodyValue(b)...
	if m, u, conf, ok := fluentChainHTTP(name, object, args, j.consts.resolve); ok {
		host, path := splitURL(u)
		fc.addEdge(src, store.EdgeHTTPCall, routeKey(m, path), line, conf,
			&store.EdgeMeta{Method: m, Path: path, Host: host, Args: args, Aliases: aliases})
		fc.contractSite(domain.ContractKindHTTP, true)
		return
	}

	cs := &callSite{Callee: callee, Name: name, Object: object, RecvType: recvType,
		Args: args, Line: line, Src: src, Aliases: aliases}
	if runDetectors(fc, &javaDetectors, cs, j.consts.resolve, nil) {
		return
	}

	// Injected gRPC stub: the type is the only evidence when the stub is a
	// constructor-injected field rather than a local newBlockingStub call —
	// and a type name is only evidence when the file corroborates it.
	if name != "" {
		if svc := grpcStubService(recvType, fc.hasGRPC() || len(j.clients) > 0); svc != "" {
			fc.addEdge(src, store.EdgeRPCCall, grpcKey(svc, capitalizeFirst(name)), line, contract.ConfCrossFile,
				&store.EdgeMeta{Args: args, Aliases: aliases})
			fc.contractSite(domain.ContractKindRPC, true)
			return
		}
	}

	// Spring Data repository access: orderRepository.save(o) / findAllByUserId(u).
	// Emitted alongside the call edge, not instead of it.
	if tbl := repositoryTable(recvType, object); tbl != "" {
		fc.contractSite(domain.ContractKindDB, true)
		kind := store.EdgeReadsFrom
		if jpaWriteMethods[name] {
			kind = store.EdgeWritesTo
		}
		fc.addEdge(src, kind, contract.DB(tbl), line, contract.ConfHeuristic,
			&store.EdgeMeta{Args: args, Aliases: aliases})
	}

	sqlEdgesFromArgs(fc, src, line, args)
	fc.addEdge(src, store.EdgeCall, name, line, contract.ConfHeuristic,
		&store.EdgeMeta{Args: args, Aliases: aliases, Receiver: object, RecvType: recvType})
}

// annotation is a parsed Java annotation attached to a declaration.
type annotation struct {
	name string
	node *sitter.Node
}

// annotations returns the annotations in a declaration's modifiers.
func annotations(fc *fileCtx, decl *sitter.Node) []annotation {
	var out []annotation
	for i := 0; i < int(decl.ChildCount()); i++ {
		c := decl.Child(i)
		if c == nil || c.Type() != "modifiers" {
			continue
		}
		for k := 0; k < int(c.NamedChildCount()); k++ {
			a := c.NamedChild(k)
			if a.Type() == "annotation" || a.Type() == "marker_annotation" {
				out = append(out, annotation{name: fc.text(a.ChildByFieldName("name")), node: a})
			}
		}
	}
	return out
}

// annotationPath returns the string value of a named annotation on a declaration.
func (j *javaExtractor) annotationPath(fc *fileCtx, decl *sitter.Node, annName string) string {
	for _, ann := range annotations(fc, decl) {
		if ann.name == annName {
			return j.annotationValue(fc, ann.node)
		}
	}
	return ""
}

// annotationValue extracts the primary string value of an annotation:
// @X("/p"), @X(value = "/p"), @X(path = "/p").
func (j *javaExtractor) annotationValue(fc *fileCtx, ann *sitter.Node) string {
	argList := ann.ChildByFieldName("arguments")
	if argList == nil {
		return ""
	}
	for i := 0; i < int(argList.NamedChildCount()); i++ {
		c := argList.NamedChild(i)
		switch c.Type() {
		case "element_value_pair":
			key := fc.text(c.ChildByFieldName("key"))
			if key == "value" || key == "path" {
				if v, ok := j.consts.resolve(fc.text(c.ChildByFieldName("value"))); ok {
					return v
				}
			}
		default:
			if v, ok := j.consts.resolve(fc.text(c)); ok {
				return v
			}
		}
	}
	return ""
}

// annotationTopics extracts destinations from a listener annotation:
// @KafkaListener(topics = "..."/{"..."}), @RabbitListener(queues = "..."). keys
// are the annotation elements this annotation names its destination with; the
// unnamed "value" element is always read.
func (j *javaExtractor) annotationTopics(fc *fileCtx, ann *sitter.Node, keys []string) []string {
	argList := ann.ChildByFieldName("arguments")
	if argList == nil {
		return nil
	}
	var topics []string
	var collect func(n *sitter.Node)
	collect = func(n *sitter.Node) {
		if v, ok := j.consts.resolve(fc.text(n)); ok && n.Type() == "string_literal" {
			topics = append(topics, v)
			return
		}
		if n.Type() == "identifier" {
			if v, ok := j.consts[fc.text(n)]; ok {
				topics = append(topics, v)
			}
			return
		}
		for i := 0; i < int(n.NamedChildCount()); i++ {
			collect(n.NamedChild(i))
		}
	}
	for i := 0; i < int(argList.NamedChildCount()); i++ {
		c := argList.NamedChild(i)
		if c.Type() == "element_value_pair" {
			key := fc.text(c.ChildByFieldName("key"))
			if key == "value" || slices.Contains(keys, key) {
				collect(c.ChildByFieldName("value"))
			}
		} else {
			collect(c)
		}
	}
	return topics
}

func (j *javaExtractor) qualify(scope, name string) string {
	if scope != "" {
		return scope + "." + name
	}
	if j.pkg != "" {
		return j.pkg + "." + name
	}
	return name
}

func joinPath(base, sub string) string {
	base = strings.Trim(base, "/")
	sub = strings.Trim(sub, "/")
	switch {
	case base == "" && sub == "":
		return "/"
	case base == "":
		return "/" + sub
	case sub == "":
		return "/" + base
	default:
		return "/" + base + "/" + sub
	}
}
