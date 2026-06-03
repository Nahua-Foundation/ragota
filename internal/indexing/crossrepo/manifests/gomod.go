// Парсер go.mod — извлекает module name и replace/require директивы
// для маппинга import_path → repo_name.
package manifests

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// parseGoMod парсит go.mod и добавляет маппинги в registry.
//
// Стратегия:
// 1. Имя модуля из `module <name>` → маппинг name → repoName
// 2. require <path> → если path содержит имя другого репо из workspace,
//    маппинг path → thatRepo
// 3. replace <old> => <new> → маппинг old → repoName (если new локальный)
func (r *Registry) parseGoMod(repoName, repoPath string) {
	goModPath := filepath.Join(repoPath, "go.mod")
	f, err := os.Open(goModPath)
	if err != nil {
		return // go.mod нет — не Go репо
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	inRequire := false
	inReplace := false

	var moduleName string
	var requires []string
	var replaces []struct{ old, new string }

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Module declaration
		if strings.HasPrefix(line, "module ") {
			moduleName = strings.TrimSpace(strings.TrimPrefix(line, "module "))
			continue
		}

		// Block tracking
		if line == "require (" {
			inRequire = true
			continue
		}
		if line == "replace (" {
			inReplace = true
			continue
		}
		if line == ")" {
			inRequire = false
			inReplace = false
			continue
		}

		if inRequire && line != "" && !strings.HasPrefix(line, "//") {
			// "github.com/company/auth-sdk v1.2.3"
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				requires = append(requires, parts[0])
			}
		}

		if inReplace && line != "" && !strings.HasPrefix(line, "//") {
			// "github.com/old => github.com/new v1.0.0" or "github.com/old => ../local"
			parts := strings.Fields(line)
			if len(parts) >= 3 && parts[1] == "=>" {
				replaces = append(replaces, struct{ old, new string }{parts[0], parts[2]})
			}
		}

		// Single-line require/replace
		if !inRequire && !inReplace {
			if strings.HasPrefix(line, "require ") {
				parts := strings.Fields(line)
				if len(parts) >= 3 {
					requires = append(requires, parts[1])
				}
			}
			if strings.HasPrefix(line, "replace ") {
				parts := strings.Fields(line)
				if len(parts) >= 4 && parts[1] == "=>" {
					replaces = append(replaces, struct{ old, new string }{parts[0], parts[2]})
				}
			}
		}
	}

	// Register module name
	if moduleName != "" {
		r.importToRepo[moduleName] = repoName
		r.repoImports[repoName] = append(r.repoImports[repoName], moduleName)
	}

	// Register requires
	for _, req := range requires {
		r.importToRepo[req] = repoName
		r.repoImports[repoName] = append(r.repoImports[repoName], req)
	}

	// Register replaces
	for _, rep := range replaces {
		r.importToRepo[rep.old] = repoName
		r.repoImports[repoName] = append(r.repoImports[repoName], rep.old)
	}
}
