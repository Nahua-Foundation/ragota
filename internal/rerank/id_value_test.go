package rerank

import (
	"encoding/json"
	"testing"
)

func TestIDValue_UnmarshalJSON_String(t *testing.T) {
	var id IDValue
	if err := json.Unmarshal([]byte(`"c1"`), &id); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "c1" {
		t.Errorf("expected 'c1', got %q", id)
	}
}

func TestIDValue_UnmarshalJSON_Number(t *testing.T) {
	var id IDValue
	if err := json.Unmarshal([]byte(`42`), &id); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "42" {
		t.Errorf("expected '42', got %q", id)
	}
}

func TestIDValue_UnmarshalJSON_Float(t *testing.T) {
	var id IDValue
	if err := json.Unmarshal([]byte(`3.14`), &id); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "3.14" {
		t.Errorf("expected '3.14', got %q", id)
	}
}

func TestIDValue_MarshalJSON(t *testing.T) {
	id := IDValue("test123")
	data, err := json.Marshal(id)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != `"test123"` {
		t.Errorf("expected '\"test123\"', got %s", string(data))
	}
}

func TestCandidate_UnmarshalJSON_MixedIDs(t *testing.T) {
	// Тест: массив кандидатов с разными типами id (строка и число)
	raw := `[{"id": "str1", "content": "hello"}, {"id": 42, "content": "world"}]`
	var cands []Candidate
	if err := json.Unmarshal([]byte(raw), &cands); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(cands))
	}
	if cands[0].ID != "str1" {
		t.Errorf("expected 'str1', got %q", cands[0].ID)
	}
	if cands[1].ID != "42" {
		t.Errorf("expected '42', got %q", cands[1].ID)
	}
}
