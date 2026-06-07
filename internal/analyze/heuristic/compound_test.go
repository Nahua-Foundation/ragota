package heuristic

import (
	"testing"
)

func TestIsSafeCompound(t *testing.T) {
	tests := []struct {
		pattern string
		want    bool
	}{
		// Safe patterns (business logic)
		{"*.controller.ts", true},
		{"*.controller.js", true},
		{"*.service.ts", true},
		{"*.service.js", true},
		{"*.repository.ts", true},
		{"*.repository.js", true},
		{"*.handler.ts", true},
		{"*.handler.js", true},
		{"*.adapter.ts", true},
		{"*.adapter.js", true},
		{"*.interceptor.ts", true},
		{"*.guard.ts", true},
		{"*.pipe.ts", true},
		{"*.middleware.ts", true},
		{"*.dto.ts", true},
		{"*.entity.ts", true},
		{"*.model.ts", true},
		{"*.schema.ts", true},

		// Unsafe patterns (should be ignored)
		{"*.gen.ts", false},
		{"*.generated.ts", false},
		{"*.pb.ts", false},
		{"*.pb.go", false},
		{"*.min.js", false},
		{"*.bundle.js", false},
		{"*.config.ts", false},
		{"*.spec.ts", false},
		{"*.test.ts", false},
	}

	for _, tt := range tests {
		got := IsSafeCompound(tt.pattern)
		if got != tt.want {
			t.Errorf("IsSafeCompound(%q) = %v, want %v", tt.pattern, got, tt.want)
		}
	}
}

func TestCompoundPattern(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
	}{
		{"ticker-info.controller.ts", "*.controller.ts"},
		{"notifications.controller.ts", "*.controller.ts"},
		{"app.service.ts", "*.service.ts"},
		{"user.repository.ts", "*.repository.ts"},
		{"data.dto.ts", "*.dto.ts"},
		{"main.ts", ""}, // Not compound
		{"app.ts", ""},  // Not compound
	}

	for _, tt := range tests {
		got := CompoundPattern(tt.name)
		if got != tt.pattern {
			t.Errorf("CompoundPattern(%q) = %q, want %q", tt.name, got, tt.pattern)
		}
	}
}
