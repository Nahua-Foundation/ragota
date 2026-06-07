package llm

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"ragota/internal/analyze/types"
)

// ProgressFunc — callback для уведомления о прогрессе LLM.
type ProgressFunc func(current, total int, status string)

// MaxBatchTokens — максимальное количество токенов в батче.
// 6000 данных + ~700 system + 2000 output = ~8700, помещается в num_ctx=8192 с запасом.
const MaxBatchTokens = 6000

// EstimatedTokensPerFile — оценка токенов на sampled-файл (JSON metadata + 3 чанка по 10 строк).
const EstimatedTokensPerFile = 500

// MaxDeepReviews — максимальное количество deep review запросов.
const MaxDeepReviews = 5

// batchWorkers — количество параллельных воркеров для обработки батчей.
// Уменьшено с 3 до 2 для снижения конкуренции за ресурсы Ollama.
const batchWorkers = 2

// deepReviewWorkers — количество параллельных воркеров для deep review.
const deepReviewWorkers = 2

// Evaluate3Pass выполняет 3-pass LLM оценку.
// Возвращает (nil, nil) если LLM недоступен — это graceful degradation.
// groups уже должны иметь подгруппы (BuildSubGroups вызывается перед этим).
func Evaluate3Pass(ctx context.Context, ollamaURL, model string, groups []types.FileGroup, root string, progress ProgressFunc) ([]types.GroupDecision, error) {
	if len(groups) == 0 {
		return nil, nil
	}

	// Инициализация логгера для детального отслеживания
	InitAnalysisLog(root)
	analysisLog.Info().
		Int("groups", len(groups)).
		Str("model", model).
		Str("ollama_url", ollamaURL).
		Msg("starting LLM evaluation")

	// Проверка доступности LLM перед началом
	if err := CheckModelAvailable(ctx, ollamaURL, model); err != nil {
		analysisLog.Warn().Err(err).Msg("LLM unavailable, graceful degradation")
		return nil, nil
	}

	// Формирование батчей по токенам
	batches := createBatchesByTokens(groups)
	analysisLog.Info().Int("batches", len(batches)).Msg("batches formed")

	if progress != nil {
		progress(1, len(batches), fmt.Sprintf("Processing %d batches in parallel...", len(batches)))
	}

	// Parallel batch processing
	type batchResult struct {
		batchIdx  int
		decisions []types.GroupDecision
		err       error
	}

	results := make([]batchResult, len(batches))
	var completedCount int
	var failedCount int
	var mu sync.Mutex
	sem := make(chan struct{}, batchWorkers)
	var wg sync.WaitGroup

	pipelineStart := time.Now()

	for batchIdx, batch := range batches {
		wg.Add(1)
		sem <- struct{}{}

		go func(idx int, b []types.FileGroup) {
			defer wg.Done()
			defer func() { <-sem }()

			batchStart := time.Now()
			analysisLog.Debug().Int("batch", idx).Int("groups_in_batch", len(b)).Msg("batch started")

			if ctx.Err() != nil {
				analysisLog.Warn().Int("batch", idx).Msg("context cancelled before batch")
				results[idx] = batchResult{batchIdx: idx, err: ctx.Err()}
				return
			}

			for i := range b {
				if len(b[i].SubGroups) > 0 {
					SelectSamplesFromSubGroups(b[i].SubGroups, root)
				}
			}

			decisions, err := evaluateBatch(ctx, ollamaURL, model, b, root)
			batchElapsed := time.Since(batchStart)

			mu.Lock()
			completedCount++
			current := completedCount
			if err != nil {
				failedCount++
				results[idx] = batchResult{batchIdx: idx, err: err}
				analysisLog.Error().
					Err(err).
					Int("batch", idx).
					Dur("batch_elapsed", batchElapsed).
					Msg("batch failed")
				mu.Unlock()

				if progress != nil {
					progress(current+1, len(batches), fmt.Sprintf("Batch %d/%d FAILED (%s)", current, len(batches), err.Error()))
				}
				return
			}

			results[idx] = batchResult{batchIdx: idx, decisions: decisions}
			analysisLog.Info().
				Int("batch", idx).
				Int("decisions", len(decisions)).
				Dur("batch_elapsed", batchElapsed).
				Msg("batch complete")
			mu.Unlock()

			if progress != nil {
				progress(current+1, len(batches), fmt.Sprintf("Batch %d/%d complete (%d decisions)", current, len(batches), len(decisions)))
			}
		}(batchIdx, batch)
	}

	wg.Wait()

	pipelineElapsed := time.Since(pipelineStart)

	var allDecisions []types.GroupDecision
	for _, r := range results {
		if r.err == nil && len(r.decisions) > 0 {
			allDecisions = append(allDecisions, r.decisions...)
		}
	}

	// Защита репозиториев: блокируем игнорирование корневых паттернов репозиториев
	protectedCount := 0
	for i := range allDecisions {
		if HasRepoRootPattern(allDecisions[i].Pattern) && allDecisions[i].Action == "ignore" {
			allDecisions[i].Action = "keep"
			allDecisions[i].Confidence = 95
			protectedCount++
			analysisLog.Info().
				Str("pattern", allDecisions[i].Pattern).
				Msg("blocked repo-root ignore pattern")
		}
	}
	if protectedCount > 0 {
		analysisLog.Info().
			Int("protected", protectedCount).
			Msg("repository root protection applied")
	}

	analysisLog.Info().
		Int("total_decisions", len(allDecisions)).
		Int("failed_batches", failedCount).
		Dur("total_elapsed", pipelineElapsed).
		Msg("batch phase complete")

	if progress != nil {
		status := fmt.Sprintf("Classification: %d/%d batches OK", len(batches)-failedCount, len(batches))
		if failedCount > 0 {
			status += fmt.Sprintf(", %d failed", failedCount)
		}
		progress(len(batches), len(batches), status)
	}

	// Deep review — parallel, limited to MaxDeepReviews lowest-confidence
	allDecisions = deepReviewParallel(ctx, ollamaURL, model, allDecisions, groups, root, progress)

	return allDecisions, nil
}

// maxGroupsPerBatch — максимальное количество групп в одном батче.
// Уменьшено с 8 до 4 для снижения размера промпта и уменьшения шансов malformed JSON.
const maxGroupsPerBatch = 4

// createBatchesByTokens разбивает группы на батчи по токенам и количеству групп.
func createBatchesByTokens(groups []types.FileGroup) [][]types.FileGroup {
	var batches [][]types.FileGroup
	var currentBatch []types.FileGroup
	currentTokens := 0

	for _, g := range groups {
		estimatedTokens := estimateGroupTokens(g)

		// Ограничиваем батч по токенам ИЛИ по количеству групп
		if (currentTokens+estimatedTokens > MaxBatchTokens || len(currentBatch) >= maxGroupsPerBatch) && len(currentBatch) > 0 {
			batches = append(batches, currentBatch)
			currentBatch = nil
			currentTokens = 0
		}

		currentBatch = append(currentBatch, g)
		currentTokens += estimatedTokens
	}

	if len(currentBatch) > 0 {
		batches = append(batches, currentBatch)
	}

	return batches
}

// estimateGroupTokens оценивает размер группы в токенах.
func estimateGroupTokens(g types.FileGroup) int {
	if len(g.SubGroups) > 0 {
		// С подгруппами: считаем по общему числу файлов × 20%
		totalFiles := 0
		for _, sg := range g.SubGroups {
			totalFiles += len(sg.Files)
		}
		// 20% файлов будут с сэмплами
		sampledFiles := totalFiles / 5
		if sampledFiles < 1 {
			sampledFiles = 1
		}
		// 250 токенов на файл (3 чанка × 10 строк × ~4 токена/строку + метаданные)
		return sampledFiles * EstimatedTokensPerFile
	}
	// Без подгрупп: грубая оценка по файлам
	if len(g.Files) <= 5 {
		return 200
	}
	return len(g.Files) * 50
}

// maxPromptBytes — максимальный размер промпта.
// Увеличено до 20KB для qwen3:4b (модель справляется с большими контекстами).
const maxPromptBytes = 20000

// evaluateBatch выполняет single-pass классификацию (classify + self-review в одном промпте).
// Если промпт превышает maxPromptBytes — обрабатывает группы по одной.
func evaluateBatch(ctx context.Context, ollamaURL, model string, groups []types.FileGroup, root string) ([]types.GroupDecision, error) {
	prompt := BuildGroupEvaluationPrompt(groups, root)

	if len(prompt) > maxPromptBytes && len(groups) > 1 {
		analysisLog.Warn().
			Int("prompt_bytes", len(prompt)).
			Int("groups", len(groups)).
			Msg("prompt too large, processing groups individually")

		// Обрабатываем группы по одной вместо split пополам
		var allDecisions []types.GroupDecision
		failedGroups := 0

		for i, g := range groups {
			d, err := evaluateBatch(ctx, ollamaURL, model, []types.FileGroup{g}, root)
			if err != nil {
				analysisLog.Warn().
					Int("group", i).
					Str("pattern", g.Pattern).
					Err(err).
					Msg("group failed, skipping")
				failedGroups++
				continue
			}
			allDecisions = append(allDecisions, d...)
		}

		if failedGroups > 0 {
			analysisLog.Warn().
				Int("failed", failedGroups).
				Int("total", len(groups)).
				Msg("individual group processing complete")
		}

		return allDecisions, nil
	}

	// Подсчёт сэмплов для логирования
	totalSamples := 0
	for _, g := range groups {
		for _, sg := range g.SubGroups {
			totalSamples += len(sg.Samples)
		}
	}

	analysisLog.Debug().
		Int("prompt_bytes", len(prompt)).
		Int("groups", len(groups)).
		Int("samples", totalSamples).
		Msg("evaluating batch prompt")

	// Retry при parse-ошибках (модель может вернуть обрезанный JSON или markdown)
	const maxParseRetries = 2
	var lastErr error

	for attempt := 0; attempt <= maxParseRetries; attempt++ {
		response, err := CallOllama(ctx, ollamaURL, model, prompt)
		if err != nil {
			return nil, err
		}

		decisions, err := ParseGroupDecisions(response)
		if err == nil && len(decisions) > 0 {
			// Валидация формата решений — фильтруем мусорные паттерны
			validDecisions := make([]types.GroupDecision, 0, len(decisions))
			invalidCount := 0
			for _, d := range decisions {
				if validationErr := validateGroupDecisions([]types.GroupDecision{d}); validationErr != nil {
					invalidCount++
					analysisLog.Warn().
						Err(validationErr).
						Str("pattern", d.Pattern).
						Msg("filtered invalid LLM pattern")
					continue
				}
				validDecisions = append(validDecisions, d)
			}
			if invalidCount > 0 {
				analysisLog.Warn().
					Int("invalid", invalidCount).
					Int("valid", len(validDecisions)).
					Msg("filtered invalid LLM decisions")
			}
			if len(validDecisions) > 0 {
				return validDecisions, nil
			}
			// Все решения невалидны — retry
			lastErr = fmt.Errorf("all decisions invalid")
			analysisLog.Warn().
				Int("attempt", attempt+1).
				Str("response_preview", truncate(response, 300)).
				Msg("all decisions invalid, retrying")
		}

		if err != nil {
			lastErr = err
			analysisLog.Warn().
				Err(err).
				Int("attempt", attempt+1).
				Str("response_preview", truncate(response, 200)).
				Msg("parse failed, retrying")
		} else {
			lastErr = fmt.Errorf("LLM returned empty decisions")
			analysisLog.Warn().
				Int("attempt", attempt+1).
				Str("response_preview", truncate(response, 200)).
				Msg("empty decisions, retrying")
		}

		if attempt < maxParseRetries {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt+1) * 3 * time.Second):
			}
		}
	}

	return nil, fmt.Errorf("parse failed after %d attempts: %w", maxParseRetries+1, lastErr)
}

type deepReviewResult struct {
	index    int
	decision *types.GroupDecision
}

// deepReviewParallel выполняет deep review параллельно для N самых low-confidence групп.
func deepReviewParallel(ctx context.Context, ollamaURL, model string, decisions []types.GroupDecision, groups []types.FileGroup, root string, progress ProgressFunc) []types.GroupDecision {
	type indexedDecision struct {
		index      int
		confidence int
		pattern    string
	}

	var lowConf []indexedDecision
	for i, d := range decisions {
		if d.Confidence < 60 {
			lowConf = append(lowConf, indexedDecision{
				index:      i,
				confidence: d.Confidence,
				pattern:    d.Pattern,
			})
		}
	}

	if len(lowConf) == 0 {
		analysisLog.Debug().Msg("no low-confidence decisions, skipping deep review")
		return decisions
	}

	sort.Slice(lowConf, func(i, j int) bool {
		return lowConf[i].confidence < lowConf[j].confidence
	})

	if len(lowConf) > MaxDeepReviews {
		lowConf = lowConf[:MaxDeepReviews]
	}

	analysisLog.Info().Int("low_conf_count", len(lowConf)).Int("max_reviews", MaxDeepReviews).Msg("starting deep review")

	groupMap := make(map[string]*types.FileGroup)
	for i := range groups {
		groupMap[groups[i].Pattern] = &groups[i]
	}

	resultCh := make(chan deepReviewResult, len(lowConf))
	sem := make(chan struct{}, deepReviewWorkers)
	var wg sync.WaitGroup
	drStart := time.Now()

	for idx, ld := range lowConf {
		wg.Add(1)
		sem <- struct{}{}

		go func(i int, ld indexedDecision) {
			defer wg.Done()
			defer func() { <-sem }()

			if ctx.Err() != nil {
				analysisLog.Warn().Str("pattern", ld.pattern).Msg("context cancelled during deep review")
				return
			}

			group, ok := groupMap[ld.pattern]
			if !ok || len(group.Files) == 0 {
				analysisLog.Debug().Str("pattern", ld.pattern).Msg("group not found or empty, skipping deep review")
				return
			}

			reviewStart := time.Now()
			analysisLog.Debug().
				Str("pattern", ld.pattern).
				Int("confidence", ld.confidence).
				Int("files", len(group.Files)).
				Msg("deep review started")

			prompt := BuildDeepReviewPrompt(*group, root)
			response, err := CallOllama(ctx, ollamaURL, model, prompt)
			if err != nil {
				analysisLog.Error().
					Err(err).
					Str("pattern", ld.pattern).
					Dur("review_elapsed", time.Since(reviewStart)).
					Msg("deep review request failed")
				return
			}

			reviewed, err := ParseGroupDecisions(response)
			if err != nil || len(reviewed) == 0 {
				analysisLog.Warn().
					Str("pattern", ld.pattern).
					Err(err).
					Msg("deep review parse failed")
				return
			}

			analysisLog.Info().
				Str("pattern", ld.pattern).
				Str("new_action", reviewed[0].Action).
				Int("new_confidence", reviewed[0].Confidence).
				Dur("review_elapsed", time.Since(reviewStart)).
				Msg("deep review complete")

			resultCh <- deepReviewResult{index: ld.index, decision: &reviewed[0]}

			if progress != nil {
				progress(i+1, len(lowConf), fmt.Sprintf("Deep review %d/%d: %s → %s (%d%%)",
					i+1, len(lowConf), ld.pattern, reviewed[0].Action, reviewed[0].Confidence))
			}
		}(idx, ld)
	}

	wg.Wait()
	close(resultCh)

	reviewCount := 0
	for r := range resultCh {
		if r.decision != nil {
			decisions[r.index] = *r.decision
			reviewCount++
		}
	}

	analysisLog.Info().
		Int("reviews_completed", reviewCount).
		Int("reviews_attempted", len(lowConf)).
		Dur("total_elapsed", time.Since(drStart)).
		Msg("deep review phase complete")

	return decisions
}

// CountSubGroups подсчитывает общее количество подгрупп.
func CountSubGroups(groups []types.FileGroup) int {
	total := 0
	for _, g := range groups {
		total += len(g.SubGroups)
	}
	return total
}

// FormatGroupSummary формирует строку статуса для группы.
func FormatGroupSummary(g types.FileGroup) string {
	if len(g.SubGroups) > 0 {
		sampleCount := 0
		for _, sg := range g.SubGroups {
			sampleCount += len(sg.Samples)
		}
		return fmt.Sprintf("%s (%d subgroups, %d samples)", g.Pattern, len(g.SubGroups), sampleCount)
	}
	return fmt.Sprintf("%s (%d files)", g.Pattern, len(g.Files))
}

// GetGroupTokenEstimate возвращает оценку токенов для группы.
func GetGroupTokenEstimate(g types.FileGroup) int {
	return estimateGroupTokens(g)
}

// HasRepoRootPattern проверяет, содержит ли группа паттерн корня репозитория.
func HasRepoRootPattern(pattern string) bool {
	// Паттерны вида **/reponame/* или reponame/*
	if strings.HasPrefix(pattern, "**/") {
		parts := strings.Split(strings.TrimPrefix(pattern, "**/"), "/")
		if len(parts) == 2 && parts[1] == "*" {
			return true
		}
	}
	parts := strings.Split(pattern, "/")
	if len(parts) == 2 && parts[1] == "*" && !strings.Contains(parts[0], "*") {
		return true
	}
	return false
}

// validateGroupDecisions проверяет, что решения имеют правильный формат.
// Возвращает ошибку, если модель вернула неправильный формат (например, path/extension вместо pattern/action).
func validateGroupDecisions(decisions []types.GroupDecision) error {
	for i, d := range decisions {
		// Проверка: pattern должен быть указан и не должен быть пустым
		if d.Pattern == "" {
			return fmt.Errorf("decision %d: missing 'pattern' field", i)
		}
		// Проверка: pattern не должен выглядеть как абсолютный путь
		if strings.HasPrefix(d.Pattern, "/") || strings.HasPrefix(d.Pattern, "C:") {
			return fmt.Errorf("decision %d: pattern '%s' looks like absolute path, expected glob pattern", i, d.Pattern)
		}
		// Проверка: action должен быть "keep" или "ignore"
		if d.Action != "keep" && d.Action != "ignore" {
			return fmt.Errorf("decision %d: invalid action '%s', expected 'keep' or 'ignore'", i, d.Action)
		}
		// Проверка: confidence должен быть в диапазоне 0-100
		if d.Confidence < 0 || d.Confidence > 100 {
			return fmt.Errorf("decision %d: confidence %d out of range [0, 100]", i, d.Confidence)
		}
		// Проверка: pattern не должен быть мусорным (LLM hallucination)
		if !isValidPattern(d.Pattern) {
			return fmt.Errorf("decision %d: pattern '%s' is invalid (likely LLM hallucination)", i, d.Pattern)
		}
	}
	return nil
}

// isValidPattern проверяет, что паттерн не является мусорным (LLM hallucination).
// Отбрасывает только явно невалидные синтаксические паттерны.
// Разрешает валидные пути типа issuance/src/report/report-creation/*.ts
func isValidPattern(pattern string) bool {
	// Убираем префиксы и суффиксы для анализа
	p := strings.TrimPrefix(pattern, "**/")
	p = strings.TrimSuffix(p, "/**")

	// Проверка 1: Множественные точки подряд (.., ..., ....) — явная галлюцинация
	if strings.Contains(p, "..") {
		return false
	}

	// Проверка 2: Слишком много звёздочек в простом паттерне (без путей)
	if !strings.Contains(p, "/") {
		stars := strings.Count(p, "*")
		if stars > 2 {
			return false
		}
	}

	// Проверка 3: Паттерны вида *.ext.* (wildcard sandwich) — только для коротких расширений
	// Разбиваем по точкам и анализируем
	parts := strings.Split(p, ".")
	if len(parts) >= 3 {
		// Проверяем middle parts на звёздочки — это признак галлюцинации
		// "*.verylongpattern*.ts" → middle part "verylongpattern*" содержит *
		middleParts := parts[1 : len(parts)-1]
		for _, mp := range middleParts {
			if strings.Contains(mp, "*") {
				// Звёздочка в middle part — это галлюцинация
				return false
			}
		}

		// Проверяем на "*.something.*" паттерн
		firstPart := parts[0]
		lastPart := parts[len(parts)-1]

		// Если начинается и заканчивается на * — это потенциальный wildcard sandwich
		if strings.HasPrefix(firstPart, "*") && strings.HasSuffix(lastPart, "*") {
			// Исключение: *.controller.ts, *.service.ts — это валидно (compound extension)
			// Проверяем что между звёздочками есть известное compound-расширение (>= 6 символов)
			hasValidMiddle := false
			for _, mp := range middleParts {
				// Валидное compound-расширение: >= 6 символов без звёздочек
				// controller, service, repository, component, module, etc.
				if len(mp) >= 6 && mp != "*" {
					hasValidMiddle = true
					break
				}
			}
			if !hasValidMiddle {
				// Нет валидного compound-расширения — это галлюцинация
				return false
			}
		}
	}

	// Проверка 4: Звёздочка в середине сегмента пути (не в начале/конце)
	for _, part := range strings.Split(p, "/") {
		if strings.Contains(part, "*") {
			// Исключение: ** (globstar) — это валидно
			if part == "**" {
				continue
			}
			// Звёздочка должна быть только в начале или конце сегмента
			// Разрешаем: *.go, **/*.ts, foo/*, *foo*, foo*bar
			// Запрещаем: f*o (звёздочка внутри без смысла)
			trimmed := strings.Trim(part, "*")
			if strings.Contains(trimmed, "*") && trimmed != "*" {
				// Multiple stars inside segment — suspicious
				if strings.Count(part, "*") > 2 {
					return false
				}
			}
			// Проверяем на длинный сегмент со звёздочкой внутри
			if strings.Contains(part, "*") && !strings.HasPrefix(part, "*") && !strings.HasSuffix(part, "*") {
				if len(part) > 12 {
					return false
				}
			}
		}
	}

	// Проверка 5: Подозрительные комбинации звёзд
	suspiciousPatterns := []string{
		"*****", "****",
		"*.*.*.*",
	}
	for _, sp := range suspiciousPatterns {
		if strings.Contains(p, sp) {
			return false
		}
	}

	return true
}
