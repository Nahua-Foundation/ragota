package ast

import (
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/storage"

	sitter "github.com/smacker/go-tree-sitter"
)

// ktExtractor extracts units and edges from Kotlin source.
//
// Framework heuristics (Spring on the JVM, shared with Java):
//   - Spring MVC routes: @GetMapping/@PostMapping/... on functions, with the
//     class-level @RequestMapping as the base path
//   - Kafka: kafkaTemplate.send(topic, ...)
//   - HTTP clients: RestTemplate methods, WebClient fluent chains
type ktExtractor struct {
	pkg     string
	consts  constResolver
	aliases aliasTable        // local aliases, scoped per function
	types   map[string]string // property/parameter name -> declared type
}

// ktDetectors: Kotlin services use the same Spring stack as Java, so the
// declarative rules are shared (see javaDetectors in lang_java.go).
var ktDetectors = &javaDetectors

func (k *ktExtractor) extract(fc *fileCtx, root *sitter.Node) {
	k.consts = constResolver{}
	k.aliases = aliasTable{}
	k.types = map[string]string{}
	k.collectConsts(fc, root)
	k.walk(fc, root, "", "")
	k.walkCalls(fc, root)
}

// collectConsts maps property names to string literal values and records
// local aliases (val x = userId / val x = req.userId / val x = f(req)).
func (k *ktExtractor) collectConsts(fc *fileCtx, n *sitter.Node) {
	if n == nil {
		return
	}
	if n.Type() == "property_declaration" {
		name := ""
		if vd := firstChildOfType(n, "variable_declaration"); vd != nil {
			name = fc.text(firstChildOfType(vd, "simple_identifier"))
		}
		// The initializer is the last named child when present (the
		// variable_declaration precedes it).
		var value *sitter.Node
		if c := int(n.NamedChildCount()); c > 0 {
			if last := n.NamedChild(c - 1); last != nil && last.Type() != "variable_declaration" {
				value = last
			}
		}
		if name != "" && value != nil {
			if v, ok := unquote(fc.text(value)); ok {
				k.consts[name] = v
			} else {
				switch value.Type() {
				case "simple_identifier", "navigation_expression", "call_expression":
					k.aliases.record(n, name, fc.text(value))
				}
			}
		}
	}
	// Constructor-injected dependencies: `private val orders: OrderServiceStub`
	// in a primary constructor, or a class property with an explicit type.
	if n.Type() == "class_parameter" || n.Type() == "variable_declaration" {
		name := fc.text(firstChildOfType(n, "simple_identifier"))
		if t := firstChildOfType(n, "user_type"); t != nil && name != "" {
			k.types[name] = fc.text(t)
		}
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		k.collectConsts(fc, n.NamedChild(i))
	}
}

// walk collects the package header, classes/objects and functions with their
// Spring mapping annotations. scope is the enclosing class qualified name;
// basePath is the class-level @RequestMapping path.
func (k *ktExtractor) walk(fc *fileCtx, n *sitter.Node, scope, basePath string) {
	if n == nil {
		return
	}
	switch n.Type() {
	case "package_header":
		if id := firstChildOfType(n, "identifier"); id != nil {
			k.pkg = fc.text(id)
		}
	case "class_declaration", "object_declaration":
		name := fc.text(firstChildOfType(n, "type_identifier"))
		if name != "" {
			qualified := k.qualify(scope, name)
			doc := extractLineComments(string(fc.src), n, "//")
			fc.addUnit(n, "class", name, qualified, "", doc)

			classBase := ktClassBasePath(fc, n)
			if body := firstChildOfType(n, "class_body"); body != nil {
				for i := 0; i < int(body.NamedChildCount()); i++ {
					k.walk(fc, body.NamedChild(i), qualified, joinPath(basePath, classBase))
				}
			}
			return
		}
	case "function_declaration":
		k.handleFunction(fc, n, scope, basePath)
		return
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		k.walk(fc, n.NamedChild(i), scope, basePath)
	}
}

func (k *ktExtractor) handleFunction(fc *fileCtx, n *sitter.Node, scope, basePath string) {
	name := fc.text(firstChildOfType(n, "simple_identifier"))
	if name == "" {
		return
	}
	sig := fc.text(firstChildOfType(n, "function_value_parameters"))
	doc := extractLineComments(string(fc.src), n, "//")
	kind := "function"
	if scope != "" {
		kind = "method"
	}
	idx := fc.addUnit(n, kind, name, k.qualify(scope, name), sig, doc)
	line := fc.units[idx].StartLine

	for _, ann := range ktAnnotations(fc, n) {
		if method, ok := javaMappingMethods[ann.name]; ok {
			path := joinPath(basePath, k.ktAnnotationValue(fc, ann.node))
			ridx := fc.addUnit(n, storage.KindHTTPRoute, method+" "+path, routeKey(method, path), "path:"+path, "")
			fc.addEdge(ridx, storage.EdgeHandledBy, name, line, contract.ConfHigh, nil)
		}
	}
}

// walkCalls emits call/http_call/produces edges from call expressions.
func (k *ktExtractor) walkCalls(fc *fileCtx, n *sitter.Node) {
	if n == nil {
		return
	}
	if n.Type() == "call_expression" {
		k.handleCall(fc, n)
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		k.walkCalls(fc, n.NamedChild(i))
	}
}

func (k *ktExtractor) handleCall(fc *fileCtx, n *sitter.Node) {
	callee := n.NamedChild(0)
	if callee == nil {
		return
	}
	fnText := fc.text(callee)
	name := lastComponent(fnText)
	object := ""
	if i := strings.LastIndex(fnText, "."); i > 0 {
		object = fnText[:i]
	}
	var args []string
	if suffix := firstChildOfType(n, "call_suffix"); suffix != nil {
		args = namedChildTexts(fc, firstChildOfType(suffix, "value_arguments"))
	}
	line := int(n.StartPoint().Row) + 1
	pos := int(n.StartByte())
	src := fc.enclosingUnit(pos, callableKinds...)
	if src < 0 {
		return
	}
	aliases := k.aliases.relevant(pos, args, nil)
	recvType := k.types[lastComponent(object)]

	// WebClient fluent chain: webClient.post().uri("/api/x").bodyValue(b)...
	if m, u, conf, ok := fluentChainHTTP(name, object, args, k.consts.resolve); ok {
		host, path := splitURL(u)
		fc.addEdge(src, storage.EdgeHTTPCall, routeKey(m, path), line, conf,
			&storage.EdgeMeta{Method: m, Path: path, Host: host, Args: args, Aliases: aliases})
		fc.contractSite(storage.ContractKindHTTP, true)
		return
	}

	// Route registered through the service's own machinery, and gRPC stubs
	// constructed in the call itself — same shapes as Java.
	if applyGenericRoute(fc, n, fnText, args, k.consts.resolve, nil, "") {
		return
	}
	if svc := inlineStubService(object, fc.hasGRPC()); svc != "" && name != "" {
		fc.addEdge(src, storage.EdgeRPCCall, grpcKey(svc, capitalizeFirst(name)), line, contract.ConfHigh,
			&storage.EdgeMeta{Args: args, Aliases: aliases})
		fc.contractSite(storage.ContractKindRPC, true)
		return
	}

	// Declarative framework detection: kafkaTemplate.send, RestTemplate calls.
	cs := &callSite{Callee: fnText, Name: name, Object: object, RecvType: recvType,
		Args: args, Line: line, Src: src, Aliases: aliases}
	if runDetectors(fc, ktDetectors, cs, k.consts.resolve, nil) {
		return
	}

	// Injected gRPC stub, then Spring Data repository access — same shapes as
	// Java, since both run on the Spring stack.
	if name != "" {
		if svc := grpcStubService(recvType, fc.hasGRPC()); svc != "" {
			fc.addEdge(src, storage.EdgeRPCCall, grpcKey(svc, capitalizeFirst(name)), line, contract.ConfCrossFile,
				&storage.EdgeMeta{Args: args, Aliases: aliases})
			fc.contractSite(storage.ContractKindRPC, true)
			return
		}
	}
	if tbl := repositoryTable(recvType, object); tbl != "" {
		fc.contractSite(storage.ContractKindDB, true)
		kind := storage.EdgeReadsFrom
		if jpaWriteMethods[name] {
			kind = storage.EdgeWritesTo
		}
		fc.addEdge(src, kind, contract.DB(tbl), line, contract.ConfHeuristic,
			&storage.EdgeMeta{Args: args, Aliases: aliases})
	}

	sqlEdgesFromArgs(fc, src, line, args)
	fc.addEdge(src, storage.EdgeCall, name, line, contract.ConfHeuristic,
		&storage.EdgeMeta{Args: args, Aliases: aliases, Receiver: object, RecvType: recvType})
}

// ktAnnotation is a parsed Kotlin annotation attached to a declaration.
type ktAnnotation struct {
	name string
	node *sitter.Node
}

// ktAnnotations returns the annotations in a declaration's modifiers:
// @PostMapping("/x") parses as annotation > constructor_invocation
// (user_type + value_arguments); marker annotations as annotation > user_type.
func ktAnnotations(fc *fileCtx, decl *sitter.Node) []ktAnnotation {
	mods := firstChildOfType(decl, "modifiers")
	if mods == nil {
		return nil
	}
	var out []ktAnnotation
	for i := 0; i < int(mods.NamedChildCount()); i++ {
		a := mods.NamedChild(i)
		if a.Type() != "annotation" {
			continue
		}
		if ut := ktFirstDescendant(a, "user_type"); ut != nil {
			out = append(out, ktAnnotation{name: fc.text(ut), node: a})
		}
	}
	return out
}

// ktAnnotationValue extracts the primary string value of an annotation:
// the first resolvable argument in its value_arguments.
func (k *ktExtractor) ktAnnotationValue(fc *fileCtx, ann *sitter.Node) string {
	va := ktFirstDescendant(ann, "value_arguments")
	if va == nil {
		return ""
	}
	for i := 0; i < int(va.NamedChildCount()); i++ {
		if v, ok := k.consts.resolve(fc.text(va.NamedChild(i))); ok {
			return v
		}
	}
	return ""
}

// ktClassBasePath returns the class-level @RequestMapping path. Class
// annotations parse either into the declaration's modifiers or — a quirk of
// the Kotlin grammar — as prefix_expression siblings directly above the
// class, so both places are scanned textually.
func ktClassBasePath(fc *fileCtx, n *sitter.Node) string {
	txt := ""
	if mods := firstChildOfType(n, "modifiers"); mods != nil {
		txt = fc.text(mods)
	}
	for p := n.PrevNamedSibling(); p != nil && p.Type() == "prefix_expression"; p = p.PrevNamedSibling() {
		txt += "\n" + fc.text(p)
	}
	return annotationPathFromText(txt, "RequestMapping")
}

// annotationPathFromText finds `@<ann>("...")` in raw annotation text and
// returns the quoted string, bounded to the annotation's own arguments.
func annotationPathFromText(txt, ann string) string {
	i := strings.Index(txt, ann)
	if i < 0 {
		return ""
	}
	rest := txt[i+len(ann):]
	if j := strings.IndexByte(rest, '@'); j >= 0 {
		rest = rest[:j] // stop at the next annotation
	}
	q := strings.IndexByte(rest, '"')
	if q < 0 {
		return ""
	}
	rest = rest[q+1:]
	e := strings.IndexByte(rest, '"')
	if e < 0 {
		return ""
	}
	return rest[:e]
}

// ktFirstDescendant returns the first named descendant of the given type
// (depth-first), or nil.
func ktFirstDescendant(n *sitter.Node, typ string) *sitter.Node {
	if n == nil {
		return nil
	}
	if n.Type() == typ {
		return n
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		if r := ktFirstDescendant(n.NamedChild(i), typ); r != nil {
			return r
		}
	}
	return nil
}

func (k *ktExtractor) qualify(scope, name string) string {
	if scope != "" {
		return scope + "." + name
	}
	if k.pkg != "" {
		return k.pkg + "." + name
	}
	return name
}
