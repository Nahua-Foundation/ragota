package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/app"
	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/graph"
	"github.com/Nahua-Foundation/ragota/internal/index"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// Service is the narrow interface of *app.Service that the API needs.
type Service interface {
	AddRepo(ctx context.Context, src domain.SourceType, req *domain.AddRequest) (*domain.Repo, error)
	ListRepos(ctx context.Context) ([]*domain.Repo, error)
	GetRepo(ctx context.Context, id string) (*domain.Repo, error)
	DeleteRepo(ctx context.Context, id string) error
	StartIndex(ctx context.Context, id string, force bool) (*app.IndexAck, error)
	ResetRepo(ctx context.Context, id string) (*domain.Repo, error)
	Ready(ctx context.Context) error
	Search(ctx context.Context, q *index.SearchQuery, mode string) (*index.SearchResult, error)
	Symbols(ctx context.Context, opts domain.QueryOpts) ([]*domain.ASTUnit, error)
	Definition(ctx context.Context, repoID, filePath string, line int) (*domain.ASTUnit, error)
	References(ctx context.Context, repoID, filePath string, line, limit int) ([]*domain.Edge, error)
	Stats(ctx context.Context) (map[string]*index.IndexerStats, error)
	GraphNeighbors(ctx context.Context, unitID string) (*graph.NeighborsResult, error)
	GraphPath(ctx context.Context, fromID, toID string, maxDepth int) ([]*graph.PathStep, error)
	GraphTrace(ctx context.Context, req *graph.TraceRequest) (*graph.TraceResult, error)
	ServicesGraph(ctx context.Context) ([]*graph.ServiceInfo, []*graph.ServiceLink, error)
	Topics(ctx context.Context, service string) ([]*graph.TopicInfo, error)
	BuildContext(ctx context.Context, query string, repos []string, mode string, limit, hops int, intent string) (*app.ContextResult, error)
	SyncRepoAsync(ctx context.Context, repoID string, force bool) error
	FindRepoByHints(ctx context.Context, hints []string) (*domain.Repo, error)
	IngestRuntimeServiceGraph(ctx context.Context, edges []app.RuntimeServiceEdge) (*app.RuntimeIngestResult, error)
	ApplyCommits(ctx context.Context, repoID string, commits []app.CommitEvent) (*app.CommitAck, error)
	RepoCoverage(ctx context.Context, repoID string) (*app.CoverageReport, error)
	RepoJobs(ctx context.Context, repoID string, limit int) ([]*domain.IndexJob, error)
	RepoJob(ctx context.Context, repoID, jobID string) (*domain.IndexJob, error)
	CompactIndexes(ctx context.Context) map[string]int64
}

type Server struct {
	svc         Service
	cfg         *config.ServerConfig
	rateLimiter *RateLimiter
	limits      bodyLimits
	// version is the binary's build version, reported by /health.
	version string
	// webhookSecret is the shared secret for /webhooks/git. Empty disables the
	// endpoint entirely (fail closed).
	webhookSecret string
}

// Option adjusts a Server at construction. Everything a Server needs that does
// not come from the configuration file arrives this way, so the value is fixed
// before the router serves its first request.
type Option func(*Server)

// WithVersion sets the build version /health reports. main stamps it through
// -ldflags; a build that was not stamped keeps "dev".
func WithVersion(v string) Option {
	return func(s *Server) {
		if v != "" {
			s.version = v
		}
	}
}

func NewServer(svc Service, cfg *config.ServerConfig, opts ...Option) *Server {
	s := &Server{
		svc:           svc,
		cfg:           cfg,
		limits:        loadBodyLimits(cfg),
		version:       "dev",
		webhookSecret: os.Getenv("RAGOTA_WEBHOOK_SECRET"),
	}
	for _, opt := range opts {
		opt(s)
	}
	if cfg.RateLimit != nil && cfg.RateLimit.Enabled {
		s.rateLimiter = NewRateLimiter(&RateLimiterConfig{
			RequestsPerMinute: cfg.RateLimit.RequestsPerMinute,
			Burst:             cfg.RateLimit.Burst,
			TrustedProxies:    ParseTrustedProxies(cfg.TrustedProxies),
		})
	}
	return s
}

// Close releases server-owned resources (rate limiter goroutine).
func (s *Server) Close() error {
	if s.rateLimiter != nil {
		s.rateLimiter.Close()
	}
	return nil
}

// errPayloadTooLarge marks a request body that exceeded the route's limit. It
// is distinct from a malformed body: the two used to be indistinguishable
// 400s, so a client pushing a legitimately large commit could not tell that
// the payload, not its JSON, was the problem.
var errPayloadTooLarge = errors.New("request body too large")

// maxBytesLimit reports the byte limit an oversized-body error was measured
// against, and whether err is such an error.
func maxBytesLimit(err error) (int64, bool) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return maxErr.Limit, true
	}
	return 0, false
}

func decode[T any](r *http.Request) (*T, error) {
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		if limit, ok := maxBytesLimit(err); ok {
			return nil, fmt.Errorf("%w: request body exceeds the %d byte limit for this endpoint", errPayloadTooLarge, limit)
		}
		return nil, fmt.Errorf("%w: invalid request body", app.ErrBadRequest)
	}
	return &v, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// writeErrorCode writes an error body with an explicit machine-readable code.
func writeErrorCode(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, ErrorResponse{Error: msg, Code: code})
}

// repoBusyRetryAfterSeconds is the hint returned with a repo_busy response: an
// index pass is running, so a client should back off rather than retry hot.
const repoBusyRetryAfterSeconds = 30

func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErrorCode(w, http.StatusNotFound, CodeNotFound, "not found")
	case errors.Is(err, app.ErrRepoBusy):
		// Busy is a retry condition, not a malformed request: 409 plus a
		// Retry-After, so clients and proxies both handle it correctly.
		w.Header().Set("Retry-After", strconv.Itoa(repoBusyRetryAfterSeconds))
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Error:             err.Error(),
			Code:              CodeRepoBusy,
			RetryAfterSeconds: repoBusyRetryAfterSeconds,
		})
	case errors.Is(err, errPayloadTooLarge):
		limit, _ := maxBytesLimit(err)
		writeJSON(w, http.StatusRequestEntityTooLarge, ErrorResponse{
			Error: err.Error(), Code: CodePayloadTooLarge, LimitBytes: limit,
		})
	case errors.Is(err, app.ErrInvalidPath):
		writeErrorCode(w, http.StatusBadRequest, CodeInvalidPath, err.Error())
	case errors.Is(err, app.ErrBadRequest):
		writeErrorCode(w, http.StatusBadRequest, CodeValidationFailed, err.Error())
	case errors.Is(err, index.ErrIndexDamaged):
		// The index, not the request, is broken, and no retry of this query
		// will do better until it is rebuilt. Reporting that as a generic 500
		// hides it among ordinary faults; a caller measuring retrieval cannot
		// tell the difference between an index that cannot answer and a query
		// that ranked badly, and reads the zero as a ranking regression.
		slog.Error("search served from a damaged index; a forced reindex is the repair", "err", err)
		writeErrorCode(w, http.StatusServiceUnavailable, CodeIndexDamaged,
			"the search index is unreadable and must be rebuilt (forced reindex)")
	default:
		slog.Error("internal error", "err", err)
		writeErrorCode(w, http.StatusInternalServerError, CodeInternal, "internal error")
	}
}

// writeTooLarge reports an oversized body when the limit is known up front
// (Content-Length), naming the limit the route enforces.
func writeTooLarge(w http.ResponseWriter, limit int64) {
	writeJSON(w, http.StatusRequestEntityTooLarge, ErrorResponse{
		Error:      fmt.Sprintf("request body exceeds the %d byte limit for this endpoint", limit),
		Code:       CodePayloadTooLarge,
		LimitBytes: limit,
	})
}

// parseSnippetMode validates the snippet mode a retrieval request asked for.
// An unknown one is rejected rather than treated as the default: a client that
// asked for "off" and silently received full chunks would blame the budget.
func parseSnippetMode(mode string) (string, error) {
	switch mode {
	case "", SnippetChunk:
		return SnippetChunk, nil
	case SnippetLine, SnippetNone:
		return mode, nil
	default:
		return "", fmt.Errorf("%w: unknown snippet mode %q (want %q, %q or %q)",
			app.ErrBadRequest, mode, SnippetChunk, SnippetLine, SnippetNone)
	}
}

// checkMaxBytes rejects a negative response budget. Zero means no budget, which
// is the default and today's behaviour.
func checkMaxBytes(n int) error {
	if n < 0 {
		return fmt.Errorf("%w: max_bytes must not be negative", app.ErrBadRequest)
	}
	return nil
}

// queryLimit parses a "limit" query parameter. Absent is 0, which each caller
// reads as its own default; anything that is not a positive integer is an
// error rather than a silently ignored cap.
func queryLimit(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%w: limit must be a positive integer", app.ErrBadRequest)
	}
	return n, nil
}

// queryList reads a repeatable query parameter that also accepts a
// comma-separated list, so ?repo=a&repo=b and ?repo=a,b ask the same thing.
func queryList(q url.Values, name string) []string {
	var out []string
	for _, raw := range q[name] {
		for _, part := range strings.Split(raw, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func toSymbol(u *domain.ASTUnit) ASTSymbol {
	return ASTSymbol{
		ID: u.ID, RepoID: u.RepoID, FilePath: u.FilePath,
		Language: u.Language, Kind: u.Kind, Name: u.Name,
		Qualified: u.Qualified, StartLine: u.StartLine, EndLine: u.EndLine, Doc: u.Doc,
	}
}

func (s *Server) HandleAddRepo(w http.ResponseWriter, r *http.Request) {
	req, err := decode[AddRepoRequest](r)
	if err != nil {
		writeError(w, err)
		return
	}
	if req.Name == "" {
		writeError(w, fmt.Errorf("%w: name is required", app.ErrBadRequest))
		return
	}
	if req.Source == "local" && req.Path == "" {
		writeError(w, fmt.Errorf("%w: path is required for local source", app.ErrBadRequest))
		return
	}
	if req.Source == "git" && req.URL == "" {
		writeError(w, fmt.Errorf("%w: url is required for git source", app.ErrBadRequest))
		return
	}
	repo, err := s.svc.AddRepo(r.Context(), domain.SourceType(req.Source), &domain.AddRequest{
		Name: req.Name, Path: req.Path, URL: req.URL, Branch: req.Branch,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toRepo(repo))
}

func (s *Server) HandleListRepos(w http.ResponseWriter, r *http.Request) {
	list, err := s.svc.ListRepos(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toRepos(list))
}

func (s *Server) HandleGetRepo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	repo, err := s.svc.GetRepo(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toRepo(repo))
}

func (s *Server) HandleDeleteRepo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.svc.DeleteRepo(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) HandleIndex(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, fmt.Errorf("%w: repo id is required", app.ErrBadRequest))
		return
	}
	var req IndexRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			if limit, ok := maxBytesLimit(err); ok {
				writeTooLarge(w, limit)
				return
			}
			writeError(w, fmt.Errorf("%w: invalid request body", app.ErrBadRequest))
			return
		}
	}
	ack, err := s.svc.StartIndex(r.Context(), id, req.Force)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toIndexAck(ack))
}

// HandleResetRepo clears a stuck indexing claim so the repo accepts work
// again. It is the escape hatch for a claim whose holder died between the
// startup recoveries.
func (s *Server) HandleResetRepo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, fmt.Errorf("%w: repo id is required", app.ErrBadRequest))
		return
	}
	repo, err := s.svc.ResetRepo(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toRepo(repo))
}

// HandleRepoCommits ingests a batch of commits pushed by an external client
// and triggers a partial reindex of the affected paths. Responds 202 when
// accepted, 409 when the first commit does not continue the stored cursor
// (the client should resend the missing range or request a full reindex).
//
// The 202 body says which of the two things happened, as /index's does: the
// batch is either being applied by this instance ("indexing") or durably
// queued for whichever worker claims it ("queued", with the job id to follow
// under /jobs).
func (s *Server) HandleRepoCommits(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	req, err := decode[CommitsRequest](r)
	if err != nil {
		writeError(w, err)
		return
	}
	if len(req.Commits) == 0 {
		writeError(w, fmt.Errorf("%w: commits are required", app.ErrBadRequest))
		return
	}
	commits := make([]app.CommitEvent, len(req.Commits))
	for i, c := range req.Commits {
		files := make([]app.CommitFile, len(c.Files))
		for j, f := range c.Files {
			files[j] = app.CommitFile{Path: f.Path, OldPath: f.OldPath, Status: f.Status, Content: f.Content}
		}
		commits[i] = app.CommitEvent{SHA: c.SHA, Parents: c.Parents, Files: files}
	}

	ack, err := s.svc.ApplyCommits(r.Context(), id, commits)
	if err != nil {
		writeError(w, err)
		return
	}
	if !ack.Accepted {
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Error:      "commit gap: the first commit does not continue the stored cursor",
			Code:       CodeCommitGap,
			LastCommit: ack.Before,
		})
		return
	}
	writeJSON(w, http.StatusAccepted, commitAckPayload(ack))
}

// defaultJobsLimit is the page size of a job listing when the caller does not
// ask for one.
const defaultJobsLimit = 50

// HandleRepoJobs lists the repository's queue entries, newest first.
//
// POST /index answers 202 whether the pass started or was only queued; this is
// where a client finds out which, and whether a queued job is still pending,
// running on some instance, or failed.
func (s *Server) HandleRepoJobs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, fmt.Errorf("%w: repo id is required", app.ErrBadRequest))
		return
	}
	limit, err := queryLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, err)
		return
	}
	if limit == 0 {
		limit = defaultJobsLimit
	}
	jobs, err := s.svc.RepoJobs(r.Context(), id, limit)
	if err != nil {
		writeError(w, err)
		return
	}
	out := toJobs(jobs)
	writeJSON(w, http.StatusOK, JobsResponse{Jobs: out, Total: len(out)})
}

// HandleRepoJob returns one queue entry of a repository.
func (s *Server) HandleRepoJob(w http.ResponseWriter, r *http.Request) {
	id, jobID := r.PathValue("id"), r.PathValue("job_id")
	if id == "" || jobID == "" {
		writeError(w, fmt.Errorf("%w: repo id and job id are required", app.ErrBadRequest))
		return
	}
	job, err := s.svc.RepoJob(r.Context(), id, jobID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toJob(job))
}

// HandleRepoCoverage reports how much of the repository's outbound contract
// surface the last index pass actually resolved.
//
// It is the difference between "this project makes 42 HTTP calls and we found
// all 42" and "this project makes thousands and we found 104": both look like
// a complete answer from the graph alone, because a call site that produced no
// edge leaves nothing behind to count.
func (s *Server) HandleRepoCoverage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, fmt.Errorf("%w: repo id is required", app.ErrBadRequest))
		return
	}
	report, err := s.svc.RepoCoverage(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toCoverage(report))
}

// HandleSyncState reports the repo's commit cursor and indexing status so an
// external client can decide what to push next.
func (s *Server) HandleSyncState(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	repo, err := s.svc.GetRepo(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, SyncStateResponse{
		RepoID:        repo.ID,
		LastCommit:    repo.LastCommit,
		Status:        string(repo.Status),
		PendingCommit: repo.PendingCommit,
		IndexedAt:     repo.IndexedAt,
		LastError:     repo.LastError,
	})
}

// HandleSearch answers a query with ranked hits.
//
// `snippet` and `max_bytes` bound what comes back. Neither has a default that
// changes anything: the caps the endpoint already had count elements, and an
// element has no size of its own — a limit of 20 has measured 34 KB, most of it
// snippet, which is a large bite out of the context window of the model that
// asked. `diagnostics` is opt-in for the same reason: the search layer computes
// it either way, but a caller that will not read it should not be sent it.
func (s *Server) HandleSearch(w http.ResponseWriter, r *http.Request) {
	req, err := decode[SearchRequest](r)
	if err != nil {
		writeError(w, err)
		return
	}
	if req.Query == "" {
		writeError(w, fmt.Errorf("%w: query is required", app.ErrBadRequest))
		return
	}
	snippet, err := parseSnippetMode(req.Snippet)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := checkMaxBytes(req.MaxBytes); err != nil {
		writeError(w, err)
		return
	}
	q := &index.SearchQuery{Query: req.Query, Repos: req.Repos, Limit: req.Limit, Filter: req.Filter, Intent: req.Intent}
	result, err := s.svc.Search(r.Context(), q, req.Mode)
	if err != nil {
		writeError(w, err)
		return
	}
	resp := SearchResponse{
		Hits:  toSearchHits(result.Hits, snippet),
		Total: result.Total, Query: result.Query, Mode: result.Mode,
	}
	if req.Diagnostics {
		resp.Diagnostics = toDiagnostics(result.Metadata)
	}
	// After the diagnostics, so that they count against the budget they add to.
	fitSearch(&resp, req.MaxBytes)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) HandleStats(w http.ResponseWriter, r *http.Request) {
	statsMap, err := s.svc.Stats(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	stats := StatsResponse{Indexers: make(map[string]IndexerStats)}
	for typ, st := range statsMap {
		stats.Indexers[typ] = IndexerStats{Documents: st.Documents, SizeBytes: st.SizeBytes, Repos: st.Repos}
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) HandleSymbolSearch(w http.ResponseWriter, r *http.Request) {
	req, err := decode[SymbolRequest](r)
	if err != nil {
		writeError(w, err)
		return
	}
	// Without at least one selector this would page through the whole unit
	// table; `qualified` was also documented but silently dropped.
	if req.RepoID == "" && req.Name == "" && req.Kind == "" && req.Qualified == "" && req.Symbol == "" {
		writeErrorCode(w, http.StatusBadRequest, CodeValidationFailed,
			"at least one of repo_id, name, kind, qualified or symbol is required")
		return
	}
	opts := domain.QueryOpts{
		RepoID: req.RepoID, Name: req.Name, Kind: req.Kind,
		Qualified: req.Qualified, NameOrQualified: req.Symbol, Limit: req.Limit,
	}
	units, err := s.svc.Symbols(r.Context(), opts)
	if err != nil {
		writeError(w, err)
		return
	}
	symbols := make([]*ASTSymbol, len(units))
	for i, u := range units {
		sym := toSymbol(u)
		symbols[i] = &sym
	}
	writeJSON(w, http.StatusOK, SymbolResponse{Symbols: symbols, Total: len(symbols)})
}

func (s *Server) HandleDefinition(w http.ResponseWriter, r *http.Request) {
	req, err := decode[DefinitionRequest](r)
	if err != nil {
		writeError(w, err)
		return
	}
	unit, err := s.svc.Definition(r.Context(), req.RepoID, req.FilePath, req.Position.Line)
	if err != nil {
		writeError(w, err)
		return
	}
	if unit == nil {
		writeJSON(w, http.StatusOK, DefinitionResponse{})
		return
	}
	sym := toSymbol(unit)
	writeJSON(w, http.StatusOK, DefinitionResponse{Definition: &sym})
}

// HandleReferences answers where the symbol at a file position is used.
//
// `limit` is the size of the answer and is bounded like /nav/symbol's, in
// app.References — it used to bound each of the endpoint's two lookups
// separately, so a request for ten could be answered with nineteen, and an
// omitted one meant the whole edge table twice over.
func (s *Server) HandleReferences(w http.ResponseWriter, r *http.Request) {
	req, err := decode[ReferencesRequest](r)
	if err != nil {
		writeError(w, err)
		return
	}
	edges, err := s.svc.References(r.Context(), req.RepoID, req.FilePath, req.Position.Line, req.Limit)
	if err != nil {
		writeError(w, err)
		return
	}
	refs := make([]*ASTReference, len(edges))
	for i, e := range edges {
		refs[i] = &ASTReference{
			RepoID: e.RepoID, FilePath: e.FilePath, Line: e.Line, Kind: e.Kind,
			Word: e.DstName, Target: e.DstID,
		}
	}
	writeJSON(w, http.StatusOK, ReferencesResponse{References: refs, Total: len(refs)})
}

// HandleHealth is a pure liveness probe: it answers as long as the process can
// serve HTTP and deliberately touches no dependency, so a database blip never
// makes an orchestrator restart otherwise healthy instances. Dependency state
// belongs to /ready.
//
// It also reports the versions, because this is the one route every client
// already calls and none of them could otherwise tell what they had connected
// to. `version` identifies the build for a log or a bug report; `api_version`
// is the wire contract, and is what a client with its own release cycle can
// actually refuse to talk to.
func (s *Server) HandleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, HealthResponse{
		Status: "ok", Version: s.version, APIVersion: SchemaVersion,
	})
}

// readinessTimeout bounds the dependency probe so a hung backend fails the
// readiness check instead of hanging it.
const readinessTimeout = 3 * time.Second

// HandleReady probes the dependencies a request needs and reports 503 while
// any of them is unavailable, so traffic is only routed to instances that can
// actually serve it.
func (s *Server) HandleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()

	if err := s.svc.Ready(ctx); err != nil {
		slog.Warn("readiness probe failed", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{
			Error: err.Error(), Code: CodeNotReady,
		})
		return
	}
	writeJSON(w, http.StatusOK, StatusPayload{Status: "ready"})
}

// --- Graph handlers ---

func (s *Server) HandleGraphNeighbors(w http.ResponseWriter, r *http.Request) {
	req, err := decode[NeighborsRequest](r)
	if err != nil {
		writeError(w, err)
		return
	}
	if req.UnitID == "" {
		writeError(w, fmt.Errorf("%w: unit_id is required", app.ErrBadRequest))
		return
	}
	res, err := s.svc.GraphNeighbors(r.Context(), req.UnitID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toNeighbors(res))
}

func (s *Server) HandleGraphPath(w http.ResponseWriter, r *http.Request) {
	req, err := decode[GraphPathRequest](r)
	if err != nil {
		writeError(w, err)
		return
	}
	if req.FromUnitID == "" || req.ToUnitID == "" {
		writeError(w, fmt.Errorf("%w: from_unit_id and to_unit_id are required", app.ErrBadRequest))
		return
	}
	steps, err := s.svc.GraphPath(r.Context(), req.FromUnitID, req.ToUnitID, req.MaxDepth)
	if errors.Is(err, store.ErrNotFound) {
		// "No path between these units" is an answer, not a missing resource:
		// the documented shape is 200 with an empty steps array.
		writeJSON(w, http.StatusOK, GraphPathResponse{Steps: []*PathStep{}})
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	dto := toPath(steps)
	writeJSON(w, http.StatusOK, GraphPathResponse{Steps: dto, Length: len(dto)})
}

func (s *Server) HandleGraphTrace(w http.ResponseWriter, r *http.Request) {
	req, err := decode[TraceRequest](r)
	if err != nil {
		writeError(w, err)
		return
	}
	if req.Symbol == "" || req.Param == "" {
		writeError(w, fmt.Errorf("%w: symbol and param are required", app.ErrBadRequest))
		return
	}
	res, err := s.svc.GraphTrace(r.Context(), &graph.TraceRequest{
		RepoID: req.RepoID, Symbol: req.Symbol, Param: req.Param, MaxDepth: req.MaxDepth,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTrace(res))
}

// HandleServices lists the detected services and the aggregated links between
// them.
//
// `repo` (repeatable, or comma-separated) narrows the graph and `limit` caps
// each list. The endpoint took no parameters at all and its size grows with
// every repository ever indexed, so the only way to ask about one service's
// neighbourhood was to receive the whole estate and filter it client-side.
func (s *Server) HandleServices(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := queryLimit(q.Get("limit"))
	if err != nil {
		writeError(w, err)
		return
	}
	services, links, err := s.svc.ServicesGraph(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toServices(services, links, queryList(q, "repo"), limit))
}

// HandleServicesExport renders the service graph as a Mermaid or DOT diagram.
func (s *Server) HandleServicesExport(w http.ResponseWriter, r *http.Request) {
	services, links, err := s.svc.ServicesGraph(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	format := r.URL.Query().Get("format")
	var text string
	switch format {
	case "dot":
		text = renderDOT(services, links)
	default:
		text = renderMermaid(services, links)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(text))
}

func (s *Server) HandleTopics(w http.ResponseWriter, r *http.Request) {
	topics, err := s.svc.Topics(r.Context(), r.URL.Query().Get("service"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTopics(topics))
}

// HandleContext returns search hits enriched with the code graph around them
// — a ready-made retrieval package for an LLM.
//
// It is the largest response the API produces (limit 20 with hops 3 has
// measured 57 KB), so it takes the same `snippet` and `max_bytes` bounds as
// /search. Items are dropped whole and best-first, which keeps each surviving
// hit next to the graph expansion that explains it.
func (s *Server) HandleContext(w http.ResponseWriter, r *http.Request) {
	req, err := decode[ContextRequest](r)
	if err != nil {
		writeError(w, err)
		return
	}
	if req.Query == "" {
		writeError(w, fmt.Errorf("%w: query is required", app.ErrBadRequest))
		return
	}
	snippet, err := parseSnippetMode(req.Snippet)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := checkMaxBytes(req.MaxBytes); err != nil {
		writeError(w, err)
		return
	}
	res, err := s.svc.BuildContext(r.Context(), req.Query, req.Repos, req.Mode, req.Limit, req.Hops, req.Intent)
	if err != nil {
		writeError(w, err)
		return
	}
	resp := toContext(res, snippet)
	fitContext(resp, req.MaxBytes)
	writeJSON(w, http.StatusOK, resp)
}

// HandleGitWebhook accepts GitHub/GitLab push payloads, finds the matching
// repository and triggers update + reindex.
//
// The endpoint sits outside API-key auth (CI systems cannot set one), so
// RAGOTA_WEBHOOK_SECRET is its only credential and is therefore mandatory:
// with the secret unset the endpoint is disabled rather than open. A request is
// authenticated by one of three constant-time checks against that secret —
// GitHub's HMAC signature (X-Hub-Signature-256), GitLab's flat token
// (X-Gitlab-Token), or a manual X-Webhook-Token — see authorizeWebhook. The
// secret is deliberately never read from the query string, which request
// loggers routinely record.
func (s *Server) HandleGitWebhook(w http.ResponseWriter, r *http.Request) {
	if s.webhookSecret == "" {
		slog.Warn("git webhook rejected: RAGOTA_WEBHOOK_SECRET is not set")
		writeErrorCode(w, http.StatusServiceUnavailable, CodeUnauthorized,
			"webhooks are disabled: RAGOTA_WEBHOOK_SECRET is not configured")
		return
	}

	// The GitHub HMAC is computed over the exact bytes received, so the body is
	// read once here and both authenticated and parsed from that buffer.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if limit, ok := maxBytesLimit(err); ok {
			writeError(w, fmt.Errorf("%w: request body exceeds the %d byte limit for this endpoint", errPayloadTooLarge, limit))
			return
		}
		writeError(w, fmt.Errorf("%w: cannot read request body", app.ErrBadRequest))
		return
	}
	if !s.authorizeWebhook(r, body) {
		writeErrorCode(w, http.StatusUnauthorized, CodeUnauthorized, "invalid webhook signature")
		return
	}

	var payload gitWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, fmt.Errorf("%w: invalid request body", app.ErrBadRequest))
		return
	}
	hints := payload.hints()
	if len(hints) == 0 {
		writeError(w, fmt.Errorf("%w: no repository identifiers in payload", app.ErrBadRequest))
		return
	}
	repo, err := s.svc.FindRepoByHints(r.Context(), hints)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.svc.SyncRepoAsync(r.Context(), repo.ID, false); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, StatusPayload{Status: "indexing", RepoID: repo.ID})
}

// authorizeWebhook reports whether a webhook request proves knowledge of the
// shared secret. It accepts, in order of preference: GitHub's HMAC signature
// over the raw body (X-Hub-Signature-256), GitLab's flat token (X-Gitlab-Token),
// or a manual X-Webhook-Token for hand-rolled callers. Every comparison is
// constant-time so it cannot be turned into a timing oracle. The query string
// is intentionally not consulted — it lands in access logs.
func (s *Server) authorizeWebhook(r *http.Request, body []byte) bool {
	if sig := r.Header.Get("X-Hub-Signature-256"); sig != "" {
		return verifyGitHubSignature(sig, s.webhookSecret, body)
	}
	if tok := r.Header.Get("X-Gitlab-Token"); tok != "" {
		return subtle.ConstantTimeCompare([]byte(tok), []byte(s.webhookSecret)) == 1
	}
	if tok := r.Header.Get("X-Webhook-Token"); tok != "" {
		return subtle.ConstantTimeCompare([]byte(tok), []byte(s.webhookSecret)) == 1
	}
	return false
}

// verifyGitHubSignature checks a GitHub "sha256=<hex>" HMAC of body keyed by
// secret, in constant time.
func verifyGitHubSignature(sig, secret string, body []byte) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(sig, prefix) {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(sig, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}

// gitWebhookPayload covers the fields of GitHub and GitLab push events we need.
type gitWebhookPayload struct {
	Repository struct { // GitHub (and GitLab "repository")
		Name     string `json:"name"`
		FullName string `json:"full_name"`
		CloneURL string `json:"clone_url"`
		GitHTTP  string `json:"git_http_url"`
		SSHURL   string `json:"ssh_url"`
	} `json:"repository"`
	Project struct { // GitLab
		Name              string `json:"name"`
		PathWithNamespace string `json:"path_with_namespace"`
		GitHTTPURL        string `json:"git_http_url"`
		SSHURL            string `json:"git_ssh_url"`
	} `json:"project"`
}

func (p *gitWebhookPayload) hints() []string {
	var hints []string
	for _, h := range []string{
		p.Repository.CloneURL, p.Repository.GitHTTP, p.Repository.SSHURL,
		p.Project.GitHTTPURL, p.Project.SSHURL,
		p.Repository.FullName, p.Project.PathWithNamespace,
		p.Repository.Name, p.Project.Name,
	} {
		if h != "" {
			hints = append(hints, h)
		}
	}
	return hints
}

// HandleOTelServiceGraph ingests a runtime service graph observed in tracing.
func (s *Server) HandleOTelServiceGraph(w http.ResponseWriter, r *http.Request) {
	req, err := decode[OTelServiceGraphRequest](r)
	if err != nil {
		writeError(w, err)
		return
	}
	if len(req.Edges) == 0 {
		writeError(w, fmt.Errorf("%w: edges are required", app.ErrBadRequest))
		return
	}
	edges := make([]app.RuntimeServiceEdge, len(req.Edges))
	for i, e := range req.Edges {
		edges[i] = app.RuntimeServiceEdge{Client: e.Client, Server: e.Server, Calls: e.Calls}
	}
	res, err := s.svc.IngestRuntimeServiceGraph(r.Context(), edges)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, IngestResultPayload{
		Received: res.Received, Stored: res.Stored, Unmatched: res.Unmatched, Known: res.Known,
	})
}

// HandleCompact settles the index layout on demand.
//
// A bulk loader — the eval harness, a first import — fills the index with many
// repositories in a row. Each pass would otherwise rewrite the whole index to
// reach the layout only the last one needs: on a corpus with one large
// repository in it, that is seconds of merging per repository to arrive
// somewhere the final pass would have arrived anyway. Such a loader sets
// indexes.bm25.no_compact and calls this once when it is done.
func (s *Server) HandleCompact(w http.ResponseWriter, r *http.Request) {
	took := s.svc.CompactIndexes(r.Context())
	writeJSON(w, http.StatusOK, CompactPayload{CompactedMS: took})
}
