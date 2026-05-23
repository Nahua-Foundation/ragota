package rerank

import (
	"regexp"
	"strconv"
	"strings"
)

// isEmbeddingReranker — эвристика: модели семейства bge-reranker / *-m3 /
// e5-reranker и подобные — это cross-encoder классификаторы без LM-head.
// Через /api/generate они физически не умеют выдать осмысленный текст
// (Ollama не экспонирует sequence-classification logits). Для них
// используем embedding-fallback: cosine(query, document) через /api/embed.
func isEmbeddingReranker(model string) bool {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "bge-reranker"),
		strings.Contains(m, "bge-m3"),
		strings.HasSuffix(m, "-m3"),
		strings.Contains(m, "-m3:"),
		strings.Contains(m, "e5-reranker"),
		strings.Contains(m, "jina-reranker"),
		strings.Contains(m, "mxbai-rerank"):
		return true
	}
	return false
}

func buildPrompt(query, symbol, path, language, content string) string {
	var b strings.Builder
	b.WriteString("Instruction: Evaluate the relevance of the Document to the Query. Output only a single number between 0.0 and 1.0.\n\n")

	// Few-shot examples
	b.WriteString("Query: \"how to install golang\"\n")
	b.WriteString("Document: \"To install Go, download the installer from golang.org...\"\n")
	b.WriteString("Relevance Score: 1.0\n\n")

	b.WriteString("Query: \"weather forecast\"\n")
	b.WriteString("Document: \"package main; func main() {}\"\n")
	b.WriteString("Relevance Score: 0.0\n\n")

	b.WriteString("Query: \"")
	b.WriteString(query)
	b.WriteString("\"\n")
	if symbol != "" || path != "" || language != "" {
		b.WriteString("Context: ")
		if symbol != "" {
			b.WriteString("symbol " + symbol + "; ")
		}
		if path != "" {
			b.WriteString("file " + path + "; ")
		}
		if language != "" {
			b.WriteString("lang " + language + "; ")
		}
		b.WriteString("\n")
	}
	b.WriteString("Document:\n")
	b.WriteString(content)
	b.WriteString("\nRelevance Score: ")
	return b.String()
}

var numRe = regexp.MustCompile(`-?\d+([.,]\d+)?`)

// parseScore — извлекает первое float-число из LLM-ответа и нормализует
// его в [0,1]. Если ничего не найдено — возвращает 0.
func parseScore(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	m := numRe.FindString(s)
	if m == "" {
		// Эвристика: yes/no.
		lower := strings.ToLower(s)
		if strings.HasPrefix(lower, "yes") || strings.HasPrefix(lower, "relevant") {
			return 1
		}
		return 0
	}
	m = strings.Replace(m, ",", ".", 1)
	v, err := strconv.ParseFloat(m, 64)
	if err != nil {
		return 0
	}
	// КЛАМПИНГ: "1.5" -> 1.0, "42.5" -> 0.425
	if v > 1 && v <= 100 {
		if strings.HasPrefix(m, "1.") {
			v = 1.0
		} else {
			v = v / 100.0
		}
	}
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return v
}
