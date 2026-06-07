package context

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"ragota/internal/analyze/llm"
)

// ProjectContext — полный контекст проекта, извлечённый на Этапе 0.
type ProjectContext struct {
	Languages []Language
	Terms     []*Term // значимые термины, отсортированные по частоте
}

// Discover выполняет полное обнаружение контекста проекта.
// Если preCollectedTerms != nil, использует их вместо отдельного WalkDir (экономия одного прохода).
func Discover(ctx context.Context, root string, skipDirs map[string]bool, ollamaURL, model string, preCollectedTerms map[string]*Term) (*ProjectContext, error) {
	langs := DetectLanguages(root)

	var termMap map[string]*Term
	if preCollectedTerms != nil {
		termMap = preCollectedTerms
	} else {
		termMap = ExtractTermsFromNames(root, skipDirs)
	}

	// Определяем минимальную частоту: не менее 3 вхождений для значимости
	significant := FilterSignificantTerms(termMap, 3)

	pc := &ProjectContext{
		Languages: langs,
		Terms:     significant,
	}

	// Категоризация через LLM (если доступен)
	if ollamaURL != "" && len(significant) > 0 {
		if err := categorizeTerms(ctx, pc, ollamaURL, model); err != nil {
			// Не блокируем — просто оставляем CategoryUnknown
			_ = err
		}
	}

	return pc, nil
}

// categorizeTerms отправляет термины в LLM для семантической категоризации.
func categorizeTerms(ctx context.Context, pc *ProjectContext, ollamaURL, model string) error {
	if len(pc.Terms) == 0 {
		return nil
	}

	// Ограничиваем время на категоризацию — 60 секунд максимум
	categorizeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// Ограничиваем количество терминов для LLM
	maxTerms := 80
	terms := pc.Terms
	if len(terms) > maxTerms {
		terms = terms[:maxTerms]
	}

	var termList strings.Builder
	for _, t := range terms {
		fmt.Fprintf(&termList, "- %s (freq: %d)\n", t.Name, t.Freq)
	}

	langInfo := ""
	for _, l := range pc.Languages {
		langInfo += l.Name + " "
	}

	prompt := fmt.Sprintf(`You are analyzing a software project to categorize domain terms.

Project languages: %s

Here are the most frequent terms found in file and directory names:
%s

Categorize each term into one of these abstract categories:
- "entity" — represents a business domain concept (e.g., user, order, payment, product, account)
- "process" — represents a business operation or workflow (e.g., checkout, billing, migration, sync)
- "interface" — represents an external interaction point (e.g., gateway, webhook, endpoint, client)
- "infrastructure" — represents technical plumbing (e.g., cache, queue, database, logger, router)

Respond with JSON array only:
[
  {"term": "payment", "category": "entity"},
  {"term": "checkout", "category": "process"},
  {"term": "gateway", "category": "interface"},
  {"term": "cache", "category": "infrastructure"}
]

If a term doesn't fit any category, use "unknown".
Respond with JSON only, no explanation.`, langInfo, termList.String())

	response, err := llm.CallOllama(categorizeCtx, ollamaURL, model, prompt)
	if err != nil {
		return fmt.Errorf("categorize terms: %w", err)
	}

	var decisions []struct {
		Term     string `json:"term"`
		Category string `json:"category"`
	}

	response = extractJSON(response)
	if err := json.Unmarshal([]byte(response), &decisions); err != nil {
		return fmt.Errorf("parse categorization: %w", err)
	}

	// Применяем категории
	termIndex := make(map[string]*Term)
	for _, t := range pc.Terms {
		termIndex[t.Name] = t
	}
	for _, d := range decisions {
		if t, ok := termIndex[strings.ToLower(d.Term)]; ok {
			switch d.Category {
			case "entity":
				t.Category = CategoryEntity
			case "process":
				t.Category = CategoryProcess
			case "interface":
				t.Category = CategoryInterface
			case "infrastructure":
				t.Category = CategoryInfrastructure
			}
		}
	}

	return nil
}

// extractJSON извлекает JSON из возможного markdown-обёртывания.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		lines := strings.Split(s, "\n")
		start := 1
		end := len(lines)
		for i := len(lines) - 1; i >= start; i-- {
			if strings.TrimSpace(lines[i]) == "```" {
				end = i
				break
			}
		}
		if start < end {
			s = strings.Join(lines[start:end], "\n")
		}
	}
	firstBracket := strings.Index(s, "[")
	lastBracket := strings.LastIndex(s, "]")
	if firstBracket >= 0 && lastBracket > firstBracket {
		s = s[firstBracket : lastBracket+1]
	}
	return strings.TrimSpace(s)
}
