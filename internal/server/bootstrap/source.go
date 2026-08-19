package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/Nahua-Foundation/ragota/internal/app"
	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/server/progress"
)

// ApplySource folds a --source directory into the configuration and returns
// the absolute path it resolved to.
//
// It is an addition to repos.sources.local.paths, not a replacement: a config
// file that already restricts the indexer to a set of roots keeps every one of
// them, and --source says "this one as well". Going through the config rather
// than around it is what makes the rest of the system unable to tell the
// difference — the allowlist that guards the API, the ignore patterns, the
// per-repository .ragota.yaml and every indexer treat a repository found this
// way exactly as they treat a configured one.
//
// Note the one visible consequence: local.paths doubles as the allowlist for
// paths an API client may add, and an empty list means "anything". A run with
// --source therefore has an allowlist where a bare config file had none.
func ApplySource(cfg *config.Config, dir string) (string, error) {
	if dir == "" {
		return "", errors.New("source directory is empty")
	}
	abs, err := filepath.Abs(config.ExpandPath(dir))
	if err != nil {
		return "", fmt.Errorf("resolve --source: %w", err)
	}
	if cfg.Repos.Sources.Local == nil {
		cfg.Repos.Sources.Local = &config.LocalSourceConfig{}
	}
	cfg.Repos.Sources.Local.Enabled = true
	for _, p := range cfg.Repos.Sources.Local.Paths {
		if existing, err := filepath.Abs(config.ExpandPath(p)); err == nil && existing == abs {
			return abs, nil
		}
	}
	cfg.Repos.Sources.Local.Paths = append(cfg.Repos.Sources.Local.Paths, abs)
	return abs, nil
}

// DiscoverAndRegister finds the repositories under root and registers each one
// with the service, returning those it registered.
//
// Registration is idempotent because AddRepo is: a repository's id is derived
// from its name and path, and re-registering an existing one preserves its
// lifecycle state instead of resetting it. Running the same command twice
// therefore adds nothing and disturbs nothing, which is what makes --source
// safe to put in a shell alias.
//
// One repository failing to register — a path outside the allowlist, a
// directory that vanished between the scan and the call — is logged and
// skipped. The user asked for the directory, not for each repository in it.
func DiscoverAndRegister(ctx context.Context, svc *app.Service, root string, bus *progress.Bus) ([]*domain.Repo, error) {
	paths, err := repos.Discover(root, repos.DefaultDiscoveryDepth)
	if err != nil {
		return nil, err
	}
	slog.Info("source scanned", "root", root, "repositories", len(paths), "max_depth", repos.DefaultDiscoveryDepth)

	registered := make([]*domain.Repo, 0, len(paths))
	for _, path := range paths {
		repo, err := svc.AddRepo(ctx, domain.SourceTypeLocal, &domain.AddRequest{Path: path})
		if err != nil {
			slog.Warn("source: cannot register repository", "path", path, "err", err)
			continue
		}
		bus.RepoRegistered(repo.ID, repo.Name, repo.Path)
		bus.IndexQueued(repo.ID)
		registered = append(registered, repo)
	}
	return registered, nil
}

// ActivateOnly makes exactly the given repositories the working set: every one
// of them active, and every other registered repository dormant.
//
// This is the half of --source that answers "which repositories is this run
// about". Registration alone cannot: repositories persist and re-registering
// one deliberately leaves its membership alone, so a source pointed at one
// project used to leave the twenty from an earlier run in the index's answers.
// Nothing here deletes or unindexes anything — a dormant repository keeps its
// files, its units and its edges, and naming it again brings it back.
//
// An empty list is passed through as an empty working set, because that is what
// store.SetActiveRepos means by it. Whether a discovery that found nothing
// should be read that way is the caller's decision, and cmd/ragota makes it:
// a source matching nothing is a mistyped path far more often than a request
// for an index that answers nothing.
func ActivateOnly(ctx context.Context, svc *app.Service, list []*domain.Repo) error {
	ids := make([]string, 0, len(list))
	for _, r := range list {
		ids = append(ids, r.ID)
	}
	if err := svc.SetActiveRepos(ctx, ids); err != nil {
		return fmt.Errorf("set the active repositories: %w", err)
	}
	slog.Info("working set", "active", len(ids))
	return nil
}

// IndexAll runs a full index pass over each repository in turn and blocks
// until they are done or ctx is cancelled. Progress reaches the status bus
// from inside the pass, which is the only layer that knows it.
//
// One at a time, deliberately. A pass holds a window of file contents in
// memory while its indexers work through it, so twelve concurrent passes cost
// twelve windows to finish no sooner than twelve sequential ones would — the
// indexers inside a single pass already run concurrently, which is where the
// parallelism that helps lives.
//
// A repository that fails is logged and the next one starts: a batch load
// stopping at the first unreadable tree leaves the user with an index of
// whatever happened to sort first.
func IndexAll(ctx context.Context, svc *app.Service, list []*domain.Repo) {
	for _, repo := range list {
		if ctx.Err() != nil {
			return
		}
		if err := svc.IndexRepoSync(ctx, repo.ID, false); err != nil {
			// Shutdown cancels the pass in flight, and reporting that as a
			// failure per repository is how quitting produced a screenful of
			// warnings about a server working exactly as asked. The loop stops
			// here rather than announcing the same cancellation once for every
			// repository still queued.
			if errors.Is(err, context.Canceled) {
				return
			}
			slog.Warn("source: indexing failed", "repo_id", repo.ID, "path", repo.Path, "err", err)
		}
	}
}
