package llm

import (
	"encoding/json"
	"fmt"
	"strings"

	"ragota/internal/analyze/types"
)

// ParseGroupDecisions парсит JSON-ответ LLM с защитой от мусорного вывода.
func ParseGroupDecisions(response string) ([]types.GroupDecision, error) {
	response = strings.TrimSpace(response)
	if response == "" {
		return nil, fmt.Errorf("empty LLM response")
	}

	analysisLog.Debug().
		Int("response_bytes", len(response)).
		Str("preview", truncate(response, 300)).
		Msg("parsing LLM response")

	extracted := extractJSON(response)

	if extracted == "" {
		analysisLog.Error().
			Str("raw_response", truncate(response, 500)).
			Msg("extractJSON returned empty string")
		return nil, fmt.Errorf("empty JSON after extraction (raw: %.200s)", response)
	}

	var decisions []types.GroupDecision
	if err := json.Unmarshal([]byte(extracted), &decisions); err != nil {
		// Пробуем восстановить обрезанный JSON
		recovered := tryRecoverJSON(extracted)
		if recovered != "" {
			analysisLog.Debug().
				Str("recovered", truncate(recovered, 300)).
				Msg("trying recovered JSON")
			if err2 := json.Unmarshal([]byte(recovered), &decisions); err2 == nil && len(decisions) > 0 {
				analysisLog.Info().
					Int("recovered_decisions", len(decisions)).
					Int("original_error_len", len(extracted)).
					Msg("JSON recovered from truncated response")
				goto normalize
			}
		}
		// Не удалось восстановить — логируем и возвращаем ошибку
		analysisLog.Error().
			Err(err).
			Str("extracted_json", truncate(extracted, 500)).
			Str("recovery_attempt", truncate(recovered, 300)).
			Msg("JSON parse failed, recovery unsuccessful")
		return nil, fmt.Errorf("parse LLM JSON: %w (extracted: %.200s)", err, extracted)
	}

normalize:
	for i := range decisions {
		if decisions[i].Confidence < 0 {
			decisions[i].Confidence = 0
		}
		if decisions[i].Confidence > 100 {
			decisions[i].Confidence = 100
		}
		if decisions[i].Action != "keep" && decisions[i].Action != "ignore" {
			decisions[i].Action = "keep"
			decisions[i].Confidence = 50
		}
		if decisions[i].Pattern == "" {
			decisions[i].Pattern = "unknown"
		}
	}

	return decisions, nil
}

// extractJSON извлекает JSON-массив из markdown code block или смешанного текста.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)

	// Если ответ пустой или только markdown — пробуем найти JSON внутри
	if s == "" || s == "```" || s == "```json" {
		return ""
	}

	// Обрабатываем markdown code blocks
	if strings.HasPrefix(s, "```") {
		lines := strings.Split(s, "\n")
		start := 1
		end := len(lines)

		// Ищем закрывающий ```
		for i := len(lines) - 1; i >= start; i-- {
			trimmed := strings.TrimSpace(lines[i])
			if trimmed == "```" {
				end = i
				break
			}
		}

		if start < end {
			s = strings.Join(lines[start:end], "\n")
		}
	}

	// Ищем JSON массив между [ и ]
	firstBracket := strings.Index(s, "[")
	lastBracket := strings.LastIndex(s, "]")

	if firstBracket >= 0 && lastBracket > firstBracket {
		s = s[firstBracket : lastBracket+1]
	}

	return strings.TrimSpace(s)
}

// tryRecoverJSON пытается восстановить обрезанный JSON-массив.
// Ищет последнюю полную запись (с "confidence") и закрывает массив после неё.
func tryRecoverJSON(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") {
		return ""
	}

	// Стратегия 1: найти последнюю строку с "confidence" — это означает полный объект
	lastConfIdx := strings.LastIndex(s, `"confidence"`)
	if lastConfIdx >= 0 {
		// Ищем значение confidence (число после двоеточия)
		afterConf := s[lastConfIdx+len(`"confidence"`):]
		// Ищем конец значения: число, затем } или ,
		colonIdx := strings.Index(afterConf, ":")
		if colonIdx >= 0 {
			rest := afterConf[colonIdx+1:]
			// Ищем закрывающую }
			closeIdx := strings.Index(rest, "}")
			if closeIdx >= 0 {
				// Позиция } в оригинальной строке
				absIdx := lastConfIdx + len(`"confidence"`) + colonIdx + 1 + closeIdx + 1
				recovered := s[:absIdx] + "\n]"
				return recovered
			}
		}
	}

	// Стратегия 2: найти последнюю } (может быть внутри строки, но обычно работает)
	lastBrace := strings.LastIndex(s, "}")
	if lastBrace >= 0 {
		recovered := s[:lastBrace+1] + "\n]"
		return recovered
	}

	return ""
}

// truncate обрезает строку до max символов, добавляя "..." если обрезана.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
