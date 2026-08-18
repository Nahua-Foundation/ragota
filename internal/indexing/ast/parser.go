// The tree-sitter parser core: one generic parser per language, the
// language registry, and the extractor plumbing every lang_*.go implements.
package ast

import (
	"fmt"

	"github.com/Nahua-Foundation/ragota/internal/storage"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/csharp"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/kotlin"
	"github.com/smacker/go-tree-sitter/python"
	tsx "github.com/smacker/go-tree-sitter/typescript/tsx"
	typescript "github.com/smacker/go-tree-sitter/typescript/typescript"
)

// TreeSitterParser is a generic tree-sitter based parser.
type TreeSitterParser struct {
	lang string
}

// NewTreeSitterParser creates a new tree-sitter parser.
func NewTreeSitterParser(lang string) *TreeSitterParser {
	return &TreeSitterParser{lang: lang}
}

// sitterLanguage returns the tree-sitter language for this parser.
func (p *TreeSitterParser) sitterLanguage() (*sitter.Language, error) {
	switch p.lang {
	case "go":
		return golang.GetLanguage(), nil
	case "java":
		return java.GetLanguage(), nil
	case "kotlin":
		return kotlin.GetLanguage(), nil
	case "csharp":
		return csharp.GetLanguage(), nil
	case "typescript":
		return typescript.GetLanguage(), nil
	case "tsx":
		return tsx.GetLanguage(), nil
	case "javascript":
		return javascript.GetLanguage(), nil
	case "python":
		return python.GetLanguage(), nil
	default:
		return nil, fmt.Errorf("unsupported language: %s", p.lang)
	}
}

// extractor is a language-specific unit/edge extractor.
type extractor interface {
	extract(fc *fileCtx, root *sitter.Node)
}

func (p *TreeSitterParser) newExtractor() (extractor, error) {
	switch p.lang {
	case "go":
		return &goExtractor{}, nil
	case "java":
		return &javaExtractor{}, nil
	case "kotlin":
		return &ktExtractor{}, nil
	case "csharp":
		return &csharpExtractor{}, nil
	case "typescript", "tsx", "javascript":
		return &tsExtractor{}, nil
	case "python":
		return &pyExtractor{}, nil
	default:
		return nil, fmt.Errorf("unsupported language: %s", p.lang)
	}
}

// Parse parses a file and extracts AST units and edges.
//
// Edges reference their source unit via a positional marker ("#<idx>" into the
// returned units slice); the indexer rewrites markers to storage IDs after the
// units are persisted.
func (p *TreeSitterParser) Parse(filePath, content string) ([]*storage.ASTUnit, []*storage.Edge, error) {
	facts, err := p.ParseFacts(filePath, content)
	if err != nil {
		return nil, nil, err
	}
	return facts.Units, facts.Edges, nil
}

// ParseFacts parses a file and returns everything the repository-level passes
// need from it: the units and edges Parse returns, plus the facts that only
// make sense once other files are in view — the local outbound wrappers and
// this file's contract-coverage counters.
func (p *TreeSitterParser) ParseFacts(filePath, content string) (*fileFacts, error) {
	lang, err := p.sitterLanguage()
	if err != nil {
		return nil, err
	}
	ext, err := p.newExtractor()
	if err != nil {
		return nil, err
	}

	parser := sitter.NewParser()
	// The parser owns a C allocation that the garbage collector only reclaims
	// through a finalizer. A parser is a tiny Go object, so nothing pressures
	// the GC into running, and indexing thousands of files kept gigabytes of
	// tree-sitter memory alive. Release it explicitly.
	defer parser.Close()
	parser.SetLanguage(lang)

	tree := parser.Parse(nil, []byte(content))
	if tree == nil {
		return nil, fmt.Errorf("failed to parse")
	}
	defer tree.Close()

	fc := &fileCtx{path: filePath, lang: p.lang, src: []byte(content), cov: newCoverage()}
	ext.extract(fc, tree.RootNode())

	// The wrapper table is built before the same-file linking runs, so an edge
	// this pass adds can never turn its own call site into another wrapper:
	// following a helper stays one level deep.
	wrappers := fc.wrappers()
	fc.linkWrappers(wrappers)

	for _, u := range fc.units {
		if u.Hash == "" && u.StartByte >= 0 && u.EndByte <= len(content) && u.StartByte < u.EndByte {
			u.Hash = hashString(content[u.StartByte:u.EndByte])
		}
	}

	return &fileFacts{Units: fc.units, Edges: fc.edges, Wrappers: wrappers,
		Coverage: fc.cov, Tables: fc.tables, Consts: fc.consts}, nil
}

// Language returns the language name.
func (p *TreeSitterParser) Language() string {
	return p.lang
}

// GetParserForLanguage returns a parser for the given language.
func GetParserForLanguage(lang string) Parser {
	switch lang {
	case "proto":
		return newProtoParser()
	case "sql":
		return newSQLParser()
	case "yaml", "json", "properties":
		return newConfigParser(lang)
	default:
		return NewTreeSitterParser(lang)
	}
}

// RegisterDefaultParsers registers default parsers for common languages.
func RegisterDefaultParsers(indexer *Indexer) {
	for _, lang := range []string{
		"go", "java", "kotlin", "csharp", "typescript", "javascript", "python",
		"proto", "sql", "yaml", "json", "properties",
	} {
		indexer.RegisterParser(GetParserForLanguage(lang))
	}
}

// ---------------------------------------------------------------------------
// fileCtx: shared per-file extraction state
// ---------------------------------------------------------------------------
