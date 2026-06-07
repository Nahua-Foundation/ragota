package context

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// TermCategory — абстрактная категория доменного термина.
type TermCategory string

const (
	CategoryEntity        TermCategory = "entity"         // сущности предметной области
	CategoryProcess       TermCategory = "process"        // бизнес-процессы
	CategoryInterface     TermCategory = "interface"      // интерфейсы взаимодействия
	CategoryInfrastructure TermCategory = "infrastructure" // инфраструктура
	CategoryUnknown       TermCategory = "unknown"        // не определена
)

// Term — доменный термин, извлечённый из проекта.
type Term struct {
	Name     string
	Freq     int
	Category TermCategory
	Source   string // "filename", "code", "docs"
}

// noiseTerms — технические паттерны, которые не являются доменными терминами.
// Универсальный список для любых проектов (Go, Java, Python, TS/JS, Rust, etc.).
// НЕ включаем архитектурные паттерны (service, handler, controller, model, repository),
// т.к. они могут быть доменными терминами в конкретных проектах.
var noiseTerms = map[string]bool{
	// Test-related
	"test": true, "tests": true, "spec": true, "specs": true,
	"mock": true, "mocks": true, "stub": true, "stubs": true,
	// Utility
	"util": true, "utils": true, "helper": true, "helpers": true,
	// Project structure
	"internal": true, "pkg": true, "cmd": true, "lib": true,
	"src": true, "app": true, "core": true, "common": true,
	// Technical protocols
	"http": true, "https": true, "grpc": true, "rest": true,
	// Config
	"config": true, "configs": true, "conf": true, "settings": true,
	// Files
	"readme": true, "changelog": true, "license": true,
	"gitignore": true, "dockerignore": true, "editorconfig": true,
	// Technical
	"index": true, "main": true, "init": true,
	"error": true, "errors": true, "exception": true, "exceptions": true,
	"request": true, "response": true, "dto": true, "vo": true,
	"interface": true, "abstract": true, "base": true, "impl": true,
	// Versions
	"v1": true, "v2": true, "v3": true, "v4": true, "v5": true,
	// File formats
	"proto": true, "protobuf": true,
	"json": true, "xml": true, "yaml": true, "toml": true,
	"js": true, "ts": true, "tsx": true, "jsx": true, "py": true,
	"md": true, "txt": true, "html": true, "css": true, "scss": true,
}

// ExtractTermsFromNames извлекает доменные термины из имён файлов и директорий.
func ExtractTermsFromNames(root string, skipDirs map[string]bool) map[string]*Term {
	terms := make(map[string]*Term)

	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}

		if d.IsDir() {
			base := filepath.Base(rel)
			if base == ".git" || base == ".hg" || base == ".svn" {
				return filepath.SkipDir
			}
			if skipDirs != nil && skipDirs[base] {
				return filepath.SkipDir
			}
			// Добавляем термины из имени директории
			for _, token := range tokenize(base) {
				if !isNoise(token) {
					addTerm(terms, token, "filename")
				}
			}
			return nil
		}

		// Термины из имени файла (без расширения)
		name := filepath.Base(path)
		ext := filepath.Ext(name)
		nameWithoutExt := strings.TrimSuffix(name, ext)
		// Для составных расширений — убираем все точки
		nameWithoutExt = strings.Split(nameWithoutExt, ".")[0]

		for _, token := range tokenize(nameWithoutExt) {
			if !isNoise(token) {
				addTerm(terms, token, "filename")
			}
		}

		return nil
	})

	return terms
}

// FilterSignificantTerms возвращает только значимые термины (freq >= minFreq).
func FilterSignificantTerms(terms map[string]*Term, minFreq int) []*Term {
	var result []*Term
	for _, t := range terms {
		if t.Freq >= minFreq {
			result = append(result, t)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Freq > result[j].Freq
	})
	return result
}

// tokenize разбивает имя на токены по camelCase, snake_case, kebab-case.
func tokenize(name string) []string {
	// Заменяем дефисы и подчёркивания на пробелы
	name = strings.ReplaceAll(name, "-", " ")
	name = strings.ReplaceAll(name, "_", " ")

	var tokens []string
	var current strings.Builder

	for i, r := range name {
		if r == ' ' || r == '.' {
			if current.Len() > 0 {
				tokens = append(tokens, strings.ToLower(current.String()))
				current.Reset()
			}
			continue
		}

		// camelCase split
		if i > 0 && unicode.IsUpper(r) {
			prev := rune(name[i-1])
			split := false
			if unicode.IsLower(prev) {
				// lowerUpper: userProfile → user|Profile
				split = true
			} else if unicode.IsUpper(prev) && i+1 < len(name) {
				next := rune(name[i+1])
				if unicode.IsLower(next) {
					// HTTPClient → HTTP|Client
					split = true
				}
			}
			if split && current.Len() > 0 {
				tokens = append(tokens, strings.ToLower(current.String()))
				current.Reset()
			}
		}

		current.WriteRune(r)
	}

	if current.Len() > 0 {
		tokens = append(tokens, strings.ToLower(current.String()))
	}

	// Фильтруем слишком короткие токены
	var filtered []string
	for _, t := range tokens {
		if len(t) >= 3 {
			filtered = append(filtered, t)
		}
	}

	return filtered
}

func isNoise(token string) bool {
	return noiseTerms[strings.ToLower(token)] || len(token) < 3
}

func addTerm(terms map[string]*Term, name, source string) {
	lower := strings.ToLower(name)
	if t, ok := terms[lower]; ok {
		t.Freq++
	} else {
		terms[lower] = &Term{
			Name:     lower,
			Freq:     1,
			Category: CategoryUnknown,
			Source:   source,
		}
	}
}

// CollectTermFromPath извлекает термины из одного пути (директории или файла) и добавляет в termMap.
// Используется для инкрементального сбора терминов во время WalkDir без отдельного прохода.
func CollectTermFromPath(termMap map[string]*Term, relPath string, isDir bool) {
	base := filepath.Base(relPath)

	if isDir {
		for _, token := range tokenize(base) {
			if !isNoise(token) {
				addTerm(termMap, token, "filename")
			}
		}
		return
	}

	ext := filepath.Ext(base)
	nameWithoutExt := strings.TrimSuffix(base, ext)
	nameWithoutExt = strings.Split(nameWithoutExt, ".")[0]

	for _, token := range tokenize(nameWithoutExt) {
		if !isNoise(token) {
			addTerm(termMap, token, "filename")
		}
	}
}
