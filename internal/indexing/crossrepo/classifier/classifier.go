// Package classifier — LLM-классификация кандидатов cross-repo вызовов.
//
// Использует Ollama (qwen2.5-coder:3b) с температурой 0.1 для
// детерминированности. Кэширует результаты по хешу кода:
//   - in-memory LRU (10K entries)
//   - file-backed кэш (cross_llm_cache.json)
//
// Порог confidence = 0.7 — ниже не записываем.
package classifier

import (
	"context"

	"ragota/internal/indexing/crossrepo/detector"
	"ragota/pkg/repos"
)

// Classifier классифицирует кандидатов через LLM.
type Classifier struct {
	ollamaURL string
	model     string
	cache     *Cache
}

// New создаёт classifier.
func New(ollamaURL, model string) *Classifier {
	return &Classifier{
		ollamaURL: ollamaURL,
		model:     model,
		cache:     NewCache(10000),
	}
}

// ClassificationResult — результат LLM-классификации одного кандидата.
type ClassificationResult struct {
	Protocol       string  `json:"type"`                 // http, grpc, kafka, npm_package, unknown
	TargetService  string  `json:"target_service"`       // имя сервиса или ""
	Endpoint       string  `json:"endpoint_or_topic"`    // endpoint, topic, method
	Confidence     float64 `json:"confidence"`           // 0.0-1.0
	Reason         string  `json:"reason"`               // объяснение
}

// ClassifyBatch классифицирует батч кандидатов.
// Возвращает количество успешно классифицированных и список edges.
func (c *Classifier) ClassifyBatch(ctx context.Context, candidates []detector.Candidate, resolver *repos.Resolver) (int, []detector.CrossEdge, error) {
	var edges []detector.CrossEdge
	classified := 0

	for _, cand := range candidates {
		result, err := c.classifyOne(ctx, cand)
		if err != nil {
			continue
		}
		if result.Confidence < 0.7 {
			continue
		}
		if result.Protocol == "unknown" {
			continue
		}

		classified++

		// Resolve target repo
		targetRepo := ""
		if resolver != nil {
			targetRepo = resolverForService(result.TargetService, resolver)
		}
		if targetRepo == "" {
			targetRepo = result.TargetService
		}

		edges = append(edges, detector.CrossEdge{
			SrcRepo:    cand.Repo,
			SrcFile:    cand.FilePath,
			SrcLine:    cand.Line,
			SrcSymbol:  cand.Symbol,
			DstRepo:    targetRepo,
			DstName:    result.Endpoint,
			Protocol:   result.Protocol,
			Confidence: result.Confidence,
			LLMReason:  result.Reason,
		})
	}

	return classified, edges, nil
}

// classifyOne классифицирует одного кандидата (с кэшем).
func (c *Classifier) classifyOne(ctx context.Context, cand detector.Candidate) (*ClassificationResult, error) {
	cacheKey := CandidateCacheKey(cand)

	// Проверяем кэш
	if cached, ok := c.cache.Get(cacheKey); ok {
		return cached, nil
	}

	// LLM вызов
	result, err := c.callLLM(ctx, cand)
	if err != nil {
		return nil, err
	}

	// Кэшируем
	c.cache.Set(cacheKey, result)

	return result, nil
}

// callLLM делает запрос к Ollama.
func (c *Classifier) callLLM(ctx context.Context, cand detector.Candidate) (*ClassificationResult, error) {
	prompt := buildClassificationPrompt(cand)
	response, err := c.ollamaGenerate(ctx, prompt)
	if err != nil {
		return nil, err
	}

	return parseLLMResponse(response)
}

// ollamaGenerate вызывает Ollama API.
func (c *Classifier) ollamaGenerate(ctx context.Context, prompt string) (string, error) {
	// Реализация через HTTP вызов к Ollama
	return ollamaCall(c.ollamaURL, c.model, prompt)
}

// resolverForService сопоставляет имя сервиса с репо.
func resolverForService(serviceName string, resolver *repos.Resolver) string {
	if resolver == nil || serviceName == "" {
		return ""
	}

	all := resolver.All()
	for _, repo := range all {
		// Точное совпадение
		if repo.Name == serviceName {
			return repo.Name
		}
		// Частичное: serviceName содержит repo.Name или наоборот
		if containsIgnoreCase(repo.Name, serviceName) || containsIgnoreCase(serviceName, repo.Name) {
			return repo.Name
		}
		// Без дефисов/undercores
		if normalizeName(repo.Name) == normalizeName(serviceName) {
			return repo.Name
		}
	}
	return ""
}

func containsIgnoreCase(a, b string) bool {
	return len(a) > 0 && len(b) > 0 &&
		(len(a) >= len(b) && stringContainsFold(a, b) ||
			len(b) >= len(a) && stringContainsFold(b, a))
}

func stringContainsFold(s, substr string) bool {
	s = toLower(s)
	substr = toLower(substr)
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func toLower(s string) string {
	result := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			result[i] = c + 32
		} else {
			result[i] = c
		}
	}
	return string(result)
}

func normalizeName(s string) string {
	result := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '-' || c == '_' {
			continue
		}
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		result = append(result, c)
	}
	return string(result)
}
