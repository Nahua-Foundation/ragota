package promote

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/indexing"
)

// Query intent: whether the question asks for a symbol itself or for the code
// that uses it. "What calls X" cannot be answered by text retrieval alone — a
// call site is one line inside a function whose name and documentation are
// about something else, so no retrieval document for the caller mentions X.
// The service therefore resolves X first and answers from the code graph
// (see callers.go).
//
// Detection works on the query language — interrogative phrasing — and never
// on anything project-specific: the same patterns apply to every repository.
// Clients that know what they want pass the intent explicitly in the request
// and skip detection entirely.

// Intent values accepted by a search request.
const (
	// IntentAuto detects the intent from the query phrasing (the default).
	IntentAuto = "auto"
	// IntentCallers returns the code that calls/uses the described symbol.
	IntentCallers = "callers"
	// IntentNone forces plain text retrieval with no intent handling.
	IntentNone = "none"
)

// actionVerbs is a closed list of common action verbs that make an
// interrogative sentence a callers-question: "what X-es Y" asks which code
// X-es Y. Copular and passive phrasings ("what is Y", "where is Y defined")
// deliberately stay plain retrieval.
const actionVerbs = `calls?|invokes?|uses?|reads?|consumes?|creates?|instantiates?|constructs?|registers?|triggers?|asks?|schedules?|starts?|launches?|opens?|closes?|deletes?|updates?|queries|fetches?|loads?|saves?|emits?|dispatches?|sends?|publishes?|produces?|writes?(?:\s+to)?|wires?\s*(?:up)?|listens?\s+to|subscribes?\s+to`

var callersPatterns = []*regexp.Regexp{
	// "what calls X", "who uses X", "which class asks X", "what code reads X".
	// The optional word after the interrogative is the subject ("code",
	// "class", "service"); the verb list keeps "what is X" out.
	regexp.MustCompile(`(?i)^\s*(?:what|who|which)(?:\s+[a-z]+)?\s+(?:` + actionVerbs + `)\s+(.{3,})$`),
	// "callers of X", "usages of X", "call sites of X".
	regexp.MustCompile(`(?i)\b(?:callers?|call\s*sites?|usages?|users?|consumers?)\s+of\s+(.{3,})$`),
	// "where is X called/used/registered ...".
	regexp.MustCompile(`(?i)^\s*where\s+(?:is|are)\s+(.{3,}?)\s+(?:called|used|invoked|referenced|instantiated|created|constructed|registered|read|written|consumed|wired\s*(?:up)?)\b`),
	// "where does <subject> wire up X" — an active subject of at most three
	// plain words; a route path ("where does POST /api/x go") never matches.
	regexp.MustCompile(`(?i)^\s*where\s+(?:do|does)\s+(?:[a-z0-9_.-]+\s+){1,3}(?:` + actionVerbs + `)\s+(.{3,})$`),
	// "how does <subject> check/call/reach X": the answer is the subject's
	// code touching X, which is exactly X's incoming edges. The subordinate
	// conjunction is stripped by cleanCallee.
	regexp.MustCompile(`(?i)^\s*how\s+do(?:es)?\s+.{3,}?\s+(?:calls?|checks?|verif(?:y|ies)|validates?|reach(?:es)?|talks?\s+to|queries|fetch(?:es)?|reads?|uses?|invokes?)\s+(.{3,})$`),
	// Russian: "кто вызывает X", "какой класс использует X".
	regexp.MustCompile(`(?i)^\s*(?:кто|что|какой\s+\S+|какая\s+\S+|какие\s+\S+)\s+(?:вызывает|вызывают|использует|используют|читает|создает|создаёт|регистрирует|запускает|публикует|потребляет|шлет|шлёт)\s+(.{3,})$`),
	// Russian passive: "где вызывается X", "где используется X".
	regexp.MustCompile(`(?i)^\s*где\s+(?:вызывается|используется|читается|создается|создаётся|регистрируется|запускается)\s+(.{3,})$`),
}

// detectCallersIntent reports whether the query asks for the callers/users of
// something, and returns the description of that something (the callee) with
// the interrogative scaffolding stripped, so retrieval can look for the callee
// itself rather than for the words "what calls".
func detectCallersIntent(query string) (string, bool) {
	q := strings.TrimSpace(query)
	q = strings.TrimSuffix(q, "?")
	q = strings.TrimSpace(q)
	for _, re := range callersPatterns {
		m := re.FindStringSubmatch(q)
		if m == nil {
			continue
		}
		if callee := cleanCallee(m[len(m)-1]); callee != "" {
			return callee, true
		}
	}
	return "", false
}

// cleanCallee trims subordinate conjunctions, articles and whitespace off a
// captured callee description ("that a product sku exists" -> "product sku
// exists").
func cleanCallee(s string) string {
	s = strings.TrimSpace(s)
	for _, conj := range []string{"that ", "if ", "whether "} {
		if len(s) > len(conj) && strings.EqualFold(s[:len(conj)], conj) {
			s = strings.TrimSpace(s[len(conj):])
			break
		}
	}
	for _, article := range []string{"the ", "a ", "an "} {
		if len(s) > len(article) && strings.EqualFold(s[:len(article)], article) {
			s = strings.TrimSpace(s[len(article):])
			break
		}
	}
	if len(s) < 3 {
		return ""
	}
	return s
}

// IntentEnabled reports whether this request wants any intent handling at
// all. "none" turns it off per request; search.intent: off turns detection off
// server-wide but never overrides an intent the client asked for by name.
func (p *Promoter) IntentEnabled(q *indexing.SearchQuery) bool {
	switch strings.ToLower(strings.TrimSpace(q.Intent)) {
	case IntentNone:
		return false
	case "", IntentAuto:
		return !p.intentOff
	}
	return true
}

// ResolveIntent normalizes and validates the request's intent, running
// detection where the intent says so. It returns the effective intent kind
// ("" when the query is plain retrieval) and the callee description for
// IntentCallers.
func (p *Promoter) ResolveIntent(q *indexing.SearchQuery) (string, string, error) {
	switch strings.ToLower(strings.TrimSpace(q.Intent)) {
	case "", IntentAuto:
		if p.intentOff {
			return "", "", nil
		}
		if callee, ok := detectCallersIntent(q.Query); ok {
			return IntentCallers, callee, nil
		}
		return "", "", nil
	case IntentCallers:
		// Explicitly requested: strip the scaffolding when present, otherwise
		// the whole query describes the callee.
		if callee, ok := detectCallersIntent(q.Query); ok {
			return IntentCallers, callee, nil
		}
		return IntentCallers, strings.TrimSpace(q.Query), nil
	case IntentNone:
		return "", "", nil
	default:
		// Reported without a sentinel: this is request validation, and the
		// service layer owns the error kind clients branch on (ErrBadRequest,
		// which the HTTP layer maps to 400).
		return "", "", fmt.Errorf("unknown intent %q (want auto, callers or none)", q.Intent)
	}
}
