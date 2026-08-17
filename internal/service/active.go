package service

import (
	"context"
	"fmt"

	"github.com/Nahua-Foundation/ragota/internal/repos"
)

// The active set is which repositories the current run is about. Repositories
// persist in the database and nothing unregisters them, so a run pointed at one
// project used to be answered out of every project the database had ever seen.
// The set is a view and nothing more: an inactive repository keeps its files,
// its units and its edges, it is still indexed and still linked, and pointing a
// run back at it makes it visible again.

// SetActiveRepos makes exactly the named repositories active and every other
// registered repository inactive. The switch is atomic: there is no moment at
// which a reader sees a working set nobody asked for.
//
// Ids naming no repository are ignored, so a caller may hand over what its
// source found without first checking what is registered. An empty list is a
// valid request and leaves nothing active — a run whose source named no
// repository is about no repository, and unscoped retrieval then answers with
// nothing rather than with everything.
func (s *Service) SetActiveRepos(ctx context.Context, ids []string) error {
	return s.storage.SetActiveRepos(ctx, ids)
}

// ActiveRepos returns the repositories in the active set, in the order
// ListRepos uses.
func (s *Service) ActiveRepos(ctx context.Context) ([]*repos.Repo, error) {
	return s.storage.ListActiveRepos(ctx)
}

// retrievalScope resolves which repositories one retrieval request may read.
// It is the single place the active set narrows anything: /search and /context
// arrive here through Search and /nav/symbol through Symbols, while the graph
// endpoints, the linker and indexing deliberately do not — the cross-repository
// graph is the point of the system and a convenience flag must not switch it
// off.
//
// /nav/symbol belongs with the searches rather than with the graph because it
// is the same question asked by a caller holding a different thing: prose goes
// to /search and an identifier here, and both endpoints' descriptions say so to
// the model choosing between them. Two doors to one question cannot answer from
// two different sets of repositories. /nav/definition and /nav/references are
// not scoped and need no exemption: both are addressed by a repository id and a
// file path, which their request schemas require, so the caller has already
// said which repository answers and the active set decides nothing.
//
// A request that names repositories gets exactly those, active or not. Naming
// one is how a client reaches a repository that is out of the way, and without
// that "inactive" would be indistinguishable from deleted.
//
// A request that names none is limited to the active set: an agent asking about
// one project must not be answered from the nineteen others still in the
// database.
//
// When every registered repository is active the scope is left empty rather
// than spelled out as the list of all of them. The searchers read an empty list
// as "everywhere", so the same documents are eligible either way — but not by
// the same query: a repository filter turns the keyword query into a
// conjunction whose score is the sum of its clauses, and an installation that
// never touched the active set would see its ranking move for no reason.
//
// none says nothing is in scope, which happens when the active set is empty
// while repositories exist. That is not the same as an empty scope and must not
// fall back to searching everywhere.
func (s *Service) retrievalScope(ctx context.Context, requested []string) (ids []string, none bool, err error) {
	if len(requested) > 0 {
		return requested, false, nil
	}
	all, err := s.storage.ListRepos(ctx)
	if err != nil {
		// Answering from every repository because the working set could not be
		// read would leak exactly what the set exists to keep out of the answer.
		return nil, false, fmt.Errorf("resolve the active repositories: %w", err)
	}
	active := make([]string, 0, len(all))
	for _, r := range all {
		if r.Active {
			active = append(active, r.ID)
		}
	}
	if len(active) == len(all) {
		return nil, false, nil
	}
	return active, len(active) == 0, nil
}
