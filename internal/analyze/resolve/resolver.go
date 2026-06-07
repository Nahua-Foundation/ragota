package resolve

import (
	"fmt"
	"path/filepath"
	"strings"

	"ragota/internal/analyze/classify"
	"ragota/internal/analyze/scoring"
	"ragota/internal/analyze/types"
)

// ProtectedFiles — файлы, которые никогда не должны быть проигнорированы.
// Универсальные файлы для любых проектов (Go, Java, Python, TS/JS, Rust, etc.).
var ProtectedFiles = map[string]bool{
	// Project structure
	"package.json": true, "package-lock.json": true,
	"go.mod": true, "go.sum": true,
	"tsconfig.json": true, ".eslintrc": true, ".eslintrc.json": true,
	"Makefile": true, "Dockerfile": true, "docker-compose.yml": true,
	"Cargo.toml": true, "Cargo.lock": true,
	"pom.xml": true, "build.gradle": true, "build.gradle.kts": true,
	// Documentation
	"README.md": true, "CONTRIBUTING.md": true, "LICENSE": true,
}

// LLMDecisions выполняет post-LLM safety checks и возвращает записи для игнорирования.
func LLMDecisions(decisions []types.GroupDecision, groups []types.FileGroup) []types.Entry {
	var entries []types.Entry

	for _, d := range decisions {
		if d.Action != "ignore" {
			continue
		}

		coveredProtected := false
		for _, g := range groups {
			if g.Pattern == d.Pattern {
				for _, f := range g.Files {
					base := filepath.Base(f)
					if ProtectedFiles[base] {
						coveredProtected = true
						break
					}
				}
				break
			}
		}

		if coveredProtected {
			continue
		}

		var groupSize int
		for _, g := range groups {
			if g.Pattern == d.Pattern {
				groupSize = len(g.Files)
				break
			}
		}

		confidence := d.Confidence
		reason := fmt.Sprintf("LLM: %s (confidence: %d)", d.Action, d.Confidence)

		if groupSize > 100 && confidence < 80 {
			reason += " ⚠ aggressive pattern"
			confidence -= 10
		}

		if confidence < 50 {
			reason += " ❓ uncertain"
		}

		entries = append(entries, types.Entry{
			Path:       d.Pattern,
			Pattern:    d.Pattern,
			Stage:      "llm",
			Reason:     reason,
			Confidence: confidence,
		})
	}

	return entries
}

// ConflictType represents the type of contradiction detected.
type ConflictType string

const (
	ConflictNone                ConflictType = "none"
	ConflictDomainTermIgnored   ConflictType = "domain_term_ignored"
	ConflictCriticalPathIgnore  ConflictType = "critical_path_ignored"
	ConflictLowConfidenceIgnore ConflictType = "low_confidence_ignore"
)

// Conflict represents a detected contradiction in LLM decisions.
type Conflict struct {
	Pattern         string
	Type            ConflictType
	Reason          string
	SuggestedAction string // "keep" or "ignore"
	Confidence      int
}

// Resolver detects and resolves contradictions in LLM decisions.
type Resolver struct {
	DomainTerms          []string
	CriticalPathPatterns []string
}

// NewResolver creates a new contradiction resolver.
func NewResolver() *Resolver {
	return &Resolver{
		DomainTerms: []string{},
		CriticalPathPatterns: []string{
			"core/", "domain/", "internal/",
			"src/", "lib/", "pkg/", "app/",
		},
	}
}

// SetDomainTerms configures domain terms for context-aware resolution.
func (r *Resolver) SetDomainTerms(terms []string) {
	r.DomainTerms = terms
}

// Resolve examines LLM decisions and detects contradictions.
func (r *Resolver) Resolve(decisions []types.GroupDecision, groups []types.FileGroup) ([]Conflict, []types.GroupDecision) {
	var conflicts []Conflict
	corrected := make([]types.GroupDecision, len(decisions))
	copy(corrected, decisions)

	for i, decision := range decisions {
		if decision.Action != "ignore" {
			continue
		}

		var group *types.FileGroup
		for j := range groups {
			if groups[j].Pattern == decision.Pattern {
				group = &groups[j]
				break
			}
		}

		if group == nil {
			continue
		}

		if conflict := r.checkDomainTermConflict(decision); conflict.Type != ConflictNone {
			conflicts = append(conflicts, conflict)
			corrected[i].Action = conflict.SuggestedAction
			corrected[i].Confidence = conflict.Confidence
		}

		if conflict := r.checkCriticalPathConflict(decision); conflict.Type != ConflictNone {
			conflicts = append(conflicts, conflict)
			corrected[i].Action = conflict.SuggestedAction
			corrected[i].Confidence = conflict.Confidence
		}

		if conflict := r.checkLowConfidenceConflict(decision); conflict.Type != ConflictNone {
			conflicts = append(conflicts, conflict)
		}
	}

	return conflicts, corrected
}

func (r *Resolver) checkDomainTermConflict(decision types.GroupDecision) Conflict {
	if len(r.DomainTerms) == 0 {
		return Conflict{Type: ConflictNone}
	}

	patternLower := strings.ToLower(decision.Pattern)

	for _, term := range r.DomainTerms {
		termLower := strings.ToLower(term)
		if strings.Contains(patternLower, termLower) {
			return Conflict{
				Pattern:         decision.Pattern,
				Type:            ConflictDomainTermIgnored,
				Reason:          "pattern contains domain term: " + term,
				SuggestedAction: "keep",
				Confidence:      85,
			}
		}
	}

	return Conflict{Type: ConflictNone}
}

func (r *Resolver) checkCriticalPathConflict(decision types.GroupDecision) Conflict {
	patternLower := strings.ToLower(decision.Pattern)

	for _, criticalPattern := range r.CriticalPathPatterns {
		if strings.Contains(patternLower, criticalPattern) {
			if decision.Confidence < 80 {
				return Conflict{
					Pattern:         decision.Pattern,
					Type:            ConflictCriticalPathIgnore,
					Reason:          "pattern matches critical path: " + criticalPattern,
					SuggestedAction: "keep",
					Confidence:      80,
				}
			}
		}
	}

	return Conflict{Type: ConflictNone}
}

func (r *Resolver) checkLowConfidenceConflict(decision types.GroupDecision) Conflict {
	if decision.Confidence < 60 && decision.Action == "ignore" {
		return Conflict{
			Pattern:         decision.Pattern,
			Type:            ConflictLowConfidenceIgnore,
			Reason:          "low confidence ignore decision",
			SuggestedAction: "keep",
			Confidence:      70,
		}
	}

	return Conflict{Type: ConflictNone}
}

// ApplyScoringContext uses classification and scoring to improve resolution.
func (r *Resolver) ApplyScoringContext(
	decisions []types.GroupDecision,
	classifications map[string]classify.ClassificationResult,
	scores map[string]scoring.ScoringResult,
) []types.GroupDecision {
	corrected := make([]types.GroupDecision, len(decisions))
	copy(corrected, decisions)

	for i, decision := range decisions {
		if decision.Action != "ignore" {
			continue
		}

		if classResult, ok := classifications[decision.Pattern]; ok {
			if classResult.Category == classify.CategoryModel ||
				classResult.Category == classify.CategoryLogic {
				if classResult.Confidence >= 75 {
					corrected[i].Action = "keep"
					corrected[i].Confidence = classResult.Confidence
				}
			}
		}

		if scoreResult, ok := scores[decision.Pattern]; ok {
			if scoreResult.Priority >= scoring.PriorityHigh {
				corrected[i].Action = "keep"
				corrected[i].Confidence = scoreResult.Score
			}
		}
	}

	return corrected
}
