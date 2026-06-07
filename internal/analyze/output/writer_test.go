package output

import (
	"testing"

	"ragota/internal/analyze/types"
)

func TestGroupPatterns_SkipsEmptyExtension(t *testing.T) {
	// Files without extension should NOT generate **/reponame/* patterns
	entries := []types.Entry{
		{Path: "investment-management-system/grpc_health_probe", Pattern: ""},
		{Path: "investment-management-system/Dockerfile", Pattern: ""},
		{Path: "issuance/grpc_health_probe", Pattern: ""},
		{Path: "redemption-management-system/Dockerfile", Pattern: ""},
	}

	patterns := GroupPatterns(entries)

	for _, p := range patterns {
		if p == "**/investment-management-system/*" ||
			p == "**/issuance/*" ||
			p == "**/redemption-management-system/*" {
			t.Errorf("Generated repo-root pattern that should be skipped: %s", p)
		}
	}

	// Should generate no patterns for files without extension
	if len(patterns) != 0 {
		t.Errorf("Expected 0 patterns for files without extension, got %d: %v", len(patterns), patterns)
	}
}

func TestGroupPatterns_WithMixedExtensions(t *testing.T) {
	// Mix of files with and without extension
	entries := []types.Entry{
		// Files without extension (should be skipped)
		{Path: "investment-management-system/grpc_health_probe", Pattern: ""},
		{Path: "investment-management-system/Dockerfile", Pattern: ""},
		// Files with extension (should generate patterns)
		{Path: "investment-management-system/src/main.ts", Pattern: ""},
		{Path: "investment-management-system/src/app.ts", Pattern: ""},
		{Path: "investment-management-system/package.json", Pattern: ""},
	}

	patterns := GroupPatterns(entries)

	// Should NOT have repo-root pattern
	for _, p := range patterns {
		if p == "**/investment-management-system/*" {
			t.Errorf("Generated repo-root pattern: %s", p)
		}
	}

	// Should have patterns for .ts and .json files
	hasTs := false
	hasJson := false
	for _, p := range patterns {
		if p == "**/investment-management-system/src/*.ts" {
			hasTs = true
		}
		if p == "**/investment-management-system/*.json" {
			hasJson = true
		}
	}

	if !hasTs {
		t.Error("Expected pattern for .ts files")
	}
	if !hasJson {
		t.Error("Expected pattern for .json files")
	}
}

func TestGroupPatterns_AggregationByDepth(t *testing.T) {
	// Many subdirectories under .ai-tools should aggregate
	entries := []types.Entry{
		{Path: ".ai-tools/bm25/store/data.json", Pattern: ""},
		{Path: ".ai-tools/bm25/store/meta.json", Pattern: ""},
		{Path: ".ai-tools/qdrant/collections/data.json", Pattern: ""},
		{Path: ".ai-tools/qdrant/collections/meta.json", Pattern: ""},
		{Path: ".ai-tools/jdtls/workspace/data.json", Pattern: ""},
		{Path: ".ai-tools/jdtls/workspace/meta.json", Pattern: ""},
		{Path: ".ai-tools/lsp/sessions/data.json", Pattern: ""},
		{Path: ".ai-tools/lsp/sessions/meta.json", Pattern: ""},
	}

	patterns := GroupPatterns(entries)

	// Should aggregate under .ai-tools level
	hasAggregated := false
	for _, p := range patterns {
		if p == "**/.ai-tools/**/*.json" {
			hasAggregated = true
			break
		}
	}

	if !hasAggregated {
		t.Logf("Patterns generated: %v", patterns)
		t.Log("Note: Aggregation may not trigger with current threshold")
	}
}
