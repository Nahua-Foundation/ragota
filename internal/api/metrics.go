package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/go-chi/chi/v5"

	"github.com/Nahua-Foundation/ragota/internal/obs"
)

// metrics is a minimal Prometheus-text-format registry: request counters by
// route pattern plus gauges pulled from the service on scrape. No external
// dependencies.
type metrics struct {
	mu       sync.Mutex
	requests map[string]*atomic.Int64 // "METHOD pattern" -> count
	errors   atomic.Int64
}

func newMetrics() *metrics {
	return &metrics{requests: map[string]*atomic.Int64{}}
}

// Middleware counts requests per chi route pattern and 5xx responses.
func (m *metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		pattern := chi.RouteContext(r.Context()).RoutePattern()
		if pattern == "" {
			pattern = "unmatched"
		}
		key := r.Method + " " + pattern
		m.mu.Lock()
		c, ok := m.requests[key]
		if !ok {
			c = &atomic.Int64{}
			m.requests[key] = c
		}
		m.mu.Unlock()
		c.Add(1)
		if sw.status >= 500 {
			m.errors.Add(1)
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// HandleMetrics renders Prometheus text format. Indexer gauges are read from
// the service at scrape time.
func (s *Server) HandleMetrics(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder

	b.WriteString("# TYPE ragota_http_requests_total counter\n")
	s.metrics.mu.Lock()
	keys := make([]string, 0, len(s.metrics.requests))
	for k := range s.metrics.requests {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		count := s.metrics.requests[k].Load()
		parts := strings.SplitN(k, " ", 2)
		fmt.Fprintf(&b, "ragota_http_requests_total{method=%q,route=%q} %d\n", parts[0], parts[1], count)
	}
	s.metrics.mu.Unlock()

	b.WriteString("# TYPE ragota_http_errors_total counter\n")
	fmt.Fprintf(&b, "ragota_http_errors_total %d\n", s.metrics.errors.Load())

	if repos, err := s.svc.ListRepos(r.Context()); err == nil {
		b.WriteString("# TYPE ragota_repos gauge\n")
		fmt.Fprintf(&b, "ragota_repos %d\n", len(repos))
	}
	if stats, err := s.svc.Stats(r.Context()); err == nil {
		b.WriteString("# TYPE ragota_indexer_documents gauge\n")
		names := make([]string, 0, len(stats))
		for name := range stats {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(&b, "ragota_indexer_documents{indexer=%q} %d\n", name, stats[name].Documents)
		}
	}

	// Process-local counters and duration summaries from the obs registry
	// (indexing/linking timings and link stats). Names are already fully
	// qualified with the ragota_ prefix.
	for _, m := range obs.Snapshot() {
		switch m.Type {
		case obs.TypeCounter:
			fmt.Fprintf(&b, "# TYPE %s counter\n", m.Name)
			fmt.Fprintf(&b, "%s %d\n", m.Name, m.Value)
		case obs.TypeSummary:
			fmt.Fprintf(&b, "# TYPE %s summary\n", m.Name)
			fmt.Fprintf(&b, "%s_count %d\n", m.Name, m.Count)
			fmt.Fprintf(&b, "%s_sum %g\n", m.Name, m.Sum)
		}
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}
