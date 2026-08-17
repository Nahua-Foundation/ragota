package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/service"
	"github.com/Nahua-Foundation/ragota/internal/setup"
	"github.com/Nahua-Foundation/ragota/internal/status"
	"github.com/Nahua-Foundation/ragota/internal/testutil"
)

// The working set is what these tests are about: which repositories a run is
// answering from, which ones it watches, and which ones it shows. They drive a
// real Service over in-memory SQLite, because the set lives in the store and a
// stub agreeing with the caller would prove nothing about it.

// newService wires a real Service over in-memory SQLite, with root as the local
// source's only allowlisted path — the state a --source run reaches before it
// registers anything.
func newService(t *testing.T, root string) *service.Service {
	t.Helper()
	t.Setenv("RAGOTA_BM25_PATH", t.TempDir())

	cfg := testutil.TestConfig(t)
	if _, err := setup.ApplySource(cfg, root); err != nil {
		t.Fatalf("ApplySource() error = %v", err)
	}
	svc, err := setup.Build(context.Background(), cfg)
	if err != nil {
		t.Fatalf("setup.Build() error = %v", err)
	}
	t.Cleanup(func() { _ = svc.Close(context.Background()) })
	return svc
}

// sourceRun lays out named repositories under one root, registers them the way
// a --source run does, and hands back the wired service.
func sourceRun(t *testing.T, names ...string) (*service.Service, string, []*repos.Repo) {
	t.Helper()

	root := t.TempDir()
	for _, name := range names {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	svc := newService(t, root)
	found, err := setup.DiscoverAndRegister(context.Background(), svc, root, nil)
	if err != nil {
		t.Fatalf("DiscoverAndRegister() error = %v", err)
	}
	if len(found) != len(names) {
		t.Fatalf("registered %d repositories, want %d", len(found), len(names))
	}
	return svc, root, found
}

// activeNames is the working set as the store holds it, by name and sorted so
// that a failure reads as a set rather than as an order.
func activeNames(t *testing.T, svc *service.Service) []string {
	t.Helper()
	active, err := svc.ActiveRepos(context.Background())
	if err != nil {
		t.Fatalf("ActiveRepos() error = %v", err)
	}
	names := make([]string, 0, len(active))
	for _, r := range active {
		names = append(names, r.Name)
	}
	slices.Sort(names)
	return names
}

// The headline: a --source run is about the repositories under that source and
// no others. The rest stay registered — dormant is not deleted — so that
// naming them again is all it takes to get them back.
func TestSourceActivatesExactlyWhatItFound(t *testing.T) {
	ctx := context.Background()
	svc, root, found := sourceRun(t, "alpha", "beta", "gamma")

	one := found[:1]
	if err := activateSource(ctx, svc, filepath.Join(root, one[0].Name), one); err != nil {
		t.Fatalf("activateSource() error = %v", err)
	}
	if got, want := activeNames(t, svc), []string{one[0].Name}; !slices.Equal(got, want) {
		t.Errorf("active = %v, want %v", got, want)
	}

	all, err := svc.ListRepos(ctx)
	if err != nil {
		t.Fatalf("ListRepos() error = %v", err)
	}
	if len(all) != 3 {
		t.Errorf("the store holds %d repositories, want all 3 still registered", len(all))
	}

	// Naming them all again restores the set: this is a view, and nothing that
	// happened above is a thing to recover from.
	if err := activateSource(ctx, svc, root, found); err != nil {
		t.Fatalf("activateSource() error = %v", err)
	}
	if got, want := activeNames(t, svc), []string{"alpha", "beta", "gamma"}; !slices.Equal(got, want) {
		t.Errorf("active after naming them again = %v, want %v", got, want)
	}
}

// A plain `ragota --config config.yaml` must not silently redefine a
// working set the user chose. It was told nothing about what this run is about,
// and "everything the database has ever seen" is not an answer it may pick.
func TestNoSourceLeavesTheWorkingSetAlone(t *testing.T) {
	ctx := context.Background()
	svc, _, found := sourceRun(t, "alpha", "beta")

	if err := svc.SetActiveRepos(ctx, []string{found[0].ID}); err != nil {
		t.Fatalf("SetActiveRepos() error = %v", err)
	}
	before := activeNames(t, svc)

	if err := activateSource(ctx, svc, "", nil); err != nil {
		t.Fatalf("activateSource() error = %v", err)
	}
	if got := activeNames(t, svc); !slices.Equal(got, before) {
		t.Errorf("active after a run with no --source = %v, want the previous %v", got, before)
	}
}

// A source that matched nothing is a mistyped path far more often than a
// request for an index that answers nothing, and an emptied working set is the
// harder of the two to recover from.
func TestEmptyDiscoveryLeavesTheWorkingSetAlone(t *testing.T) {
	ctx := context.Background()
	svc, _, found := sourceRun(t, "alpha", "beta")

	if err := svc.SetActiveRepos(ctx, []string{found[1].ID}); err != nil {
		t.Fatalf("SetActiveRepos() error = %v", err)
	}
	before := activeNames(t, svc)

	if err := activateSource(ctx, svc, "/tmp/typo-in-this-path", nil); err != nil {
		t.Fatalf("activateSource() error = %v", err)
	}
	if got := activeNames(t, svc); !slices.Equal(got, before) {
		t.Errorf("active after a source that found nothing = %v, want the previous %v", got, before)
	}
}

// The watcher follows the working set. Beyond tidiness: on the kqueue
// platforms every watched directory costs a file descriptor, and a budget spent
// on repositories the run is not about is what makes the bound bite on the ones
// it is.
func TestStartWatcherFollowsOnlyTheActiveRepositories(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc, _, found := sourceRun(t, "alpha", "beta")
	if err := svc.SetActiveRepos(ctx, []string{found[0].ID}); err != nil {
		t.Fatalf("SetActiveRepos() error = %v", err)
	}

	bus := status.NewBus(16)
	svc.SetStatusBus(bus)
	var bg sync.WaitGroup
	stop := startWatcher(ctx, &bg, svc, bus)
	if stop == nil {
		t.Fatal("startWatcher() started no watcher over an active local repository")
	}
	defer func() {
		cancel()
		stop()
		bg.Wait()
	}()

	var watched []string
	for _, r := range bus.Snapshot().Repos {
		if r.Watched {
			watched = append(watched, r.ID)
		}
	}
	if want := []string{found[0].ID}; !slices.Equal(watched, want) {
		t.Errorf("watching %v, want only the active repository %v", watched, want)
	}
}

// A run whose working set is empty of local repositories starts no watcher at
// all, rather than one that follows the dormant ones for want of anything else.
func TestStartWatcherSkipsAnEmptyWorkingSet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc, _, _ := sourceRun(t, "alpha")
	if err := svc.SetActiveRepos(ctx, nil); err != nil {
		t.Fatalf("SetActiveRepos() error = %v", err)
	}

	var bg sync.WaitGroup
	if stop := startWatcher(ctx, &bg, svc, nil); stop != nil {
		stop()
		bg.Wait()
		t.Error("startWatcher() followed repositories outside the working set")
	}
}

// The dashboard is told about the working set and about how much of the index
// it is leaving out — as a count, because a row for a repository nothing will
// ever happen to reads as one waiting its turn.
func TestPrimeStatusBusPublishesTheWorkingSet(t *testing.T) {
	ctx := context.Background()
	svc, _, found := sourceRun(t, "alpha", "beta", "gamma")
	if err := svc.SetActiveRepos(ctx, []string{found[0].ID}); err != nil {
		t.Fatalf("SetActiveRepos() error = %v", err)
	}

	bus := status.NewBus(16)
	primeStatusBus(ctx, svc, bus)

	snap := bus.Snapshot()
	if len(snap.Repos) != 1 || snap.Repos[0].ID != found[0].ID {
		t.Errorf("published %+v, want only the active repository %q", snap.Repos, found[0].ID)
	}
	if snap.Dormant != 2 {
		t.Errorf("dormant = %d, want the 2 repositories that were left out", snap.Dormant)
	}
	if snap.Repos[0].Name == "" || snap.Repos[0].Path == "" {
		t.Errorf("published %+v, want the name and path the table renders", snap.Repos[0])
	}
}
