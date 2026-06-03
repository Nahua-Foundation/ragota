package astindex

import (
	"regexp"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
)

var jsDocTypeRef = regexp.MustCompile(`@(?:param|returns?|type|throws|see)\s+\{([^}]+)\}`)
var identRe = regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_]*`)

// extractJSDocRefs извлекает упоминания типов из JSDoc-комментария.
func extractJSDocRefs(doc string, srcIdx int, edges *[]pendingEdge, line int) {
	matches := jsDocTypeRef.FindAllStringSubmatch(doc, -1)
	for _, m := range matches {
		typeContent := m[1]
		idents := identRe.FindAllString(typeContent, -1)
		for _, id := range idents {
			switch id {
			case "string", "number", "boolean", "any", "void", "null", "undefined", "object", "Array", "Promise", "Map", "Set":
				continue
			}
			*edges = append(*edges, pendingEdge{
				srcIdx:  srcIdx,
				dstIdx:  -1,
				dstName: id,
				kind:    "reference",
				line:    line,
			})
		}
	}
}

// collectCalls обходит тело узла и собирает call_expression → edge call.
func collectCalls(node *sitter.Node, src []byte, srcIdx int, edges *[]pendingEdge) {
	var walk func(n *sitter.Node, first bool)
	walk = func(n *sitter.Node, first bool) {
		if !first && isUnitType(n.Type()) {
			return
		}
		cnt := int(n.NamedChildCount())
		for i := 0; i < cnt; i++ {
			ch := n.NamedChild(i)
			t := ch.Type()
			if t == "call_expression" || t == "method_invocation" || t == "object_creation_expression" {
				if name := callTarget(ch, src); name != "" {
					*edges = append(*edges, pendingEdge{
						srcIdx: srcIdx, dstIdx: -1, dstName: name, kind: "call",
						line: int(ch.StartPoint().Row) + 1,
					})
				}
			}
			if t == "type_identifier" || t == "scoped_type_identifier" || t == "generic_type" {
				if name := nodeText(ch, src); name != "" {
					*edges = append(*edges, pendingEdge{
						srcIdx: srcIdx, dstIdx: -1, dstName: name, kind: "reference",
						line: int(ch.StartPoint().Row) + 1,
					})
				}
			}
			walk(ch, false)
		}
	}
	walk(node, true)
}

func isUnitType(t string) bool {
	switch t {
	case "class_declaration", "class_definition", "interface_declaration", "enum_declaration",
		"type_declaration", "struct_declaration", "type_alias_declaration",
		"function_declaration", "function_definition", "method_definition", "method_declaration",
		"constructor_declaration", "function", "method_signature",
		"variable_declarator", "field_declaration", "module",
		"module_declaration", "internal_module", "namespace_declaration":
		return true
	}
	return false
}

// callTarget извлекает имя цели вызова.
func callTarget(call *sitter.Node, src []byte) string {
	if call.Type() == "object_creation_expression" {
		if t := call.ChildByFieldName("type"); t != nil {
			return nodeText(t, src)
		}
		return ""
	}
	if fn := call.ChildByFieldName("function"); fn != nil {
		if fn.Type() == "member_expression" {
			if prop := fn.ChildByFieldName("property"); prop != nil {
				return nodeText(prop, src)
			}
		}
		return nodeText(fn, src)
	}
	name := ""
	if obj := call.ChildByFieldName("object"); obj != nil {
		name = nodeText(obj, src) + "."
	}
	if nm := call.ChildByFieldName("name"); nm != nil {
		return name + nodeText(nm, src)
	}
	return ""
}

// findRequireTarget ищет require('...') / import('...').
func findRequireTarget(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	cnt := int(n.NamedChildCount())
	for i := 0; i < cnt; i++ {
		ch := n.NamedChild(i)
		if ch.Type() == "call_expression" {
			fn := ch.ChildByFieldName("function")
			if fn != nil {
				name := nodeText(fn, src)
				if name == "require" || name == "import" {
					if args := ch.ChildByFieldName("arguments"); args != nil {
						for j := 0; j < int(args.NamedChildCount()); j++ {
							a := args.NamedChild(j)
							t := a.Type()
							if t == "string" || t == "string_literal" || t == "template_string" {
								return strings.Trim(nodeText(a, src), "\"'`")
							}
						}
					}
				}
			}
		}
		if r := findRequireTarget(ch, src); r != "" {
			return r
		}
	}
	return ""
}

// importTarget извлекает имя импортируемого модуля.
func importTarget(n *sitter.Node, src []byte) string {
	if src2 := n.ChildByFieldName("source"); src2 != nil {
		return strings.Trim(nodeText(src2, src), "\"'`")
	}
	cnt := int(n.NamedChildCount())
	for i := 0; i < cnt; i++ {
		ch := n.NamedChild(i)
		if ch.Type() == "scoped_identifier" || ch.Type() == "identifier" {
			return nodeText(ch, src)
		}
		if ch.Type() == "string" || ch.Type() == "string_literal" {
			return strings.Trim(nodeText(ch, src), "\"'`")
		}
	}
	return strings.TrimSpace(nodeText(n, src))
}

func searchHeritage(n *sitter.Node, src []byte, srcIdx int, edges *[]pendingEdge) {
	t := n.Type()
	if t == "superclass" || t == "extends_clause" || t == "heritage" || t == "extends_type_clause" {
		if tname := firstTypeRef(n, src); tname != "" {
			*edges = append(*edges, pendingEdge{
				srcIdx: srcIdx, dstIdx: -1, dstName: tname, kind: "extends",
				line: int(n.StartPoint().Row) + 1,
			})
		}
		return
	}
	if t == "implements_clause" || t == "super_interfaces" {
		for i := 0; i < int(n.NamedChildCount()); i++ {
			c := n.NamedChild(i)
			if tname := firstTypeRef(c, src); tname != "" {
				*edges = append(*edges, pendingEdge{
					srcIdx: srcIdx, dstIdx: -1, dstName: tname, kind: "implements",
					line: int(c.StartPoint().Row) + 1,
				})
			}
		}
		return
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		searchHeritage(n.NamedChild(i), src, srcIdx, edges)
	}
}

// firstTypeRef ищет первое имя типа в подузле extends/implements.
func firstTypeRef(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	switch n.Type() {
	case "identifier", "type_identifier", "scoped_type_identifier", "scoped_identifier":
		return nodeText(n, src)
	}
	cnt := int(n.NamedChildCount())
	for i := 0; i < cnt; i++ {
		ch := n.NamedChild(i)
		switch ch.Type() {
		case "identifier", "type_identifier", "scoped_type_identifier", "scoped_identifier":
			return nodeText(ch, src)
		}
		if res := firstTypeRef(ch, src); res != "" {
			return res
		}
	}
	return ""
}
