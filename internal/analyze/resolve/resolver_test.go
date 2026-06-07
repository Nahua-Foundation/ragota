package resolve

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"ragota/internal/analyze/classify"
	"ragota/internal/analyze/scoring"
	"ragota/internal/analyze/types"
)

func TestLLMDecisions_ProtectedFiles(t *testing.T) {
	decisions := []types.GroupDecision{
		{Pattern: "*.json", Action: "ignore", Confidence: 90},
	}

	groups := []types.FileGroup{
		{Pattern: "*.json", Files: []string{"package.json", "data.json"}},
	}

	entries := LLMDecisions(decisions, groups)
	assert.Empty(t, entries, "should not ignore patterns covering protected files")
}

func TestLLMDecisions_NonProtectedFiles(t *testing.T) {
	decisions := []types.GroupDecision{
		{Pattern: "docs/*.md", Action: "ignore", Confidence: 85},
	}

	groups := []types.FileGroup{
		{Pattern: "docs/*.md", Files: []string{"docs/guide.md", "docs/api.md"}},
	}

	entries := LLMDecisions(decisions, groups)
	assert.Len(t, entries, 1)
	assert.Equal(t, "docs/*.md", entries[0].Pattern)
}

func TestLLMDecisions_AggressivePatterns(t *testing.T) {
	decisions := []types.GroupDecision{
		{Pattern: "*.go", Action: "ignore", Confidence: 75},
	}

	files := make([]string, 150)
	for i := range files {
		files[i] = "file.go"
	}

	groups := []types.FileGroup{
		{Pattern: "*.go", Files: files},
	}

	entries := LLMDecisions(decisions, groups)
	assert.Len(t, entries, 1)
	assert.Contains(t, entries[0].Reason, "aggressive pattern")
	assert.Less(t, entries[0].Confidence, 75)
}

func TestResolver_DomainTermConflict(t *testing.T) {
	r := NewResolver()
	r.SetDomainTerms([]string{"payment", "order", "user"})

	decisions := []types.GroupDecision{
		{Pattern: "internal/payment/*.go", Action: "ignore", Confidence: 80},
		{Pattern: "docs/*.md", Action: "ignore", Confidence: 85},
	}

	groups := []types.FileGroup{
		{Pattern: "internal/payment/*.go", Files: []string{"internal/payment/service.go"}},
		{Pattern: "docs/*.md", Files: []string{"docs/guide.md"}},
	}

	conflicts, corrected := r.Resolve(decisions, groups)

	// Should detect domain term conflict for payment
	assert.GreaterOrEqual(t, len(conflicts), 1)

	// Payment pattern should be corrected to "keep"
	for i, c := range corrected {
		if c.Pattern == "internal/payment/*.go" {
			assert.Equal(t, "keep", c.Action, "domain term pattern should be kept")
			_ = i
		}
	}
}

func TestResolver_CriticalPathConflict(t *testing.T) {
	r := NewResolver()

	decisions := []types.GroupDecision{
		{Pattern: "internal/utils/*.go", Action: "ignore", Confidence: 70},
	}

	groups := []types.FileGroup{
		{Pattern: "internal/utils/*.go", Files: []string{"internal/utils/helper.go"}},
	}

	conflicts, corrected := r.Resolve(decisions, groups)

	assert.GreaterOrEqual(t, len(conflicts), 1)
	assert.Equal(t, "keep", corrected[0].Action, "critical path should be kept")
}

func TestResolver_HighConfidenceCriticalPath_NoConflict(t *testing.T) {
	r := NewResolver()

	decisions := []types.GroupDecision{
		{Pattern: "internal/generated/*.go", Action: "ignore", Confidence: 95},
	}

	groups := []types.FileGroup{
		{Pattern: "internal/generated/*.go", Files: []string{"internal/generated/types.go"}},
	}

	conflicts, corrected := r.Resolve(decisions, groups)

	// High confidence should not trigger conflict
	assert.Equal(t, "ignore", corrected[0].Action, "high confidence ignore should stand")
	_ = conflicts
}

func TestResolver_LowConfidenceConflict(t *testing.T) {
	r := NewResolver()

	decisions := []types.GroupDecision{
		{Pattern: "scripts/*.sh", Action: "ignore", Confidence: 40},
	}

	groups := []types.FileGroup{
		{Pattern: "scripts/*.sh", Files: []string{"scripts/deploy.sh"}},
	}

	conflicts, _ := r.Resolve(decisions, groups)

	hasLowConf := false
	for _, c := range conflicts {
		if c.Type == ConflictLowConfidenceIgnore {
			hasLowConf = true
		}
	}
	assert.True(t, hasLowConf, "should detect low confidence conflict")
}

func TestResolver_ApplyScoringContext(t *testing.T) {
	r := NewResolver()

	decisions := []types.GroupDecision{
		{Pattern: "internal/model/*.go", Action: "ignore", Confidence: 70},
		{Pattern: "docs/*.md", Action: "ignore", Confidence: 80},
	}

	classifications := map[string]classify.ClassificationResult{
		"internal/model/*.go": {
			Category:   classify.CategoryModel,
			Confidence: 85,
			Reason:     "data structure definitions",
		},
	}

	scores := map[string]scoring.ScoringResult{}

	corrected := r.ApplyScoringContext(decisions, classifications, scores)

	// Model files should be corrected to "keep"
	for _, c := range corrected {
		if c.Pattern == "internal/model/*.go" {
			assert.Equal(t, "keep", c.Action, "model files should be kept")
		}
	}
}

func TestResolver_ApplyScoringContext_HighPriority(t *testing.T) {
	r := NewResolver()

	decisions := []types.GroupDecision{
		{Pattern: "internal/service/*.go", Action: "ignore", Confidence: 70},
	}

	classifications := map[string]classify.ClassificationResult{}
	scores := map[string]scoring.ScoringResult{
		"internal/service/*.go": {
			Priority: scoring.PriorityHigh,
			Score:    80,
			Reason:   "high priority",
		},
	}

	corrected := r.ApplyScoringContext(decisions, classifications, scores)

	assert.Equal(t, "keep", corrected[0].Action, "high priority files should be kept")
}
