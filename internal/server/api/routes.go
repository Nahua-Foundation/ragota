package api

import (
	_ "embed"
	"net/http"
	"os"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/Nahua-Foundation/ragota/internal/config"
)

//go:embed openapi.yaml
var openAPISpec []byte

// handleOpenAPI serves the embedded OpenAPI 3.0 specification.
func handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(openAPISpec)
}

// Router returns the HTTP router for the API.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()

	// Base middleware (always applied)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Optional CORS middleware
	if s.cfg.CORS.Enabled && len(s.cfg.CORS.Origins) > 0 {
		r.Use(corsMiddleware(s.cfg.CORS.Origins))
	}

	// Request metrics (all routes)
	r.Use(metricsMiddleware)

	// Public routes (no auth/rate limit)
	r.Get("/health", s.HandleHealth)
	r.Get("/ready", s.HandleReady)
	r.Get("/metrics", s.HandleMetrics)
	r.Get("/openapi.yaml", handleOpenAPI)

	// Webhooks: outside API-key auth (CI systems can't set API keys), but not
	// outside the protections that used to be skipped along with it — the
	// route gets the body cap and the rate limiter, and its own mandatory
	// shared secret (see HandleGitWebhook).
	r.Group(func(r chi.Router) {
		if s.rateLimiter != nil {
			r.Use(RateLimitMiddleware(s.rateLimiter))
		}
		r.Use(maxBodyMiddleware(s.limits.Default))
		r.Post("/webhooks/git", s.HandleGitWebhook)
	})

	// API routes
	r.Route("/api/v1", func(r chi.Router) {
		// Apply auth middleware if configured
		if s.cfg.Auth.Type == "api_key" {
			r.Use(AuthMiddleware(NewAPIKeyAuth(s.cfg.Auth.APIKeys)))
		}

		// Apply rate limit middleware if configured and enabled
		if s.rateLimiter != nil {
			r.Use(RateLimitMiddleware(s.rateLimiter))
		}

		// Everything that changes state or administers the instance. A key
		// without the admin scope answers 403 here and keeps every retrieval
		// route, which is the whole point: a retrieval client acts for a
		// language model, and no prompt should be able to reach a DELETE.
		admin := requireScope(ScopeAdmin)

		// Commit ingestion carries file contents, so it gets its own, much
		// larger cap. It is registered outside the group below because
		// middleware nests: the general cap would otherwise reject the body
		// before the larger one is ever consulted.
		r.With(maxBodyMiddleware(s.limits.Commits), admin).
			Post("/repos/{id}/commits", s.HandleRepoCommits)

		r.Group(func(r chi.Router) {
			r.Use(maxBodyMiddleware(s.limits.Default))

			// Repositories
			r.Route("/repos", func(r chi.Router) {
				r.Get("/", s.HandleListRepos)
				r.With(admin).Post("/", s.HandleAddRepo)
				r.Get("/{id}", s.HandleGetRepo)
				r.With(admin).Delete("/{id}", s.HandleDeleteRepo)
				r.With(admin).Post("/{id}/index", s.HandleIndex)
				r.With(admin).Post("/{id}/reset", s.HandleResetRepo)
				r.Get("/{id}/sync-state", s.HandleSyncState)
				r.Get("/{id}/coverage", s.HandleRepoCoverage)
				r.Get("/{id}/jobs", s.HandleRepoJobs)
				r.Get("/{id}/jobs/{job_id}", s.HandleRepoJob)
			})

			// Search
			r.Route("/search", func(r chi.Router) {
				r.Post("/", s.HandleSearch)
			})

			// Navigation
			r.Route("/nav", func(r chi.Router) {
				r.Post("/definition", s.HandleDefinition)
				r.Post("/references", s.HandleReferences)
				r.Post("/symbol", s.HandleSymbolSearch)
			})

			// Graph
			r.Route("/graph", func(r chi.Router) {
				r.Post("/neighbors", s.HandleGraphNeighbors)
				r.Post("/path", s.HandleGraphPath)
				r.Post("/trace", s.HandleGraphTrace)
			})

			// Retrieval context for LLMs
			r.Post("/context", s.HandleContext)

			// Services & topics
			r.Get("/services", s.HandleServices)
			r.Get("/services/export", s.HandleServicesExport)
			r.Get("/topics", s.HandleTopics)

			// Runtime observability ingest
			r.With(admin).Post("/otel/service-graph", s.HandleOTelServiceGraph)

			// Stats
			r.Get("/stats", s.HandleStats)

			// Index maintenance
			r.With(admin).Post("/admin/compact", s.HandleCompact)
		})
	})

	return r
}

// corsMiddleware adds CORS headers with the given allowed origins.
func corsMiddleware(origins []string) func(http.Handler) http.Handler {
	originSet := make(map[string]bool, len(origins))
	allowAll := false
	for _, o := range origins {
		if o == "*" {
			allowAll = true
		}
		originSet[o] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqOrigin := r.Header.Get("Origin")
			// The response varies by Origin whichever branch answers, so the
			// header must be set unconditionally or shared caches poison it.
			w.Header().Add("Vary", "Origin")
			// "*" is the documented default and means any origin. Echoing the
			// request's Origin (rather than literally "*") keeps credentialed
			// requests working.
			if reqOrigin != "" && (allowAll || originSet[reqOrigin]) {
				w.Header().Set("Access-Control-Allow-Origin", reqOrigin)
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// Body size limits. The general cap protects the small JSON endpoints; commit
// ingestion needs its own because it legitimately carries file contents and a
// realistic commit is several megabytes. Both are overridable via environment
// so an operator can raise them without a rebuild.
const (
	defaultMaxBodyBytes  int64 = 1 << 20  // 1 MiB
	defaultMaxCommitByte int64 = 64 << 20 // 64 MiB
)

// bodyLimits holds the effective per-route request body caps.
type bodyLimits struct {
	Default int64
	Commits int64
}

// loadBodyLimits takes the caps from the configuration, which already applies
// the defaults and the RAGOTA_MAX_*_BODY_BYTES environment overrides. The
// environment is still read here so a Server built without a loaded config
// (tests, embedding callers) keeps honouring it.
func loadBodyLimits(cfg *config.ServerConfig) bodyLimits {
	l := bodyLimits{Default: defaultMaxBodyBytes, Commits: defaultMaxCommitByte}
	if cfg != nil && cfg.MaxBodyBytes > 0 {
		l.Default = cfg.MaxBodyBytes
	}
	if cfg != nil && cfg.MaxCommitBodyBytes > 0 {
		l.Commits = cfg.MaxCommitBodyBytes
	}
	if v, err := strconv.ParseInt(os.Getenv("RAGOTA_MAX_BODY_BYTES"), 10, 64); err == nil && v > 0 {
		l.Default = v
	}
	if v, err := strconv.ParseInt(os.Getenv("RAGOTA_MAX_COMMIT_BODY_BYTES"), 10, 64); err == nil && v > 0 {
		l.Commits = v
	}
	return l
}

// maxBodyMiddleware caps request body size to protect against oversized
// payloads. A body that declares itself too large is rejected before it is
// read; one that only turns out too large while streaming surfaces as
// *http.MaxBytesError, which the handlers map to 413 (see writeError).
func maxBodyMiddleware(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > n {
				writeTooLarge(w, n)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
	}
}
