package api

import (
	"github.com/Nahua-Foundation/ragota/internal/service"
)

// The wire shapes that belong to the ingestion endpoints, which no client in
// this module speaks: commit push and the runtime service graph are things a
// CI job or a tracing exporter sends, not something a retrieval client reads.
// The contract everything else shares lives in wire.go. Responses are defined
// here too — serializing the service layer's own structs would make internal
// field names the HTTP contract (see dto.go).

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

// RuntimeServiceEdgePayload is one observed client->server link in a pushed
// runtime service graph.
type RuntimeServiceEdgePayload struct {
	Client string `json:"client"`
	Server string `json:"server"`
	Calls  int64  `json:"calls,omitempty"`
}

// OTelServiceGraphRequest carries a runtime service graph from tracing data.
type OTelServiceGraphRequest struct {
	Edges []RuntimeServiceEdgePayload `json:"edges"`
}

// CommitAckPayload is the 202 body of a commit push.
type CommitAckPayload struct {
	Status string `json:"status"` // indexing | queued
	Queued bool   `json:"queued"` // true when the batch is only enqueued
	JobID  string `json:"job_id,omitempty"`
	// PendingCommit is the SHA the batch advances the cursor to;
	// LastCommitBefore is the cursor before this batch.
	PendingCommit    string `json:"pending_commit"`
	LastCommitBefore string `json:"last_commit_before"`
}

func commitAckPayload(a *service.CommitAck) CommitAckPayload {
	return CommitAckPayload{
		Status:           a.Status,
		Queued:           a.Queued,
		JobID:            a.JobID,
		PendingCommit:    a.Target,
		LastCommitBefore: a.Before,
	}
}

// IngestResultPayload reports what a runtime-graph ingest actually did.
type IngestResultPayload struct {
	Received  int      `json:"received"`
	Stored    int      `json:"stored"`
	Unmatched []string `json:"unmatched,omitempty"` // service names with no detected service
	Known     []string `json:"known,omitempty"`     // detected service names, when nothing matched
}

// StatusPayload is the small acknowledgement body of /ready and the webhook
// reindex: a status word, and the repository it is about when there is one.
type StatusPayload struct {
	Status string `json:"status"`
	RepoID string `json:"repo_id,omitempty"`
}

// CompactPayload reports per-index compaction time in milliseconds.
type CompactPayload struct {
	CompactedMS map[string]int64 `json:"compacted_ms"`
}
