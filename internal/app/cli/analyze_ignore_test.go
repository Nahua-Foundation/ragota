package cli

import (
	"os"
	"sort"
	"strings"
	"testing"

	"ragota/internal/analyze"
	"ragota/internal/analyze/heuristic"
	"ragota/internal/analyze/llm"
	"ragota/internal/analyze/output"
	"ragota/internal/analyze/resolve"
	"ragota/pkg/config"
	"ragota/pkg/ragignore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFilterIndexedExtensions(t *testing.T) {
	tests := []struct {
		name     string
		input    []string
		expected []string
	}{
		{
			name:     "proto extension filtered",
			input:    []string{"*.proto", "*.sql", "testdata"},
			expected: []string{"*.sql", "testdata"},
		},
		{
			name:     "md and json filtered",
			input:    []string{"*.md", "*.json", "*.yaml", "build"},
			expected: []string{"build"},
		},
		{
			name:     "no indexed extensions",
			input:    []string{"*.png", "*.zap", "*.prefs", "fixtures"},
			expected: []string{"*.png", "*.zap", "*.prefs", "fixtures"},
		},
		{
			name:     "mixed",
			input:    []string{"*.proto", "*.go", "*.png", "vendor", "coverage"},
			expected: []string{"*.png", "vendor", "coverage"},
		},
		{
			name:     "empty input",
			input:    []string{},
			expected: nil,
		},
		{
			name:     "non-glob patterns pass through",
			input:    []string{"vendor", "build", "gen"},
			expected: []string{"vendor", "build", "gen"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, _ := output.FilterIndexedExtensions(tt.input, nil)
			sort.Strings(result)
			sort.Strings(tt.expected)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFilterIndexedExtensions_AllDefaultExtensionsFiltered(t *testing.T) {
	var patterns []string
	for _, ext := range config.DefaultExtensions {
		patterns = append(patterns, "*"+ext)
	}

	result, _ := output.FilterIndexedExtensions(patterns, nil)
	assert.Empty(t, result, "all indexed extension patterns should be filtered")
}

func TestFilterIndexedExtensions_CompoundPatternsPass(t *testing.T) {
	input := []string{
		"*.swagger.json",
		"*.openapi.yaml",
		"*.schema.json",
		"*.pb.go",
		"*.gen.go",
	}

	result, _ := output.FilterIndexedExtensions(input, nil)
	assert.Equal(t, input, result, "compound patterns should pass through")
}

func TestFilterIndexedExtensions_NegationPasses(t *testing.T) {
	input := []string{
		"*.test.go",
		"!main_test.go",
		"!handler_test.go",
	}

	result, _ := output.FilterIndexedExtensions(input, nil)
	assert.Equal(t, input, result, "negation patterns should pass through")
}

func TestSaveIgnorePatterns_ContainsDefaultPatterns(t *testing.T) {
	tmp := t.TempDir()

	err := analyze.SavePatterns(tmp, []string{"*.sql", "testdata"})
	require.NoError(t, err)

	data, err := os.ReadFile(ragignore.Path(tmp))
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	sort.Strings(lines)

	for _, dp := range ragignore.DefaultPatterns {
		assert.Contains(t, lines, dp, "DefaultPatterns should contain: %s", dp)
	}

	assert.Contains(t, lines, "*.sql")
	assert.Contains(t, lines, "testdata")
}

func TestSaveIgnorePatterns_NoDuplicateDefaults(t *testing.T) {
	tmp := t.TempDir()

	existing := append([]string{}, ragignore.DefaultPatterns[:5]...)
	existing = append(existing, "*.custom")
	err := ragignore.Save(tmp, existing)
	require.NoError(t, err)

	err = analyze.SavePatterns(tmp, []string{"*.sql"})
	require.NoError(t, err)

	data, err := os.ReadFile(ragignore.Path(tmp))
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	seen := make(map[string]int)
	for _, line := range lines {
		seen[line]++
	}
	for _, dp := range ragignore.DefaultPatterns {
		assert.Equal(t, 1, seen[dp], "DefaultPattern %s should appear exactly once", dp)
	}
}

func TestSaveIgnorePatterns_EmptyFile(t *testing.T) {
	tmp := t.TempDir()

	err := analyze.SavePatterns(tmp, []string{})
	require.NoError(t, err)

	data, err := os.ReadFile(ragignore.Path(tmp))
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	assert.Len(t, lines, len(ragignore.DefaultPatterns),
		"empty suggested should still write DefaultPatterns")
}

func TestIsCompoundName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"simple json", "package.json", false},
		{"simple go", "main.go", false},
		{"swagger json", "api.swagger.json", true},
		{"openapi yaml", "v1.openapi.yaml", true},
		{"pb go", "user.pb.go", true},
		{"gen go", "service.gen.go", true},
		{"multiple dots", "api.v1.openapi.json", true},
		{"hidden file", ".gitignore", false},
		{"hidden compound", ".config.backup.json", true},
		{"no extension", "Makefile", false},
		{"underscore", "user_service.go", false},
		{"schema json", "schema.json", false},
		{"spec yaml", "api.spec.yaml", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, heuristic.IsCompoundName(tt.input))
		})
	}
}

func TestCompoundPattern(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"swagger json", "api.swagger.json", "*.swagger.json"},
		{"openapi yaml", "v1.openapi.yaml", "*.openapi.yaml"},
		{"pb go", "user.pb.go", "*.pb.go"},
		{"gen go", "service.gen.go", "*.gen.go"},
		{"multiple dots", "api.v1.openapi.json", "*.openapi.json"},
		{"simple go", "main.go", ""},
		{"simple json", "package.json", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, heuristic.CompoundPattern(tt.input))
		})
	}
}

func TestHasGeneratedMarker(t *testing.T) {
	assert.True(t, heuristic.HasGeneratedMarker("// Code generated by protoc-gen-go. DO NOT EDIT."))
	assert.True(t, heuristic.HasGeneratedMarker("// Code generated by mockgen"))
	assert.True(t, heuristic.HasGeneratedMarker("// This file was generated by go generate"))
	assert.True(t, heuristic.HasGeneratedMarker("@generated"))
	assert.True(t, heuristic.HasGeneratedMarker("// Code generated by swagger-codegen"))

	assert.False(t, heuristic.HasGeneratedMarker("// DO NOT EDIT this config manually"))
	assert.False(t, heuristic.HasGeneratedMarker("package main\nfunc main() {}"))
	assert.False(t, heuristic.HasGeneratedMarker(`{"openapi": "3.0.0"}`))
	assert.False(t, heuristic.HasGeneratedMarker("// This file contains important configuration"))
}

func TestHasSpecMarker(t *testing.T) {
	assert.True(t, heuristic.HasSpecMarker(`{"openapi": "3.0.0", "paths": {}}`))
	assert.True(t, heuristic.HasSpecMarker(`{"swagger": "2.0"}`))
	assert.True(t, heuristic.HasSpecMarker(`openapi: 3.0.0`))
	assert.True(t, heuristic.HasSpecMarker(`swagger: 2.0`))
	assert.True(t, heuristic.HasSpecMarker(`syntax = "proto3"`))
	assert.True(t, heuristic.HasSpecMarker(`syntax='proto2'`))

	assert.False(t, heuristic.HasSpecMarker(`{"paths": {"/users": {}}}`))
	assert.False(t, heuristic.HasSpecMarker(`{"definitions": {}}`))
	assert.False(t, heuristic.HasSpecMarker(`paths:`))

	assert.True(t, heuristic.HasSpecMarker(`{"paths": {}, "definitions": {}}`))

	assert.False(t, heuristic.HasSpecMarker(`{"name": "my-app", "version": "1.0.0"}`))
	assert.False(t, heuristic.HasSpecMarker("package main\nfunc main() {}"))
}

func TestIsSourceFile(t *testing.T) {
	assert.True(t, heuristic.IsSourceFile("main.go"))
	assert.True(t, heuristic.IsSourceFile("handler.ts"))
	assert.True(t, heuristic.IsSourceFile("component.tsx"))
	assert.True(t, heuristic.IsSourceFile("index.js"))
	assert.True(t, heuristic.IsSourceFile("app.py"))
	assert.True(t, heuristic.IsSourceFile("Service.java"))
	assert.True(t, heuristic.IsSourceFile("api.proto"))

	assert.False(t, heuristic.IsSourceFile("api.swagger.json"))
	assert.False(t, heuristic.IsSourceFile("schema.json"))
	assert.False(t, heuristic.IsSourceFile("config.yaml"))
	assert.False(t, heuristic.IsSourceFile("readme.md"))
}

func TestIsBinaryFile(t *testing.T) {
	tmp := t.TempDir()

	// Текстовый файл
	textFile := tmp + "/text.txt"
	err := os.WriteFile(textFile, []byte("Hello, world!\nThis is a text file."), 0o644)
	require.NoError(t, err)
	assert.False(t, heuristic.IsBinaryFile(textFile))

	// Бинарный файл (с null bytes)
	binFile := tmp + "/binary.bin"
	err = os.WriteFile(binFile, []byte{0x00, 0x01, 0x02, 0x89, 0x50, 0x4E, 0x47}, 0o644)
	require.NoError(t, err)
	assert.True(t, heuristic.IsBinaryFile(binFile))

	// Пустой файл
	emptyFile := tmp + "/empty.txt"
	err = os.WriteFile(emptyFile, []byte{}, 0o644)
	require.NoError(t, err)
	assert.False(t, heuristic.IsBinaryFile(emptyFile))
}

func TestEstimateLines(t *testing.T) {
	assert.Equal(t, 0, heuristic.EstimateLines(0))
	assert.Equal(t, 0, heuristic.EstimateLines(-1))
	assert.Equal(t, 2, heuristic.EstimateLines(80))
	assert.Equal(t, 25, heuristic.EstimateLines(1000))
	assert.Equal(t, 250, heuristic.EstimateLines(10000))
}

func TestPreScreen(t *testing.T) {
	tmp := t.TempDir()

	// Создаём тестовые файлы
	files := map[string]string{
		"generated.go":         "// Code generated by mockgen. DO NOT EDIT.\npackage main",
		"spec.yaml":            "openapi: 3.0.0\npaths:\n  /users:\n    get:",
		"main_test.go":         "package main\nfunc TestMain(t *testing.T) {}",
		"handler.go":           "package main\nfunc HandleUsers() {}",
		"README.md":            "# My Project\n\nThis is a readme.",
		"package.json":         `{"name": "test", "version": "1.0.0"}`,
		"large_doc.md":         strings.Repeat("# Documentation\n\nLorem ipsum.\n", 500),
	}

	for name, content := range files {
		err := os.WriteFile(tmp+"/"+name, []byte(content), 0o644)
		require.NoError(t, err)
	}

	fileList := []string{
		"generated.go", "spec.yaml", "main_test.go", "handler.go",
		"README.md", "package.json", "large_doc.md",
	}

	result := heuristic.PreScreen(fileList, tmp)

	// Проверяем auto-ignored
	ignoredPaths := make(map[string]bool)
	for _, e := range result.AutoIgnored {
		ignoredPaths[e.Path] = true
	}
	assert.True(t, ignoredPaths["generated.go"], "generated.go should be auto-ignored")
	assert.True(t, ignoredPaths["spec.yaml"], "spec.yaml should be auto-ignored")

	// Проверяем auto-kept
	keptPaths := make(map[string]bool)
	for _, f := range result.AutoKept {
		keptPaths[f] = true
	}
	assert.True(t, keptPaths["main_test.go"], "test file should be auto-kept")
	assert.True(t, keptPaths["README.md"], "README should be auto-kept")
	assert.True(t, keptPaths["package.json"], "package.json should be auto-kept")
	assert.True(t, keptPaths["handler.go"], "small source file should be auto-kept (source ext + <2KB)")
}

func TestGroupFilesByScope(t *testing.T) {
	files := []string{
		"config/database.yaml",
		"config/redis.yaml",
		"k8s/deployment.yaml",
		"k8s/service.yaml",
		"docs/api.yaml",
		"src/handler.go",
		"src/service.go",
	}

	groups := analyze.GroupFilesByScope(files)

	// Проверяем, что файлы сгруппированы по scoped паттернам
	patternMap := make(map[string][]string)
	for _, g := range groups {
		patternMap[g.Pattern] = g.Files
	}

	// Должны быть группы типа "config/*.yaml", "k8s/*.yaml"
	assert.Contains(t, patternMap, "config/*.yaml")
	assert.Contains(t, patternMap, "k8s/*.yaml")
	assert.Contains(t, patternMap, "docs/*.yaml")
	assert.Contains(t, patternMap, "src/*.go")

	assert.Len(t, patternMap["config/*.yaml"], 2)
	assert.Len(t, patternMap["k8s/*.yaml"], 2)
}

func TestKnownDirNames(t *testing.T) {
	// KnownDirNames — универсальные директории для пропуска
	assert.True(t, heuristic.KnownDirNames["build"])
	assert.True(t, heuristic.KnownDirNames["vendor"])
	assert.True(t, heuristic.KnownDirNames["node_modules"])
	assert.True(t, heuristic.KnownDirNames["gen"])
	assert.True(t, heuristic.KnownDirNames["testdata"])
	assert.True(t, heuristic.KnownDirNames["deprecated"])
	assert.True(t, heuristic.KnownDirNames["k8s"])
	assert.True(t, heuristic.KnownDirNames["terraform"])

	// ProtoScanDirNames — директории для сканирования .proto файлов
	assert.True(t, heuristic.ProtoScanDirNames["swagger"])
	assert.True(t, heuristic.ProtoScanDirNames["openapi"])

	// Эти директории НЕ должны быть в KnownDirNames (это исходный код)
	assert.False(t, heuristic.KnownDirNames["internal"])
	assert.False(t, heuristic.KnownDirNames["pkg"])
	assert.False(t, heuristic.KnownDirNames["cmd"])
	assert.False(t, heuristic.KnownDirNames["api"])
	assert.False(t, heuristic.KnownDirNames["lib"])
	assert.False(t, heuristic.KnownDirNames["third_party"])
}

func TestParseGroupDecisions(t *testing.T) {
	t.Run("valid JSON", func(t *testing.T) {
		response := `[{"pattern":"*.yaml","action":"ignore","confidence":85},{"pattern":"*.go","action":"keep","confidence":90}]`
		decisions, err := llm.ParseGroupDecisions(response)
		require.NoError(t, err)
		assert.Len(t, decisions, 2)
		assert.Equal(t, "ignore", decisions[0].Action)
		assert.Equal(t, 85, decisions[0].Confidence)
		assert.Equal(t, "keep", decisions[1].Action)
	})

	t.Run("with markdown", func(t *testing.T) {
		response := "```json\n[{\"pattern\":\"*.yaml\",\"action\":\"ignore\",\"confidence\":80}]\n```"
		decisions, err := llm.ParseGroupDecisions(response)
		require.NoError(t, err)
		assert.Len(t, decisions, 1)
	})

	t.Run("invalid action defaults to keep", func(t *testing.T) {
		response := `[{"pattern":"*.json","action":"maybe","confidence":70}]`
		decisions, err := llm.ParseGroupDecisions(response)
		require.NoError(t, err)
		assert.Equal(t, "keep", decisions[0].Action)
		assert.Equal(t, 50, decisions[0].Confidence)
	})

	t.Run("confidence clamped to 0-100", func(t *testing.T) {
		response := `[{"pattern":"*.yaml","action":"keep","confidence":150},{"pattern":"*.go","action":"keep","confidence":-10}]`
		decisions, err := llm.ParseGroupDecisions(response)
		require.NoError(t, err)
		assert.Equal(t, 100, decisions[0].Confidence)
		assert.Equal(t, 0, decisions[1].Confidence)
	})
}

func TestValidateLLMDecisions(t *testing.T) {
	t.Run("protected files not ignored", func(t *testing.T) {
		decisions := []analyze.GroupDecision{
			{Pattern: "*.json", Action: "ignore", Confidence: 90},
		}
		groups := []analyze.FileGroup{
			{Pattern: "*.json", Files: []string{"package.json", "tsconfig.json", "data.json"}},
		}

		entries := resolve.LLMDecisions(decisions, groups)
		// *.json не должен быть в entries, потому что покрывает protected files
		assert.Empty(t, entries)
	})

	t.Run("non-protected files can be ignored", func(t *testing.T) {
		decisions := []analyze.GroupDecision{
			{Pattern: "k8s/*.yaml", Action: "ignore", Confidence: 95},
		}
		groups := []analyze.FileGroup{
			{Pattern: "k8s/*.yaml", Files: []string{"k8s/deployment.yaml", "k8s/service.yaml"}},
		}

		entries := resolve.LLMDecisions(decisions, groups)
		assert.Len(t, entries, 1)
		assert.Equal(t, "k8s/*.yaml", entries[0].Pattern)
	})

	t.Run("low confidence marked as uncertain", func(t *testing.T) {
		decisions := []analyze.GroupDecision{
			{Pattern: "*.md", Action: "ignore", Confidence: 40},
		}
		groups := []analyze.FileGroup{
			// Используем файлы, которых НЕТ в ProtectedFiles
			// CHANGELOG.md и notes.md не защищены, в отличие от CONTRIBUTING.md
			{Pattern: "*.md", Files: []string{"CHANGELOG.md", "notes.md"}},
		}

		entries := resolve.LLMDecisions(decisions, groups)
		assert.Len(t, entries, 1)
		assert.Contains(t, entries[0].Reason, "uncertain")
	})
}

// ── TUI model tests ────────────────────────────────────────────────────────

func newTestModel(items []analyzeItem) *analyzeModel {
	m := &analyzeModel{
		items:            items,
		collapsed:        make(map[string]bool),
		existingPatterns: make(map[string]bool),
		viewportH:        15,
	}
	m.rebuildFiltered()
	return m
}

func TestAnalyzeModel_RebuildFiltered_All(t *testing.T) {
	m := newTestModel([]analyzeItem{
		{Pattern: "node_modules", Stage: "heuristic", Confidence: 100, Selected: true},
		{Pattern: "k8s/*.yaml", Stage: "llm", Confidence: 80, Selected: true},
		{Pattern: "!config.yaml", Stage: "negation", Confidence: 90, Selected: false},
	})

	assert.Len(t, m.filtered, 3)
}

func TestAnalyzeModel_RebuildFiltered_LLMOnly(t *testing.T) {
	m := newTestModel([]analyzeItem{
		{Pattern: "node_modules", Stage: "heuristic", Confidence: 100, Selected: true},
		{Pattern: "k8s/*.yaml", Stage: "llm", Confidence: 80, Selected: true},
		{Pattern: "!config.yaml", Stage: "negation", Confidence: 90, Selected: false},
	})

	m.filter = filterLLM
	m.rebuildFiltered()

	assert.Len(t, m.filtered, 2) // llm + negation
	for _, idx := range m.filtered {
		assert.NotEqual(t, "heuristic", m.items[idx].Stage)
	}
}

func TestAnalyzeModel_RebuildFiltered_Unselected(t *testing.T) {
	m := newTestModel([]analyzeItem{
		{Pattern: "node_modules", Stage: "heuristic", Confidence: 100, Selected: true},
		{Pattern: "k8s/*.yaml", Stage: "llm", Confidence: 80, Selected: false},
		{Pattern: "docs", Stage: "llm", Confidence: 70, Selected: false},
	})

	m.filter = filterUnselected
	m.rebuildFiltered()

	assert.Len(t, m.filtered, 2)
	for _, idx := range m.filtered {
		assert.False(t, m.items[idx].Selected)
	}
}

func TestAnalyzeModel_SortItems_ByStageThenConfidence(t *testing.T) {
	m := newTestModel([]analyzeItem{
		{Pattern: "low-llm", Stage: "llm", Confidence: 50},
		{Pattern: "high-heur", Stage: "heuristic", Confidence: 80},
		{Pattern: "high-llm", Stage: "llm", Confidence: 90},
		{Pattern: "low-heur", Stage: "heuristic", Confidence: 60},
		{Pattern: "neg", Stage: "negation", Confidence: 95},
	})

	// After rebuildFiltered, items are sorted
	assert.Equal(t, "high-heur", m.items[m.filtered[0]].Pattern)
	assert.Equal(t, "low-heur", m.items[m.filtered[1]].Pattern)
	assert.Equal(t, "high-llm", m.items[m.filtered[2]].Pattern)
	assert.Equal(t, "low-llm", m.items[m.filtered[3]].Pattern)
	assert.Equal(t, "neg", m.items[m.filtered[4]].Pattern)
}

func TestAnalyzeModel_ComputeGroups(t *testing.T) {
	m := newTestModel([]analyzeItem{
		{Pattern: "a", Stage: "heuristic", Confidence: 100},
		{Pattern: "b", Stage: "heuristic", Confidence: 90},
		{Pattern: "c", Stage: "llm", Confidence: 80},
		{Pattern: "d", Stage: "negation", Confidence: 95},
	})

	assert.Len(t, m.groups, 3)
	assert.Equal(t, "heuristic", m.groups[0].stage)
	assert.Equal(t, 2, m.groups[0].count)
	assert.Equal(t, "llm", m.groups[1].stage)
	assert.Equal(t, 1, m.groups[1].count)
	assert.Equal(t, "negation", m.groups[2].stage)
	assert.Equal(t, 1, m.groups[2].count)
}

func TestAnalyzeModel_ToggleCurrent(t *testing.T) {
	m := newTestModel([]analyzeItem{
		{Pattern: "a", Stage: "heuristic", Confidence: 100, Selected: true},
		{Pattern: "b", Stage: "llm", Confidence: 80, Selected: false},
	})

	m.cursor = 0
	m.toggleCurrent()
	assert.False(t, m.items[m.filtered[0]].Selected, "should toggle off")

	m.toggleCurrent()
	assert.True(t, m.items[m.filtered[0]].Selected, "should toggle back on")
}

func TestAnalyzeModel_SelectDeselectAll(t *testing.T) {
	m := newTestModel([]analyzeItem{
		{Pattern: "a", Stage: "heuristic", Confidence: 100, Selected: true},
		{Pattern: "b", Stage: "llm", Confidence: 80, Selected: false},
		{Pattern: "c", Stage: "llm", Confidence: 70, Selected: true},
	})

	m.deselectAll()
	for _, item := range m.items {
		assert.False(t, item.Selected)
	}

	m.selectAll()
	for _, item := range m.items {
		assert.True(t, item.Selected)
	}
}

func TestAnalyzeModel_Undo(t *testing.T) {
	m := newTestModel([]analyzeItem{
		{Pattern: "a", Stage: "heuristic", Confidence: 100, Selected: true},
		{Pattern: "b", Stage: "llm", Confidence: 80, Selected: false},
	})

	// Toggle item 0 (true → false)
	m.cursor = 0
	m.toggleCurrent()
	assert.False(t, m.items[m.filtered[0]].Selected)

	// Undo → should be true again
	m.undo()
	assert.True(t, m.items[m.filtered[0]].Selected)

	// Undo with no state → no-op
	m.undo()
	assert.True(t, m.items[m.filtered[0]].Selected)
}

func TestAnalyzeModel_UndoSelectAll(t *testing.T) {
	m := newTestModel([]analyzeItem{
		{Pattern: "a", Stage: "heuristic", Confidence: 100, Selected: true},
		{Pattern: "b", Stage: "llm", Confidence: 80, Selected: false},
		{Pattern: "c", Stage: "llm", Confidence: 70, Selected: true},
	})

	m.deselectAll()
	for _, item := range m.items {
		assert.False(t, item.Selected)
	}

	m.undo()
	assert.True(t, m.items[0].Selected, "item 0 was true before deselectAll")
	assert.False(t, m.items[1].Selected, "item 1 was false before deselectAll")
	assert.True(t, m.items[2].Selected, "item 2 was true before deselectAll")
}

func TestAnalyzeModel_SelectGroup(t *testing.T) {
	m := newTestModel([]analyzeItem{
		{Pattern: "a", Stage: "heuristic", Confidence: 100, Selected: false},
		{Pattern: "b", Stage: "heuristic", Confidence: 90, Selected: false},
		{Pattern: "c", Stage: "llm", Confidence: 80, Selected: true},
	})

	// Cursor in heuristic group
	m.cursor = 0
	m.selectGroup(true)

	// All heuristic items should be selected
	for _, idx := range m.filtered {
		if m.items[idx].Stage == "heuristic" {
			assert.True(t, m.items[idx].Selected)
		}
	}
}

func TestAnalyzeModel_InvertGroup(t *testing.T) {
	m := newTestModel([]analyzeItem{
		{Pattern: "a", Stage: "heuristic", Confidence: 100, Selected: true},
		{Pattern: "b", Stage: "heuristic", Confidence: 90, Selected: false},
		{Pattern: "c", Stage: "llm", Confidence: 80, Selected: true},
	})

	m.cursor = 0
	m.invertGroup()

	// Heuristic items should be inverted
	heuristicItems := make(map[string]bool)
	for _, idx := range m.filtered {
		if m.items[idx].Stage == "heuristic" {
			heuristicItems[m.items[idx].Pattern] = m.items[idx].Selected
		}
	}
	assert.False(t, heuristicItems["a"], "was true, should be false")
	assert.True(t, heuristicItems["b"], "was false, should be true")

	// LLM item should be unchanged
	for _, idx := range m.filtered {
		if m.items[idx].Pattern == "c" {
			assert.True(t, m.items[idx].Selected, "llm item should be unchanged")
		}
	}
}

func TestAnalyzeModel_SkipCollapsed(t *testing.T) {
	m := newTestModel([]analyzeItem{
		{Pattern: "a", Stage: "heuristic", Confidence: 100},
		{Pattern: "b", Stage: "heuristic", Confidence: 90},
		{Pattern: "c", Stage: "llm", Confidence: 80},
	})

	// Collapse heuristic group
	m.collapsed["heuristic"] = true

	// Cursor at start → should skip to llm
	m.cursor = 0
	m.skipCollapsed()
	assert.Equal(t, 2, m.cursor, "should skip to llm group (index 2)")
}

func TestAnalyzeModel_AdjustScroll(t *testing.T) {
	m := newTestModel([]analyzeItem{
		{Pattern: "a", Stage: "heuristic", Confidence: 100},
		{Pattern: "b", Stage: "heuristic", Confidence: 90},
		{Pattern: "c", Stage: "llm", Confidence: 80},
		{Pattern: "d", Stage: "llm", Confidence: 70},
		{Pattern: "e", Stage: "llm", Confidence: 60},
	})
	m.viewportH = 3

	// Cursor beyond viewport → scroll should follow
	m.cursor = 4
	m.adjustScroll()
	assert.Equal(t, 2, m.scrollOff, "scroll should keep cursor visible")

	// Cursor before scroll → scroll should move up
	m.cursor = 0
	m.adjustScroll()
	assert.Equal(t, 0, m.scrollOff)
}

func TestAnalyzeModel_SelectedCount(t *testing.T) {
	m := newTestModel([]analyzeItem{
		{Pattern: "a", Selected: true},
		{Pattern: "b", Selected: false},
		{Pattern: "c", Selected: true},
	})

	assert.Equal(t, 2, m.selectedCount())
}

func TestAnalyzeModel_MarkExisting(t *testing.T) {
	m := &analyzeModel{
		items: []analyzeItem{
			{Pattern: "node_modules"},
			{Pattern: "k8s/*.yaml"},
			{Pattern: "*.png"},
		},
		existingPatterns: map[string]bool{
			"node_modules": true,
			"*.png":        true,
		},
		collapsed: make(map[string]bool),
	}

	m.markExisting()

	assert.True(t, m.items[0].Exists)
	assert.False(t, m.items[1].Exists)
	assert.True(t, m.items[2].Exists)
}

func TestMatchPatternToFiles(t *testing.T) {
	files := []string{
		"k8s/deployment.yaml",
		"k8s/service.yaml",
		"config/database.yaml",
		"src/handler.go",
	}

	tests := []struct {
		name    string
		pattern string
		expect  int
	}{
		{"glob pattern", "k8s/*.yaml", 2},
		{"directory prefix", "k8s", 2},
		{"no match", "nonexistent/*.txt", 0},
		{"negation pattern stripped", "!k8s/*.yaml", 2},
		{"single file match", "src/*.go", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := analyze.MatchPatternToFiles(tt.pattern, files)
			assert.Len(t, result, tt.expect)
		})
	}
}

func TestFilterMode_String(t *testing.T) {
	assert.Equal(t, "all", filterAll.String())
	assert.Equal(t, "llm only", filterLLM.String())
	assert.Equal(t, "unselected", filterUnselected.String())
}

func TestStageOrder(t *testing.T) {
	assert.Less(t, stageOrder("heuristic"), stageOrder("llm"))
	assert.Less(t, stageOrder("llm"), stageOrder("negation"))
	assert.Equal(t, 0, stageOrder("heuristic"))
}

func TestStageTitle(t *testing.T) {
	assert.Equal(t, "Heuristic (auto-detected)", stageTitle("heuristic"))
	assert.Equal(t, "LLM Analysis", stageTitle("llm"))
	assert.Equal(t, "Exceptions (negation)", stageTitle("negation"))
}
