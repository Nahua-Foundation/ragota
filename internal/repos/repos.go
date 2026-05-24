// Package repos — auto-discovery репозиториев в рабочем каталоге.
//
// Поддерживаются два режима, выбираемых автоматически по содержимому
// корня (cfg.Root):
//
//  1. Single-repo: если в самом cfg.Root есть .git, то это единственное
//     репо, имя = basename(Root).
//
//  2. Multi-repo workspace: если в cfg.Root нет .git, но среди его
//     непосредственных поддиректорий есть директории с .git, то каждая
//     такая поддиректория — отдельное репо. Соседние поддиректории на
//     том же верхнем уровне (без .git) тоже считаются «репами», т.к.
//     индекс охватывает все доступные исходники.
//
// Правила:
//   - репа не может быть внутри другой репы (обход в Discover ограничен
//     первым уровнем поддиректорий cfg.Root);
//   - имена должны быть уникальными; при коллизии добавляется короткий
//     суффикс из sha1-хэша абсолютного пути (форма `name-abcd1234`);
//   - скрытые директории (.*) на верхнем уровне пропускаются.
package repos

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Repo — описание одной репозитории в workspace.
type Repo struct {
	// Name — уникальный идентификатор внутри workspace; используется как
	// значение payload-поля `repo` в SQLite/Qdrant/BM25.
	Name string `json:"name"`
	// Path — абсолютный путь к корню репы.
	Path string `json:"path"`
	// HasGit — true, если внутри Path есть .git; false для соседних
	// «прицепленных» поддиректорий workspace.
	HasGit bool `json:"has_git"`
}

// Discover применяет правила выше и возвращает упорядоченный список репо.
// При коллизии имён добавляет короткий hash-суффикс к дубликатам.
//
// Возвращаемая ошибка — только для I/O проблем (отсутствие/неверный root).
func Discover(root string) ([]Repo, error) {
	if root == "" {
		return nil, fmt.Errorf("repos: пустой корень")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("repos: abs %q: %w", root, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("repos: stat %q: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("repos: %q не директория", abs)
	}

	// Single-repo: .git прямо в root.
	if dirHasGit(abs) {
		return []Repo{{
			Name:   filepath.Base(abs),
			Path:   abs,
			HasGit: true,
		}}, nil
	}

	// Multi-repo workspace: смотрим только верхний уровень поддиректорий.
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, fmt.Errorf("repos: readdir %q: %w", abs, err)
	}

	var (
		gitRepos    []Repo
		otherChilds []Repo
	)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// Пропускаем скрытые директории (включая .ai-tools, .git, .idea и т.п.).
		if strings.HasPrefix(name, ".") {
			continue
		}
		sub := filepath.Join(abs, name)
		r := Repo{Name: name, Path: sub}
		if dirHasGit(sub) {
			r.HasGit = true
			gitRepos = append(gitRepos, r)
		} else {
			otherChilds = append(otherChilds, r)
		}
	}

	// Если нет ни одной .git-подпапки — workspace не определён как
	// multi-repo. Возвращаем единственное репо = сам root (имя=basename).
	// Это совместимо со старым однопроектным сценарием.
	if len(gitRepos) == 0 {
		return []Repo{{
			Name:   filepath.Base(abs),
			Path:   abs,
			HasGit: false,
		}}, nil
	}

	out := append([]Repo{}, gitRepos...)
	out = append(out, otherChilds...)

	// Сортируем по имени — стабильный порядок упрощает воспроизводимость.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	// Гарантируем уникальность имён. При коллизии добавляем -<hash[:8]>.
	used := map[string]int{}
	for i := range out {
		base := out[i].Name
		if used[base] == 0 {
			used[base] = 1
			continue
		}
		// Коллизия: к текущему элементу добавляем суффикс.
		out[i].Name = base + "-" + pathHash(out[i].Path)
		used[out[i].Name]++
	}

	return out, nil
}

// dirHasGit возвращает true, если внутри dir есть подпапка/файл .git
// (поддерживает как обычные репозитории, так и worktrees, где .git — файл).
func dirHasGit(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// pathHash — короткий стабильный суффикс для разрешения коллизий имён.
func pathHash(path string) string {
	sum := sha1.Sum([]byte(path))
	return hex.EncodeToString(sum[:4])
}

// Resolver сопоставляет абсолютный путь файла с репой, к которой он
// принадлежит (по prefix-match). Если ни одна репа не подошла, репо =
// fallback (первое из списка). Используется индексаторами и watcher'ом.
type Resolver struct {
	repos []Repo
}

// NewResolver создаёт резолвер на основе уже найденных репо.
// Список должен быть непустым; иначе For всегда вернёт пустую строку.
func NewResolver(repos []Repo) *Resolver {
	// Сортируем по длине пути убывая — чтобы более «глубокое» совпадение
	// побеждало (вложенные репы запрещены правилами, но защита не лишняя).
	sorted := make([]Repo, len(repos))
	copy(sorted, repos)
	sort.Slice(sorted, func(i, j int) bool {
		return len(sorted[i].Path) > len(sorted[j].Path)
	})
	return &Resolver{repos: sorted}
}

// For возвращает имя репы, к которой принадлежит absPath, либо пустую
// строку, если совпадений нет.
func (r *Resolver) For(absPath string) string {
	if r == nil || len(r.repos) == 0 {
		return ""
	}
	for _, rp := range r.repos {
		if absPath == rp.Path {
			return rp.Name
		}
		if strings.HasPrefix(absPath, rp.Path+string(filepath.Separator)) {
			return rp.Name
		}
	}
	return ""
}

// All возвращает копию списка известных репо в исходном порядке (по имени).
func (r *Resolver) All() []Repo {
	if r == nil {
		return nil
	}
	out := make([]Repo, len(r.repos))
	copy(out, r.repos)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Signature — стабильная подпись текущего набора репо (имена+пути в
// фиксированном порядке). Используется для invalidation SQLite при смене
// workspace (cfg.Root и/или состав репо изменились — старый индекс не
// согласован).
func Signature(repos []Repo) string {
	parts := make([]string, 0, len(repos))
	for _, r := range repos {
		parts = append(parts, r.Name+"="+r.Path)
	}
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, ";")))
	return hex.EncodeToString(sum[:])
}
