package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// analysisLog — логгер для детального логирования LLM-анализа.
var (
	analysisLog     zerolog.Logger
	analysisLogOnce sync.Once
)

// InitAnalysisLog инициализирует файловый логгер для анализа.
// Пишет в {root}/logs/analysis.log. Безопасно для многократного вызова.
func InitAnalysisLog(root string) {
	analysisLogOnce.Do(func() {
		logDir := filepath.Join(root, "logs")
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			analysisLog = zerolog.New(os.Stderr).With().Str("module", "analyze").Logger()
			analysisLog.Error().Err(err).Msg("failed to create log dir")
			return
		}
		path := filepath.Join(logDir, "analysis.log")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			analysisLog = zerolog.New(os.Stderr).With().Str("module", "analyze").Logger()
			analysisLog.Error().Err(err).Msg("failed to open analysis log")
			return
		}
		analysisLog = zerolog.New(zerolog.ConsoleWriter{Out: f, TimeFormat: "15:04:05.000"}).
			Level(zerolog.DebugLevel).With().Timestamp().Str("module", "analyze").Logger()
	})
}

// CheckModelAvailable проверяет доступность модели в Ollama.
func CheckModelAvailable(ctx context.Context, baseURL, model string) error {
	analysisLog.Debug().Str("base_url", baseURL).Str("model", model).Msg("checking model availability")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/tags", nil)
	if err != nil {
		analysisLog.Error().Err(err).Msg("failed to create /api/tags request")
		return fmt.Errorf("create request: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		analysisLog.Error().Err(err).Msg("ollama not reachable")
		return fmt.Errorf("ollama not available: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		analysisLog.Error().Int("status", resp.StatusCode).Msg("ollama /api/tags returned error")
		return fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		analysisLog.Error().Err(err).Msg("failed to decode /api/tags response")
		return fmt.Errorf("decode tags: %w", err)
	}

	for _, m := range tags.Models {
		if m.Name == model || strings.HasPrefix(m.Name, model+":") {
			analysisLog.Info().Str("model", model).Msg("model found in Ollama")
			return nil
		}
	}

	analysisLog.Warn().Str("model", model).Int("available_models", len(tags.Models)).Msg("model not found")
	return fmt.Errorf("model %q not found in Ollama (available: %d models)", model, len(tags.Models))
}

// CallOllama отправляет запрос к Ollama (non-streaming) с retry.
// Проверка доступности модели должна быть выполнена один раз перед началом работы (CheckModelAvailable).
func CallOllama(ctx context.Context, baseURL, model, prompt string) (string, error) {
	const maxRetries = 3
	var lastErr error

	promptLen := len(prompt)
	analysisLog.Debug().
		Int("prompt_bytes", promptLen).
		Int("prompt_lines", strings.Count(prompt, "\n")+1).
		Msg("sending request to Ollama")

	start := time.Now()

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			analysisLog.Warn().
				Int("attempt", attempt+1).
				Int("max_retries", maxRetries).
				Err(lastErr).
				Msg("retrying request")
		}

		result, err := callOllamaOnce(ctx, baseURL, model, prompt)
		elapsed := time.Since(start)

		if err == nil && len(result) > 0 {
			analysisLog.Info().
				Dur("elapsed", elapsed).
				Int("response_bytes", len(result)).
				Int("attempt", attempt+1).
				Msg("response received")
			return result, nil
		}

		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("empty response from Ollama (attempt %d/%d)", attempt+1, maxRetries)
		}

		analysisLog.Error().
			Err(lastErr).
			Dur("elapsed", elapsed).
			Int("attempt", attempt+1).
			Msg("request failed")

		if attempt < maxRetries-1 {
			select {
			case <-ctx.Done():
				analysisLog.Error().Err(ctx.Err()).Msg("context cancelled during retry backoff")
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt+1) * 2 * time.Second):
			}
		}
	}

	analysisLog.Error().
		Err(lastErr).
		Dur("total_elapsed", time.Since(start)).
		Int("attempts", maxRetries).
		Msg("all retries exhausted")

	return "", lastErr
}

func callOllamaOnce(ctx context.Context, baseURL, model, prompt string) (string, error) {
	// Динамический num_ctx: подстраиваем под размер промпта
	// ~4 символа на токен — грубая оценка
	estimatedPromptTokens := len(prompt) / 4
	numCtx := 8192
	if estimatedPromptTokens+4096 > numCtx {
		// Оставляем запас: prompt + 4096 для ответа + 512 margin
		numCtx = estimatedPromptTokens + 4096 + 512
		// Кратное 512 для эффективности KV-кэша
		numCtx = ((numCtx + 511) / 512) * 512
		// Разумный потолок
		if numCtx > 32768 {
			numCtx = 32768
		}
	}

	analysisLog.Debug().
		Int("estimated_prompt_tokens", estimatedPromptTokens).
		Int("num_ctx", numCtx).
		Msg("calculated dynamic context window")

	reqBody := map[string]interface{}{
		"model":  model,
		"prompt": prompt,
		"stream": false,
		"options": map[string]interface{}{
			"temperature": 0.1,
			"num_predict": 4096,
			"num_ctx":     numCtx,
		},
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	analysisLog.Debug().
		Int("request_bytes", len(data)).
		Str("url", baseURL+"/api/generate").
		Int("num_ctx_sent", numCtx).
		Msg("HTTP POST to Ollama")

	httpStart := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/generate", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	httpElapsed := time.Since(httpStart)

	if err != nil {
		analysisLog.Error().Err(err).Dur("http_elapsed", httpElapsed).Msg("HTTP request error")
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		analysisLog.Error().
			Int("status", resp.StatusCode).
			Dur("http_elapsed", httpElapsed).
			Str("body", string(body)).
			Msg("Ollama returned non-200 status")
		return "", fmt.Errorf("ollama status %d: %s", resp.StatusCode, string(body))
	}

	var response struct {
		Response   string `json:"response"`
		Error      string `json:"error,omitempty"`
		DoneReason string `json:"done_reason,omitempty"`
		EvalCount  int    `json:"eval_count,omitempty"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		analysisLog.Error().Err(err).Dur("http_elapsed", httpElapsed).Msg("failed to decode Ollama response")
		return "", fmt.Errorf("decode response: %w", err)
	}

	if response.Error != "" {
		analysisLog.Error().
			Str("ollama_error", response.Error).
			Dur("http_elapsed", httpElapsed).
			Msg("Ollama returned error in response body")
		return "", fmt.Errorf("ollama error: %s", response.Error)
	}

	// Логируем причину остановки
	stopReason := response.DoneReason
	if stopReason == "" {
		stopReason = "unknown"
	}

	analysisLog.Debug().
		Int("response_bytes", len(response.Response)).
		Dur("http_elapsed", httpElapsed).
		Str("stop_reason", stopReason).
		Int("eval_count", response.EvalCount).
		Int("num_ctx_sent", numCtx).
		Msg("HTTP response received")

	// Предупреждение если модель остановилась по лимиту токенов
	if stopReason == "length" {
		analysisLog.Warn().
			Int("response_bytes", len(response.Response)).
			Int("eval_count", response.EvalCount).
			Int("num_ctx_sent", numCtx).
			Msg("model stopped due to length limit (num_predict) - response may be truncated")
	}

	return response.Response, nil
}
