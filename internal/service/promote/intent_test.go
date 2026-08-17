package promote

import (
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/indexing"
)

func TestDetectCallersIntent(t *testing.T) {
	positives := []struct {
		query  string
		callee string
	}{
		{"what calls the shipping service ShipOrder rpc", "shipping service ShipOrder rpc"},
		{"who uses the AugmentSyncMsg helper from util/argo", "AugmentSyncMsg helper from util/argo"},
		{"what code reads the per-run log filename template off a dag run", "per-run log filename template off a dag run"},
		{"which class asks the workflow executor to decide after an async system task finishes", "workflow executor to decide after an async system task finishes"},
		{"what creates the expand search phase during a search", "expand search phase during a search"},
		{"who calls the metadata service that adds a block to an index", "metadata service that adds a block to an index"},
		{"where does grafana wire up the panel screenshot service during app startup", "panel screenshot service during app startup"},
		{"callers of ParseFilters", "ParseFilters"},
		{"usages of the retry helper", "retry helper"},
		{"where is blendRerankScores called", "blendRerankScores"},
		{"What calls validateAndNormalizeApp?", "validateAndNormalizeApp"},
		{"кто вызывает функцию promoteCallers", "функцию promoteCallers"},
		{"где используется blendRerankScores", "blendRerankScores"},
		{"how does the ratings service check that a product sku exists in the catalogue", "product sku exists in the catalogue"},
	}
	for _, tt := range positives {
		callee, ok := detectCallersIntent(tt.query)
		if !ok {
			t.Errorf("detectCallersIntent(%q) = no intent, want callee %q", tt.query, tt.callee)
			continue
		}
		if callee != tt.callee {
			t.Errorf("detectCallersIntent(%q) callee = %q, want %q", tt.query, callee, tt.callee)
		}
	}

	negatives := []string{
		// Definitions and implementations: the passive/copular phrasings.
		"where is the retry logic implemented",
		"what is the expand search phase",
		"where does POST /api/orders/cancel go",
		"how does the reranker work",
		"which model maps to the orders table",
		"blendRerankScores",
		"error reading frequency",
		// Too short a callee to mean anything.
		"what calls it",
	}
	for _, q := range negatives {
		if callee, ok := detectCallersIntent(q); ok {
			t.Errorf("detectCallersIntent(%q) = callee %q, want no intent", q, callee)
		}
	}
}

func TestResolveIntent(t *testing.T) {
	p := New(nil, nil, "")

	// Auto: detection decides.
	kind, callee, err := p.ResolveIntent(&indexing.SearchQuery{Query: "who calls ParseFilters"})
	if err != nil || kind != IntentCallers || callee != "ParseFilters" {
		t.Fatalf("auto = (%q, %q, %v), want (callers, ParseFilters, nil)", kind, callee, err)
	}
	kind, _, err = p.ResolveIntent(&indexing.SearchQuery{Query: "parse filters"})
	if err != nil || kind != "" {
		t.Fatalf("auto plain = (%q, %v), want no intent", kind, err)
	}

	// Explicit callers on a bare symbol: the whole query is the callee.
	kind, callee, err = p.ResolveIntent(&indexing.SearchQuery{Query: "ParseFilters", Intent: IntentCallers})
	if err != nil || kind != IntentCallers || callee != "ParseFilters" {
		t.Fatalf("explicit = (%q, %q, %v), want (callers, ParseFilters, nil)", kind, callee, err)
	}

	// Explicit none wins over a detectable phrasing.
	kind, _, err = p.ResolveIntent(&indexing.SearchQuery{Query: "who calls ParseFilters", Intent: IntentNone})
	if err != nil || kind != "" {
		t.Fatalf("none = (%q, %v), want no intent", kind, err)
	}

	// Unknown intents are a client error, not a silent fallback.
	// Unknown intents are reported; the service layer turns this into its
	// ErrBadRequest sentinel (see TestSearchRejectsUnknownIntent).
	if _, _, err := p.ResolveIntent(&indexing.SearchQuery{Query: "x", Intent: "sideways"}); err == nil {
		t.Fatal("unknown intent = nil error, want a rejection")
	}
}

func TestResolveIntentConfigOff(t *testing.T) {
	p := New(nil, nil, "off")

	// Auto-detection is disabled...
	kind, _, err := p.ResolveIntent(&indexing.SearchQuery{Query: "who calls ParseFilters"})
	if err != nil || kind != "" {
		t.Fatalf("off/auto = (%q, %v), want no intent", kind, err)
	}
	// ...but an explicit request is still honoured.
	kind, callee, err := p.ResolveIntent(&indexing.SearchQuery{Query: "who calls ParseFilters", Intent: IntentCallers})
	if err != nil || kind != IntentCallers || callee != "ParseFilters" {
		t.Fatalf("off/explicit = (%q, %q, %v), want (callers, ParseFilters, nil)", kind, callee, err)
	}
}

func TestIdentifierTokens(t *testing.T) {
	got := identifierTokens("the AugmentSyncMsg helper from util/argo uses sha256 and _render_filename but not plain words")
	want := map[string]bool{"AugmentSyncMsg": true, "sha256": true, "_render_filename": true}
	if len(got) != len(want) {
		t.Fatalf("identifierTokens = %v, want keys %v", got, want)
	}
	for _, tok := range got {
		if !want[tok] {
			t.Errorf("identifierTokens returned %q, not identifier-shaped", tok)
		}
	}
}
