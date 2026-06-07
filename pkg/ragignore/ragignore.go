// Package ragignore загружает и сохраняет паттерны игнорирования из .ragotaignore.
//
// .ragotaignore — файл в корне проекта, один паттерн на строку (формат .gitignore).
// Комментарии начинаются с #, пустые строки игнорируются.
//
// Если .ragotaignore отсутствует — используются только DefaultPatterns
// (базовые системные паттерны: .git, vendor, node_modules и т.д.).
package ragignore

import (
	"os"
	"path/filepath"
	"strings"
)

const fileName = ".ragotaignore"

// DefaultPatterns — базовые паттерны, которые всегда применяются.
// Универсальные паттерны для любых проектов (Go, Java, Python, TS/JS, Rust, etc.).
var DefaultPatterns = []string{
	// VCS
	".git", ".hg", ".svn", ".idea", ".vscode", ".fleet",
	// Package managers (универсальные)
	"vendor",
	"node_modules", "bower_components",
	"__pycache__", ".venv", "venv", "env", ".tox",
	".mypy_cache", ".pytest_cache", ".ruff_cache", "site-packages",
	".gradle", "packages", ".paket",
	// Build artifacts
	".next", ".nuxt", "dist", "build", ".turbo", ".svelte-kit",
	"target", "out",
	// Generated code (универсальные расширения)
	"*.pb.go", "*_grpc.pb.go", "*.gen.go",
	"*.pb.js", "*.pb.ts",
	"*_pb2.py", "*_pb2_grpc.py",
	"*.pb.cc", "*.pb.h",
	"*.min.js", "*.min.css",
	// Coverage and temp
	".cache", "coverage", "tmp",
	// Ragota
	"ragota",
	".ragota",
	// Migrations and generated (универсальные)
	"migrations", "alembic", "protodeps", "gen", "generated", "codegen",
	"release-notes",
}

// Load загружает паттерны из .ragotaignore в указанной директории.
// Если файл отсутствует — возвращает DefaultPatterns.
// Если файл есть — DefaultPatterns + паттерны из файла.
func Load(root string) ([]string, error) {
	path := filepath.Join(root, fileName)

	// Всегда начинаем с дефолтных паттернов
	patterns := make([]string, len(DefaultPatterns))
	copy(patterns, DefaultPatterns)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return patterns, nil
		}
		return nil, err
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}

	return patterns, nil
}

// Save записывает паттерны в .ragotaignore.
// Дефолтные паттерны НЕ записываются — только кастомные.
func Save(root string, patterns []string) error {
	path := filepath.Join(root, fileName)
	var sb strings.Builder
	for _, p := range patterns {
		sb.WriteString(p)
		sb.WriteString("\n")
	}
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

// Path возвращает путь к .ragotaignore в указанной директории.
func Path(root string) string {
	return filepath.Join(root, fileName)
}

// Exists проверяет наличие .ragotaignore.
func Exists(root string) bool {
	_, err := os.Stat(filepath.Join(root, fileName))
	return err == nil
}
