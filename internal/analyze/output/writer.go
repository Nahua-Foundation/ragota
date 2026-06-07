package output

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"ragota/internal/analyze/types"
	"ragota/pkg/config"
	"ragota/pkg/ragignore"
)

// FilterIndexedExtensions убирает паттерны-расширения из DefaultExtensions.
func FilterIndexedExtensions(patterns []string, entries []types.Entry) ([]string, []types.Entry) {
	indexed := make(map[string]bool, len(config.DefaultExtensions))
	for _, ext := range config.DefaultExtensions {
		indexed[ext] = true
	}

	var filtered []string
	for _, p := range patterns {
		if strings.HasPrefix(p, "!") {
			filtered = append(filtered, p)
			continue
		}
		if strings.HasPrefix(p, "*.") {
			ext := p[1:]
			if indexed[ext] {
				continue
			}
		}
		filtered = append(filtered, p)
	}

	var filteredEntries []types.Entry
	for _, e := range entries {
		if containsPattern(filtered, e.Pattern) || e.Pattern == e.Path {
			filteredEntries = append(filteredEntries, e)
		}
	}
	return filtered, filteredEntries
}

func containsPattern(slice []string, pattern string) bool {
	for _, p := range slice {
		if p == pattern {
			return true
		}
	}
	return false
}

// GroupPatterns группирует конкретные пути в обобщённые паттерны.
// Вместо тысяч конкретных путей генерирует компактные паттерны.
func GroupPatterns(entries []types.Entry) []string {
	// Собираем паттерны по расширениям и директориям
	extMap := make(map[string][]string)      // ext → [dirs]
	dirMap := make(map[string]map[string]bool) // dir → {ext: true}
	specificPatterns := make(map[string]bool)

	for _, e := range entries {
		path := e.Path
		if e.Pattern != "" && e.Pattern != e.Path {
			path = e.Pattern
		}

		// Если уже паттерн — оставляем как есть
		if strings.Contains(path, "*") {
			specificPatterns[path] = true
			continue
		}

		ext := strings.ToLower(filepath.Ext(path))
		dir := filepath.Dir(path)
		base := filepath.Base(path)

		// Игнорируем файлы в корне
		if dir == "." {
			specificPatterns[base] = true
			continue
		}

		// Группируем по директориям
		if dirMap[dir] == nil {
			dirMap[dir] = make(map[string]bool)
		}
		dirMap[dir][ext] = true

		// Также собираем для анализа
		if ext != "" {
			extMap[ext] = append(extMap[ext], dir)
		}
	}

	var patterns []string

	// Агрегация по глубине: если 5+ директорий на одном уровне → один паттерн
	depthMap := make(map[string][]string) // parentDir → [childDirs]
	for dir := range dirMap {
		parent := filepath.Dir(dir)
		if parent == "." {
			parent = filepath.Base(dir)
		}
		depthMap[parent] = append(depthMap[parent], dir)
	}

	// Генерируем паттерны для директорий с множеством файлов одного расширения
	for dir, exts := range dirMap {
		// Проверяем, есть ли 5+ дочерних директорий у родителя
		parent := filepath.Dir(dir)
		if parent == "." {
			parent = filepath.Base(dir)
		}
		siblings := depthMap[parent]

		// Если 5+ sibling-директорий → агрегируем на уровне родителя
		if len(siblings) >= 5 {
			// Уже обработано в цикле выше, пропускаем
			continue
		}

		if len(exts) == 1 {
			// Одно расширение в директории → **/dir/*.{ext}
			for ext := range exts {
				if ext == "" {
					continue // Пропускаем файлы без расширения (защита от **/reponame/*)
				}
				patterns = append(patterns, "**/"+dir+"/*"+ext)
			}
		} else if len(exts) <= 3 {
			// Несколько расширений → генерируем паттерны для каждого
			for ext := range exts {
				if ext == "" {
					continue // Пропускаем файлы без расширения
				}
				patterns = append(patterns, "**/"+dir+"/*"+ext)
			}
		} else {
			// Много расширений → игнорируем всю директорию
			patterns = append(patterns, dir+"/**")
		}
	}

	// Добавляем агрегированные паттерны для родителей с 5+ дочерними директориями
	for parent, childDirs := range depthMap {
		if len(childDirs) >= 5 {
			// Собираем все уникальные расширения из дочерних директорий
			allExts := make(map[string]bool)
			for _, childDir := range childDirs {
				for ext := range dirMap[childDir] {
					if ext != "" {
						allExts[ext] = true
					}
				}
			}
			// Если расширений немного → генерируем паттерны на уровне родителя
			if len(allExts) <= 5 {
				for ext := range allExts {
					patterns = append(patterns, fmt.Sprintf("**/%s/**/*%s", parent, ext))
				}
			} else {
				// Много расширений → один паттерн на родителя
				patterns = append(patterns, fmt.Sprintf("**/%s/**", parent))
			}
		}
	}

	// Добавляем специфичные паттерны
	for p := range specificPatterns {
		if !strings.Contains(p, "/") {
			// Файл в корне
			patterns = append(patterns, p)
		} else {
			// Путь с директорией
			patterns = append(patterns, p)
		}
	}

	// Сортируем для консистентности
	sort.Strings(patterns)

	return patterns
}

// GroupPatternsFromPaths группирует список путей в обобщённые паттерны.
func GroupPatternsFromPaths(paths []string) []string {
	entries := make([]types.Entry, len(paths))
	for i, p := range paths {
		entries[i] = types.Entry{Path: p, Pattern: p}
	}
	return GroupPatterns(entries)
}

// SavePatterns сохраняет паттерны в .ragotaignore.
// Сначала группирует конкретные пути в паттерны, затем сохраняет.
func SavePatterns(root string, patterns []string) error {
	existingPath := ragignore.Path(root)
	var existing []string

	if data, err := os.ReadFile(existingPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			isDefault := false
			for _, dp := range ragignore.DefaultPatterns {
				if line == dp {
					isDefault = true
					break
				}
			}
			if !isDefault {
				existing = append(existing, line)
			}
		}
	}

	seen := make(map[string]bool)
	var merged []string

	// 1. Default patterns
	for _, p := range ragignore.DefaultPatterns {
		if !seen[p] {
			seen[p] = true
			merged = append(merged, p)
		}
	}

	// 2. Existing custom patterns (не default)
	for _, p := range existing {
		if !seen[p] {
			seen[p] = true
			merged = append(merged, p)
		}
	}

	// 3. New patterns (сгруппированные)
	for _, p := range patterns {
		if !seen[p] {
			seen[p] = true
			merged = append(merged, p)
		}
	}

	return ragignore.Save(root, merged)
}
