package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	httpRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ragota_http_requests_total",
		Help: "requests served, by method and route pattern",
	}, []string{"method", "route"})

	httpErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_http_errors_total",
		Help: "responses with status >= 500",
	})

	reposGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ragota_repos",
		Help: "number of indexed repositories",
	})

	indexerDocuments = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ragota_indexer_documents",
		Help: "indexed documents per indexer",
	}, []string{"indexer"})
)

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// metricsMiddleware counts requests per chi route pattern and 5xx responses.
func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		pattern := chi.RouteContext(r.Context()).RoutePattern()
		if pattern == "" {
			pattern = "unmatched"
		}
		httpRequests.WithLabelValues(r.Method, pattern).Inc()
		if sw.status >= 500 {
			httpErrors.Inc()
		}
	})
}

// HandleMetrics serves Prometheus text format. The scrape-time gauges (repos,
// per-indexer documents) are refreshed from the service before the registry is
// rendered, preserving the previous "query on scrape" behaviour.
func (s *Server) HandleMetrics(w http.ResponseWriter, r *http.Request) {
	if repos, err := s.svc.ListRepos(r.Context()); err == nil {
		reposGauge.Set(float64(len(repos)))
	}
	if stats, err := s.svc.Stats(r.Context()); err == nil {
		indexerDocuments.Reset()
		for name, st := range stats {
			indexerDocuments.WithLabelValues(name).Set(float64(st.Documents))
		}
	}
	promhttp.Handler().ServeHTTP(w, r)
}
