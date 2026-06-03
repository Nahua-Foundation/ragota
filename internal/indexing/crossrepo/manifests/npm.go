// Парсер package.json — извлекает name, dependencies, devDependencies
// для маппинга import_path → repo_name.
package manifests

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// parsePackageJSON парсит package.json и добавляет маппинги в registry.
//
// Стратегия:
// 1. name из package.json → маппинг @scope/name → repoName
// 2. dependencies/devDependencies → если dependency имя совпадает
//    с именем другого репо, маппинг dependency → thatRepo
func (r *Registry) parsePackageJSON(repoName, repoPath string) {
	pkgPath := filepath.Join(repoPath, "package.json")
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		return
	}

	var pkg struct {
		Name            string            `json:"name"`
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return
	}

	// Register package name
	if pkg.Name != "" {
		r.importToRepo[pkg.Name] = repoName
		r.repoImports[repoName] = append(r.repoImports[repoName], pkg.Name)
	}

	// Register dependencies
	for dep := range pkg.Dependencies {
		r.importToRepo[dep] = repoName
		r.repoImports[repoName] = append(r.repoImports[repoName], dep)
	}
	for dep := range pkg.DevDependencies {
		r.importToRepo[dep] = repoName
		r.repoImports[repoName] = append(r.repoImports[repoName], dep)
	}
}
