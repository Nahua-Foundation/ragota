package context

import (
	"os"
	"path/filepath"
)

// Language — обнаруженный язык программирования.
type Language struct {
	Name       string
	Extensions []string
	Confidence int // 0-100
}

// langMarker — маркер для авто-детекции языка по корневому файлу.
type langMarker struct {
	file    string
	lang    string
	exts    []string
	confirm int
}

var langMarkers = []langMarker{
	{file: "go.mod", lang: "go", exts: []string{".go"}, confirm: 95},
	{file: "go.sum", lang: "go", exts: []string{".go"}, confirm: 80},
	{file: "Cargo.toml", lang: "rust", exts: []string{".rs"}, confirm: 95},
	{file: "Cargo.lock", lang: "rust", exts: []string{".rs"}, confirm: 80},
	{file: "package.json", lang: "typescript", exts: []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"}, confirm: 70},
	{file: "tsconfig.json", lang: "typescript", exts: []string{".ts", ".tsx"}, confirm: 95},
	{file: "requirements.txt", lang: "python", exts: []string{".py"}, confirm: 90},
	{file: "pyproject.toml", lang: "python", exts: []string{".py"}, confirm: 95},
	{file: "setup.py", lang: "python", exts: []string{".py"}, confirm: 90},
	{file: "setup.cfg", lang: "python", exts: []string{".py"}, confirm: 85},
	{file: "Pipfile", lang: "python", exts: []string{".py"}, confirm: 90},
	{file: "pom.xml", lang: "java", exts: []string{".java"}, confirm: 95},
	{file: "build.gradle", lang: "java", exts: []string{".java"}, confirm: 95},
	{file: "build.gradle.kts", lang: "kotlin", exts: []string{".kt", ".kts"}, confirm: 95},
	{file: "Gemfile", lang: "ruby", exts: []string{".rb"}, confirm: 95},
	{file: "composer.json", lang: "php", exts: []string{".php"}, confirm: 95},
	{file: "mix.exs", lang: "elixir", exts: []string{".ex", ".exs"}, confirm: 95},
	{file: "CMakeLists.txt", lang: "cpp", exts: []string{".cpp", ".cc", ".cxx", ".h", ".hpp"}, confirm: 90},
	{file: "Makefile", lang: "c", exts: []string{".c", ".h"}, confirm: 30}, // low — Makefile is generic
	{file: "*.sln", lang: "csharp", exts: []string{".cs"}, confirm: 90},
	{file: "pubspec.yaml", lang: "dart", exts: []string{".dart"}, confirm: 95},
	{file: "Package.swift", lang: "swift", exts: []string{".swift"}, confirm: 95},
}

// DetectLanguages обнаруживает языки проекта по корневым файлам.
func DetectLanguages(root string) []Language {
	langMap := make(map[string]*Language)

	for _, m := range langMarkers {
		path := filepath.Join(root, m.file)
		if _, err := os.Stat(path); err == nil {
			if existing, ok := langMap[m.lang]; ok {
				if m.confirm > existing.Confidence {
					existing.Confidence = m.confirm
				}
			} else {
				langMap[m.lang] = &Language{
					Name:       m.lang,
					Extensions: m.exts,
					Confidence: m.confirm,
				}
			}
		}
	}

	// Disambiguate TypeScript vs JavaScript
	if ts, ok := langMap["typescript"]; ok {
		if _, hasTsconfig := os.Stat(filepath.Join(root, "tsconfig.json")); hasTsconfig == nil {
			ts.Confidence = 95
			ts.Extensions = []string{".ts", ".tsx"}
		}
	}

	var langs []Language
	for _, l := range langMap {
		langs = append(langs, *l)
	}
	return langs
}
