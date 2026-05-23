package parser

// Юнит-тесты для чистых helper'ов пакета parser (без tree-sitter / CGO):
// languageFor, SupportedLanguages, canonicalKind, lineForByte, indexByte.
// firstLine тестируется в parser_test.go через end-to-end Parse, где
// доступен sitter.Node.

import "testing"

func TestLanguageFor(t *testing.T) {
	cases := []struct {
		lang, path string
		wantNil    bool
	}{
		{"go", "x.go", false},
		{"python", "x.py", false},
		{"java", "X.java", false},
		{"typescript", "x.ts", false},
		{"typescript", "x.tsx", false},
		{"javascript", "x.js", false},
		{"javascript", "x.jsx", false},
		{"unknown", "x.unk", true},
		{"", "x", true},
	}
	for _, c := range cases {
		got := languageFor(c.lang, c.path)
		if (got == nil) != c.wantNil {
			t.Errorf("languageFor(%q,%q) nil=%v, want nil=%v", c.lang, c.path, got == nil, c.wantNil)
		}
	}
}

func TestSupportedLanguages(t *testing.T) {
	got := SupportedLanguages()
	want := map[string]bool{"go": true, "typescript": true, "javascript": true, "python": true, "java": true}
	if len(got) != len(want) {
		t.Errorf("SupportedLanguages len=%d, want %d", len(got), len(want))
	}
	for _, l := range got {
		if !want[l] {
			t.Errorf("unexpected lang %q", l)
		}
	}
}

func TestCanonicalKind(t *testing.T) {
	cases := map[string]string{
		"function_declaration":   "function",
		"method_declaration":     "method",
		"type_declaration":       "type",
		"type_spec":              "type",
		"const_declaration":      "const",
		"var_declaration":        "var",
		"function_definition":    "function",
		"class_definition":       "class",
		"class_declaration":      "class",
		"interface_declaration":  "interface",
		"enum_declaration":       "enum",
		"method_definition":      "method",
		"type_alias_declaration": "type",
		"random_node":            "",
		"":                       "",
	}
	for in, want := range cases {
		if got := canonicalKind(in); got != want {
			t.Errorf("canonicalKind(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestLineForByte(t *testing.T) {
	src := []byte("a\nbb\nccc\n")
	cases := []struct {
		off, want int
	}{
		{0, 1},
		{1, 1}, // 'a' at line 1
		{2, 2}, // first char after first \n
		{5, 3},
		{100, 4}, // past end clamps to end -> after 3 newlines = line 4
		{-5, 1},
	}
	for _, c := range cases {
		if got := lineForByte(src, c.off); got != c.want {
			t.Errorf("lineForByte(%d)=%d, want %d", c.off, got, c.want)
		}
	}
}

func TestIndexByte(t *testing.T) {
	if indexByte([]byte("abc\ndef"), '\n') != 3 {
		t.Error("indexByte \\n")
	}
	if indexByte([]byte("abc"), '\n') != -1 {
		t.Error("indexByte not found should be -1")
	}
	if indexByte(nil, 'x') != -1 {
		t.Error("indexByte(nil) should be -1")
	}
	// First match only.
	if indexByte([]byte("aabaa"), 'b') != 2 {
		t.Error("indexByte first-match")
	}
}
