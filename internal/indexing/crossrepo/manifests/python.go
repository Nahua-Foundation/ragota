// Парсер requirements.txt — извлекает Python-зависимости
// для маппинга import_path → repo_name.
package manifests

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// parseRequirementsTXT парсит requirements.txt и добавляет маппинги.
//
// Стратегия:
// 1. Каждая строка — пакет (возможно с версией: package==1.0.0, package>=1.0)
// 2. Маппинг normalized_name → repoName
func (r *Registry) parseRequirementsTXT(repoName, repoPath string) {
	reqPath := filepath.Join(repoPath, "requirements.txt")
	f, err := os.Open(reqPath)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "-") {
			continue
		}

		// Убираем версию: "package==1.0.0" → "package"
		pkg := line
		for _, sep := range []string{">=", "<=", "==", "!=", ">", "<", "~="} {
			if idx := strings.Index(line, sep); idx >= 0 {
				pkg = line[:idx]
				break
			}
		}
		pkg = strings.TrimSpace(pkg)
		if pkg == "" {
			continue
		}

		// Нормализуем: my-package → my_package
		normalized := strings.ReplaceAll(pkg, "-", "_")
		r.importToRepo[normalized] = repoName
		r.importToRepo[pkg] = repoName
		r.repoImports[repoName] = append(r.repoImports[repoName], pkg)
	}
}
