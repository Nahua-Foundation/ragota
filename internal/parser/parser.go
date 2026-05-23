// Package parser — обёртка над tree-sitter для извлечения символов
// (function/method/class/struct/interface/var/const) и семантического
// разбиения файлов на чанки. Поддерживаемые языки: Go, TypeScript/JavaScript,
// Python, Java. Использует биндинги github.com/smacker/go-tree-sitter (CGO).
//
// Реализация декомпозирована по доменам (все файлы — package parser):
//
//   - parser.go    — публичный API: типы Symbol/Parser, конструктор New,
//     функции Parse/ParseChunks/ParseAll (полный проход AST за один вызов);
//   - languages.go — выбор tree-sitter грамматики (languageFor),
//     SupportedLanguages и canonicalKind (нормализация типов узлов);
//   - symbols.go   — обход AST для извлечения символов (walk) и сбор
//     лидирующих комментариев/Python-docstring, extractName, isCommentNode;
//   - chunks.go    — collectChunks: семантическое разбиение по AST с
//     группировкой мелких детей до maxBytes;
//   - imports.go   — extractImports: список import-узлов для разных языков;
//   - util.go      — чистые helper'ы (firstLine, lineForByte, indexByte).
package parser

import (
	"context"

	sitter "github.com/smacker/go-tree-sitter"
)

// Symbol — извлечённый символ или семантический чанк.
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

// Parse извлекает символы из source с учётом языка lang и пути path
// (нужно для .tsx vs .ts).
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
