package scoring

import (
	"path/filepath"
	"strings"

	"ragota/internal/analyze/classify"
)

// Priority represents the indexing priority level.
type Priority int

const (
	PrioritySkip   Priority = 0  // Skip indexing entirely
	PriorityLow    Priority = 25 // Low priority, index if resources available
	PriorityMedium Priority = 50 // Medium priority, standard indexing
	PriorityHigh   Priority = 75 // High priority, index first
	PriorityCritical Priority = 100 // Critical, must be indexed
)

// String returns human-readable priority name.
func (p Priority) String() string {
	switch p {
	case PrioritySkip:
		return "skip"
	case PriorityLow:
		return "low"
	case PriorityMedium:
		return "medium"
	case PriorityHigh:
		return "high"
	case PriorityCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// ScoringResult holds the priority score with reasoning.
type ScoringResult struct {
	Priority Priority
	Score    int    // 0-100
	Reason   string // explanation
}

// Scorer determines indexing priority based on file classification and context.
type Scorer struct {
	// Domain terms can be injected for context-aware scoring
	DomainTerms []string
}

// NewScorer creates a new priority scorer.
func NewScorer() *Scorer {
	return &Scorer{
		DomainTerms: []string{},
	}
}

// SetDomainTerms configures domain-specific terms for context-aware scoring.
func (s *Scorer) SetDomainTerms(terms []string) {
	s.DomainTerms = terms
}

// Score determines the indexing priority for a file.
//
// Parameters:
//   - path: relative file path
//   - classification: result from classifier
//   - importCount: number of imports (proxy for connectivity)
//   - sizeBytes: file size in bytes
func (s *Scorer) Score(path string, classification classify.ClassificationResult, importCount int, sizeBytes int64) ScoringResult {
	// Start with base priority from classification
	basePriority := s.basePriorityFromCategory(classification.Category)

	// Adjust based on path patterns
	pathAdjustment := s.pathAdjustment(path)

	// Adjust based on domain relevance
	domainAdjustment := s.domainRelevanceAdjustment(path)

	// Adjust based on connectivity (imports suggest importance)
	connectivityAdjustment := s.connectivityAdjustment(importCount)

	// Calculate final score
	finalScore := basePriority + pathAdjustment + domainAdjustment + connectivityAdjustment

	// Clamp to valid range
	if finalScore < 0 {
		finalScore = 0
	}
	if finalScore > 100 {
		finalScore = 100
	}

	// Determine priority level
	priority := s.scoreToPriority(finalScore)

	// Build reason
	reason := s.buildReason(classification.Category, pathAdjustment, domainAdjustment, connectivityAdjustment)

	return ScoringResult{
		Priority: priority,
		Score:    finalScore,
		Reason:   reason,
	}
}

func (s *Scorer) basePriorityFromCategory(category classify.FileCategory) int {
	switch category {
	case classify.CategoryModel:
		return 85 // Domain models are critical
	case classify.CategoryLogic:
		return 80 // Business logic is high priority
	case classify.CategoryInterface:
		return 80 // API boundaries (handlers, controllers, endpoints) — CRITICAL for cross-repo indexing
	case classify.CategoryInfrastructure:
		return 40 // Infrastructure is less critical
	case classify.CategoryTest:
		return 35 // Tests are useful but secondary
	case classify.CategoryConfig:
		return 50 // Config can be important
	case classify.CategoryDocumentation:
		return 20 // Docs are low priority for indexing
	case classify.CategoryUnknown:
		return 50 // Default to medium
	default:
		return 50
	}
}

func (s *Scorer) pathAdjustment(path string) int {
	lower := strings.ToLower(path)
	// Ensure leading slash for consistent matching
	if len(lower) > 0 && lower[0] != '/' {
		lower = "/" + lower
	}

	// Low-value directories checked FIRST (take priority)
	lowValuePatterns := []string{
		"/vendor/", "/node_modules/", "/build/", "/dist/",
		"/generated/", "/tmp/", "/temp/", "/cache/",
	}
	for _, p := range lowValuePatterns {
		if strings.Contains(lower, p) {
			return -30
		}
	}

	// API boundaries — CRITICAL for cross-repo indexing (HTTP/gRPC/Kafka endpoints)
	apiBoundaryPatterns := []string{
		"/api/", "/handler/", "/handlers/", "/controller/", "/controllers/",
		"/endpoint/", "/endpoints/", "/route/", "/routes/", "/server/",
	}
	for _, p := range apiBoundaryPatterns {
		if strings.Contains(lower, p) {
			return 15 // High bonus for API boundary code
		}
	}

	// High-value directories
	highValuePatterns := []string{
		"/core/", "/domain/", "/internal/", "/src/",
		"/lib/", "/pkg/", "/app/",
	}
	for _, p := range highValuePatterns {
		if strings.Contains(lower, p) {
			return 10
		}
	}

	return 0
}

func (s *Scorer) domainRelevanceAdjustment(path string) int {
	if len(s.DomainTerms) == 0 {
		return 0
	}

	lower := strings.ToLower(path)
	filename := strings.ToLower(filepath.Base(path))

	matches := 0
	for _, term := range s.DomainTerms {
		termLower := strings.ToLower(term)
		if strings.Contains(lower, termLower) || strings.Contains(filename, termLower) {
			matches++
		}
	}

	if matches == 0 {
		return 0
	}
	if matches == 1 {
		return 5
	}
	if matches <= 3 {
		return 10
	}
	return 15 // Many domain terms = very relevant
}

func (s *Scorer) connectivityAdjustment(importCount int) int {
	// Files with many imports are likely important (central to the codebase)
	if importCount >= 10 {
		return 10
	}
	if importCount >= 5 {
		return 5
	}
	return 0
}

func (s *Scorer) scoreToPriority(score int) Priority {
	if score >= 80 {
		return PriorityHigh
	}
	if score >= 60 {
		return PriorityMedium
	}
	if score >= 40 {
		return PriorityLow
	}
	return PrioritySkip
}

func (s *Scorer) buildReason(category classify.FileCategory, pathAdj, domainAdj, connAdj int) string {
	var parts []string

	parts = append(parts, "category:"+string(category))

	if pathAdj != 0 {
		if pathAdj > 0 {
			parts = append(parts, "high-value-path")
		} else {
			parts = append(parts, "low-value-path")
		}
	}

	if domainAdj > 0 {
		parts = append(parts, "domain-relevant")
	}

	if connAdj > 0 {
		parts = append(parts, "high-connectivity")
	}

	return strings.Join(parts, ", ")
}
