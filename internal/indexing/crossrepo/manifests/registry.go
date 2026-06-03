// Package manifests парсит dependency-манифесты репозиториев и строит
// маппинг import_path → repo_name для cross-repo import resolution.
//
// Поддерживаемые форматы:
//   - go.mod (Go modules)
//   - package.json (npm/Node.js)
//   - requirements.txt / pyproject.toml (Python)
//
// Архитектура: Registry хранит маппинги из всех репо, отдельные парсеры
// (gomod, npm, python) извлекают зависимости из конкретных форматов.
package manifests

// Registry хранит маппинги import_path → repo_name из всех репозиториев.
type Registry struct {
	// importPath → repoName. Например:
	// "github.com/company/auth-sdk" → "auth-service"
	// "@company/auth-sdk" → "auth-service"
	// "auth_sdk" → "auth-service"
	importToRepo map[string]string

	// repoName → список его import paths (обратный индекс)
	repoImports map[string][]string
}

// New создаёт пустую registry.
func New() *Registry {
	return &Registry{
		importToRepo: make(map[string]string),
		repoImports:  make(map[string][]string),
	}
}

// AddRepo парсит манифесты репо и добавляет маппинги в registry.
func (r *Registry) AddRepo(repoName, repoPath string) {
	r.parseGoMod(repoName, repoPath)
	r.parsePackageJSON(repoName, repoPath)
	r.parseRequirementsTXT(repoName, repoPath)
}

// ResolveImport возвращает имя репо для данного import path.
// Возвращает пустую строку, если не найдено.
func (r *Registry) ResolveImport(importPath string) string {
	// Точное совпадение
	if repo, ok := r.importToRepo[importPath]; ok {
		return repo
	}

	// Частичное совпадение: ищем по суффиксу
	for path, repo := range r.importToRepo {
		if len(path) > 0 && len(importPath) >= len(path) {
			if importPath == path || importPath[len(importPath)-len(path):] == path {
				return repo
			}
		}
		// Обратное: importPath может быть префиксом
		if len(importPath) > 0 && len(path) >= len(importPath) {
			if path == importPath || path[len(path)-len(importPath):] == importPath {
				return repo
			}
		}
	}

	return ""
}

// KnownImports возвращает все известные import paths.
func (r *Registry) KnownImports() map[string]string {
	out := make(map[string]string, len(r.importToRepo))
	for k, v := range r.importToRepo {
		out[k] = v
	}
	return out
}
