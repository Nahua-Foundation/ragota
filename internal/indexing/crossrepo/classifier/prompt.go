package classifier

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"ragota/internal/indexing/crossrepo/detector"
)

// buildClassificationPrompt строит промпт для LLM-классификации.
func buildClassificationPrompt(cand detector.Candidate) string {
	return fmt.Sprintf(`You are analyzing code to detect cross-service calls. Determine what type of inter-service communication this code performs.

Code context:
- File: %s
- Language: %s
- Function: %s
- Line: %d

Code snippet:
%s

Analyze the code and respond with JSON only:
{
  "type": "http" | "grpc" | "kafka" | "npm_package" | "import" | "unknown",
  "target_service": "service name or empty string",
  "endpoint_or_topic": "URL path, Kafka topic, gRPC method, or package name",
  "confidence": 0.0 to 1.0,
  "reason": "brief explanation of why"
}

Rules:
- If it's an HTTP call (fetch, axios, http.Get, requests.get, etc.) → type: "http"
- If it's a gRPC call (grpc.Dial, grpc.Invoke, generated stub) → type: "grpc"  
- If it's a Kafka producer/consumer → type: "kafka"
- If it's a package import from another service's SDK → type: "npm_package" or "import"
- If you cannot determine → type: "unknown", confidence: 0.0
- Set confidence low (< 0.7) if uncertain
- Extract the target service name from URLs, package names, or variable names`,
		cand.FilePath, cand.Language, cand.Symbol, cand.Line, cand.RawCode)
}

// extractJSON извлекает JSON из markdown-обёртки.
func extractJSON(llmRes string) string {
	extracted := llmRes
	if start := strings.Index(llmRes, "```"); start >= 0 {
		rest := llmRes[start+3:]
		if strings.HasPrefix(rest, "json") || strings.HasPrefix(rest, "JSON") {
			nl := strings.Index(rest, "\n")
			if nl >= 0 {
				rest = rest[nl+1:]
			}
		}
		end := strings.Index(rest, "```")
		if end > 0 {
			extracted = strings.TrimSpace(rest[:end])
		}
	}
	return extracted
}

// parseLLMResponse парсит ответ LLM в ClassificationResult.
func parseLLMResponse(response string) (*ClassificationResult, error) {
	var result ClassificationResult

	// Пробуем прямой парсинг
	err := json.Unmarshal([]byte(response), &result)
	if err != nil {
		// Пробуем извлечь из markdown
		extracted := extractJSON(response)
		err = json.Unmarshal([]byte(extracted), &result)
		if err != nil {
			return nil, fmt.Errorf("LLM response parse failed: %w, raw: %q", err, response)
		}
	}

	return &result, nil
}

// ollamaCall вызывает Ollama /api/generate.
func ollamaCall(ollamaURL, model, prompt string) (string, error) {
	url := strings.TrimRight(ollamaURL, "/") + "/api/generate"

	body, _ := json.Marshal(map[string]any{
		"model":  model,
		"prompt": prompt,
		"stream": false,
		"format": "json",
		"options": map[string]any{
			"temperature": 0.1,
		},
	})

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ollama error %d: %s", resp.StatusCode, string(b))
	}

	var res struct {
		Response string `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", fmt.Errorf("ollama decode: %w", err)
	}

	return res.Response, nil
}
