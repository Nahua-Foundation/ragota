package heuristic

import (
	"os"
	"path/filepath"
	"strings"

	"ragota/internal/analyze/types"
)

// WellKnownFiles — файлы проекта, которые всегда сохраняются.
// Универсальные файлы для любых проектов (Go, Java, Python, TS/JS, Rust, etc.).
var WellKnownFiles = map[string]bool{
	"package.json": true, "package-lock.json": true,
	"go.mod": true, "go.sum": true,
	"tsconfig.json": true, ".eslintrc": true, ".eslintrc.json": true,
	".gitignore": true, ".dockerignore": true,
	"Makefile": true, "Dockerfile": true, "docker-compose.yml": true,
	"Cargo.toml": true, "Cargo.lock": true,
	"pom.xml": true, "build.gradle": true, "build.gradle.kts": true,
	"README.md": true, "CHANGELOG.md": true, "LICENSE": true,
}

// PreScreen выполняет многоуровневую эвристику без LLM.
func PreScreen(files []string, root string) *types.PreScreenResult {
	var autoIgnored []types.Entry
	var autoKept []string
	var remaining []string

	for _, f := range files {
		base := filepath.Base(f)
		ext := strings.ToLower(filepath.Ext(f))

		// FAST-PATH 0: Backup/temp files → auto-ignore
		if IsBackupOrTempFile(base) {
			autoIgnored = append(autoIgnored, types.Entry{
				Path:       f,
				Pattern:    "*.bak",
				Stage:      "heuristic",
				Reason:     "backup or temporary file",
				Confidence: 95,
			})
			continue
		}

		// FAST-PATH 1: Бинарные расширения → auto-ignore без чтения
		if IsBinaryExt(ext) {
			autoIgnored = append(autoIgnored, types.Entry{
				Path:       f,
				Pattern:    f,
				Stage:      "heuristic",
				Reason:     "binary file (by extension)",
				Confidence: 95,
			})
			continue
		}

		// FAST-PATH 2: Test файлы → auto-keep без чтения
		if strings.HasSuffix(f, "_test.go") || strings.HasSuffix(f, "_test.ts") ||
			strings.HasSuffix(f, "_test.js") || strings.HasSuffix(f, "_test.py") ||
			strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
			autoKept = append(autoKept, f)
			continue
		}

		// FAST-PATH 3: Well-known файлы → auto-keep без чтения
		if WellKnownFiles[base] {
			autoKept = append(autoKept, f)
			continue
		}

		// FAST-PATH 4: SQL миграции → auto-ignore
		if ext == ".sql" && isSQLMigration(f) {
			autoIgnored = append(autoIgnored, types.Entry{
				Path:       f,
				Pattern:    "**/migrations/*.sql",
				Stage:      "heuristic",
				Reason:     "database migration (schema history)",
				Confidence: 90,
			})
			continue
		}

		// FAST-PATH 5: SQL запросы → auto-keep (бизнес-логика на уровне БД)
		if ext == ".sql" && isSQLQuery(f) {
			autoKept = append(autoKept, f)
			continue
		}

		// SLOW-PATH: Читаем файл для эвристики
		path := filepath.Join(root, f)

		info, err := os.Stat(path)
		if err != nil {
			remaining = append(remaining, f)
			continue
		}

		// Markdown: проверяем на архитектурную документацию
		if ext == ".md" {
			if isArchitecturalMarkdown(f, path) {
				autoKept = append(autoKept, f)
				continue
			}
			// Large docs без архитектурных маркеров → auto-ignore
			if info.Size() > 10000 {
				autoIgnored = append(autoIgnored, types.Entry{
					Path:       f,
					Pattern:    "*.md",
					Stage:      "heuristic",
					Reason:     "large documentation file (no architectural markers)",
					Confidence: 70,
				})
				continue
			}
		}

		head := ReadFileHead(path, 20)

		if HasGeneratedMarker(head) {
			autoIgnored = append(autoIgnored, types.Entry{
				Path:       f,
				Pattern:    f,
				Stage:      "heuristic",
				Reason:     "generated file (marker detected)",
				Confidence: 95,
			})
			continue
		}

		// Source-файлы (.go, .ts, .py, .proto, ...) — auto-keep после проверки на generated.
		// HasSpecMarker НЕ применяем: .proto содержит `syntax = "proto3"`, что не делает
		// его API-спецификацией — это hand-written gRPC контракт.
		if IsSourceFile(base) {
			autoKept = append(autoKept, f)
			continue
		}

		if HasSpecMarker(head) {
			autoIgnored = append(autoIgnored, types.Entry{
				Path:       f,
				Pattern:    f,
				Stage:      "heuristic",
				Reason:     "API specification (marker detected)",
				Confidence: 90,
			})
			continue
		}

		if IsBinaryFile(path) {
			autoIgnored = append(autoIgnored, types.Entry{
				Path:       f,
				Pattern:    f,
				Stage:      "heuristic",
				Reason:     "binary file",
				Confidence: 85,
			})
			continue
		}

		remaining = append(remaining, f)
	}

	return &types.PreScreenResult{
		AutoIgnored: autoIgnored,
		AutoKept:    autoKept,
		Remaining:   remaining,
	}
}

// isSQLMigration проверяет, является ли SQL файл миграцией (история изменений схемы).
func isSQLMigration(path string) bool {
	// Миграции обычно в директории migrations/ или имеют суффиксы .up.sql/.down.sql
	return strings.Contains(path, "/migrations/") ||
		strings.HasSuffix(path, ".up.sql") ||
		strings.HasSuffix(path, ".down.sql") ||
		strings.Contains(path, "/alembic/")
}

// isSQLQuery проверяет, является ли SQL файл рабочим запросом (бизнес-логика).
func isSQLQuery(path string) bool {
	// Рабочие запросы: queries.*.sql, queries/*.sql, db/*.sql (не migrations)
	base := filepath.Base(path)
	return strings.HasPrefix(base, "queries.") ||
		strings.Contains(path, "/queries/") ||
		(strings.Contains(path, "/db/") && !isSQLMigration(path))
}

// isArchitecturalMarkdown проверяет, является ли Markdown файл архитектурной документацией.
// Универсальные маркеры — без привязки к конкретному домену.
func isArchitecturalMarkdown(filename, path string) bool {
	base := strings.ToLower(filepath.Base(filename))
	
	// Архитектурные маркеры в имени файла
	archPatterns := []string{
		"architecture", "design", "pattern", "workflow",
		"lifecycle", "distribution", "overview",
		"universal", "object", "method",
		"terminology", "glossary", "context",
		"adr", "rfc", "proposal", "decision",
		"runbook", "playbook", "onboarding",
		"incident", "postmortem", "troubleshoot",
	}
	
	for _, pattern := range archPatterns {
		if strings.Contains(base, pattern) {
			return true
		}
	}
	
	// README.md всегда сохраняется
	if base == "readme.md" {
		return true
	}
	
	return false
}
