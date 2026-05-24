package astindex

import (
	"context"
	"database/sql"
	"path/filepath"
	"regexp"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
	"github.com/smacker/go-tree-sitter/typescript/typescript"

	"aitools/internal/store"
)

// parseWithTreeSitter — общий driver для Java / TypeScript / JavaScript:
// извлекает AST units (module/class/interface/method/function) и edges
// (call, import, extends, implements, reference).
//
// Для TypeScript/TSX используется грамматика typescript/tsx (надмножество JS),
// что позволяет покрыть .js/.jsx/.mjs/.cjs одним парсером.
func (i *Indexer) parseWithTreeSitter(ctx context.Context, lang, path string, src []byte) ([]store.ASTUnit, []pendingEdge, error) {
	tsLang := languageGrammar(lang, path)
	if tsLang == nil {
		return nil, nil, nil
	}

	p := sitter.NewParser()
	defer p.Close()
	p.SetLanguage(tsLang)
	tree, err := p.ParseCtx(ctx, nil, src)
	if err != nil {
		return nil, nil, err
	}
	defer tree.Close()
	root := tree.RootNode()

	moduleName := filepath.Base(path)
	parentQualified := moduleName

	// Для Java: пытаемся найти package declaration, чтобы построить правильный qualified name.
	if lang == "java" {
		for i := 0; i < int(root.NamedChildCount()); i++ {
			ch := root.NamedChild(i)
			if ch.Type() == "package_declaration" {
				// Внутри package_declaration обычно identifier или scoped_identifier.
				for j := 0; j < int(ch.NamedChildCount()); j++ {
					sub := ch.NamedChild(j)
					if sub.Type() == "identifier" || sub.Type() == "scoped_identifier" {
						parentQualified = nodeText(sub, src)
						break
					}
				}
				break
			}
		}
	}

	var units []store.ASTUnit
	var edges []pendingEdge

	// module unit (idx 0).
	moduleIdx := 0
	units = append(units, store.ASTUnit{
		FilePath:  path,
		Language:  lang,
		Kind:      "module",
		Name:      moduleName,
		Qualified: parentQualified,
		StartLine: 1,
		EndLine:   int(root.EndPoint().Row) + 1,
		StartByte: 0,
		EndByte:   len(src),
		Hash:      hashBytes(src),
	})

	walkTS(root, src, lang, parentQualified, moduleIdx, moduleIdx, &units, &edges)
	collectCalls(root, src, moduleIdx, &edges)
	return units, edges, nil
}

func languageGrammar(lang, path string) *sitter.Language {
	switch lang {
	case "java":
		return java.GetLanguage()
	case "typescript":
		if strings.HasSuffix(path, ".tsx") {
			return tsx.GetLanguage()
		}
		return typescript.GetLanguage()
	case "javascript":
		// JS парсим TS-грамматикой (TS — надмножество JS).
		if strings.HasSuffix(path, ".jsx") {
			return tsx.GetLanguage()
		}
		return typescript.GetLanguage()
	}
	return nil
}

// walkTS — рекурсивный обход дерева. parentIdx — индекс ближайшего
// container (class/interface) либо moduleIdx; parentQualified — его
// qualified-имя для построения имён детей.
func walkTS(node *sitter.Node, src []byte, lang, parentQualified string, parentIdx, moduleIdx int, units *[]store.ASTUnit, edges *[]pendingEdge) {
	n := int(node.NamedChildCount())
	var pendingDoc string
	for i := 0; i < n; i++ {
		ch := node.NamedChild(i)
		t := ch.Type()

		// Накапливаем лидирующие комментарии — назначим их следующему unit.
		if t == "comment" || t == "line_comment" || t == "block_comment" || t == "documentation_comment" {
			txt := strings.TrimSpace(string(src[ch.StartByte():ch.EndByte()]))
			if pendingDoc == "" {
				pendingDoc = txt
			} else {
				pendingDoc += "\n" + txt
			}
			continue
		}
		doc := pendingDoc
		pendingDoc = ""

		switch t {
		// ---------- imports ----------
		case "import_declaration":
			// Java + TS используют один и тот же node.Type() == "import_declaration"
			// для импортов. У TS встречается также "import_statement".
			if name := importTarget(ch, src); name != "" {
				*edges = append(*edges, pendingEdge{
					srcIdx: moduleIdx, dstIdx: -1, dstName: name, kind: "import",
					line: int(ch.StartPoint().Row) + 1,
				})
			}
		case "import_statement":
			if name := importTarget(ch, src); name != "" {
				*edges = append(*edges, pendingEdge{
					srcIdx: moduleIdx, dstIdx: -1, dstName: name, kind: "import",
					line: int(ch.StartPoint().Row) + 1,
				})
			}

		// ---------- CommonJS require / dynamic import ----------
		// JS/TS: `const x = require('foo')` / `require('foo')` / `import('foo')`.
		// Эти конструкции не покрываются import_statement, но семантически
		// эквивалентны импорту модуля — поэтому эмулируем import-edge.
		case "expression_statement", "lexical_declaration", "variable_declaration":
			if name := findRequireTarget(ch, src); name != "" {
				*edges = append(*edges, pendingEdge{
					srcIdx: moduleIdx, dstIdx: -1, dstName: name, kind: "import",
					line: int(ch.StartPoint().Row) + 1,
				})
			}
			// и продолжаем рекурсию ниже через default-ветку.
			walkTS(ch, src, lang, parentQualified, parentIdx, moduleIdx, units, edges)
			continue

		// ---------- container types ----------
		case "class_declaration", "class_definition", "interface_declaration", "enum_declaration", "type_declaration", "struct_declaration", "type_alias_declaration",
			"module_declaration", "internal_module", "namespace_declaration":
			name := fieldName(ch, src)
			if name == "" {
				walkTS(ch, src, lang, parentQualified, parentIdx, moduleIdx, units, edges)
				continue
			}
			kind := "class"
			switch t {
			case "interface_declaration":
				kind = "interface"
			case "enum_declaration":
				kind = "enum"
			case "type_declaration", "struct_declaration", "type_alias_declaration":
				kind = "type"
			case "module_declaration", "internal_module", "namespace_declaration":
				kind = "namespace"
			}
			qualified := parentQualified + "." + name
			idx := len(*units)
			*units = append(*units, makeUnit(src, ch, lang, kind, name, qualified, doc, parentIdx))

			// Извлекаем ссылки из JSDoc.
			if doc != "" {
				extractJSDocRefs(doc, idx, edges, int(ch.StartPoint().Row)+1)
			}

			// extends / implements (Java + TS).
			for j := 0; j < int(ch.NamedChildCount()); j++ {
				sub := ch.NamedChild(j)
				stype := sub.Type()
				if stype == "class_body" || stype == "interface_body" || stype == "enum_body" {
					continue
				}
				searchHeritage(sub, src, idx, edges)
			}

			// Рекурсия внутрь body — за методами/полями.
			walkTS(ch, src, lang, qualified, idx, moduleIdx, units, edges)

		// ---------- functions / methods ----------
		case "function_declaration", "function_definition", "method_definition", "method_declaration",
			"constructor_declaration", "function", "method_signature":
			name := fieldName(ch, src)
			if name == "" {
				walkTS(ch, src, lang, parentQualified, parentIdx, moduleIdx, units, edges)
				continue
			}
			kind := "function"
			if t == "method_definition" || t == "method_declaration" || t == "constructor_declaration" || t == "method_signature" {
				kind = "method"
			}
			qualified := parentQualified + "." + name
			idx := len(*units)
			*units = append(*units, makeUnit(src, ch, lang, kind, name, qualified, doc, parentIdx))

			// Извлекаем ссылки из JSDoc.
			if doc != "" {
				extractJSDocRefs(doc, idx, edges, int(ch.StartPoint().Row)+1)
			}

			// call edges внутри тела.
			collectCalls(ch, src, idx, edges)

			// Рекурсия внутрь тела (для вложенных функций/классов).
			walkTS(ch, src, lang, qualified, idx, moduleIdx, units, edges)

		case "arrow_function", "function_expression":
			// Для анонимных функций на месте (не назначенных переменной)
			// мы не создаем отдельный ASTUnit, чтобы не засорять граф,
			// но рекурсируем внутрь для поиска вложенных именованных сущностей.
			walkTS(ch, src, lang, parentQualified, parentIdx, moduleIdx, units, edges)

		// ---------- variables / fields ----------
		case "variable_declarator", "field_declaration":
			name := fieldName(ch, src)
			if name == "" {
				walkTS(ch, src, lang, parentQualified, parentIdx, moduleIdx, units, edges)
				continue
			}
			kind := "variable"
			if t == "field_declaration" {
				kind = "field"
			}
			qualified := parentQualified + "." + name
			idx := len(*units)
			*units = append(*units, makeUnit(src, ch, lang, kind, name, qualified, doc, parentIdx))

			// Извлекаем ссылки из JSDoc.
			if doc != "" {
				extractJSDocRefs(doc, idx, edges, int(ch.StartPoint().Row)+1)
			}

			// Собираем вызовы в инициализаторе переменной.
			collectCalls(ch, src, idx, edges)

			// Рекурсия внутрь для поиска вложенных сущностей (например, анонимных функций).
			walkTS(ch, src, lang, qualified, idx, moduleIdx, units, edges)

		default:
			walkTS(ch, src, lang, parentQualified, parentIdx, moduleIdx, units, edges)
		}
	}
}

// makeUnit формирует store.ASTUnit для tree-sitter узла.
func makeUnit(src []byte, n *sitter.Node, lang, kind, name, qualified, doc string, parentIdx int) store.ASTUnit {
	startByte := int(n.StartByte())
	endByte := int(n.EndByte())
	if endByte > len(src) {
		endByte = len(src)
	}
	body := ""
	if startByte < endByte {
		body = string(src[startByte:endByte])
	}
	return store.ASTUnit{
		FilePath:  fileFromNode(n),
		Language:  lang,
		Kind:      kind,
		Name:      name,
		Qualified: qualified,
		Doc:       doc,
		ParentID:  sql.NullInt64{Int64: int64(parentIdx), Valid: true},
		StartLine: int(n.StartPoint().Row) + 1,
		EndLine:   int(n.EndPoint().Row) + 1,
		StartByte: startByte,
		EndByte:   endByte,
		NameStartLine: func() int {
			if nm := nameNode(n); nm != nil {
				return int(nm.StartPoint().Row) + 1
			}
			return int(n.StartPoint().Row) + 1
		}(),
		NameStartCol: func() int {
			if nm := nameNode(n); nm != nil {
				return int(nm.StartPoint().Column)
			}
			return int(n.StartPoint().Column)
		}(),
		Signature: firstLine(body),
		Hash:      hashBytes([]byte(body)),
	}
}

// collectCalls обходит тело узла и собирает call_expression → edge call,
// а также ищет упоминания типов (reference). Она не заходит внутрь вложенных
// узлов, которые сами являются ASTUnit (функции, классы и т.д.), так как
// для них вызовы будут собраны отдельно.
func collectCalls(node *sitter.Node, src []byte, srcIdx int, edges *[]pendingEdge) {
	var walk func(n *sitter.Node, first bool)
	walk = func(n *sitter.Node, first bool) {
		if !first && isUnitType(n.Type()) {
			return // Останавливаемся на границе другого юнита.
		}

		cnt := int(n.NamedChildCount())
		for i := 0; i < cnt; i++ {
			ch := n.NamedChild(i)
			t := ch.Type()
			// JS/TS: call_expression; Java: method_invocation / object_creation_expression.
			if t == "call_expression" || t == "method_invocation" || t == "object_creation_expression" {
				if name := callTarget(ch, src); name != "" {
					*edges = append(*edges, pendingEdge{
						srcIdx: srcIdx, dstIdx: -1, dstName: name, kind: "call",
						line: int(ch.StartPoint().Row) + 1,
					})
				}
			}

			// References: типы параметров, возвращаемые типы, типы переменных.
			// В TS/Java часто используется type_identifier или аналоги.
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

// callTarget извлекает имя цели вызова (function/method).
// Для object_creation_expression (Java new Foo()) использует поле "type".
func callTarget(call *sitter.Node, src []byte) string {
	if call.Type() == "object_creation_expression" {
		if t := call.ChildByFieldName("type"); t != nil {
			return nodeText(t, src)
		}
		return ""
	}
	// JS/TS call_expression: field "function". Java method_invocation: "name" + optional "object".
	if fn := call.ChildByFieldName("function"); fn != nil {
		// В TS вызов метода l.log() имеет fn.Type() == "member_expression"
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

// findRequireTarget рекурсивно ищет внутри узла вызов require('...') /
// import('...') и возвращает имя модуля. Возвращает "" если не найден.
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

// importTarget извлекает имя импортируемого модуля/пакета.
func importTarget(n *sitter.Node, src []byte) string {
	// Java: import_declaration → scoped_identifier (foo.bar.Baz).
	// TS: import_statement → source: string_literal.
	if src2 := n.ChildByFieldName("source"); src2 != nil {
		return strings.Trim(nodeText(src2, src), "\"'`")
	}
	// Java имеет одиночный scoped_identifier потомок.
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
	// Сам узел может быть идентификатором / type_identifier.
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
		// Рекурсивно ищем внутри (например в extends_clause)
		if res := firstTypeRef(ch, src); res != "" {
			return res
		}
	}
	return ""
}

func nameNode(n *sitter.Node) *sitter.Node {
	if n == nil {
		return nil
	}
	if nm := n.ChildByFieldName("name"); nm != nil {
		return nm
	}
	cnt := int(n.NamedChildCount())
	for i := 0; i < cnt; i++ {
		ch := n.NamedChild(i)
		switch ch.Type() {
		case "identifier", "property_identifier", "type_identifier":
			return ch
		}
	}
	return nil
}

func fieldName(n *sitter.Node, src []byte) string {
	return nodeText(nameNode(n), src)
}

func nodeText(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	s, e := int(n.StartByte()), int(n.EndByte())
	if s < 0 || e > len(src) || s >= e {
		return ""
	}
	return string(src[s:e])
}

func fileFromNode(n *sitter.Node) string {
	// tree-sitter не хранит имя файла на узле; вернём пусто, заполнится
	// вызывающим (parseWithTreeSitter) через сам path.
	return ""
}

var jsDocTypeRef = regexp.MustCompile(`@(?:param|returns?|type|throws|see)\s+\{([^}]+)\}`)
var identRe = regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_]*`)

// extractJSDocRefs извлекает упоминания типов из JSDoc-комментария.
// Она ищет конструкции {Type} и вытягивает из них все идентификаторы.
func extractJSDocRefs(doc string, srcIdx int, edges *[]pendingEdge, line int) {
	matches := jsDocTypeRef.FindAllStringSubmatch(doc, -1)
	for _, m := range matches {
		typeContent := m[1]
		// Внутри { ... } может быть сложный тип, например Array<User> или User|null.
		// Извлекаем все слова, похожие на идентификаторы.
		idents := identRe.FindAllString(typeContent, -1)
		for _, id := range idents {
			// Пропускаем встроенные примитивы JS.
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
