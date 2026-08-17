package api

import (
	"github.com/Nahua-Foundation/ragota/internal/service"
)

// The request shapes that belong to the ingestion endpoints, which no client
// in this module speaks: commit push and the runtime service graph are things
// a CI job or a tracing exporter sends, not something a retrieval client
// reads. The contract everything else shares lives in wire.go.

// CommitFilePayload is one file change inside a pushed commit.
type CommitFilePayload struct {
	Path    string `json:"path"`
	OldPath string `json:"old_path,omitempty"`
	Status  string `json:"status"` // A | M | D | R
	Content string `json:"content,omitempty"`
}

// CommitPayload is one commit with its file changes.
type CommitPayload struct {
	SHA     string              `json:"sha"`
	Parents []string            `json:"parents,omitempty"`
	Files   []CommitFilePayload `json:"files"`
}

// CommitsRequest is a batch of commits pushed by an external client.
type CommitsRequest struct {
	Commits []CommitPayload `json:"commits"`
}

// OTelServiceGraphRequest carries a runtime service graph from tracing data.
type OTelServiceGraphRequest struct {
	Edges []service.RuntimeServiceEdge `json:"edges"`
}
