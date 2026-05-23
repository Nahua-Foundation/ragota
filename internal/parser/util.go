package parser

// Файл содержит чистые helper'ы: firstLine — короткая «сигнатурная» строка
// узла, lineForByte — 1-based номер строки по байтовому смещению,
// indexByte — поиск байта в срезе без зависимости от bytes.IndexByte
// (минимизирует импорты пакета).

import (
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
)

// firstLine возвращает первую строку текста узла (обрезанную до 200 символов).
func firstLine(source []byte, n *sitter.Node) string {
	start := int(n.StartByte())
	end := int(n.EndByte())
	if end > len(source) {
		end = len(source)
	}
	chunk := source[start:end]
	if nl := indexByte(chunk, '\n'); nl >= 0 {
		chunk = chunk[:nl]
	}
	s := strings.TrimSpace(string(chunk))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// lineForByte возвращает 1-based номер строки для байтового смещения.
func lineForByte(source []byte, off int) int {
	if off < 0 {
		off = 0
	}
	if off > len(source) {
		off = len(source)
	}
	line := 1
	for i := 0; i < off; i++ {
		if source[i] == '\n' {
			line++
		}
	}
	return line
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
