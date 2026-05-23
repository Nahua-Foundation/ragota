package fileutil

import (
	"testing"
)

func TestMatcher_IsIgnored(t *testing.T) {
	patterns := []string{
		".git", "vendor", "node_modules",
		"*.pb.go", "*_grpc.pb.go", "*.pb.js", "*.pb.ts", "*_pb2.py", "*_pb2_grpc.py",
	}
	m := NewMatcher(patterns)

	tests := []struct {
		rel     string
		isDir   bool
		ignored bool
	}{
		{"main.go", false, false},
		{".git", true, true},
		{"vendor/pkg", true, true},
		{"node_modules/pkg", true, true},
		{"api.proto", false, false},
		{"api.pb.go", false, true},
		{"api_grpc.pb.go", false, true},
		{"api.pb.js", false, true},
		{"api.pb.ts", false, true},
		{"api_pb2.py", false, true},
		{"api_pb2_grpc.py", false, true},
		{"internal/api/api.pb.go", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			if got := m.IsIgnored(tt.rel, tt.isDir); got != tt.ignored {
				t.Errorf("IsIgnored(%q, %v) = %v, want %v", tt.rel, tt.isDir, got, tt.ignored)
			}
		})
	}
}

func TestLanguageByExt(t *testing.T) {
	tests := []struct {
		ext  string
		want string
	}{
		{".go", "go"},
		{".proto", "proto"},
		{".ts", "typescript"},
		{".js", "javascript"},
		{".py", "python"},
		{".md", "text"},
		{".unknown", ""},
	}

	for _, tt := range tests {
		t.Run(tt.ext, func(t *testing.T) {
			if got := LanguageByExt(tt.ext); got != tt.want {
				t.Errorf("LanguageByExt(%q) = %q, want %q", tt.ext, got, tt.want)
			}
		})
	}
}
