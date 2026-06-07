// Package gitignore парсит .gitignore файлы для обхода директорий.
//
// Поддерживает базовые паттерны: *, ?, **, negation (!), directory-only (/).
// Используется для пропуска файлов, которые сам git считает ненужными.
package gitignore

import (
	"os"
	"path/filepath"
	"strings"
)

// Matcher проверяет, попадает ли путь под .gitignore паттерны.
type Matcher struct {
	patterns []pattern
}

type pattern struct {
	pattern   string
	negate    bool   // начинается с !
	dirOnly   bool   // заканчивается на /
	original  string
}

// Load загружает .gitignore из указанной директории.
// Если файл отсутствует — возвращает пустой Matcher.
func Load(root string) (*Matcher, error) {
	path := filepath.Join(root, ".gitignore")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Matcher{}, nil
		}
		return nil, err
	}

	var patterns []pattern
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		p := pattern{original: line}

		// Negation
		if strings.HasPrefix(line, "!") {
			p.negate = true
			line = line[1:]
		}

		// Directory-only
		if strings.HasSuffix(line, "/") {
			p.dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}

		p.pattern = line
		patterns = append(patterns, p)
	}

	return &Matcher{patterns: patterns}, nil
}

// Match проверяет, попадает ли путь под паттерны.
// Возвращает (ignored, negated) — если negated=true, путь исключается из ignore.
func (m *Matcher) Match(path string) (ignored bool, negated bool) {
	if m == nil || len(m.patterns) == 0 {
		return false, false
	}

	for _, p := range m.patterns {
		if matchPattern(p.pattern, path, p.dirOnly) {
			if p.negate {
				return false, true
			}
			ignored = true
		}
	}

	return ignored, false
}

// matchPattern проверяет один паттерн против пути.
func matchPattern(pattern, path string, dirOnly bool) bool {
	// Убираем leading / из паттерна (он означает "от корня")
	pattern = strings.TrimPrefix(pattern, "/")

	// Проверяем базовое имя файла/директории
	base := filepath.Base(path)

	// Пробуем match на базовом имени
	if matched, _ := filepath.Match(pattern, base); matched {
		return true
	}

	// Пробуем match на полном пути
	if matched, _ := filepath.Match(pattern, path); matched {
		return true
	}

	// Паттерн-директория: "build" матчит "build/anything"
	if dirOnly && (path == pattern || strings.HasPrefix(path, pattern+"/")) {
		return true
	}

	// Простой паттерн (без wildcard) матчит как префикс директории
	if !strings.ContainsAny(pattern, "*?[") {
		if path == pattern || strings.HasPrefix(path, pattern+"/") {
			return true
		}
	}

	// Обрабатываем ** (рекурсивный wildcard)
	if strings.Contains(pattern, "**") {
		parts := strings.SplitN(pattern, "**", 2)
		prefix := strings.TrimSuffix(parts[0], "/")
		suffix := ""
		if len(parts) > 1 {
			suffix = strings.TrimPrefix(parts[1], "/")
		}

		// prefix empty → match from root
		if prefix == "" {
			if suffix == "" {
				return true
			}
			// Match suffix on any path segment
			if matched, _ := filepath.Match(suffix, base); matched {
				return true
			}
			// Or on full path
			if matched, _ := filepath.Match(suffix, path); matched {
				return true
			}
			// Or anywhere in path
			return strings.Contains(path, "/"+suffix) || path == suffix
		}

		// prefix non-empty
		if strings.HasPrefix(path, prefix) {
			if suffix == "" {
				return true
			}
			remaining := strings.TrimPrefix(path, prefix)
			remaining = strings.TrimPrefix(remaining, "/")
			if matched, _ := filepath.Match(suffix, filepath.Base(remaining)); matched {
				return true
			}
			if matched, _ := filepath.Match(suffix, remaining); matched {
				return true
			}
			return strings.Contains(remaining, "/"+suffix) || remaining == suffix
		}
	}

	return false
}

// ShouldSkip проверяет, нужно ли пропустить директорию при обходе.
// Возвращает true, если директория попадает под .gitignore.
func (m *Matcher) ShouldSkip(relPath string) bool {
	ignored, negated := m.Match(relPath)
	return ignored && !negated
}
