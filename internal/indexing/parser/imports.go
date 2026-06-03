package parser

// Файл реализует извлечение списка импортов из корня AST для разных
// языков. Возвращается срез строк — содержимое import-узлов как есть
// (без парсинга путей); используется как контекст для чанков (Symbol.Imports).

import sitter "github.com/smacker/go-tree-sitter"

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
