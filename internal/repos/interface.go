package repos

import (
	"context"

	"github.com/Nahua-Foundation/ragota/internal/domain"
)

// RepoSource is the interface for repository sources.
type RepoSource interface {
	// Name returns the source name.
	Name() string

	// Type returns the source type.
	Type() domain.SourceType

	// Init initializes the source with configuration.
	Init(ctx context.Context, config map[string]interface{}) error

	// Add adds a repository and returns it.
	Add(ctx context.Context, req *domain.AddRequest) (*domain.Repo, error)

	// Remove removes a repository.
	Remove(ctx context.Context, repoID string) error

	// Update updates a repository (e.g., git pull).
	Update(ctx context.Context, repo *domain.Repo) error

	// GetFiles returns files in a repository for index.
	GetFiles(ctx context.Context, repo *domain.Repo, ignorePatterns []string) ([]*domain.RepoFile, error)

	// Clean removes repository files from disk (if applicable).
	Clean(ctx context.Context, repo *domain.Repo) error

	// Close closes the source and releases resources.
	Close() error
}
