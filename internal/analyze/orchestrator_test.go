package analyze

import (
	"testing"
)

func TestMatchPatternToFiles_Globstar(t *testing.T) {
	files := []string{
		".ai-tools/bm25/store/data.json",
		".ai-tools/bm25/store/meta.json",
		".ai-tools/qdrant/collections/data.json",
		".ai-tools/qdrant/collections/meta.json",
		"investment-management-system/src/main.ts",
		"investment-management-system/package.json",
		"balance/internal/handler.go",
	}

	tests := []struct {
		pattern string
		want    []string
	}{
		// Простые паттерны с ** в начале
		{
			pattern: "**/*.json",
			want: []string{
				".ai-tools/bm25/store/data.json",
				".ai-tools/bm25/store/meta.json",
				".ai-tools/qdrant/collections/data.json",
				".ai-tools/qdrant/collections/meta.json",
				"investment-management-system/package.json",
			},
		},
		{
			pattern: "**/*.ts",
			want:    []string{"investment-management-system/src/main.ts"},
		},
		{
			pattern: "**/*.go",
			want:    []string{"balance/internal/handler.go"},
		},
		// Паттерны с префиксом
		{
			pattern: "balance/**",
			want:    []string{"balance/internal/handler.go"},
		},
		{
			pattern: "*.go",
			want:    []string{"balance/internal/handler.go"},
		},
	}

	for _, tt := range tests {
		got := MatchPatternToFiles(tt.pattern, files)
		if len(got) != len(tt.want) {
			t.Logf("Pattern: %s", tt.pattern)
			t.Logf("Got:  %v", got)
			t.Logf("Want: %v", tt.want)
			t.Errorf("MatchPatternToFiles(%q) returned %d files, want %d", tt.pattern, len(got), len(tt.want))
			continue
		}
		for i, w := range tt.want {
			if i >= len(got) || got[i] != w {
				t.Errorf("MatchPatternToFiles(%q)[%d] = %q, want %q", tt.pattern, i, got[i], w)
			}
		}
	}
}
