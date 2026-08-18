package client

import (
	"context"
	"net/url"
)

// ListRepos returns every registered repository.
func (c *Client) ListRepos(ctx context.Context) ([]*Repo, error) {
	var out []*Repo
	if err := c.get(ctx, apiPath("repos"), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetRepo returns one repository. An unknown id is ErrNotFound.
func (c *Client) GetRepo(ctx context.Context, id string) (*Repo, error) {
	var out Repo
	if err := c.get(ctx, apiPath("repos", id), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AddRepo registers a repository. Requires a key with the admin scope;
// without one the call is ErrForbidden.
//
// The id is derived from name and path, so re-posting the same repository
// updates its definition and preserves its lifecycle state — status, the
// commit cursor, an in-progress indexing claim. The call is not retried
// automatically all the same; see postOnce.
func (c *Client) AddRepo(ctx context.Context, req *AddRepoRequest) (*Repo, error) {
	var out Repo
	if err := c.postOnce(ctx, apiPath("repos"), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Index starts a full index pass over a repository, or queues one. Requires a
// key with the admin scope. A nil request is the same as one with Force false.
//
// It returns as soon as the work is accepted, never when it is done: read
// IndexAck.Status to learn which of the two happened, then follow the
// repository with GetRepo (single instance) or the job with Job (distributed).
// A repository already indexing is ErrRepoBusy, with Error.RetryAfter set.
func (c *Client) Index(ctx context.Context, id string, req *IndexRequest) (*IndexAck, error) {
	if req == nil {
		req = &IndexRequest{}
	}
	var out IndexAck
	if err := c.postOnce(ctx, apiPath("repos", id, "index"), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Jobs lists a repository's queue entries, newest first. limit 0 takes the
// server's page size.
//
// This is where an accepted-but-not-started index pass becomes visible: Index
// answers the same way whether it began the work or only queued it.
func (c *Client) Jobs(ctx context.Context, repoID string, limit int) (*JobsResponse, error) {
	q := url.Values{}
	positiveInt(q, "limit", limit)
	var out JobsResponse
	if err := c.get(ctx, withQuery(apiPath("repos", repoID, "jobs"), q), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Job returns one queue entry. A job belonging to another repository is
// ErrNotFound rather than someone else's job.
func (c *Client) Job(ctx context.Context, repoID, jobID string) (*Job, error) {
	var out Job
	if err := c.get(ctx, apiPath("repos", repoID, "jobs", jobID), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SyncState returns a repository's commit cursor and indexing status — what a
// client pushing commits needs to decide what to send next.
func (c *Client) SyncState(ctx context.Context, repoID string) (*SyncStateResponse, error) {
	var out SyncStateResponse
	if err := c.get(ctx, apiPath("repos", repoID, "sync-state"), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Coverage reports how much of a repository's outbound contract surface the
// last full index pass resolved.
//
// Check Coverage.Reported before reading the counters, and check it before
// trusting an empty graph answer: a low ratio means the indexer does not
// understand how this project calls out, so "nothing found" is not "nothing
// there".
func (c *Client) Coverage(ctx context.Context, repoID string) (*Coverage, error) {
	var out Coverage
	if err := c.get(ctx, apiPath("repos", repoID, "coverage"), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Stats returns per-indexer document counts and index sizes.
func (c *Client) Stats(ctx context.Context) (*StatsResponse, error) {
	var out StatsResponse
	if err := c.get(ctx, apiPath("stats"), &out); err != nil {
		return nil, err
	}
	return &out, nil
}
