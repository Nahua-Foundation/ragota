package local

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/repos"
)

// Source is a local file system repository source.
type Source struct {
	paths []string
}

// New creates a new local source.
func New() *Source {
	return &Source{}
}

// Name returns the source name.
func (s *Source) Name() string {
	return "local"
}

// Type returns the source type.
func (s *Source) Type() repos.SourceType {
	return repos.SourceTypeLocal
}

// Init initializes the local source.
func (s *Source) Init(ctx context.Context, config map[string]interface{}) error {
	paths, ok := config["paths"].([]string)
	if !ok {
		paths = []string{}
	}
	s.paths = paths
	return nil
}

// Add adds a local repository.
func (s *Source) Add(ctx context.Context, req *repos.AddRequest) (*repos.Repo, error) {
	if req.Path == "" {
		return nil, fmt.Errorf("path is required for local source")
	}

	// Validate path exists
	path, err := filepath.Abs(req.Path)
	if err != nil {
		return nil, fmt.Errorf("resolve path: %w", err)
	}

	// Enforce the configured allowlist. When repos.sources.local.paths is set,
	// only directories at or under one of those roots may be indexed — without
	// this check any API caller could point the indexer at an arbitrary host
	// directory and read its contents back through search. An empty allowlist
	// keeps the previous unrestricted behaviour.
	if !s.pathAllowed(path) {
		return nil, fmt.Errorf("path %s is outside the configured local.paths allowlist", path)
	}

	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("path does not exist: %w", err)
	}

	// Use directory name as repo name if not provided
	name := req.Name
	if name == "" {
		name = filepath.Base(path)
	}

	return &repos.Repo{
		ID:     repos.GenerateID(name, path),
		Name:   name,
		Source: repos.SourceTypeLocal,
		Path:   path,
		Status: repos.StatusIdle,
	}, nil
}

// pathAllowed reports whether abs (an absolute, cleaned path) is at or under
// one of the configured allowlist roots. An empty allowlist allows everything,
// preserving behaviour for deployments that never set local.paths.
func (s *Source) pathAllowed(abs string) bool {
	if len(s.paths) == 0 {
		return true
	}
	for _, root := range s.paths {
		root, err := filepath.Abs(config.ExpandPath(root))
		if err != nil {
			continue
		}
		if abs == root {
			return true
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			continue
		}
		// Inside root iff the relative path neither escapes upward nor is the
		// bare root; ".." as a component means abs sits above or beside root.
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Remove removes a repository.
// For local source, we don't delete files, just stop tracking.
func (s *Source) Remove(ctx context.Context, repoID string) error {
	// Nothing to clean up for local source
	return nil
}

// Update updates a repository.
// For local source, this is a no-op (files are already there).
func (s *Source) Update(ctx context.Context, repo *repos.Repo) error {
	// Local source doesn't need updates
	return nil
}

// GetFiles returns files in a repository for indexing. The walk itself is
// shared with the git source, which also means a local checkout's .git
// directory is skipped rather than descended into.
func (s *Source) GetFiles(ctx context.Context, repo *repos.Repo, ignorePatterns []string) ([]*repos.RepoFile, error) {
	return repos.WalkFiles(repo.Path, ignorePatterns)
}

// Clean removes repository files from disk.
// For local source, this is a no-op (we don't delete user files).
func (s *Source) Clean(ctx context.Context, repo *repos.Repo) error {
	return nil
}

// Close closes the source.
func (s *Source) Close() error {
	return nil
}
