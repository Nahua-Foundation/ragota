package parser

// Файл реализует семантическое разбиение файла на чанки по AST:
// collectChunks рекурсивно обходит дерево и формирует Symbol{Kind:"chunk"}
// с размером не более maxBytes, при необходимости группируя соседних
// мелких детей в один чанк.

import sitter "github.com/smacker/go-tree-sitter"

func (p *Parser) collectChunks(n *sitter.Node, source []byte, maxBytes int, parent string, imports []string, out *[]Symbol) {
	start := int(n.StartByte())
	end := int(n.EndByte())
	size := end - start

	// Определяем parent для вложенных структур
	currentParent := parent
	kind := canonicalKind(n.Type())
	if kind == "class" || kind == "interface" || kind == "type" {
		if name := extractName(n, source); name != "" {
			currentParent = name
		}
	}

	// Если узел целиком влезает — это отличный чанк.
	if size <= maxBytes {
		*out = append(*out, Symbol{
			Kind:      "chunk",
			StartByte: start,
			EndByte:   end,
			StartLine: int(n.StartPoint().Row) + 1,
			EndLine:   int(n.EndPoint().Row) + 1,
			Signature: n.Type(),
			Parent:    parent,
			Imports:   imports,
		})
		return
	}

	// Узел слишком большой, идем в детей.
	childCount := int(n.ChildCount())
	if childCount == 0 {
		// Лист (например, огромный строковый литерал или комментарий)
		*out = append(*out, Symbol{
			Kind:      "chunk",
			StartByte: start,
			EndByte:   end,
			StartLine: int(n.StartPoint().Row) + 1,
			EndLine:   int(n.EndPoint().Row) + 1,
			Signature: n.Type(),
		})
		return
	}

	// Группируем детей, чтобы чанки не были слишком мелкими.
	var groupStart = -1
	var groupEnd = -1
	var groupStartRow = -1
	var groupEndRow = -1
	var currentSize = 0

	for i := 0; i < childCount; i++ {
		ch := n.Child(i)
		chStart := int(ch.StartByte())
		chEnd := int(ch.EndByte())
		chSize := chEnd - chStart

		if chSize > maxBytes {
			// Сброс текущей группы перед обработкой большого ребенка.
			if groupStart != -1 {
				*out = append(*out, Symbol{
					Kind:      "chunk",
					StartByte: groupStart,
					EndByte:   groupEnd,
					StartLine: groupStartRow + 1,
					EndLine:   groupEndRow + 1,
					Signature: "group",
					Parent:    parent,
					Imports:   imports,
				})
				groupStart = -1
				currentSize = 0
			}
			p.collectChunks(ch, source, maxBytes, currentParent, imports, out)
			continue
		}

		if currentSize > 0 && currentSize+chSize > maxBytes {
			// Сброс группы, если следующий ребенок не влезает.
			*out = append(*out, Symbol{
				Kind:      "chunk",
				StartByte: groupStart,
				EndByte:   groupEnd,
				StartLine: groupStartRow + 1,
				EndLine:   groupEndRow + 1,
				Signature: "group",
				Parent:    parent,
				Imports:   imports,
			})
			groupStart = -1
			currentSize = 0
		}

		if groupStart == -1 {
			groupStart = chStart
			groupStartRow = int(ch.StartPoint().Row)
		}
		groupEnd = chEnd
		groupEndRow = int(ch.EndPoint().Row)
		currentSize += chSize
	}

	// Хвост
	if groupStart != -1 {
		*out = append(*out, Symbol{
			Kind:      "chunk",
			StartByte: groupStart,
			EndByte:   groupEnd,
			StartLine: groupStartRow + 1,
			EndLine:   groupEndRow + 1,
			Signature: "group",
			Parent:    parent,
			Imports:   imports,
		})
	}
}
