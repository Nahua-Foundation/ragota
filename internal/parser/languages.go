package parser

// Файл реализует выбор tree-sitter грамматики по языку/пути и нормализацию
// типов AST-узлов в каноничные виды символов (canonicalKind).

import (
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
