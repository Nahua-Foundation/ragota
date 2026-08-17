package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Nahua-Foundation/ragota/pkg/client"
)

// repoCacheTTL is how long a repository listing is reused.
//
// The list changes only when an operator adds or removes a repository, and the
// cost of being a minute stale is one confusing "unknown repository" error;
// the cost of not caching is a second round trip on every scoped call.
const repoCacheTTL = time.Minute

// repoIndex resolves what a caller wrote in a `repo` argument to a repository id.
//
// Ids are `name-<12 hex>` and derived from name and path, so a model cannot
// produce one from the question it was asked — it either carries one back from
// an earlier answer or writes the human name. ragota filters on ids and
// answers an unknown one with zero hits rather than an error, which is the worst
// possible shape: a scoping typo becomes "there is no such code". Resolving here
// turns it into a message that names the repositories that do exist.
type repoIndex struct {
	c *client.Client

	mu      sync.Mutex
	repos   []*client.Repo
	fetched time.Time
}

func newRepoIndex(c *client.Client) *repoIndex { return &repoIndex{c: c} }

// list returns the repositories, refreshing a stale cache.
func (r *repoIndex) list(ctx context.Context) ([]*client.Repo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.repos != nil && time.Since(r.fetched) < repoCacheTTL {
		return r.repos, nil
	}
	repos, err := r.c.ListRepos(ctx)
	if err != nil {
		return nil, err
	}
	r.repos, r.fetched = repos, time.Now()
	return repos, nil
}

// invalidate drops the cache, so that the next lookup of a name the cache does
// not know asks the server before declaring it unknown.
func (r *repoIndex) invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.repos = nil
}

// resolve maps one id, name or id prefix to a repository id.
func (r *repoIndex) resolve(ctx context.Context, want string) (string, error) {
	want = strings.TrimSpace(want)
	if want == "" {
		return "", nil
	}

	id, err := r.match(ctx, want)
	if err == nil || !isUnknownRepo(err) {
		return id, err
	}
	// A repository added since the cache was filled looks exactly like a typo.
	// Pay one listing to tell them apart before refusing.
	r.invalidate()
	return r.match(ctx, want)
}

// resolveAll maps a whole scope, and reports every unknown name at once so that
// a caller fixing a list does not discover its entries one call at a time.
func (r *repoIndex) resolveAll(ctx context.Context, want []string) ([]string, error) {
	if len(want) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(want))
	var problems []string
	for _, w := range want {
		id, err := r.resolve(ctx, w)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if id != "" {
			out = append(out, id)
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return out, nil
}

type unknownRepoError struct{ msg string }

func (e *unknownRepoError) Error() string { return e.msg }

func isUnknownRepo(err error) bool {
	_, ok := err.(*unknownRepoError)
	return ok
}

func (r *repoIndex) match(ctx context.Context, want string) (string, error) {
	repos, err := r.list(ctx)
	if err != nil {
		return "", fmt.Errorf("could not list repositories to resolve %q: %w", want, err)
	}

	var byName, byPrefix []string
	for _, repo := range repos {
		if repo.ID == want {
			return repo.ID, nil
		}
		if strings.EqualFold(repo.Name, want) {
			byName = append(byName, repo.ID)
		}
		if strings.HasPrefix(repo.ID, want) {
			byPrefix = append(byPrefix, repo.ID)
		}
	}

	for _, candidates := range [][]string{byName, byPrefix} {
		switch len(candidates) {
		case 0:
			continue
		case 1:
			return candidates[0], nil
		default:
			return "", fmt.Errorf("%q names %d repositories (%s); use the full id",
				want, len(candidates), strings.Join(candidates, ", "))
		}
	}
	return "", &unknownRepoError{fmt.Sprintf("unknown repository %q; known repositories are %s", want, known(repos))}
}

func known(repos []*client.Repo) string {
	if len(repos) == 0 {
		return "none — nothing is registered with ragota"
	}
	ids := make([]string, 0, len(repos))
	for _, r := range repos {
		ids = append(ids, r.ID)
	}
	return strings.Join(ids, ", ")
}
