package classifier

import (
	"testing"

	"ragota/internal/indexing/crossrepo/detector"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── buildClassificationPrompt ──

func TestBuildClassificationPrompt_ContainsCandidateFields(t *testing.T) {
	cand := detector.Candidate{
		FilePath: "src/auth/handler.go",
		Language: "go",
		Symbol:   "HandleLogin",
		Line:     42,
		RawCode:  "http.Get(url)",
	}

	prompt := buildClassificationPrompt(cand)

	assert.Contains(t, prompt, cand.FilePath, "prompt must contain FilePath")
	assert.Contains(t, prompt, cand.Language, "prompt must contain Language")
	assert.Contains(t, prompt, cand.Symbol, "prompt must contain Symbol")
	assert.Contains(t, prompt, "42", "prompt must contain Line")
	assert.Contains(t, prompt, cand.RawCode, "prompt must contain RawCode")
}

func TestBuildClassificationPrompt_Structure(t *testing.T) {
	cand := detector.Candidate{
		FilePath: "test.go",
		Language: "go",
		Symbol:   "Test",
		Line:     1,
		RawCode:  "fmt.Println()",
	}

	prompt := buildClassificationPrompt(cand)

	assert.Contains(t, prompt, "cross-service calls", "prompt must explain purpose")
	assert.Contains(t, prompt, "type", "prompt must ask for type")
	assert.Contains(t, prompt, "target_service", "prompt must ask for target_service")
	assert.Contains(t, prompt, "endpoint_or_topic", "prompt must ask for endpoint")
	assert.Contains(t, prompt, "confidence", "prompt must ask for confidence")
	assert.Contains(t, prompt, "reason", "prompt must ask for reason")
}

func TestBuildClassificationPrompt_RulesIncluded(t *testing.T) {
	cand := detector.Candidate{
		FilePath: "test.go",
		Language: "go",
		Symbol:   "Test",
		Line:     1,
		RawCode:  "test",
	}

	prompt := buildClassificationPrompt(cand)

	assert.Contains(t, prompt, "http", "prompt must mention http")
	assert.Contains(t, prompt, "grpc", "prompt must mention grpc")
	assert.Contains(t, prompt, "kafka", "prompt must mention kafka")
	assert.Contains(t, prompt, "unknown", "prompt must mention unknown")
	assert.Contains(t, prompt, "0.7", "prompt must mention confidence threshold")
}

func TestBuildClassificationPrompt_UsesFmtSprintf(t *testing.T) {
	cand := detector.Candidate{
		FilePath: "src/test.go",
		Language: "typescript",
		Symbol:   "fetchData",
		Line:     100,
		RawCode:  "await fetch('/api')",
	}

	prompt := buildClassificationPrompt(cand)

	// Verify all 5 fields are interpolated
	assert.Contains(t, prompt, "src/test.go")
	assert.Contains(t, prompt, "typescript")
	assert.Contains(t, prompt, "fetchData")
	assert.Contains(t, prompt, "100")
	assert.Contains(t, prompt, "await fetch('/api')")
}

// ── extractJSON ──

func TestExtractJSON_DirectJSON(t *testing.T) {
	input := `{"type": "http", "target_service": "auth"}`
	result := extractJSON(input)
	assert.Equal(t, input, result)
}

func TestExtractJSON_MarkdownWithJSONKeyword(t *testing.T) {
	input := "```json\n{\"type\": \"http\"}\n```"
	expected := `{"type": "http"}`
	result := extractJSON(input)
	assert.Equal(t, expected, result)
}

func TestExtractJSON_MarkdownWithoutJSONKeyword(t *testing.T) {
	input := "```\n{\"type\": \"http\"}\n```"
	expected := `{"type": "http"}`
	result := extractJSON(input)
	assert.Equal(t, expected, result)
}

func TestExtractJSON_MarkdownUppercaseJSON(t *testing.T) {
	input := "```JSON\n{\"type\": \"grpc\"}\n```"
	expected := `{"type": "grpc"}`
	result := extractJSON(input)
	assert.Equal(t, expected, result)
}

func TestExtractJSON_MalformedMarkdown_NoClosing(t *testing.T) {
	// Opening but no closing backticks
	input := "```json\n{\"type\": \"http\"}\n"
	result := extractJSON(input)
	// Should return original since no closing ```
	assert.Equal(t, input, result)
}

func TestExtractJSON_NoJSONAtAll(t *testing.T) {
	input := "This is just plain text with no code blocks"
	result := extractJSON(input)
	assert.Equal(t, input, result)
}

func TestExtractJSON_MultipleCodeBlocks(t *testing.T) {
	// Should extract from first code block
	input := "```json\n{\"type\": \"http\"}\n```\n\n```json\n{\"type\": \"grpc\"}\n```"
	expected := `{"type": "http"}`
	result := extractJSON(input)
	assert.Equal(t, expected, result)
}

func TestExtractJSON_JSONKeywordNoNewline(t *testing.T) {
	// json keyword but no newline after it — extractJSON keeps "json" prefix
	input := "```json{\"type\": \"http\"}```"
	result := extractJSON(input)
	// When there's no \n after "json", the code doesn't strip it,
	// so "json" prefix remains in the extracted content
	assert.Equal(t, `json{"type": "http"}`, result)
}

func TestExtractJSON_EmptyString(t *testing.T) {
	result := extractJSON("")
	assert.Equal(t, "", result)
}

func TestExtractJSON_OnlyBackticks(t *testing.T) {
	input := "```"
	result := extractJSON(input)
	// start = 0, rest = "" after start+3, no "```" found at end > 0
	assert.Equal(t, input, result)
}

func TestExtractJSON_PreservesWhitespace(t *testing.T) {
	input := "```json\n{\n  \"type\": \"http\"\n}\n```"
	expected := "{\n  \"type\": \"http\"\n}"
	result := extractJSON(input)
	assert.Equal(t, expected, result)
}

// ── parseLLMResponse ──

func TestParseLLMResponse_ValidJSON(t *testing.T) {
	input := `{"type": "http", "target_service": "auth-service", "endpoint_or_topic": "/api/login", "confidence": 0.95, "reason": "HTTP call"}`

	result, err := parseLLMResponse(input)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "http", result.Protocol)
	assert.Equal(t, "auth-service", result.TargetService)
	assert.Equal(t, "/api/login", result.Endpoint)
	assert.InDelta(t, 0.95, result.Confidence, 0.001)
	assert.Equal(t, "HTTP call", result.Reason)
}

func TestParseLLMResponse_InvalidJSON(t *testing.T) {
	input := "not json at all"

	_, err := parseLLMResponse(input)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "LLM response parse failed")
}

func TestParseLLMResponse_MarkdownWrappedValidJSON(t *testing.T) {
	input := "```json\n{\"type\": \"grpc\", \"target_service\": \"user-service\", \"endpoint_or_topic\": \"UserService.Get\", \"confidence\": 0.85, \"reason\": \"gRPC stub call\"}\n```"

	result, err := parseLLMResponse(input)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "grpc", result.Protocol)
	assert.Equal(t, "user-service", result.TargetService)
	assert.Equal(t, "UserService.Get", result.Endpoint)
	assert.InDelta(t, 0.85, result.Confidence, 0.001)
	assert.Equal(t, "gRPC stub call", result.Reason)
}

func TestParseLLMResponse_EmptyResponse(t *testing.T) {
	_, err := parseLLMResponse("")
	require.Error(t, err)
}

func TestParseLLMResponse_EmptyJSON(t *testing.T) {
	_, err := parseLLMResponse("{}")
	require.NoError(t, err)
	// Empty struct — all zero values
}

func TestParseLLMResponse_PartialJSON(t *testing.T) {
	input := `{"type": "http", "target_service":`
	_, err := parseLLMResponse(input)
	require.Error(t, err)
}

func TestParseLLMResponse_MarkdownWrappedInvalidJSON(t *testing.T) {
	input := "```json\n{\"type\": \"http\", broken json\n```"
	_, err := parseLLMResponse(input)
	require.Error(t, err)
}

func TestParseLLMResponse_AllProtocolTypes(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		expected string
	}{
		{"http", `{"type": "http"}`, "http"},
		{"grpc", `{"type": "grpc"}`, "grpc"},
		{"kafka", `{"type": "kafka"}`, "kafka"},
		{"npm_package", `{"type": "npm_package"}`, "npm_package"},
		{"import", `{"type": "import"}`, "import"},
		{"unknown", `{"type": "unknown"}`, "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseLLMResponse(tt.json)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, result.Protocol)
		})
	}
}

func TestParseLLMResponse_ConfidenceRange(t *testing.T) {
	input := `{"type": "http", "confidence": 0.99}`
	result, err := parseLLMResponse(input)
	require.NoError(t, err)
	assert.InDelta(t, 0.99, result.Confidence, 0.001)
}

func TestParseLLMResponse_MissingFields(t *testing.T) {
	input := `{"type": "http"}`
	result, err := parseLLMResponse(input)
	require.NoError(t, err)
	assert.Equal(t, "http", result.Protocol)
	assert.Empty(t, result.TargetService)
	assert.Empty(t, result.Endpoint)
	assert.Zero(t, result.Confidence)
	assert.Empty(t, result.Reason)
}

// ── extractJSON + parseLLMResponse integration ──

func TestParseLLMResponse_ViaExtractJSON_PrecedingText(t *testing.T) {
	// LLM sometimes outputs text before the code block
	input := "Here is my analysis:\n\n```json\n{\"type\": \"kafka\", \"target_service\": \"events\", \"endpoint_or_topic\": \"topic.user.created\", \"confidence\": 0.8, \"reason\": \"Kafka producer\"}\n```\n\nDone."

	result, err := parseLLMResponse(input)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "kafka", result.Protocol)
	assert.Equal(t, "events", result.TargetService)
	assert.Equal(t, "topic.user.created", result.Endpoint)
	assert.InDelta(t, 0.8, result.Confidence, 0.001)
	assert.Equal(t, "Kafka producer", result.Reason)
}

func TestExtractJSON_NestedInText(t *testing.T) {
	input := "Sure, I'll analyze this.\n\n```json\n{\"type\": \"http\"}\n```\n\nHope that helps!"
	result := extractJSON(input)
	assert.Equal(t, `{"type": "http"}`, result)
}

// ── buildClassificationPrompt with empty fields ──

func TestBuildClassificationPrompt_EmptyFields(t *testing.T) {
	cand := detector.Candidate{
		FilePath: "",
		Language: "",
		Symbol:   "",
		Line:     0,
		RawCode:  "",
	}

	prompt := buildClassificationPrompt(cand)

	assert.NotEmpty(t, prompt)
	assert.Contains(t, prompt, "cross-service calls")
}

// ── extractJSON with various markdown variations ──

func TestExtractJSON_MarkdownWithExtraWhitespace(t *testing.T) {
	input := "  ```json  \n{\"type\": \"http\"}\n  ```  "
	result := extractJSON(input)
	// TrimSpace is called, so leading/trailing spaces removed
	assert.Equal(t, `{"type": "http"}`, result)
}

func TestExtractJSON_NoMarkdownJustTextWithJSON(t *testing.T) {
	input := `Some text {"type": "http"} more text`
	result := extractJSON(input)
	// No backticks, so returns original
	assert.Equal(t, input, result)
}

// ── ollamaCall error cases (structural tests, no actual HTTP) ──

func TestOllamaCall_InvalidURL(t *testing.T) {
	// This will fail to connect, testing error path
	_, err := ollamaCall("http://localhost:99999", "model", "prompt")
	// Either URL parse error or connection error — both are errors
	assert.Error(t, err)
}

// ── Edge case: response with markdown but no valid JSON inside ──

func TestParseLLMResponse_MarkdownWithNonJSON(t *testing.T) {
	input := "```python\nprint('hello')\n```"
	_, err := parseLLMResponse(input)
	require.Error(t, err)
}
