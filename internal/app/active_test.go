package app

import (
	"context"
	"errors"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/index"
)

// activeRepoList builds the mock's repository table out of the two halves the
// scoping decision reads.
func activeRepoList(active, inactive []string) []*domain.Repo {
	out := make([]*domain.Repo, 0, len(active)+len(inactive))
	for _, id := range active {
		out = append(out, &domain.Repo{ID: id, Name: id, Active: true})
	}
	for _, id := range inactive {
		out = append(out, &domain.Repo{ID: id, Name: id})
	}
	return out
}

// TestSearchDefaultsToActiveRepos: a request that names no repository is
// answered from the working set alone. This is the daily case — an agent asking
// about one project used to be answered out of every project the database had
// ever seen.
func TestSearchDefaultsToActiveRepos(t *testing.T) {
	srch := &stubSearcher{}
	st := &mockStorage{repoList: activeRepoList([]string{"r1"}, []string{"r2", "r3"})}
	svc := serviceWithSearcher(srch, st)
	defer svc.Close(context.Background())

	if _, err := svc.Search(context.Background(), &index.SearchQuery{Query: "add"}, "keyword"); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got := srch.got.Repos; len(got) != 1 || got[0] != "r1" {
		t.Errorf("searcher saw repos %v, want only the active r1", got)
	}
}

// TestSearchExplicitReposReachInactive: naming a repository reaches it whether
// or not it is active. Without that, "inactive" would mean "deleted".
func TestSearchExplicitReposReachInactive(t *testing.T) {
	srch := &stubSearcher{}
	st := &mockStorage{repoList: activeRepoList([]string{"r1"}, []string{"r2"})}
	svc := serviceWithSearcher(srch, st)
	defer svc.Close(context.Background())

	q := &index.SearchQuery{Query: "add", Repos: []string{"r2"}}
	if _, err := svc.Search(context.Background(), q, "keyword"); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got := srch.got.Repos; len(got) != 1 || got[0] != "r2" {
		t.Errorf("searcher saw repos %v, want the requested r2", got)
	}
}

// TestSearchUnfilteredWhenEverythingIsActive pins the property the eval harness
// depends on: with nothing put away, the query goes out exactly as it did
// before the working set existed. Spelling the scope out as "all of them" would
// select the same documents but score them differently — a repository filter
// becomes another clause of the keyword query.
func TestSearchUnfilteredWhenEverythingIsActive(t *testing.T) {
	srch := &stubSearcher{}
	st := &mockStorage{repoList: activeRepoList([]string{"r1", "r2"}, nil)}
	svc := serviceWithSearcher(srch, st)
	defer svc.Close(context.Background())

	if _, err := svc.Search(context.Background(), &index.SearchQuery{Query: "add"}, "keyword"); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got := srch.got.Repos; len(got) != 0 {
		t.Errorf("searcher saw a repository filter %v, want none", got)
	}
}

// TestSearchWithNothingActive: an empty working set is answered with nothing,
// not with everything. A run whose source named no repository is about no
// repository.
func TestSearchWithNothingActive(t *testing.T) {
	srch := &stubSearcher{hits: hitsForFiles("a.go")}
	st := &mockStorage{repoList: activeRepoList(nil, []string{"r1", "r2"})}
	svc := serviceWithSearcher(srch, st)
	defer svc.Close(context.Background())

	res, err := svc.Search(context.Background(), &index.SearchQuery{Query: "add"}, "keyword")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(res.Hits) != 0 || res.Total != 0 {
		t.Errorf("Search returned %d hits with nothing active", len(res.Hits))
	}
	if res.Mode != "keyword" {
		t.Errorf("Mode = %q, want the resolved keyword", res.Mode)
	}
	if srch.got != nil {
		t.Error("the searcher ran although no repository was in scope")
	}
}

// TestSearchFailsWhenTheWorkingSetIsUnreadable: falling back to every
// repository would leak exactly what the working set keeps out of the answer.
func TestSearchFailsWhenTheWorkingSetIsUnreadable(t *testing.T) {
	srch := &stubSearcher{}
	st := &mockStorage{listReposErr: errors.New("database is gone")}
	svc := serviceWithSearcher(srch, st)
	defer svc.Close(context.Background())

	if _, err := svc.Search(context.Background(), &index.SearchQuery{Query: "add"}, "keyword"); err == nil {
		t.Fatal("Search() succeeded although the working set could not be read")
	}
	if srch.got != nil {
		t.Error("the searcher ran on an unresolved scope")
	}
}

// TestBuildContextDefaultsToActiveRepos: /context reaches retrieval through
// Search, so it is scoped by the same decision and in the same place.
func TestBuildContextDefaultsToActiveRepos(t *testing.T) {
	srch := &stubSearcher{}
	st := &mockStorage{repoList: activeRepoList([]string{"r1"}, []string{"r2"})}
	svc := serviceWithSearcher(srch, st)
	defer svc.Close(context.Background())

	if _, err := svc.BuildContext(context.Background(), "add", nil, "keyword", 3, 1, ""); err != nil {
		t.Fatalf("BuildContext() error = %v", err)
	}
	if got := srch.got.Repos; len(got) != 1 || got[0] != "r1" {
		t.Errorf("searcher saw repos %v, want only the active r1", got)
	}
}

// symbolStorage records the unit query Symbols composed, which is where the
// scope has to end up: /nav/symbol narrows in the query it sends, not by
// filtering an answer it has already read.
type symbolStorage struct {
	*mockStorage
	queries []domain.QueryOpts
	units   []*domain.ASTUnit
}

func (s *symbolStorage) GetASTUnits(_ context.Context, opts domain.QueryOpts) ([]*domain.ASTUnit, error) {
	s.queries = append(s.queries, opts)
	return s.units, nil
}

func symbolService(active, inactive []string) (*Service, *symbolStorage) {
	st := &symbolStorage{
		mockStorage: &mockStorage{repoList: activeRepoList(active, inactive)},
		units:       []*domain.ASTUnit{{ID: "u1", RepoID: "r1", Name: "Handle"}},
	}
	return &Service{store: st}, st
}

// theQuery returns the single unit query Symbols issued, failing when it issued
// any other number of them.
func theQuery(t *testing.T, st *symbolStorage) domain.QueryOpts {
	t.Helper()
	if len(st.queries) != 1 {
		t.Fatalf("Symbols issued %d unit queries, want exactly 1", len(st.queries))
	}
	return st.queries[0]
}

// TestSymbolsDefaultToActiveRepos is the leak this scoping closes: with one
// repository active, /nav/symbol answered a bare identifier out of every
// repository the database held — the same question /search would have answered
// from the working set alone, asked through the other door.
func TestSymbolsDefaultToActiveRepos(t *testing.T) {
	svc, st := symbolService([]string{"r1"}, []string{"r2", "r3"})

	if _, err := svc.Symbols(context.Background(), domain.QueryOpts{Name: "Handle"}); err != nil {
		t.Fatalf("Symbols() error = %v", err)
	}
	if got := theQuery(t, st).Repos; len(got) != 1 || got[0] != "r1" {
		t.Errorf("the unit query was scoped to %v, want only the active r1", got)
	}
}

// TestSymbolsExplicitRepoReachesInactive: naming a repository reaches it active
// or not, exactly as naming one in /search's `repos` does. Without that,
// "inactive" would mean "deleted" here too.
func TestSymbolsExplicitRepoReachesInactive(t *testing.T) {
	svc, st := symbolService([]string{"r1"}, []string{"r2"})

	units, err := svc.Symbols(context.Background(), domain.QueryOpts{RepoID: "r2", Name: "Handle"})
	if err != nil {
		t.Fatalf("Symbols() error = %v", err)
	}
	if len(units) != 1 {
		t.Errorf("Symbols returned %d units for a named dormant repository, want the store's answer", len(units))
	}
	q := theQuery(t, st)
	if q.RepoID != "r2" {
		t.Errorf("the unit query names repository %q, want the requested r2", q.RepoID)
	}
	// The name is the whole scope, so it is not repeated as a set: a second
	// clause saying what repo_id = already says buys nothing and changes the
	// statement.
	if len(q.Repos) != 0 {
		t.Errorf("the unit query also carries the set %v, want repo_id alone", q.Repos)
	}

	// And it is resolved without reading the working set at all — a named
	// repository already answers "which repositories may this read".
	svc, st = symbolService(nil, nil)
	st.listReposErr = errors.New("the working set must not be consulted")
	if _, err := svc.Symbols(context.Background(), domain.QueryOpts{RepoID: "r2", Name: "Handle"}); err != nil {
		t.Errorf("Symbols() error = %v; a named repository must not depend on the working set", err)
	}
}

// TestSymbolsUnfilteredWhenEverythingIsActive pins for /nav/symbol what
// TestSearchUnfilteredWhenEverythingIsActive pins for /search: with nothing put
// away the query goes out as it did before the working set existed. Spelling the
// scope out as "all of them" selects the same units, but through a repository
// filter the ranking and the query plan would then carry for no reason — and the
// eval harness registers its repositories through the API, where they are active
// by default, so this is the branch every measured run takes.
func TestSymbolsUnfilteredWhenEverythingIsActive(t *testing.T) {
	svc, st := symbolService([]string{"r1", "r2"}, nil)

	if _, err := svc.Symbols(context.Background(), domain.QueryOpts{Name: "Handle"}); err != nil {
		t.Fatalf("Symbols() error = %v", err)
	}
	q := theQuery(t, st)
	if len(q.Repos) != 0 || q.RepoID != "" {
		t.Errorf("the unit query carries a repository filter (repo_id=%q, repos=%v), want none", q.RepoID, q.Repos)
	}
}

// TestSymbolsWithNothingActive: an empty working set is answered with nothing,
// not with everything. Nothing in scope is not the same as no scope at all, and
// the store must not be asked a question whose answer would be the whole index.
func TestSymbolsWithNothingActive(t *testing.T) {
	svc, st := symbolService(nil, []string{"r1", "r2"})

	units, err := svc.Symbols(context.Background(), domain.QueryOpts{Name: "Handle"})
	if err != nil {
		t.Fatalf("Symbols() error = %v", err)
	}
	if len(units) != 0 {
		t.Errorf("Symbols returned %d units with nothing active", len(units))
	}
	if len(st.queries) != 0 {
		t.Errorf("the store was queried %d times although no repository was in scope", len(st.queries))
	}
}

// TestSymbolsFailWhenTheWorkingSetIsUnreadable: answering from every repository
// because the set could not be read would leak exactly what the set keeps out.
func TestSymbolsFailWhenTheWorkingSetIsUnreadable(t *testing.T) {
	svc, st := symbolService([]string{"r1"}, nil)
	st.listReposErr = errors.New("database is gone")

	if _, err := svc.Symbols(context.Background(), domain.QueryOpts{Name: "Handle"}); err == nil {
		t.Fatal("Symbols() succeeded although the working set could not be read")
	}
	if len(st.queries) != 0 {
		t.Error("the store was queried on an unresolved scope")
	}
}

// TestSetActiveRepos: the service hands the whole set to the store in one call,
// and reads it back through ActiveRepos.
func TestSetActiveRepos(t *testing.T) {
	st := &mockStorage{repoList: activeRepoList([]string{"r1", "r2"}, []string{"r3"})}
	svc := serviceWithSearcher(&stubSearcher{}, st)
	defer svc.Close(context.Background())
	ctx := context.Background()

	if err := svc.SetActiveRepos(ctx, []string{"r3"}); err != nil {
		t.Fatalf("SetActiveRepos: %v", err)
	}
	if len(st.activeSets) != 1 || len(st.activeSets[0]) != 1 || st.activeSets[0][0] != "r3" {
		t.Errorf("store was asked for %v, want one call naming r3", st.activeSets)
	}
	active, err := svc.ActiveRepos(ctx)
	if err != nil {
		t.Fatalf("ActiveRepos: %v", err)
	}
	if len(active) != 1 || active[0].ID != "r3" {
		t.Errorf("ActiveRepos = %v, want only r3", active)
	}
}
