package config

import (
	"path/filepath"
	"strings"
)

// langSpec describes one language a file can be labelled with: its tag — the
// value File.Language carries and the AST parser registry keys on — and the
// extensions that identify it. indexable marks the subset that has an AST
// parser; the rest are recognized and stored for retrieval but never parsed.
type langSpec struct {
	name      string
	exts      []string
	indexable bool
}

// languages is the single table mapping file extensions to language tags.
// DetectLanguage, ASTLanguages and the AST parser registry all derive from it,
// so adding a language is one entry here rather than three hand-synced lists.
//
// Indexable entries are ordered so that ASTLanguages preserves the historical
// ordering (the validation message and the parser-registration order depend on
// it); the non-indexable ones follow.
var languages = []langSpec{
	{name: "go", exts: []string{".go"}, indexable: true},
	{name: "java", exts: []string{".java"}, indexable: true},
	{name: "kotlin", exts: []string{".kt", ".kts"}, indexable: true},
	{name: "csharp", exts: []string{".cs"}, indexable: true},
	{name: "typescript", exts: []string{".ts", ".tsx"}, indexable: true},
	{name: "javascript", exts: []string{".js", ".jsx"}, indexable: true},
	{name: "python", exts: []string{".py"}, indexable: true},
	{name: "proto", exts: []string{".proto"}, indexable: true},
	{name: "sql", exts: []string{".sql"}, indexable: true},
	{name: "yaml", exts: []string{".yaml", ".yml"}, indexable: true},
	{name: "json", exts: []string{".json"}, indexable: true},
	{name: "properties", exts: []string{".properties", ".env"}, indexable: true},
	{name: "toml", exts: []string{".toml"}},
	{name: "markdown", exts: []string{".md"}},
	{name: "rst", exts: []string{".rst"}},
	{name: "text", exts: []string{".txt"}},
}

// LanguageForFile returns the language tag for path based on its extension,
// or "" when no language recognizes it.
func LanguageForFile(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	for _, l := range languages {
		for _, e := range l.exts {
			if e == ext {
				return l.name
			}
		}
	}
	return ""
}

// ASTLanguages returns the languages the AST indexer can register parsers for.
func ASTLanguages() []string {
	out := make([]string, 0, len(languages))
	for _, l := range languages {
		if l.indexable {
			out = append(out, l.name)
		}
	}
	return out
}
