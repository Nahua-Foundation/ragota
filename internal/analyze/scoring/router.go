package scoring

// LLMPass determines which LLM evaluation pass a file should go through.
type LLMPass int

const (
	LLMPassNone  LLMPass = 0 // No LLM needed, heuristics sufficient
	LLMPassFast  LLMPass = 1 // Fast pass: minimal context, quick decision
	LLMPassDeep  LLMPass = 2 // Deep pass: full context, detailed analysis
)

// String returns human-readable LLM pass name.
func (p LLMPass) String() string {
	switch p {
	case LLMPassNone:
		return "none"
	case LLMPassFast:
		return "fast"
	case LLMPassDeep:
		return "deep"
	default:
		return "unknown"
	}
}

// RoutingDecision holds the LLM routing decision for a file.
type RoutingDecision struct {
	Pass        LLMPass
	Confidence  int    // confidence in the routing decision itself
	Reason      string // why this routing was chosen
}

// Router determines which LLM pass (if any) a file should go through.
type Router struct {
	// Thresholds for routing decisions
	HighConfidenceThreshold int // >= this: no LLM needed
	LowConfidenceThreshold  int // < this: deep pass needed
	ScoreThreshold          int // score below this: needs LLM review
}

// NewRouter creates a new LLM router with default thresholds.
func NewRouter() *Router {
	return &Router{
		HighConfidenceThreshold: 85,
		LowConfidenceThreshold:  50,
		ScoreThreshold:          60,
	}
}

// Route determines which LLM pass a file should go through.
//
// Parameters:
//   - scoring: result from scorer
//   - heuristicConfidence: confidence from heuristic classification (0-100)
func (r *Router) Route(scoring ScoringResult, heuristicConfidence int) RoutingDecision {
	// Skip LLM for low-priority files
	if scoring.Priority == PrioritySkip {
		return RoutingDecision{
			Pass:       LLMPassNone,
			Confidence: 100,
			Reason:     "skip priority, no LLM needed",
		}
	}

	// Skip LLM for high-confidence heuristic decisions
	if heuristicConfidence >= r.HighConfidenceThreshold {
		return RoutingDecision{
			Pass:       LLMPassNone,
			Confidence: heuristicConfidence,
			Reason:     "high heuristic confidence, no LLM needed",
		}
	}

	// Deep pass for low-confidence or low-score files
	if heuristicConfidence < r.LowConfidenceThreshold || scoring.Score < r.ScoreThreshold {
		return RoutingDecision{
			Pass:       LLMPassDeep,
			Confidence: 60,
			Reason:     "low confidence or score, needs deep analysis",
		}
	}

	// Fast pass for borderline cases
	return RoutingDecision{
		Pass:       LLMPassFast,
		Confidence: 75,
		Reason:     "borderline case, quick LLM review",
	}
}

// ShouldUseLLM returns true if the file should be sent to LLM.
func (r *Router) ShouldUseLLM(scoring ScoringResult, heuristicConfidence int) bool {
	decision := r.Route(scoring, heuristicConfidence)
	return decision.Pass != LLMPassNone
}
