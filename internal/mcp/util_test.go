package mcp

// Unit-тесты для helper'ов упаковки MCP-ответов.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestJSONResult_Nil(t *testing.T) {
	res, err := jsonResult(nil)
	if err != nil {
		t.Fatalf("jsonResult(nil): %v", err)
	}
	if res == nil || len(res.Content) == 0 {
		t.Fatalf("empty result: %+v", res)
	}
	// Содержимое должно быть "[]" для nil.
	text := contentText(res.Content[0])
	if text != "[]" {
		t.Errorf("nil → expected [], got %q", text)
	}
}

func TestJSONResult_Marshal(t *testing.T) {
	payload := map[string]any{"a": 1, "b": []string{"x", "y"}}
	res, err := jsonResult(payload)
	if err != nil {
		t.Fatalf("jsonResult: %v", err)
	}
	text := contentText(res.Content[0])
	var got map[string]any
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, text)
	}
	if got["a"].(float64) != 1 {
		t.Errorf("roundtrip: %+v", got)
	}
}

func TestErrorToResult(t *testing.T) {
	res, err := errorToResult("mytool", errAssertion("boom"))
	if err != nil {
		t.Fatalf("errorToResult: %v", err)
	}
	if !res.IsError {
		t.Error("IsError must be true")
	}
	text := contentText(res.Content[0])
	if !strings.Contains(text, "mytool") || !strings.Contains(text, "boom") {
		t.Errorf("message: %q", text)
	}
}

// errAssertion — простая ошибка-строка для теста.
type errAssertion string

func (e errAssertion) Error() string { return string(e) }

// contentText извлекает text из mcp.Content (TextContent).
// Используем JSON roundtrip, чтобы не зависеть от точного типа.
func contentText(c any) string {
	b, _ := json.Marshal(c)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if s, ok := m["text"].(string); ok {
		return s
	}
	return ""
}
