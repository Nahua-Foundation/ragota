// Команда ragota current: показывает статистику по файлам в текущей директории
// с учётом настроек игнорирования из конфигурации.

package cli

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"ragota/pkg/config"
	"ragota/pkg/fileutil"

	"github.com/spf13/cobra"
)

// newCurrentCmd возвращает cobra-команду для вывода статистики по файлам.
func newCurrentCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "current [directory]",
		Short: "Show file statistics with ignore patterns applied",
		Long: `Show file statistics for the current directory.

Counts all files, files after applying ignore patterns,
shows the 10 largest directories and the most common file
patterns (e.g., *.pb.go, *.md) within them.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			cfg, err := config.Load(dir, configPath)
			if err != nil {
				return err
			}
			return runCurrent(cfg)
		},
	}
	return c
}

// dirStats хранит агрегированную статистику по директории.
type dirStats struct {
	path      string
	fileCount int
	totalSize int64
	patterns  map[string]int
}

// runCurrent выполняет два прохода по файловой системе и выводит статистику.
func runCurrent(cfg *config.Config) error {
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return fmt.Errorf("resolve root: %w", err)
	}

	matcher := fileutil.NewMatcher(cfg.Ignore)

	// Первый проход: все файлы без игнорирования
	allFiles, _, err := walkAll(root, nil)
	if err != nil {
		return fmt.Errorf("walk all files: %w", err)
	}

	// Второй проход: файлы с учётом игнорирования
	filteredFiles, filteredDirs, err := walkAll(root, matcher)
	if err != nil {
		return fmt.Errorf("walk filtered files: %w", err)
	}

	// Вывод сводки
	fmt.Printf("\nDirectory: %s\n", root)
	fmt.Printf("Total files:         %d\n", len(allFiles))
	fmt.Printf("Files after ignore:  %d\n", len(filteredFiles))
	fmt.Printf("Ignored files:       %d (%.1f%%)\n",
		len(allFiles)-len(filteredFiles),
		100*float64(len(allFiles)-len(filteredFiles))/maxFloat(1, float64(len(allFiles))))

	// Топ-10 крупнейших директорий
	fmt.Printf("\nTop 10 largest directories:\n")
	topDirs := topDirectories(filteredDirs, 10)
	for i, d := range topDirs {
		pct := 100 * float64(d.fileCount) / maxFloat(1, float64(len(filteredFiles)))
		topPattern := mostCommonPattern(d.patterns)
		fmt.Printf("%2d. %-60s  %5d files (%.1f%%)  %8s  top: %s\n",
			i+1,
			truncatePath(d.path, root, 60),
			d.fileCount, pct,
			humanSize(d.totalSize),
			topPattern)
	}

	// Топ-10 паттернов файлов
	fmt.Printf("\nTop 10 file patterns:\n")
	allPatterns := aggregatePatterns(filteredDirs)
	for i, p := range topPatterns(allPatterns, 10) {
		fmt.Printf("%2d. %-30s  %5d files\n", i+1, p.name, p.count)
	}

	return nil
}

// walkAll обходит дерево файлов и возвращает список файлов и статистику по директориям.
// Если matcher не nil, применяется игнорирование.
func walkAll(root string, matcher *fileutil.Matcher) ([]string, map[string]*dirStats, error) {
	var files []string
	dirs := make(map[string]*dirStats)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // пропускаем ошибки доступа
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}

		if d.IsDir() {
			if matcher != nil && matcher.IsIgnored(rel, true) {
				return filepath.SkipDir
			}
			// Инициализируем статистику для директории
			if _, ok := dirs[rel]; !ok {
				dirs[rel] = &dirStats{
					path:     rel,
					patterns: make(map[string]int),
				}
			}
			return nil
		}

		if matcher != nil && matcher.IsIgnored(rel, false) {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		files = append(files, rel)

		// Определяем родительскую директорию
		parentDir := filepath.Dir(rel)
		if parentDir == "." {
			parentDir = ""
		}

		// Обновляем статистику для всех родительских директорий
		parts := strings.Split(parentDir, string(filepath.Separator))
		for i := range parts {
			dirPath := strings.Join(parts[:i+1], string(filepath.Separator))
			if dirPath == "" {
				continue
			}
			if _, ok := dirs[dirPath]; !ok {
				dirs[dirPath] = &dirStats{
					path:     dirPath,
					patterns: make(map[string]int),
				}
			}
			dirs[dirPath].fileCount++
			dirs[dirPath].totalSize += info.Size()
			pattern := filePattern(d.Name())
			dirs[dirPath].patterns[pattern]++
		}

		// Корневая директория
		if _, ok := dirs[""]; !ok {
			dirs[""] = &dirStats{path: "", patterns: make(map[string]int)}
		}
		dirs[""].fileCount++
		dirs[""].totalSize += info.Size()
		pattern := filePattern(d.Name())
		dirs[""].patterns[pattern]++

		return nil
	})

	return files, dirs, err
}

// filePattern извлекает паттерн из имени файла.
// Примеры: "*.pb.go", "*.gen.go", "*_test.go", "*.go", "*.md"
func filePattern(name string) string {
	// Проверяем двухуровневые суффиксы (длинные перед короткими!)
	twoLevel := []string{
		"_grpc.pb.go", "_pb2_grpc.py",
		".pb.go", ".gen.go", "_test.go",
		".pb.ts", ".pb.js", "_pb2.py",
	}
	for _, suffix := range twoLevel {
		if strings.HasSuffix(name, suffix) {
			return "*" + suffix
		}
	}

	// Простое расширение (пропускаем dotfiles — у них имя начинается с точки)
	ext := filepath.Ext(name)
	if ext != "" && !strings.HasPrefix(name, ".") {
		return "*" + ext
	}

	// Dotfile или файл без расширения
	return name
}

// topDirectories возвращает топ-N директорий по количеству файлов.
func topDirectories(dirs map[string]*dirStats, n int) []*dirStats {
	var list []*dirStats
	for _, d := range dirs {
		if d.path == "" {
			continue // пропускаем корень
		}
		list = append(list, d)
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].fileCount > list[j].fileCount
	})

	if len(list) > n {
		list = list[:n]
	}
	return list
}

// patternCount представляет пару паттерн-количество.
type patternCount struct {
	name  string
	count int
}

// aggregatePatterns собирает все паттерны из всех директорий.
func aggregatePatterns(dirs map[string]*dirStats) map[string]int {
	result := make(map[string]int)
	for _, d := range dirs {
		for p, c := range d.patterns {
			result[p] += c
		}
	}
	return result
}

// topPatterns возвращает топ-N паттернов по количеству.
func topPatterns(patterns map[string]int, n int) []patternCount {
	var list []patternCount
	for name, count := range patterns {
		list = append(list, patternCount{name, count})
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].count > list[j].count
	})

	if len(list) > n {
		list = list[:n]
	}
	return list
}

// mostCommonPattern возвращает самый частый паттерн из map.
func mostCommonPattern(patterns map[string]int) string {
	var best string
	var bestCount int
	for p, c := range patterns {
		if c > bestCount {
			best = p
			bestCount = c
		}
	}
	if best == "" {
		return "-"
	}
	return best
}

// truncatePath обрезает путь для вывода, заменяя начало на "..."
func truncatePath(path, root string, maxLen int) string {
	if path == "" {
		return "(root)"
	}
	if len(path) <= maxLen {
		return path
	}
	return "..." + path[len(path)-maxLen+3:]
}

// humanSize форматирует размер в человекочитаемый вид.
func humanSize(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.1fG", float64(bytes)/GB)
	case bytes >= MB:
		return fmt.Sprintf("%.1fM", float64(bytes)/MB)
	case bytes >= KB:
		return fmt.Sprintf("%.1fK", float64(bytes)/KB)
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}

// maxFloat возвращает максимум из двух float64.
func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
