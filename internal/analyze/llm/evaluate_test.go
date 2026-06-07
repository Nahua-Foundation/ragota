package llm

import (
	"testing"
)

func TestHasRepoRootPattern(t *testing.T) {
	tests := []struct {
		pattern string
		want    bool
	}{
		// Should match (repo-root patterns)
		{"**/investment-management-system/*", true},
		{"**/issuance/*", true},
		{"**/redemption-management-system/*", true},
		{"**/balance/*", true},
		{"investment-management-system/*", true},
		{"issuance/*", true},

		// Should NOT match (specific file patterns)
		{"**/investment-management-system/*.ts", false},
		{"**/investment-management-system/src/*", false},
		{"**/investment-management-system/src/*.ts", false},
		{"**/.ai-tools/**/*.json", false},
		{"*.json", false},
		{"**/*.proto", false},
		{"src/**", false},
		{"", false},
	}

	for _, tt := range tests {
		got := HasRepoRootPattern(tt.pattern)
		if got != tt.want {
			t.Errorf("HasRepoRootPattern(%q) = %v, want %v", tt.pattern, got, tt.want)
		}
	}
}

func TestIsValidPattern(t *testing.T) {
	tests := []struct {
		pattern string
		want    bool
		reason  string
	}{
		// Valid patterns
		{"*.log", true, "simple extension"},
		{"*.bak", true, "backup extension"},
		{"*.lock", true, "lock extension"},
		{"*.err", true, "error extension"},
		{"**/*.log", true, "globstar pattern"},
		{"src/**/*.ts", true, "path with globstar"},
		{"*.controller.ts", true, "compound extension (NestJS)"},
		{"*.service.ts", true, "compound extension (NestJS)"},
		{"*.repository.ts", true, "compound extension"},
		{"*.handler.ts", true, "compound extension"},
		{"*.adapter.ts", true, "compound extension"},
		{"*.min.js", true, "compound with min"},
		{"*.pb.go", true, "protobuf extension"},
		{"**/vendor/**", true, "vendor pattern"},
		{"node_modules/**", true, "node_modules pattern"},

		// Invalid patterns (LLM hallucinations)
		{"*.err.*", false, "wildcard sandwich (short middle)"},
		{"*.err..*", false, "double dots"},
		{"*.err...*", false, "triple dots"},
		{"*.lock.*", false, "wildcard sandwich (lock < 8 chars)"},
		{"*.lock..*", false, "double dots"},
		{"*.lock......*", false, "many dots"},
		{"*.log.*", false, "wildcard sandwich (short middle)"},
		{"*.log..*", false, "double dots"},
		{"*.log........*", false, "many dots"},
		{"*.tmp.*", false, "wildcard sandwich (short middle)"},
		{"*.tmp....*", false, "many dots"},
		{"*.swp.*", false, "wildcard sandwich (short middle)"},
		{"*.pid.*", false, "wildcard sandwich (short middle)"},
		{"*.out.*", false, "wildcard sandwich (short middle)"},
		{"*.d.*", false, "wildcard sandwich (short middle)"},
		{"*.*.*", false, "three wildcards"},
		{"*.*.*.*", false, "four wildcards"},
		{"***", false, "multiple stars"},
		{"****", false, "many stars"},
		{"*****", false, "too many stars"},
		{"*.er*r.*", false, "star in middle of segment"},
		{"*.verylongpattern*.ts", false, "long segment with star"},
	}

	for _, tt := range tests {
		got := isValidPattern(tt.pattern)
		if got != tt.want {
			t.Errorf("isValidPattern(%q) = %v, want %v (%s)", tt.pattern, got, tt.want, tt.reason)
		}
	}
}
