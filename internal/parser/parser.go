// Package parser — обёртка над tree-sitter для извлечения символов
// (function/method/class/struct/interface/var/const) из исходного кода
// языков Go, TypeScript/JavaScript, Python, Java.
//
// Использует биндинги github.com/smacker/go-tree-sitter (CGO).
package parser

import (
	"context"
	"strings"
	"sync"

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
}

// Parser извлекает символы из исходников.
type Parser struct {
	mu sync.Mutex
}

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
	ts := languageFor(lang, path)
	if ts == nil {
		return nil, nil // язык не поддерживается tree-sitter'ом — это нормально
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	tsp := sitter.NewParser()
	defer tsp.Close()
	tsp.SetLanguage(ts)

	tree, err := tsp.ParseCtx(ctx, nil, source)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()
	var symbols []Symbol
	walk(root, source, lang, "", &symbols)
	return symbols, nil
}

// ParseChunks разбивает файл на семантические куски с помощью дерева AST.
// Гарантирует покрытие всего файла. maxBytes — желаемый (но не жесткий для AST) лимит.
func (p *Parser) ParseChunks(ctx context.Context, lang, path string, source []byte, maxBytes int) []Symbol {
	ts := languageFor(lang, path)
	if ts == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	tsp := sitter.NewParser()
	defer tsp.Close()
	tsp.SetLanguage(ts)

	tree, err := tsp.ParseCtx(ctx, nil, source)
	if err != nil {
		return nil
	}
	defer tree.Close()

	imports := p.extractImports(tree.RootNode(), source, lang)

	var chunks []Symbol
	p.collectChunks(tree.RootNode(), source, maxBytes, "", imports, &chunks)
	return chunks
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
func walk(node *sitter.Node, source []byte, lang, parent string, out *[]Symbol) {
	count := int(node.NamedChildCount())
	for i := 0; i < count; i++ {
		ch := node.NamedChild(i)
		kind := canonicalKind(ch.Type())
		if kind != "" {
			name := extractName(ch, source)
			if name != "" {
				sym := Symbol{
					Name:      name,
					Kind:      kind,
					StartLine: int(ch.StartPoint().Row) + 1,
					EndLine:   int(ch.EndPoint().Row) + 1,
					StartByte: int(ch.StartByte()),
					EndByte:   int(ch.EndByte()),
					Parent:    parent,
					Signature: firstLine(source, ch),
				}
				*out = append(*out, sym)
				// для классов спускаемся внутрь, чтобы найти методы
				if kind == "class" || kind == "interface" {
					walk(ch, source, lang, name, out)
					continue
				}
			}
		}
		// рекурсивно идём в дочерние узлы, чтобы не пропустить функции,
		// объявленные внутри блоков (export, namespaces, и т.п.)
		walk(ch, source, lang, parent, out)
	}
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
