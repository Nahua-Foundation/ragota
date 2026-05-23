// Package parser — обёртка над tree-sitter для извлечения символов
// (function/method/class/struct/interface/var/const) из исходного кода
// языков Go, TypeScript/JavaScript, Python, Java.
//
// Использует биндинги github.com/smacker/go-tree-sitter (CGO).
package parser

import (
	"context"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
)

// JS/JSX парсятся TypeScript/TSX грамматикой (TS — надмножество JS),
// чтобы не тянуть отдельный пакет github.com/smacker/go-tree-sitter/javascript,
// который конфликтует (ambiguous import) с подпакетом основного модуля.

// Symbol — извлечённый символ.
type Symbol struct {
	Name      string
	Kind      string
	StartLine int
	EndLine   int
	StartByte int
	EndByte   int
	Parent    string
	Signature string
	Imports   []string
	// Doc — лидирующий комментарий перед объявлением (если есть).
	// Для tree-sitter языков собирается из соседних `comment`-узлов,
	// расположенных непосредственно над декларацией.
	Doc string
}

// Parser извлекает символы из исходников.
type Parser struct{}

// New создаёт парсер.
func New() *Parser { return &Parser{} }

func languageFor(lang string, path string) *sitter.Language {
	switch lang {
	case "go":
		return golang.GetLanguage()
	case "python":
		return python.GetLanguage()
	case "java":
		return java.GetLanguage()
	case "javascript":
		// .jsx → TSX (надмножество с JSX), .js/.mjs/.cjs → TypeScript
		if strings.HasSuffix(path, ".jsx") {
			return tsx.GetLanguage()
		}
		return typescript.GetLanguage()
	case "typescript":
		if strings.HasSuffix(path, ".tsx") {
			return tsx.GetLanguage()
		}
		return typescript.GetLanguage()
	}
	return nil
}

// SupportedLanguages возвращает список поддерживаемых tree-sitter языков.
func SupportedLanguages() []string {
	return []string{"go", "typescript", "javascript", "python", "java"}
}

// Parse извлекает символы из source с учётом языка lang и пути path (нужно для .tsx vs .ts).
func (p *Parser) Parse(ctx context.Context, lang, path string, source []byte) ([]Symbol, error) {
	syms, _, err := p.ParseAll(ctx, lang, path, source, 1000000)
	return syms, err
}

// ParseChunks разбивает файл на семантические куски с помощью дерева AST.
// Гарантирует покрытие всего файла. maxBytes — желаемый (но не жесткий для AST) лимит.
func (p *Parser) ParseChunks(ctx context.Context, lang, path string, source []byte, maxBytes int) []Symbol {
	_, chunks, _ := p.ParseAll(ctx, lang, path, source, maxBytes)
	return chunks
}

// ParseAll выполняет полный разбор файла за один проход AST.
// Возвращает список символов и список семантических чанков.
func (p *Parser) ParseAll(ctx context.Context, lang, path string, source []byte, maxBytes int) ([]Symbol, []Symbol, error) {
	ts := languageFor(lang, path)
	if ts == nil {
		return nil, nil, nil
	}

	tsp := sitter.NewParser()
	defer tsp.Close()
	tsp.SetLanguage(ts)

	tree, err := tsp.ParseCtx(ctx, nil, source)
	if err != nil {
		return nil, nil, err
	}
	defer tree.Close()

	root := tree.RootNode()

	// 1. Извлекаем символы
	var symbols []Symbol
	walk(root, source, lang, "", &symbols)

	// 2. Извлекаем импорты (нужны для чанков)
	imports := p.extractImports(root, source, lang)

	// 3. Собираем чанки
	var chunks []Symbol
	p.collectChunks(root, source, maxBytes, "", imports, &chunks)

	return symbols, chunks, nil
}

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

func (p *Parser) extractImports(root *sitter.Node, source []byte, lang string) []string {
	var imports []string
	count := int(root.NamedChildCount())
	for i := 0; i < count; i++ {
		ch := root.NamedChild(i)
		t := ch.Type()
		switch lang {
		case "go":
			if t == "import_declaration" {
				// В Go импорты могут быть одиночные или блоком.
				// Проще всего взять текст узла и почистить.
				imports = append(imports, ch.Content(source))
			}
		case "python":
			if t == "import_statement" || t == "import_from_statement" {
				imports = append(imports, ch.Content(source))
			}
		case "java":
			if t == "import_declaration" {
				imports = append(imports, ch.Content(source))
			}
		case "typescript", "javascript":
			if t == "import_statement" {
				imports = append(imports, ch.Content(source))
			}
		}
	}
	return imports
}

// canonicalKind отображает типы узлов AST tree-sitter в каноничные виды символов.
// Если тип узла не распознан — возвращается "" (узел игнорируется как «не символ»).
// Один и тот же caceContent у нескольких языков: class_declaration используется
// и в Java, и в JS/TS — это нормально, обрабатываем через switch.
func canonicalKind(nodeType string) string {
	switch nodeType {
	// Go
	case "function_declaration":
		return "function"
	case "method_declaration":
		return "method"
	case "type_declaration", "type_spec":
		return "type"
	case "const_declaration":
		return "const"
	case "var_declaration":
		return "var"
	// Python
	case "function_definition":
		return "function"
	case "class_definition":
		return "class"
	// Java / JS / TS
	case "class_declaration":
		return "class"
	case "interface_declaration":
		return "interface"
	case "enum_declaration":
		return "enum"
	case "method_definition":
		return "method"
	// TS-specific
	case "type_alias_declaration":
		return "type"
	// JS-функции — берём только именованные function_declaration,
	// чтобы избегать шума от анонимных function/arrow.
	case "function_declaration_js": // плейсхолдер, реальные function в JS — function_declaration
		return "function"
	}
	return ""
}

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

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
