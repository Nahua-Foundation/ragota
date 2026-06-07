package heuristic

// KnownDirNames — имена директорий, которые однозначно игнорируются.
// Универсальные паттерны для любых проектов (Go, Java, Python, TS/JS, Rust, etc.).
// Эти директории пропускаются целиком, без сканирования содержимого.
var KnownDirNames = map[string]bool{
	// Build artifacts
	"build": true, "dist": true, "out": true, "target": true,
	"coverage": true, ".coverage": true,
	"__snapshots__": true, "snapshots": true,
	".next": true, ".nuxt": true, ".svelte-kit": true,
	".turbo": true, ".cache": true,
	"gen": true, "generated": true, "codegen": true,
	"fixtures": true, "testdata": true, "test-fixtures": true,
	// Package managers
	"vendor": true,
	"node_modules": true, "bower_components": true,
	"__pycache__": true, ".venv": true, "venv": true,
	".tox": true, ".mypy_cache": true, ".pytest_cache": true,
	".ruff_cache": true, "site-packages": true,
	".gradle": true,
	".paket": true,
	// Deprecated
	"deprecated": true, "archived": true, "legacy": true, "old": true,
	// Infrastructure
	"k8s": true, "kubernetes": true, "terraform": true, "infra": true,
	"ansible": true, "playbooks": true, "helm": true, "charts": true,
	// Assets and static
	"assets": true, "static": true, "public": true, "media": true,
	"images": true, "fonts": true, "icons": true, "img": true,
	// Оставляем только migrations — это только миграции БД
	"migrations": true, "alembic": true,
	// Logs and temp
	"logs": true, "log": true, "tmp": true, "temp": true,
}

// ProtoScanDirNames — имена директорий, которые пропускаются для общей индексации,
// но требуют сканирования .proto файлов (gRPC контракты для crossrepo detection).
// Orchestrator при встрече этих директорий:
// 1. Пропускает всё содержимое кроме .proto
// 2. Добавляет найденные .proto файлы в allFiles для дальнейшего анализа.
var ProtoScanDirNames = map[string]bool{
	"schemas":       true,
	"spec":          true,
	"swagger":       true,
	"openapi":       true,
	"api-docs":      true,
	"api-specs":     true,
	"apidocs":       true,
	"api_reference": true,
	"proto-specs":   true,
	"api-contracts": true,
}

// ConditionalDirNames — директории, которые требуют проверки содержимого
// перед принятием решения о пропуске. Обрабатываются функцией shouldSkipKnownDir.
// - "packages": пропуск для Composer/Ruby cache, но не для монорепозиториев
// - "env": пропуск для Python venv, но не для environment-конфигов
var ConditionalDirNames = map[string]bool{
	"packages": true,
	"env":      true,
}
