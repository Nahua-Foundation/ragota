package cli

import (
	"os"
	"path/filepath"
	"testing"

	"ragota/pkg/fileutil"
)

func TestFilePattern(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		want     string
	}{
		{"go file", "main.go", "*.go"},
		{"test file", "main_test.go", "*_test.go"},
		{"protobuf", "user.pb.go", "*.pb.go"},
		{"grpc protobuf", "user_grpc.pb.go", "*_grpc.pb.go"},
		{"generated", "types.gen.go", "*.gen.go"},
		{"python", "app.py", "*.py"},
		{"python pb", "user_pb2.py", "*_pb2.py"},
		{"python grpc", "user_pb2_grpc.py", "*_pb2_grpc.py"},
		{"typescript", "app.ts", "*.ts"},
		{"typescript pb", "user.pb.ts", "*.pb.ts"},
		{"javascript", "app.js", "*.js"},
		{"javascript pb", "user.pb.js", "*.pb.js"},
		{"markdown", "README.md", "*.md"},
		{"json", "config.json", "*.json"},
		{"no extension", "Makefile", "Makefile"},
		{"dotfile", ".gitignore", ".gitignore"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filePattern(tt.filename)
			if got != tt.want {
				t.Errorf("filePattern(%q) = %q, want %q", tt.filename, got, tt.want)
			}
		})
	}
}

func TestTopDirectories(t *testing.T) {
	dirs := map[string]*dirStats{
		"a":     {path: "a", fileCount: 10},
		"b":     {path: "b", fileCount: 50},
		"c":     {path: "c", fileCount: 30},
		"d":     {path: "d", fileCount: 20},
		"e":     {path: "e", fileCount: 40},
		"":      {path: "", fileCount: 1000}, // root should be skipped
		"f/g/h": {path: "f/g/h", fileCount: 5},
	}

	top := topDirectories(dirs, 3)
	if len(top) != 3 {
		t.Fatalf("expected 3 directories, got %d", len(top))
	}

	// Should be sorted by fileCount descending
	if top[0].path != "b" {
		t.Errorf("expected first to be 'b', got %q", top[0].path)
	}
	if top[1].path != "e" {
		t.Errorf("expected second to be 'e', got %q", top[1].path)
	}
	if top[2].path != "c" {
		t.Errorf("expected third to be 'c', got %q", top[2].path)
	}
}

func TestTopDirectories_LessThanN(t *testing.T) {
	dirs := map[string]*dirStats{
		"a": {path: "a", fileCount: 10},
		"b": {path: "b", fileCount: 20},
	}

	top := topDirectories(dirs, 10)
	if len(top) != 2 {
		t.Fatalf("expected 2 directories, got %d", len(top))
	}
}

func TestTopPatterns(t *testing.T) {
	patterns := map[string]int{
		"*.go":      100,
		"*_test.go": 50,
		"*.md":      30,
		"*.py":      20,
		"*.json":    10,
	}

	top := topPatterns(patterns, 3)
	if len(top) != 3 {
		t.Fatalf("expected 3 patterns, got %d", len(top))
	}

	if top[0].name != "*.go" {
		t.Errorf("expected first to be '*.go', got %q", top[0].name)
	}
	if top[1].name != "*_test.go" {
		t.Errorf("expected second to be '*_test.go', got %q", top[1].name)
	}
	if top[2].name != "*.md" {
		t.Errorf("expected third to be '*.md', got %q", top[2].name)
	}
}

func TestMostCommonPattern(t *testing.T) {
	tests := []struct {
		name     string
		patterns map[string]int
		want     string
	}{
		{
			name:     "single pattern",
			patterns: map[string]int{"*.go": 10},
			want:     "*.go",
		},
		{
			name:     "multiple patterns",
			patterns: map[string]int{"*.go": 10, "*.md": 5, "*.py": 3},
			want:     "*.go",
		},
		{
			name:     "empty map",
			patterns: map[string]int{},
			want:     "-",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mostCommonPattern(tt.patterns)
			if got != tt.want {
				t.Errorf("mostCommonPattern() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHumanSize(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{500, "500B"},
		{1024, "1.0K"},
		{1536, "1.5K"},
		{1048576, "1.0M"},
		{1572864, "1.5M"},
		{1073741824, "1.0G"},
		{1610612736, "1.5G"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := humanSize(tt.bytes)
			if got != tt.want {
				t.Errorf("humanSize(%d) = %q, want %q", tt.bytes, got, tt.want)
			}
		})
	}
}

func TestTruncatePath(t *testing.T) {
	root := "/project"

	tests := []struct {
		path   string
		maxLen int
		want   string
	}{
		{"", 60, "(root)"},
		{"src", 60, "src"},
		{"very/long/path/to/some/deeply/nested/directory/structure", 30, ".../nested/directory/structure"},
		{"short", 100, "short"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := truncatePath(tt.path, root, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncatePath(%q, %q, %d) = %q, want %q", tt.path, root, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestMaxFloat(t *testing.T) {
	tests := []struct {
		a, b float64
		want float64
	}{
		{1.0, 2.0, 2.0},
		{2.0, 1.0, 2.0},
		{1.5, 1.5, 1.5},
		{0.0, 0.0, 0.0},
		{-1.0, 1.0, 1.0},
	}

	for _, tt := range tests {
		got := maxFloat(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("maxFloat(%f, %f) = %f, want %f", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestWalkAll(t *testing.T) {
	// Create temporary directory structure
	tmpDir := t.TempDir()

	// Create test files
	files := []string{
		"main.go",
		"main_test.go",
		"src/app.py",
		"src/utils.py",
		"vendor/lib.go",
		"node_modules/pkg.js",
		"docs/README.md",
		"user.pb.go",
	}

	for _, f := range files {
		path := filepath.Join(tmpDir, f)
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("test"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Test without matcher (all files)
	allFiles, allDirs, err := walkAll(tmpDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(allFiles) != 8 {
		t.Errorf("expected 8 files without matcher, got %d", len(allFiles))
	}
	if len(allDirs) == 0 {
		t.Error("expected directories to be populated")
	}

	// Test with matcher (ignore vendor and node_modules)
	matcher := fileutil.NewMatcher([]string{"vendor", "node_modules"})
	filteredFiles, filteredDirs, err := walkAll(tmpDir, matcher)
	if err != nil {
		t.Fatal(err)
	}
	if len(filteredFiles) != 6 {
		t.Errorf("expected 6 files with matcher, got %d", len(filteredFiles))
	}

	// Check that vendor and node_modules are not in filtered dirs
	for path := range filteredDirs {
		if path == "vendor" || path == "node_modules" {
			t.Errorf("directory %q should have been ignored", path)
		}
	}

	// Verify patterns are collected correctly
	if rootStats, ok := filteredDirs[""]; ok {
		if rootStats.fileCount != 6 {
			t.Errorf("expected root to have 6 files, got %d", rootStats.fileCount)
		}
		if _, ok := rootStats.patterns["*.go"]; !ok {
			t.Error("expected *.go pattern in root")
		}
		if _, ok := rootStats.patterns["*.pb.go"]; !ok {
			t.Error("expected *.pb.go pattern in root")
		}
	}
}

func TestWalkAll_PatternCollection(t *testing.T) {
	tmpDir := t.TempDir()

	// Create files with various patterns
	files := []string{
		"main.go",
		"main_test.go",
		"user.pb.go",
		"types.gen.go",
		"app.py",
		"app_pb2.py",
	}

	for _, f := range files {
		path := filepath.Join(tmpDir, f)
		if err := os.WriteFile(path, []byte("test"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	_, dirs, err := walkAll(tmpDir, nil)
	if err != nil {
		t.Fatal(err)
	}

	rootStats := dirs[""]
	expectedPatterns := []string{"*.go", "*_test.go", "*.pb.go", "*.gen.go", "*.py", "*_pb2.py"}

	for _, pattern := range expectedPatterns {
		if _, ok := rootStats.patterns[pattern]; !ok {
			t.Errorf("expected pattern %q to be collected", pattern)
		}
	}
}
