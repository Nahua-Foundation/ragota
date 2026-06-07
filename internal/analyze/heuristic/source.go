package heuristic

import (
	"path/filepath"
	"strings"
)

// SourceFileExts — расширения source-файлов, которые НИКОГДА не блокируются.
var SourceFileExts = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".mjs": true, ".cjs": true, ".py": true, ".java": true, ".proto": true,
}

// IsSourceFile возвращает true, если файл — source code.
func IsSourceFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return SourceFileExts[ext]
}
