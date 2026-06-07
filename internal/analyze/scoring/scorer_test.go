package scoring

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"ragota/internal/analyze/classify"
)

func TestScorer_ModelFiles_HighPriority(t *testing.T) {
	s := NewScorer()

	classification := classify.ClassificationResult{
		Category:   classify.CategoryModel,
		Confidence: 85,
		Reason:     "data structure definitions",
	}

	result := s.Score("internal/model/user.go", classification, 3, 2000)

	assert.Equal(t, PriorityHigh, result.Priority)
	assert.GreaterOrEqual(t, result.Score, 70)
}

func TestScorer_LogicFiles_HighPriority(t *testing.T) {
	s := NewScorer()

	classification := classify.ClassificationResult{
		Category:   classify.CategoryLogic,
		Confidence: 80,
		Reason:     "business logic",
	}

	result := s.Score("internal/service/payment_service.go", classification, 8, 5000)

	assert.Equal(t, PriorityHigh, result.Priority)
	assert.GreaterOrEqual(t, result.Score, 70)
}

func TestScorer_InfrastructureFiles_LowPriority(t *testing.T) {
	s := NewScorer()

	classification := classify.ClassificationResult{
		Category:   classify.CategoryInfrastructure,
		Confidence: 75,
		Reason:     "infrastructure code",
	}

	result := s.Score("internal/repository/user_repo.go", classification, 2, 3000)

	assert.LessOrEqual(t, result.Score, 60)
}

func TestScorer_TestFiles_LowPriority(t *testing.T) {
	s := NewScorer()

	classification := classify.ClassificationResult{
		Category:   classify.CategoryTest,
		Confidence: 95,
		Reason:     "test file path",
	}

	result := s.Score("internal/user/user_test.go", classification, 5, 4000)

	assert.LessOrEqual(t, result.Score, 50)
}

func TestScorer_DocumentationFiles_SkipPriority(t *testing.T) {
	s := NewScorer()

	classification := classify.ClassificationResult{
		Category:   classify.CategoryDocumentation,
		Confidence: 90,
		Reason:     "documentation file",
	}

	result := s.Score("docs/api.md", classification, 0, 10000)

	assert.LessOrEqual(t, result.Score, 40)
}

func TestScorer_PathAdjustment_HighValue(t *testing.T) {
	s := NewScorer()

	classification := classify.ClassificationResult{
		Category:   classify.CategoryLogic,
		Confidence: 70,
		Reason:     "business logic",
	}

	// File in high-value directory
	result := s.Score("core/domain/user_service.go", classification, 3, 3000)
	assert.GreaterOrEqual(t, result.Score, 70)
}

func TestScorer_PathAdjustment_LowValue(t *testing.T) {
	s := NewScorer()

	classification := classify.ClassificationResult{
		Category:   classify.CategoryLogic,
		Confidence: 70,
		Reason:     "business logic",
	}

	// File in low-value directory (vendor gets -30 penalty)
	// Base priority for CategoryLogic is 80, so 80 - 30 = 50
	result := s.Score("vendor/lib/utils.go", classification, 3, 3000)
	assert.LessOrEqual(t, result.Score, 50)
}

func TestScorer_DomainRelevance(t *testing.T) {
	s := NewScorer()
	s.SetDomainTerms([]string{"payment", "order", "user"})

	classification := classify.ClassificationResult{
		Category:   classify.CategoryLogic,
		Confidence: 75,
		Reason:     "business logic",
	}

	// File with domain terms in path
	resultWithDomain := s.Score("internal/service/payment_service.go", classification, 3, 3000)

	// File without domain terms
	resultWithoutDomain := s.Score("internal/service/utils.go", classification, 3, 3000)

	assert.Greater(t, resultWithDomain.Score, resultWithoutDomain.Score,
		"file with domain terms should score higher")
}

func TestScorer_Connectivity(t *testing.T) {
	s := NewScorer()

	classification := classify.ClassificationResult{
		Category:   classify.CategoryLogic,
		Confidence: 75,
		Reason:     "business logic",
	}

	// File with many imports (high connectivity)
	resultHighConn := s.Score("internal/service/user_service.go", classification, 15, 5000)

	// File with few imports (low connectivity)
	resultLowConn := s.Score("internal/service/utils.go", classification, 2, 2000)

	assert.Greater(t, resultHighConn.Score, resultLowConn.Score,
		"file with more imports should score higher")
}

func TestScorer_ScoreClamping(t *testing.T) {
	s := NewScorer()
	s.SetDomainTerms([]string{"payment", "order", "user", "account", "transaction"})

	classification := classify.ClassificationResult{
		Category:   classify.CategoryModel,
		Confidence: 90,
		Reason:     "data structure definitions",
	}

	// Extreme case: high base + all adjustments
	result := s.Score("core/domain/payment_order_user.go", classification, 20, 10000)

	// Score should be clamped to 100
	assert.LessOrEqual(t, result.Score, 100)
	assert.GreaterOrEqual(t, result.Score, 0)
}

func TestPriority_String(t *testing.T) {
	tests := []struct {
		priority Priority
		expected string
	}{
		{PrioritySkip, "skip"},
		{PriorityLow, "low"},
		{PriorityMedium, "medium"},
		{PriorityHigh, "high"},
		{PriorityCritical, "critical"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.priority.String())
		})
	}
}
