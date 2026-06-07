package heuristic

import (
	"path/filepath"
	"strings"
)

// SafeCompoundPatterns — составные расширения, которые НИКОГДА не блокируются.
// Это паттерны бизнес-логики (NestJS, Angular, etc.).
var SafeCompoundPatterns = map[string]bool{
	// NestJS / Angular
	"*.controller.ts": true,
	"*.controller.js": true,
	"*.service.ts":    true,
	"*.service.js":    true,
	"*.repository.ts": true,
	"*.repository.js": true,
	"*.handler.ts":    true,
	"*.handler.js":    true,
	"*.adapter.ts":    true,
	"*.adapter.js":    true,
	"*.interceptor.ts": true,
	"*.interceptor.js": true,
	"*.guard.ts":       true,
	"*.guard.js":       true,
	"*.pipe.ts":        true,
	"*.pipe.js":        true,
	"*.middleware.ts":  true,
	"*.middleware.js":  true,
	"*.module.ts":      true,
	"*.module.js":      true,
	// DTO / Entity / Model
	"*.dto.ts":    true,
	"*.dto.js":    true,
	"*.entity.ts": true,
	"*.entity.js": true,
	"*.model.ts":  true,
	"*.model.js":  true,
	// Schema
	"*.schema.ts": true,
	"*.schema.js": true,
}

// IsCompoundName возвращает true для имён с составным расширением.
func IsCompoundName(name string) bool {
	if strings.HasPrefix(name, ".") {
		name = name[1:]
	}
	return strings.Count(name, ".") > 1
}

// CompoundPattern извлекает паттерн для составного имени.
func CompoundPattern(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	base := name[:len(name)-len(ext)]
	parts := strings.Split(base, ".")
	if len(parts) < 2 {
		return ""
	}
	mid := parts[len(parts)-1]
	return "*." + mid + ext
}

// IsSafeCompound возвращает true, если compound-паттерн безопасен (бизнес-логика).
func IsSafeCompound(pattern string) bool {
	return SafeCompoundPatterns[pattern]
}
