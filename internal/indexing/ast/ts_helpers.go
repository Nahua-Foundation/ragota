package astindex

import (
	"database/sql"

	sitter "github.com/smacker/go-tree-sitter"

	"ragota/internal/store"
)

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
	nm := nameNode(n)
	nameLine := int(n.StartPoint().Row) + 1
	nameCol := int(n.StartPoint().Column)
	if nm != nil {
		nameLine = int(nm.StartPoint().Row) + 1
		nameCol = int(nm.StartPoint().Column)
	}
	return store.ASTUnit{
		FilePath:      "", // заполняется вызывающим
		Language:      lang,
		Kind:          kind,
		Name:          name,
		Qualified:     qualified,
		Doc:           doc,
		ParentID:      sql.NullInt64{Int64: int64(parentIdx), Valid: true},
		StartLine:     int(n.StartPoint().Row) + 1,
		EndLine:       int(n.EndPoint().Row) + 1,
		StartByte:     startByte,
		EndByte:       endByte,
		NameStartLine: nameLine,
		NameStartCol:  nameCol,
		Signature:     firstLine(body),
		Hash:          hashBytes([]byte(body)),
	}
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
