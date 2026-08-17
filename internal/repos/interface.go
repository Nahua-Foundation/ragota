package repos

import (
	"context"
)

// SourceType represents the type of repository source.
type SourceType string

const (
	SourceTypeLocal SourceType = "local"
	SourceTypeGit   SourceType = "git"
)

// Repo represents a code repository.
type Repo struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Source    SourceType `json:"source"`
	URL       string     `json:"url,omitempty"` // For git sources
	Path      string     `json:"path"`          // Local path or cloned path
	Branch    string     `json:"branch,omitempty"`
	IndexedAt int64      `json:"indexed_at"` // Unix timestamp
	Status    Status     `json:"status"`
	CreatedAt int64      `json:"created_at"`
	LastError string     `json:"last_error,omitempty"`
	// LastCommit is the SHA of the last commit applied via the commit
	// ingestion API. Empty until an external client starts pushing commits.
	LastCommit string `json:"last_commit,omitempty"`
	// PendingCommit is the SHA a currently running commit batch is applying.
	// It lets a client tell "in flight" from "lost" while status is indexing.
	PendingCommit string `json:"pending_commit,omitempty"`
	// Active says whether the repository belongs to the working set the current
	// run is about — what a --source names, in the local-tool case. It is a
	// view, not a lifecycle state: an inactive repository keeps its index, its
	// edges and its place in the cross-repository graph, and pointing a run back
	// at it brings it back.
	//
	// The store owns the field. StoreRepo never writes it — a repository is
	// active when it is first registered and keeps whatever it had when it is
	// re-registered — and it changes only through storage.SetActiveRepos.
	Active bool `json:"active"`
}

// Status represents repository status.
type Status string

const (
	StatusIdle     Status = "idle"
	StatusIndexing Status = "indexing"
	StatusError    Status = "error"
)

// RepoSource is the interface for repository sources.
type RepoSource interface {
	// Name returns the source name.
	Name() string

	// Type returns the source type.
	Type() SourceType

	// Init initializes the source with configuration.
	Init(ctx context.Context, config map[string]interface{}) error

	// Add adds a repository and returns it.
	Add(ctx context.Context, req *AddRequest) (*Repo, error)

	// Remove removes a repository.
	Remove(ctx context.Context, repoID string) error

	// Update updates a repository (e.g., git pull).
	Update(ctx context.Context, repo *Repo) error

	// GetFiles returns files in a repository for indexing.
	GetFiles(ctx context.Context, repo *Repo, ignorePatterns []string) ([]*RepoFile, error)

	// Clean removes repository files from disk (if applicable).
	Clean(ctx context.Context, repo *Repo) error

	// Close closes the source and releases resources.
	Close() error
}

// AddRequest is a request to add a repository.
type AddRequest struct {
	Name   string
	URL    string // For git
	Path   string // For local
	Branch string // For git
}

// RepoFile represents a file in a repository.
type RepoFile struct {
	Path     string
	Hash     string
	Language string
	Size     int64
}
