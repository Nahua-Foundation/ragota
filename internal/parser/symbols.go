package parser

// Файл реализует обход AST для извлечения символов (walk) и сбор
// лидирующих комментариев/Python-docstring как Symbol.Doc, а также
// helper'ы извлечения имени узла (extractName) и распознавания
// comment-узлов (isCommentNode).

import (
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
)

// walk обходит AST и собирает символы верхнего и второго уровня
// (методы внутри классов получают parent).
//
// Дополнительно собирает лидирующие комментарии (//, /* */, ///, """ … """):
// перед каждой декларацией смотрим непосредственно предшествующие узлы
// типа `comment` (или `block_comment` / `line_comment`). Это работает для
// TS/JS/Java/Python (для Python docstring — отдельный node внутри тела,
// обрабатывается ниже).
func walk(node *sitter.Node, source []byte, lang, parent string, out *[]Symbol) {
	count := int(node.NamedChildCount())
	var pendingDoc string
	var pendingDocStart = -1
	for i := 0; i < count; i++ {
		ch := node.NamedChild(i)
		t := ch.Type()
		if isCommentNode(t) {
			text := strings.TrimSpace(ch.Content(source))
			if pendingDoc == "" {
				pendingDoc = text
				pendingDocStart = int(ch.StartByte())
			} else {
				pendingDoc += "\n" + text
			}
			continue
		}
		kind := canonicalKind(t)
		if kind != "" {
			name := extractName(ch, source)
			if name != "" {
				startByte := int(ch.StartByte())
				startLine := int(ch.StartPoint().Row) + 1
				doc := pendingDoc
				if doc != "" && pendingDocStart >= 0 {
					startByte = pendingDocStart
					// номер строки пересчитаем по байту
					startLine = lineForByte(source, startByte)
				}
				// Для Python: docstring — первый stmt в теле (string).
				if doc == "" && lang == "python" {
					doc = pythonDocstring(ch, source)
				}
				sym := Symbol{
					Name:      name,
					Kind:      kind,
					StartLine: startLine,
					EndLine:   int(ch.EndPoint().Row) + 1,
					StartByte: startByte,
					EndByte:   int(ch.EndByte()),
					Parent:    parent,
					Signature: firstLine(source, ch),
					Doc:       doc,
				}
				*out = append(*out, sym)
				// для классов спускаемся внутрь, чтобы найти методы
				if kind == "class" || kind == "interface" {
					walk(ch, source, lang, name, out)
					pendingDoc = ""
					pendingDocStart = -1
					continue
				}
			}
		}
		pendingDoc = ""
		pendingDocStart = -1
		// рекурсивно идём в дочерние узлы, чтобы не пропустить функции,
		// объявленные внутри блоков (export, namespaces, и т.п.)
		walk(ch, source, lang, parent, out)
	}
}

// isCommentNode распознаёт comment-узлы tree-sitter в разных грамматиках.
func isCommentNode(t string) bool {
	switch t {
	case "comment", "line_comment", "block_comment", "documentation_comment":
		return true
	}
	return false
}

// pythonDocstring извлекает docstring (первый string-stmt в теле функции/класса).
func pythonDocstring(n *sitter.Node, source []byte) string {
	body := n.ChildByFieldName("body")
	if body == nil {
		return ""
	}
	for i := 0; i < int(body.NamedChildCount()); i++ {
		ch := body.NamedChild(i)
		if ch.Type() == "expression_statement" && ch.NamedChildCount() > 0 {
			s := ch.NamedChild(0)
			if s.Type() == "string" {
				return strings.TrimSpace(s.Content(source))
			}
		}
		break
	}
	return ""
}

// extractName пытается получить имя символа из подходящего поля.
func extractName(n *sitter.Node, source []byte) string {
	// 1. Field "name" — самый частый случай (Go, Python, Java, JS methods)
	if name := n.ChildByFieldName("name"); name != nil {
		return name.Content(source)
	}
	// 2. type_spec в Go: первый identifier-чайлд
	for i := 0; i < int(n.NamedChildCount()); i++ {
		c := n.NamedChild(i)
		if c.Type() == "identifier" || c.Type() == "type_identifier" || c.Type() == "property_identifier" {
			return c.Content(source)
		}
	}
	return ""
}
