package astindex

import (
	"context"
	"database/sql"
	"path/filepath"
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

	var units []store.ASTUnit
	var edges []pendingEdge

	// module unit (idx 0).
	moduleIdx := 0
	units = append(units, store.ASTUnit{
		FilePath:  path,
		Language:  lang,
		Kind:      "module",
		Name:      moduleName,
		Qualified: moduleName,
		StartLine: 1,
		EndLine:   int(root.EndPoint().Row) + 1,
		StartByte: 0,
		EndByte:   len(src),
		Hash:      hashBytes(src),
	})

	walkTS(root, src, lang, moduleName, moduleIdx, moduleIdx, &units, &edges)
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

		// ---------- container types ----------
		case "class_declaration", "class_definition", "interface_declaration", "enum_declaration", "type_declaration", "struct_declaration", "type_alias_declaration":
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
			}
			qualified := parentQualified + "." + name
			idx := len(*units)
			*units = append(*units, makeUnit(src, ch, lang, kind, name, qualified, doc, parentIdx))

			// extends / implements (Java + TS).
			for j := 0; j < int(ch.NamedChildCount()); j++ {
				sub := ch.NamedChild(j)
				switch sub.Type() {
				case "superclass", "extends_clause":
					if tname := firstTypeRef(sub, src); tname != "" {
						*edges = append(*edges, pendingEdge{
							srcIdx: idx, dstIdx: -1, dstName: tname, kind: "extends",
							line: int(sub.StartPoint().Row) + 1,
						})
					}
				case "super_interfaces", "implements_clause", "extends_type_clause":
					// extends_type_clause встречается у TS interface-наследования
					// и должно тоже маппиться в extends.
					ekind := "implements"
					if sub.Type() == "extends_type_clause" {
						ekind = "extends"
					}
					for k := 0; k < int(sub.NamedChildCount()); k++ {
						if tname := firstTypeRef(sub.NamedChild(k), src); tname != "" {
							*edges = append(*edges, pendingEdge{
								srcIdx: idx, dstIdx: -1, dstName: tname, kind: ekind,
								line: int(sub.StartPoint().Row) + 1,
							})
						}
					}
				}
			}

			// Рекурсия внутрь body — за методами/полями.
			walkTS(ch, src, lang, qualified, idx, moduleIdx, units, edges)

		// ---------- functions / methods ----------
		case "function_declaration", "function_definition", "method_definition", "method_declaration",
			"constructor_declaration", "function":
			name := fieldName(ch, src)
			if name == "" {
				walkTS(ch, src, lang, parentQualified, parentIdx, moduleIdx, units, edges)
				continue
			}
			kind := "function"
			if t == "method_definition" || t == "method_declaration" || t == "constructor_declaration" {
				kind = "method"
			}
			qualified := parentQualified + "." + name
			idx := len(*units)
			*units = append(*units, makeUnit(src, ch, lang, kind, name, qualified, doc, parentIdx))

			// call edges внутри тела.
			collectCalls(ch, src, idx, edges)

		// ---------- variables / fields ----------
		case "variable_declarator", "field_declaration":
			name := fieldName(ch, src)
			if name == "" {
				walkTS(ch, src, lang, parentQualified, parentIdx, moduleIdx, units, edges)
				continue
			}
			kind := "variable"
			qualified := parentQualified + "." + name
			*units = append(*units, makeUnit(src, ch, lang, kind, name, qualified, doc, parentIdx))

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
		Signature: firstLine(body),
		Hash:      hashBytes([]byte(body)),
	}
}

// collectCalls обходит тело узла и собирает call_expression → edge call.
func collectCalls(node *sitter.Node, src []byte, srcIdx int, edges *[]pendingEdge) {
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
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
			walk(ch)
		}
	}
	walk(node)
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
	}
	return strings.TrimSpace(nodeText(n, src))
}

func fieldName(n *sitter.Node, src []byte) string {
	if nm := n.ChildByFieldName("name"); nm != nil {
		return nodeText(nm, src)
	}
	cnt := int(n.NamedChildCount())
	for i := 0; i < cnt; i++ {
		ch := n.NamedChild(i)
		switch ch.Type() {
		case "identifier", "property_identifier", "type_identifier":
			return nodeText(ch, src)
		}
	}
	return ""
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
