package analyze

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"ragota/internal/analyze/classify"
	analyzeContext "ragota/internal/analyze/context"
	"ragota/internal/analyze/heuristic"
	"ragota/internal/analyze/llm"
	"ragota/internal/analyze/output"
	"ragota/internal/analyze/resolve"
	"ragota/internal/analyze/scoring"
	"ragota/internal/analyze/types"
	"ragota/pkg/gitignore"
)

// Type aliases — re-export types for convenience.
type Entry = types.Entry
type FileMeta = types.FileMeta
type FileGroup = types.FileGroup
type PreScreenResult = types.PreScreenResult
type GroupDecision = types.GroupDecision

// ProgressCallback — callback для уведомления о прогрессе.
type ProgressCallback func(msg ProgressMsg)

// ProgressMsg — сообщение о прогрессе анализа.
type ProgressMsg struct {
	Phase   string // "scan", "context", "prescreen", "classify", "scoring", "llm", "resolve", "done"
	Detail  string
	Files   int
	LLMPass int
}

// PipelineConfig — конфигурация конвейера анализа.
type PipelineConfig struct {
	Root    string
	Ollama  string // URL Ollama
	Model   string // model name
	NoLLM   bool
	Context context.Context
}

// PipelineResult — результат работы конвейера.
type PipelineResult struct {
	Entries      []types.Entry
	FilesScanned int
	AutoIgnored  int
	AutoKept     int
	Remaining    int
	LLMPasses    int
	// V2 context
	ProjectContext *analyzeContext.ProjectContext
	Classifications map[string]classify.ClassificationResult
	Scores          map[string]scoring.ScoringResult
	Conflicts       []resolve.Conflict
}

// RunPipeline запускает полный конвейер анализа.
func RunPipeline(cfg PipelineConfig, progress ProgressCallback) (*PipelineResult, error) {
	ctx := cfg.Context
	if ctx == nil {
		ctx = context.Background()
	}

	// Phase 1: Scan
	gitIgnore, _ := gitignore.Load(cfg.Root)

	excludedPaths := make(map[string]bool)
	heuristicSeen := make(map[string]bool)
	var heuristicEntries []types.Entry
	dirPreviewFiles := make(map[string][]string)
	var allFiles []string
	totalFiles := 0

	// Собираем термины инкрементально во время WalkDir (вместо отдельного прохода)
	termMap := make(map[string]*analyzeContext.Term)

	filepath.WalkDir(cfg.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		rel, _ := filepath.Rel(cfg.Root, path)
		if rel == "." {
			return nil
		}

		if d.Type()&fs.ModeSymlink != 0 || d.Type()&fs.ModeNamedPipe != 0 || d.Type()&fs.ModeSocket != 0 || d.Type()&fs.ModeDevice != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			base := filepath.Base(rel)
			if base == ".git" || base == ".hg" || base == ".svn" {
				return filepath.SkipDir
			}
			if heuristic.KnownDirNames[base] {
				excludedPaths[rel] = true
				if !heuristicSeen[base] {
					heuristicSeen[base] = true
					heuristicEntries = append(heuristicEntries, types.Entry{
						Path:       rel,
						Pattern:    base,
						Stage:      "heuristic",
						Reason:     "known ignore directory",
						Confidence: 100,
					})
				}
				return filepath.SkipDir
			}
			// ProtoScan директории: пропускаем, но сканируем .proto файлы
			if heuristic.ProtoScanDirNames[base] {
				excludedPaths[rel] = true
				if !heuristicSeen[base] {
					heuristicSeen[base] = true
					heuristicEntries = append(heuristicEntries, types.Entry{
						Path:       rel,
						Pattern:    base,
						Stage:      "heuristic",
						Reason:     "known ignore directory (proto-scan enabled)",
						Confidence: 100,
					})
				}
				// Быстрый scan только .proto файлов внутри директории
				collectProtoFiles(cfg.Root, rel, &allFiles, &totalFiles)
				return filepath.SkipDir
			}
			// Conditional директории: требуют проверки содержимого
			if heuristic.ConditionalDirNames[base] {
				if shouldSkipKnownDir(cfg.Root, path, base) {
					excludedPaths[rel] = true
					if !heuristicSeen[base] {
						heuristicSeen[base] = true
						heuristicEntries = append(heuristicEntries, types.Entry{
							Path:       rel,
							Pattern:    base,
							Stage:      "heuristic",
							Reason:     "conditional ignore directory",
							Confidence: 90,
						})
					}
					return filepath.SkipDir
				}
				// Не пропускаем — идём внутрь как обычно
			}
			if gitIgnore != nil && gitIgnore.ShouldSkip(rel) {
				return filepath.SkipDir
			}
			// Собираем термины из имени директории
			analyzeContext.CollectTermFromPath(termMap, rel, true)
			return nil
		}

		totalFiles++

		// Собираем preview для excluded директорий (без вложенного WalkDir)
		for exclPath := range excludedPaths {
			if strings.HasPrefix(rel, exclPath+"/") {
				base := filepath.Base(exclPath)
				if len(dirPreviewFiles[base]) < 10 {
					dirPreviewFiles[base] = append(dirPreviewFiles[base], rel)
				}
				break
			}
		}

		if excludedPaths[rel] {
			return nil
		}
		if gitIgnore != nil && gitIgnore.ShouldSkip(rel) {
			return nil
		}

		allFiles = append(allFiles, rel)

		// Собираем термины из имени файла
		analyzeContext.CollectTermFromPath(termMap, rel, false)

		if totalFiles%100 == 0 && progress != nil {
			progress(ProgressMsg{Phase: "scan", Files: totalFiles})
		}

		return nil
	})

	if progress != nil {
		progress(ProgressMsg{Phase: "scan", Detail: fmt.Sprintf("Scan complete: %d files, %d known dirs skipped", totalFiles, len(heuristicEntries)), Files: totalFiles})
	}

	// Compound files — НЕ блокируем автоматически!
	// Compound-имя (user.controller.ts) не означает мусор.
	// Проверка на сгенерированные файлы будет в PreScreen (HasGeneratedMarker).
	var remainingFiles []string
	for _, f := range allFiles {
		remainingFiles = append(remainingFiles, f)
	}

	// Phase 1.5: Context Discovery
	var projCtx *analyzeContext.ProjectContext
	if progress != nil {
		progress(ProgressMsg{Phase: "context", Detail: "Discovering project context..."})
	}

	ollamaURL := cfg.Ollama
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434"
	}
	model := cfg.Model
	if model == "" {
		model = "qwen2.5-coder:3b"
	}

	projCtx, _ = analyzeContext.Discover(ctx, cfg.Root, heuristic.KnownDirNames, ollamaURL, model, termMap)

	if progress != nil && projCtx != nil {
		detail := fmt.Sprintf("Discovered %d languages", len(projCtx.Languages))
		if len(projCtx.Terms) > 0 {
			detail += fmt.Sprintf(", %d domain terms", len(projCtx.Terms))
		}
		progress(ProgressMsg{Phase: "context", Detail: detail})
	}

	// Phase 2: Pre-screen
	if progress != nil {
		progress(ProgressMsg{Phase: "prescreen", Detail: fmt.Sprintf("Classifying %d files...", len(remainingFiles)), Files: len(remainingFiles)})
	}

	preResult := heuristic.PreScreen(remainingFiles, cfg.Root)

	if progress != nil {
		progress(ProgressMsg{
			Phase:  "prescreen",
			Detail: fmt.Sprintf("Heuristics done: %d ignored, %d kept, %d → LLM", len(preResult.AutoIgnored), len(preResult.AutoKept), len(preResult.Remaining)),
		})
	}

	// Collect entries
	seenPatterns := make(map[string]bool)
	var items []types.Entry

	for _, e := range heuristicEntries {
		if seenPatterns[e.Pattern] {
			continue
		}
		seenPatterns[e.Pattern] = true
		items = append(items, e)
	}

	for _, e := range preResult.AutoIgnored {
		if seenPatterns[e.Pattern] {
			continue
		}
		seenPatterns[e.Pattern] = true
		items = append(items, e)
	}

	result := &PipelineResult{
		FilesScanned: totalFiles,
		AutoIgnored:  len(preResult.AutoIgnored),
		AutoKept:     len(preResult.AutoKept),
		Remaining:    len(preResult.Remaining),
	}

	if cfg.NoLLM || len(preResult.Remaining) == 0 {
		result.Entries = items
		result.ProjectContext = projCtx
		return result, nil
	}

	// Phase 2.5: Classify + Score remaining files
	if progress != nil {
		progress(ProgressMsg{Phase: "classify", Detail: fmt.Sprintf("Classifying %d files...", len(preResult.Remaining))})
	}

	cls := classify.NewClassifier()
	scr := scoring.NewScorer()

	// Configure scorer with domain terms
	if projCtx != nil && len(projCtx.Terms) > 0 {
		var domainTermNames []string
		for _, t := range projCtx.Terms {
			domainTermNames = append(domainTermNames, t.Name)
		}
		scr.SetDomainTerms(domainTermNames)
	}

	classifications := make(map[string]classify.ClassificationResult)
	scores := make(map[string]scoring.ScoringResult)

	for _, f := range preResult.Remaining {
		classResult := cls.Classify(f, "", nil, nil)
		classifications[f] = classResult

		scoreResult := scr.Score(f, classResult, 0, 0)
		scores[f] = scoreResult
	}

	if progress != nil {
		progress(ProgressMsg{Phase: "classify", Detail: "Classification and scoring complete"})
	}

	// Phase 3: LLM
	groups := GroupFilesByScope(preResult.Remaining)

	if progress != nil {
		progress(ProgressMsg{Phase: "llm", Detail: fmt.Sprintf("Grouped %d files into %d scoped patterns", len(preResult.Remaining), len(groups))})
	}

	// Построение подгрупп для всех групп
	if progress != nil {
		progress(ProgressMsg{Phase: "llm", Detail: "Building subgroup trees..."})
	}
	for i := range groups {
		BuildSubGroups(&groups[i], cfg.Root)
	}

	subGroupCount := 0
	for _, g := range groups {
		subGroupCount += len(g.SubGroups)
	}
	if progress != nil {
		progress(ProgressMsg{Phase: "llm", Detail: fmt.Sprintf("Built %d subgroups from %d groups", subGroupCount, len(groups))})
	}

	llmProgress := func(current, total int, status string) {
		if progress != nil {
			progress(ProgressMsg{Phase: "llm", Detail: fmt.Sprintf("LLM batch %d/%d: %s", current, total, status)})
		}
	}

	decisions, err := llm.Evaluate3Pass(ctx, ollamaURL, model, groups, cfg.Root, llmProgress)
	if err != nil {
		return nil, fmt.Errorf("LLM evaluation: %w", err)
	}

	// Graceful degradation: если LLM недоступен, продолжаем без LLM-решений
	if decisions == nil {
		if progress != nil {
			progress(ProgressMsg{Phase: "llm", Detail: "LLM unavailable, continuing with heuristics only"})
		}
		result.Entries = items
		result.ProjectContext = projCtx
		result.Classifications = classifications
		result.Scores = scores
		return result, nil
	}

	if progress != nil {
		progress(ProgressMsg{Phase: "llm", Detail: fmt.Sprintf("LLM done: %d decisions", len(decisions))})
	}

	// Phase 4: Resolve contradictions with context
	if progress != nil {
		progress(ProgressMsg{Phase: "resolve", Detail: "Resolving contradictions..."})
	}

	resolver := resolve.NewResolver()
	if projCtx != nil && len(projCtx.Terms) > 0 {
		var domainTermNames []string
		for _, t := range projCtx.Terms {
			domainTermNames = append(domainTermNames, t.Name)
		}
		resolver.SetDomainTerms(domainTermNames)
	}

	conflicts, resolvedDecisions := resolver.Resolve(decisions, groups)
	resolvedDecisions = resolver.ApplyScoringContext(resolvedDecisions, classifications, scores)

	if progress != nil {
		progress(ProgressMsg{Phase: "resolve", Detail: fmt.Sprintf("Resolution complete: %d conflicts detected", len(conflicts))})
	}

	// Validate
	llmEntries := resolve.LLMDecisions(resolvedDecisions, groups)

	// Filter indexed extensions
	var patterns []string
	for _, d := range resolvedDecisions {
		if d.Action == "ignore" {
			patterns = append(patterns, d.Pattern)
		}
	}
	patterns, llmEntries = output.FilterIndexedExtensions(patterns, llmEntries)

	// Add LLM entries
	for _, e := range llmEntries {
		if !seenPatterns[e.Pattern] {
			seenPatterns[e.Pattern] = true
			items = append(items, e)
		}
	}

	// Add grouped patterns
	for _, p := range patterns {
		if !seenPatterns[p] {
			seenPatterns[p] = true
			stage := "llm"
			confidence := 80
			if len(p) > 0 && p[0] == '!' {
				stage = "negation"
				confidence = 90
			}
			items = append(items, types.Entry{
				Path:       p,
				Pattern:    p,
				Stage:      stage,
				Reason:     "LLM suggested",
				Confidence: confidence,
			})
		}
	}

	result.Entries = items
	result.ProjectContext = projCtx
	result.Classifications = classifications
	result.Scores = scores
	result.Conflicts = conflicts
	return result, nil
}

// MatchPatternToFiles находит файлы, подходящие под паттерн.
func MatchPatternToFiles(pattern string, files []string) []string {
	p := pattern
	if len(p) > 0 && p[0] == '!' {
		p = p[1:]
	}
	var matches []string
	for _, f := range files {
		if matchPattern(p, f) {
			matches = append(matches, f)
		}
	}
	return matches
}

// matchPattern поддерживает ** для рекурсивного匹配.
func matchPattern(pattern, path string) bool {
	// Простая проверка: если нет wildcard, проверяем как префикс директории
	if !containsWildcard(pattern) {
		// Точное совпадение
		if pattern == path {
			return true
		}
		// Префикс директории: k8s → k8s/deployment.yaml
		if strings.HasPrefix(path, pattern+"/") {
			return true
		}
		return false
	}

	// Обрабатываем ** отдельно
	if strings.Contains(pattern, "**") {
		return matchGlobstar(pattern, path)
	}

	// Стандартный filepath.Match
	matched, _ := filepath.Match(pattern, filepath.Base(path))
	if matched {
		return true
	}
	matched, _ = filepath.Match(pattern, path)
	return matched
}

// matchGlobstar обрабатывает паттерны с ** (рекурсивное совпадение).
func matchGlobstar(pattern, path string) bool {
	// Паттерн **/suffix: путь должен заканчиваться на suffix
	if strings.HasPrefix(pattern, "**/") {
		suffix := pattern[3:]
		// Точное совпадение
		if path == suffix {
			return true
		}
		// Путь заканчивается на /suffix
		if strings.HasSuffix(path, "/"+suffix) {
			return true
		}
		// Для суффиксов с wildcard: проверяем basename
		if strings.Contains(suffix, "*") || strings.Contains(suffix, "?") {
			matched, _ := filepath.Match(suffix, filepath.Base(path))
			return matched
		}
		return false
	}

	// Паттерн prefix/**: путь должен начинаться с prefix/
	if strings.HasSuffix(pattern, "/**") {
		prefix := pattern[:len(pattern)-3]
		return strings.HasPrefix(path, prefix+"/") || path == prefix
	}

	// Общий случай: используем regexp
	re := pattern
	re = strings.ReplaceAll(re, ".", "\\.")
	re = strings.ReplaceAll(re, "**", ".*")
	re = strings.ReplaceAll(re, "*", "[^/]*")
	re = strings.ReplaceAll(re, "?", ".")
	re = "^" + re + "$"

	matched, err := regexp.MatchString(re, path)
	return err == nil && matched
}

func containsWildcard(s string) bool {
	for _, c := range s {
		if c == '*' || c == '?' {
			return true
		}
	}
	return false
}

// SavePatterns — прокси для output.SavePatterns с группировкой паттернов.
func SavePatterns(root string, patterns []string) error {
	// Группируем паттерны перед сохранением
	groupedPatterns := output.GroupPatternsFromPaths(patterns)
	return output.SavePatterns(root, groupedPatterns)
}

// collectProtoFiles сканирует директорию на предмет .proto файлов
// и добавляет их в allFiles для дальнейшего анализа (gRPC контракты для crossrepo).
func collectProtoFiles(root, dirRel string, allFiles *[]string, totalFiles *int) {
	dirAbs := filepath.Join(root, dirRel)
	filepath.WalkDir(dirAbs, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(strings.ToLower(d.Name()), ".proto") {
			rel, _ := filepath.Rel(root, path)
			*allFiles = append(*allFiles, rel)
			*totalFiles++
		}
		return nil
	})
}

// shouldSkipKnownDir определяет, нужно ли пропускать условную директорию.
// Проверяет маркеры содержимого для принятия решения.
func shouldSkipKnownDir(root, absPath, base string) bool {
	switch base {
	case "packages":
		// Пропускаем для Composer/Ruby package cache.
		// НЕ пропускаем для монорепозиториев (JS/TS workspaces, Go workspaces).
		return !isMonorepoPackages(root)

	case "env":
		// Пропускаем только если это Python virtual environment.
		// Не пропускаем environment-конфиги.
		return isPythonVenv(absPath)

	default:
		return true
	}
}

// isMonorepoPackages проверяет, является ли packages/ директорией монорепозитория.
func isMonorepoPackages(root string) bool {
	// JS/TS workspaces: package.json с "workspaces"
	if data, err := os.ReadFile(filepath.Join(root, "package.json")); err == nil {
		if strings.Contains(string(data), "workspaces") {
			return true
		}
	}
	// Lerna
	if _, err := os.Stat(filepath.Join(root, "lerna.json")); err == nil {
		return true
	}
	// pnpm workspaces
	if _, err := os.Stat(filepath.Join(root, "pnpm-workspace.yaml")); err == nil {
		return true
	}
	// Go workspaces
	if _, err := os.Stat(filepath.Join(root, "go.work")); err == nil {
		return true
	}
	return false
}

// isPythonVenv проверяет, является ли директория Python virtual environment.
func isPythonVenv(absPath string) bool {
	// pyvenv.cfg — маркер Python venv
	if _, err := os.Stat(filepath.Join(absPath, "pyvenv.cfg")); err == nil {
		return true
	}
	// bin/activate — fallback-маркер
	if _, err := os.Stat(filepath.Join(absPath, "bin", "activate")); err == nil {
		return true
	}
	// Scripts/activate.ps1 — Windows
	if _, err := os.Stat(filepath.Join(absPath, "Scripts", "activate.ps1")); err == nil {
		return true
	}
	return false
}
