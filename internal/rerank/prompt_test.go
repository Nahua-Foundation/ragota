package rerank

// Unit-тесты для чистых helper'ов из prompt.go: parseScore (числа, проценты,
// отрицательные, yes/no, мусор) и buildPrompt (структурные маркеры).
// Внешних зависимостей нет.

import (
	"strings"
	"testing"
)

func TestParseScore(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"", 0},
		{"0.83", 0.83},
		{"  0,75 ", 0.75},
		{"score: 0.42 done", 0.42},
		{"yes", 1},
		{"Relevant, definitely.", 1},
		{"no", 0},
		{"I don't know", 0},
		{"-0.5", 0},
		{"1.5", 1},
		{"75", 0.75},
		{"100", 1},
		{"42.5", 0.425},
		{"200", 1}, // >100 клампится в 1
	}
	for _, c := range cases {
		got := parseScore(c.in)
		if got < c.want-1e-9 || got > c.want+1e-9 {
			t.Errorf("parseScore(%q) = %v; want %v", c.in, got, c.want)
		}
	}
}

func TestBuildPrompt_StructuralMarkers(t *testing.T) {
	p := buildPrompt("find handler", "MyHandler", "pkg/h.go", "go", "func MyHandler() {}")
	for _, want := range []string{
		"Instruction:",
		"Relevance Score: 1.0",
		"Relevance Score: 0.0",
		"find handler",
		"symbol MyHandler",
		"file pkg/h.go",
		"lang go",
		"func MyHandler() {}",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if !strings.HasSuffix(p, "Relevance Score: ") {
		t.Errorf("prompt must end with 'Relevance Score: '; got tail %q", p[len(p)-30:])
	}
}

func TestBuildPrompt_NoContext(t *testing.T) {
	p := buildPrompt("q", "", "", "", "doc")
	if strings.Contains(p, "Context:") {
		t.Errorf("Context section must be omitted when symbol/path/lang are empty")
	}
}
