// Package obs is a process-local metrics registry with no external
// dependencies. It accumulates counters and duration summaries (count+sum,
// no quantiles) that the /metrics endpoint renders in Prometheus text format.
package obs

import (
	"sort"
	"sync"
	"time"
)

// Metric types reported by Snapshot.
const (
	TypeCounter = "counter"
	TypeSummary = "summary"
)

// Metric is one entry of a registry snapshot. For counters only Value is
// meaningful; for summaries Count and Sum are.
type Metric struct {
	Name  string
	Type  string // counter | summary
	Count int64  // summary: number of observations
	Sum   float64
	Value int64 // counter value
}

type summary struct {
	count int64
	sum   float64
}

var (
	mu        sync.Mutex
	counters  = map[string]int64{}
	summaries = map[string]*summary{}
)

// Metric names shared across packages. Every soft failure — a path where the
// process keeps going with less data than it should have — increments one of
// these, so "it worked but returned nothing" is visible on /metrics.
const (
	MetricLSPPassTotal        = "ragota_lsp_pass_total"         // LSP refinement passes started
	MetricLSPServerFailures   = "ragota_lsp_server_failures"    // language skipped: dial/initialize failed
	MetricLSPEmptyLanguages   = "ragota_lsp_empty_languages"    // language yielded no symbols at all
	MetricLSPFilesRefined     = "ragota_lsp_files_refined"      // files successfully refined
	MetricLSPFilesFailed      = "ragota_lsp_files_failed"       // per-file refinement errors
	MetricLSPFilesSkipped     = "ragota_lsp_files_skipped"      // no server configured for the language
	MetricLSPReferenceErrors  = "ragota_lsp_reference_errors"   // textDocument/references failed
	MetricLSPDialChecks       = "ragota_lsp_dial_checks"        // startup reachability probes
	MetricLSPDialFailures     = "ragota_lsp_dial_failures"      // startup reachability probes that failed
	MetricLSPPassSeconds      = "ragota_lsp_pass_seconds"       // duration of a refinement pass
	MetricLSPCallPassSeconds  = "ragota_lsp_call_pass_seconds"  // duration of a call-edge correction pass
	MetricLSPCallRequests     = "ragota_lsp_call_requests"      // textDocument/references requests issued
	MetricLSPCallConfirmed    = "ragota_lsp_call_confirmed"     // call edges confirmed at their existing target
	MetricLSPCallRepointed    = "ragota_lsp_call_repointed"     // call edges moved to another definition
	MetricLSPCallAdded        = "ragota_lsp_call_added"         // call edges the tree-sitter pass had missed
	MetricLSPCallContradicted = "ragota_lsp_call_contradicted"  // resolutions the language server denies
	MetricLSPCallTruncated    = "ragota_lsp_call_truncated"     // repos where the symbol budget ran out
	MetricEmbedderInitFailure = "ragota_embedder_init_failures" // embedder could not be constructed
	MetricGitAuthFromEnv      = "ragota_git_auth_env_fallback"  // git token taken from the environment
	MetricConfigWarnings      = "ragota_config_warnings"        // suspicious-but-valid config settings
	MetricParserSkippedLangs  = "ragota_ast_languages_disabled" // parsers not registered by config
)

// RecordDuration accumulates an observation for a summary metric
// (rendered as <name>_count and <name>_sum).
func RecordDuration(name string, seconds float64) {
	mu.Lock()
	s, ok := summaries[name]
	if !ok {
		s = &summary{}
		summaries[name] = s
	}
	s.count++
	s.sum += seconds
	mu.Unlock()
}

// Inc adds delta to a counter metric.
func Inc(name string, delta int64) {
	mu.Lock()
	counters[name] += delta
	mu.Unlock()
}

// IncBy adds delta to a counter only when delta is positive, so callers can
// pass a count straight from a result struct without guarding it.
func IncBy(name string, delta int) {
	if delta <= 0 {
		return
	}
	Inc(name, int64(delta))
}

// Observe times fn and records its duration under name, incrementing
// failName when fn returns an error. It is the shape most call sites need:
// one line around a best-effort step.
func Observe(name, failName string, fn func() error) error {
	start := time.Now()
	err := fn()
	RecordDuration(name, time.Since(start).Seconds())
	if err != nil && failName != "" {
		Inc(failName, 1)
	}
	return err
}

// Value returns the current value of a counter (0 when unset). Intended for
// tests and for /metrics-adjacent assertions.
func Value(name string) int64 {
	mu.Lock()
	defer mu.Unlock()
	return counters[name]
}

// SummaryOf returns the observation count and sum of a summary metric.
func SummaryOf(name string) (int64, float64) {
	mu.Lock()
	defer mu.Unlock()
	s, ok := summaries[name]
	if !ok {
		return 0, 0
	}
	return s.count, s.sum
}

// Reset clears the registry. Tests use it to isolate assertions; the running
// server never calls it (Prometheus counters must not go backwards).
func Reset() {
	mu.Lock()
	counters = map[string]int64{}
	summaries = map[string]*summary{}
	mu.Unlock()
}

// Snapshot returns all registered metrics sorted by name.
func Snapshot() []Metric {
	mu.Lock()
	out := make([]Metric, 0, len(counters)+len(summaries))
	for name, v := range counters {
		out = append(out, Metric{Name: name, Type: TypeCounter, Value: v})
	}
	for name, s := range summaries {
		out = append(out, Metric{Name: name, Type: TypeSummary, Count: s.count, Sum: s.sum})
	}
	mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
