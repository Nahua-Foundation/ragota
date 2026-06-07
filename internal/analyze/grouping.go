package analyze

import (
	"path/filepath"
	"sort"
	"strings"

	"ragota/internal/analyze/types"
)

// GroupFilesByScope группирует файлы по паттерну с учётом директорий.
func GroupFilesByScope(files []string) []types.FileGroup {
	groupMap := make(map[string]*types.FileGroup)

	for _, f := range files {
		dir := filepath.Dir(f)
		ext := strings.ToLower(filepath.Ext(f))

		// Простая группировка по расширению: *.go, *.ts, *.js
		pattern := "*" + ext

		scopedPattern := filepath.Join(dir, pattern)

		g, exists := groupMap[scopedPattern]
		if !exists {
			g = &types.FileGroup{Pattern: scopedPattern}
			groupMap[scopedPattern] = g
		}
		g.Files = append(g.Files, f)

		dirFound := false
		for _, d := range g.Dirs {
			if d == dir {
				dirFound = true
				break
			}
		}
		if !dirFound {
			g.Dirs = append(g.Dirs, dir)
		}
	}

	var groups []types.FileGroup
	for _, g := range groupMap {
		groups = append(groups, *g)
	}
	sort.Slice(groups, func(i, j int) bool {
		return len(groups[i].Files) > len(groups[j].Files)
	})

	return groups
}

// BuildSubGroups строит рекурсивное дерево подпапок для группы файлов.
// Возвращает подгруппы из листьев дерева (конечные директории).
func BuildSubGroups(group *types.FileGroup, root string) []*types.SubGroup {
	// Строим дерево директорий
	dirTree := make(map[string][]string) // dir → files
	for _, f := range group.Files {
		dir := filepath.Dir(f)
		dirTree[dir] = append(dirTree[dir], f)
	}

	// Находим листья дерева (директории без поддиректорий в наборе)
	leafDirs := findLeafDirs(dirTree)

	// Создаём подгруппы из листьев
	var subGroups []*types.SubGroup
	for _, leafDir := range leafDirs {
		files := dirTree[leafDir]
		if len(files) == 0 {
			continue
		}

		// Вычисляем глубину
		depth := strings.Count(leafDir, "/") + 1

		subGroups = append(subGroups, &types.SubGroup{
			DirPath: leafDir,
			Files:   files,
			Depth:   depth,
		})
	}

	// Сортируем по глубине (сначала глубокие, потом мелкие)
	sort.Slice(subGroups, func(i, j int) bool {
		return subGroups[i].Depth > subGroups[j].Depth
	})

	group.SubGroups = subGroups
	return subGroups
}

// findLeafDirs находит директории-листья (без поддиректорий в наборе). O(n) алгоритм.
func findLeafDirs(dirTree map[string][]string) []string {
	hasChild := make(map[string]bool)

	for dir := range dirTree {
		// Поднимаемся вверх по дереву, помечая всех предков как имеющих потомков
		parent := filepath.Dir(dir)
		for parent != "." && parent != dir {
			if _, ok := dirTree[parent]; !ok {
				break
			}
			if hasChild[parent] {
				break
			}
			hasChild[parent] = true
			parent = filepath.Dir(parent)
		}
		// Корневая директория "." тоже может быть предком
		if parent == "." {
			if _, ok := dirTree["."]; ok && dir != "." {
				hasChild["."] = true
			}
		}
	}

	var leafDirs []string
	for dir := range dirTree {
		if !hasChild[dir] {
			leafDirs = append(leafDirs, dir)
		}
	}

	return leafDirs
}
