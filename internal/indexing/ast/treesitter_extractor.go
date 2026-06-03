package astindex

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
	"github.com/smacker/go-tree-sitter/typescript/typescript"

	"ragota/internal/store"
)

// parseWithTreeSitter — общий driver для Java / TypeScript / JavaScript:
// извлекает AST units (module/class/interface/method/function) и edges.
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

	if lang == "java" {
		for i := 0; i < int(root.NamedChildCount()); i++ {
			ch := root.NamedChild(i)
			if ch.Type() == "package_declaration" {
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
		if strings.HasSuffix(path, ".jsx") {
			return tsx.GetLanguage()
		}
		return typescript.GetLanguage()
	}
	return nil
}

// walkTS — рекурсивный обход дерева.
func walkTS(node *sitter.Node, src []byte, lang, parentQualified string, parentIdx, moduleIdx int, units *[]store.ASTUnit, edges *[]pendingEdge) {
	n := int(node.NamedChildCount())
	var pendingDoc string
	for i := 0; i < n; i++ {
		ch := node.NamedChild(i)
		t := ch.Type()

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
		case "import_declaration":
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

		case "expression_statement", "lexical_declaration", "variable_declaration":
			if name := findRequireTarget(ch, src); name != "" {
				*edges = append(*edges, pendingEdge{
					srcIdx: moduleIdx, dstIdx: -1, dstName: name, kind: "import",
					line: int(ch.StartPoint().Row) + 1,
				})
			}
			walkTS(ch, src, lang, parentQualified, parentIdx, moduleIdx, units, edges)
			continue

		case "class_declaration", "class_definition", "interface_declaration", "enum_declaration", "type_declaration", "struct_declaration", "type_alias_declaration",
			"module_declaration", "internal_module", "namespace_declaration":
			name := fieldName(ch, src)
			if name == "" {
				walkTS(ch, src, lang, parentQualified, parentIdx, moduleIdx, units, edges)
				continue
			}
			kind := tsContainerKind(t)
			qualified := parentQualified + "." + name
			idx := len(*units)
			*units = append(*units, makeUnit(src, ch, lang, kind, name, qualified, doc, parentIdx))

			if doc != "" {
				extractJSDocRefs(doc, idx, edges, int(ch.StartPoint().Row)+1)
			}

			for j := 0; j < int(ch.NamedChildCount()); j++ {
				sub := ch.NamedChild(j)
				stype := sub.Type()
				if stype == "class_body" || stype == "interface_body" || stype == "enum_body" {
					continue
				}
				searchHeritage(sub, src, idx, edges)
			}

			walkTS(ch, src, lang, qualified, idx, moduleIdx, units, edges)

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

			if doc != "" {
				extractJSDocRefs(doc, idx, edges, int(ch.StartPoint().Row)+1)
			}

			collectCalls(ch, src, idx, edges)
			walkTS(ch, src, lang, qualified, idx, moduleIdx, units, edges)

		case "arrow_function", "function_expression":
			walkTS(ch, src, lang, parentQualified, parentIdx, moduleIdx, units, edges)

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

			if doc != "" {
				extractJSDocRefs(doc, idx, edges, int(ch.StartPoint().Row)+1)
			}

			collectCalls(ch, src, idx, edges)
			walkTS(ch, src, lang, qualified, idx, moduleIdx, units, edges)

		default:
			walkTS(ch, src, lang, parentQualified, parentIdx, moduleIdx, units, edges)
		}
	}
}

func tsContainerKind(t string) string {
	switch t {
	case "interface_declaration":
		return "interface"
	case "enum_declaration":
		return "enum"
	case "type_declaration", "struct_declaration", "type_alias_declaration":
		return "type"
	case "module_declaration", "internal_module", "namespace_declaration":
		return "namespace"
	default:
		return "class"
	}
}
