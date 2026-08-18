package service

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/graph"
	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// SyncRepo updates a repository from its source (git pull for git repos)
// and starts reindexing in the background. The source update runs
// synchronously; prefer SyncRepoAsync in request handlers.
func (s *Service) SyncRepo(ctx context.Context, repoID string, force bool) error {
	repo, err := s.storage.GetRepo(ctx, repoID)
	if err != nil {
		return err
	}
	if source, ok := s.sources[repo.Source]; ok {
		if err := source.Update(ctx, repo); err != nil {
			return fmt.Errorf("update source: %w", err)
		}
	}
	return s.IndexRepo(ctx, repoID, force)
}

// SyncRepoAsync claims the repository for indexing and returns immediately;
// the source update (git pull) and the full reindex run in a background
// goroutine. If the update fails, the repo status is set to error.
func (s *Service) SyncRepoAsync(ctx context.Context, repoID string, force bool) error {
	repo, err := s.storage.GetRepo(ctx, repoID)
	if err != nil {
		return err
	}

	claimed, err := s.storage.ClaimRepoForIndexing(ctx, repoID, s.ownerID(), repoClaimTTLSeconds)
	if err != nil {
		return err
	}
	if !claimed {
		return fmt.Errorf("%w: %s", ErrRepoBusy, repoID)
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if source, ok := s.sources[repo.Source]; ok {
			if err := source.Update(s.baseCtx, repo); err != nil {
				slog.Error("sync update failed", "repo_id", repo.ID, "err", err)
				// Detached: the failure may itself be shutdown cancelling the
				// pull, and a dropped status write leaves the repo claimed.
				statusCtx, cancel := terminalCtx(s.baseCtx)
				uerr := s.storage.UpdateRepoStatus(statusCtx, repo.ID, repos.StatusError, fmt.Sprintf("update source: %v", err), repo.IndexedAt)
				cancel()
				if uerr != nil {
					slog.Error("update repo status to error failed", "repo_id", repo.ID, "err", uerr)
				}
				return
			}
		}
		_ = s.runIndex(s.baseCtx, repo, force)
	}()

	return nil
}

// normalizeRepoRef reduces a clone URL or repository reference to a comparable
// form: lowercase, no scheme, no user@, no trailing ".git" or "/".
func normalizeRepoRef(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if i := strings.Index(v, "://"); i >= 0 {
		v = v[i+3:]
	}
	v = strings.TrimPrefix(v, "git@")
	if i := strings.Index(v, "@"); i >= 0 {
		// Strip credentials in https://user:token@host/... style URLs.
		v = v[i+1:]
	}
	v = strings.TrimSuffix(v, "/")
	return strings.TrimSuffix(v, ".git")
}

// FindRepoByHints locates a repository from the identifiers in a webhook
// payload.
//
// Matching is exact, in decreasing order of confidence: normalized clone URL,
// then exact repository name, then the "<namespace>/<name>" tail of a hint.
// Substring matching is deliberately absent — it let a push to "gateway"
// select "gateway-v2" and reindex the wrong repository.
func (s *Service) FindRepoByHints(ctx context.Context, hints []string) (*repos.Repo, error) {
	all, err := s.storage.ListRepos(ctx)
	if err != nil {
		return nil, err
	}

	byURL := make(map[string]*repos.Repo, len(all))
	for _, r := range all {
		if r.URL != "" {
			byURL[normalizeRepoRef(r.URL)] = r
		}
	}
	for _, hint := range hints {
		if hint == "" {
			continue
		}
		if r, ok := byURL[normalizeRepoRef(hint)]; ok {
			return r, nil
		}
	}

	for _, hint := range hints {
		for _, r := range all {
			if r.Name != "" && r.Name == hint {
				return r, nil
			}
		}
	}

	// Last resort: "<owner>/<repo>" style hints, matched on the full final
	// path segment rather than on any substring of it.
	for _, hint := range hints {
		h := normalizeRepoRef(hint)
		idx := strings.LastIndex(h, "/")
		if idx < 0 {
			continue
		}
		tail := h[idx+1:]
		for _, r := range all {
			if r.Name != "" && strings.EqualFold(r.Name, tail) {
				return r, nil
			}
		}
	}

	return nil, storage.ErrNotFound
}

// RuntimeServiceEdge is one observed client->server link from tracing data.
type RuntimeServiceEdge struct {
	Client string
	Server string
	Calls  int64
}

// RuntimeIngestResult reports what an ingest actually did. A tracing backend
// names services its own way ("checkout-service", "checkout_service",
// "checkoutservice"), so some names will not match what service detection
// found — and an endpoint that answers "stored: 0" without saying which names
// it could not place is unusable: the caller has no way to tell a
// configuration mistake from an empty graph.
type RuntimeIngestResult struct {
	Received  int
	Stored    int
	Unmatched []string // service names with no detected service
	Known     []string // detected service names, when nothing matched
}

// IngestRuntimeServiceGraph replaces runtime_call edges with links observed in
// tracing data (e.g. a Jaeger/Tempo service graph). Endpoints are matched to
// detected service units by name, comparing word components so that the
// separator conventions of a tracing backend do not have to match the
// repository's.
//
// Nothing is deleted when nothing matched: a payload whose names are all
// unknown is a misconfiguration, and wiping a working runtime graph in
// response to it would turn a mistake into data loss.
func (s *Service) IngestRuntimeServiceGraph(ctx context.Context, edges []RuntimeServiceEdge) (*RuntimeIngestResult, error) {
	svcUnits, err := s.storage.GetASTUnits(ctx, storage.QueryOpts{Kind: storage.KindService})
	if err != nil {
		return nil, err
	}
	byName := map[string]*storage.ASTUnit{}
	known := make([]string, 0, len(svcUnits))
	for _, u := range svcUnits {
		byName[serviceNameKey(u.Name)] = u
		known = append(known, u.Name)
	}
	sort.Strings(known)

	result := &RuntimeIngestResult{Received: len(edges)}
	unmatched := map[string]bool{}
	matched := make([]RuntimeServiceEdge, 0, len(edges))
	for _, e := range edges {
		_, cok := byName[serviceNameKey(e.Client)]
		_, sok := byName[serviceNameKey(e.Server)]
		if !cok {
			unmatched[e.Client] = true
		}
		if !sok {
			unmatched[e.Server] = true
		}
		if cok && sok {
			matched = append(matched, e)
		}
	}
	for name := range unmatched {
		result.Unmatched = append(result.Unmatched, name)
	}
	sort.Strings(result.Unmatched)

	if len(matched) == 0 {
		result.Known = known
		return result, nil
	}

	if err := s.storage.DeleteEdgesByKind(ctx, "", storage.EdgeRuntimeCall); err != nil {
		return nil, err
	}
	stored := 0
	for _, e := range matched {
		client := byName[serviceNameKey(e.Client)]
		server := byName[serviceNameKey(e.Server)]
		edge := &storage.Edge{
			RepoID:     client.RepoID,
			SrcID:      client.ID,
			DstID:      server.ID,
			DstRepoID:  server.RepoID,
			Kind:       storage.EdgeRuntimeCall,
			DstName:    "runtime:" + e.Server,
			FilePath:   client.FilePath,
			Confidence: 1.0,
			Meta:       fmt.Sprintf(`{"calls":%d}`, e.Calls),
		}
		if err := s.storage.StoreEdge(ctx, edge); err != nil {
			return nil, err
		}
		stored++
	}
	result.Stored = stored
	return result, nil
}

// serviceNameKey normalizes a service name for matching: a tracing backend
// writes "checkout-service" where detection found "checkoutservice", and
// neither spelling is more correct than the other.
func serviceNameKey(name string) string {
	return strings.Join(graph.WordComponents(name), "")
}
