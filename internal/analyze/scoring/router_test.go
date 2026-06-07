package scoring

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRouter_SkipPriority_NoLLM(t *testing.T) {
	r := NewRouter()

	scoring := ScoringResult{
		Priority: PrioritySkip,
		Score:    20,
		Reason:   "low priority",
	}

	decision := r.Route(scoring, 50)

	assert.Equal(t, LLMPassNone, decision.Pass)
	assert.Equal(t, 100, decision.Confidence)
}

func TestRouter_HighConfidence_NoLLM(t *testing.T) {
	r := NewRouter()

	scoring := ScoringResult{
		Priority: PriorityHigh,
		Score:    85,
		Reason:   "high priority",
	}

	decision := r.Route(scoring, 90)

	assert.Equal(t, LLMPassNone, decision.Pass)
	assert.GreaterOrEqual(t, decision.Confidence, 85)
}

func TestRouter_LowConfidence_DeepPass(t *testing.T) {
	r := NewRouter()

	scoring := ScoringResult{
		Priority: PriorityMedium,
		Score:    65,
		Reason:   "medium priority",
	}

	decision := r.Route(scoring, 40)

	assert.Equal(t, LLMPassDeep, decision.Pass)
}

func TestRouter_LowScore_DeepPass(t *testing.T) {
	r := NewRouter()

	scoring := ScoringResult{
		Priority: PriorityLow,
		Score:    45,
		Reason:   "low score",
	}

	decision := r.Route(scoring, 70)

	assert.Equal(t, LLMPassDeep, decision.Pass)
}

func TestRouter_Borderline_FastPass(t *testing.T) {
	r := NewRouter()

	scoring := ScoringResult{
		Priority: PriorityMedium,
		Score:    65,
		Reason:   "medium priority",
	}

	decision := r.Route(scoring, 70)

	assert.Equal(t, LLMPassFast, decision.Pass)
	assert.GreaterOrEqual(t, decision.Confidence, 70)
}

func TestRouter_ShouldUseLLM(t *testing.T) {
	r := NewRouter()

	tests := []struct {
		name              string
		scoring           ScoringResult
		heuristicConf     int
		shouldUseLLM      bool
	}{
		{
			name: "skip priority",
			scoring: ScoringResult{
				Priority: PrioritySkip,
				Score:    20,
			},
			heuristicConf: 50,
			shouldUseLLM:  false,
		},
		{
			name: "high confidence",
			scoring: ScoringResult{
				Priority: PriorityHigh,
				Score:    85,
			},
			heuristicConf: 90,
			shouldUseLLM:  false,
		},
		{
			name: "low confidence",
			scoring: ScoringResult{
				Priority: PriorityMedium,
				Score:    65,
			},
			heuristicConf: 40,
			shouldUseLLM:  true,
		},
		{
			name: "borderline",
			scoring: ScoringResult{
				Priority: PriorityMedium,
				Score:    65,
			},
			heuristicConf: 70,
			shouldUseLLM:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := r.ShouldUseLLM(tt.scoring, tt.heuristicConf)
			assert.Equal(t, tt.shouldUseLLM, result)
		})
	}
}

func TestLLMPass_String(t *testing.T) {
	tests := []struct {
		pass     LLMPass
		expected string
	}{
		{LLMPassNone, "none"},
		{LLMPassFast, "fast"},
		{LLMPassDeep, "deep"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.pass.String())
		})
	}
}
