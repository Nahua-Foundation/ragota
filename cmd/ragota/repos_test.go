package main

import (
	"context"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
)

// `repos list` is the answer to "which repositories does it know about, and
// which are active" — so every registered repository is on it, dormant ones
// included, each with the state that decides whether a search reaches it.
func TestReposListShowsEveryRepositoryAndItsState(t *testing.T) {
	ctx := context.Background()
	svc, _, found := sourceRun(t, "alpha", "beta", "gamma")
	if err := svc.SetActiveRepos(ctx, []string{found[0].ID, found[2].ID}); err != nil {
		t.Fatalf("SetActiveRepos() error = %v", err)
	}

	out := captureStdout(t, func() {
		if code := listRepos(ctx, svc); code != 0 {
			t.Errorf("listRepos() = %d, want 0", code)
		}
	})

	for _, r := range found {
		if !strings.Contains(out, r.Name) {
			t.Errorf("%q is missing from the listing:\n%s", r.Name, out)
		}
		if !strings.Contains(out, r.Path) {
			t.Errorf("the path of %q is missing from the listing:\n%s", r.Name, out)
		}
	}
	if !strings.Contains(out, "dormant") {
		t.Errorf("the repository outside the working set is not marked:\n%s", out)
	}
	if !strings.Contains(out, "3 repositories, 2 active") {
		t.Errorf("the listing does not total up:\n%s", out)
	}
	if n := strings.Count(out, "dormant"); n != 1 {
		t.Errorf("%d rows marked dormant, want the 1 that is:\n%s", n, out)
	}
}

// Activating and deactivating move one repository across the boundary and leave
// the rest of the set where it was. SetActiveRepos replaces the whole set, so
// this is the one place where getting the read-modify-write wrong would quietly
// drop the repositories nobody mentioned.
func TestReposActivateAndDeactivateKeepTheRest(t *testing.T) {
	ctx := context.Background()
	svc, _, found := sourceRun(t, "alpha", "beta", "gamma")
	if err := svc.SetActiveRepos(ctx, []string{found[0].ID, found[1].ID, found[2].ID}); err != nil {
		t.Fatalf("SetActiveRepos() error = %v", err)
	}

	captureStdout(t, func() {
		if code := setRepoActive(ctx, svc, "beta", false); code != 0 {
			t.Errorf("deactivate = %d, want 0", code)
		}
	})
	if got, want := activeNames(t, svc), []string{"alpha", "gamma"}; !slices.Equal(got, want) {
		t.Errorf("active after deactivating beta = %v, want %v", got, want)
	}

	// The repository is out of the way, not gone: it is still registered, and
	// naming it again brings it back.
	all, err := svc.ListRepos(ctx)
	if err != nil {
		t.Fatalf("ListRepos() error = %v", err)
	}
	if len(all) != 3 {
		t.Errorf("the store holds %d repositories after a deactivation, want 3", len(all))
	}

	captureStdout(t, func() {
		if code := setRepoActive(ctx, svc, "beta", true); code != 0 {
			t.Errorf("activate = %d, want 0", code)
		}
	})
	if got, want := activeNames(t, svc), []string{"alpha", "beta", "gamma"}; !slices.Equal(got, want) {
		t.Errorf("active after activating beta again = %v, want %v", got, want)
	}

	// Asking for the state it is already in is not an error and not a write.
	out := captureStdout(t, func() {
		if code := setRepoActive(ctx, svc, "beta", true); code != 0 {
			t.Errorf("activate of an active repository = %d, want 0", code)
		}
	})
	if !strings.Contains(out, "already") {
		t.Errorf("activating an active repository said %q, want it to say so", out)
	}
}

// A repository nobody registered is a failure with a message, not a silent
// no-op and not a change to the set.
func TestReposRejectsAnUnknownRepository(t *testing.T) {
	ctx := context.Background()
	svc, _, found := sourceRun(t, "alpha", "beta")
	if err := svc.SetActiveRepos(ctx, []string{found[0].ID}); err != nil {
		t.Fatalf("SetActiveRepos() error = %v", err)
	}
	before := activeNames(t, svc)

	if code := setRepoActive(ctx, svc, "no-such-project", false); code == 0 {
		t.Error("deactivating an unknown repository reported success")
	}
	if got := activeNames(t, svc); !slices.Equal(got, before) {
		t.Errorf("active = %v, want the untouched %v", got, before)
	}
}

// An empty index says so rather than printing a header with nothing under it.
func TestReposListWithNothingRegistered(t *testing.T) {
	svc := newService(t, t.TempDir())
	out := captureStdout(t, func() {
		if code := listRepos(context.Background(), svc); code != 0 {
			t.Errorf("listRepos() = %d, want 0", code)
		}
	})
	if !strings.Contains(out, "no repositories registered") {
		t.Errorf("an empty index prints %q", out)
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// wrote there. The pipe is drained by a goroutine because a listing longer than
// the kernel's buffer would otherwise block the writer inside fn forever.
func captureStdout(t *testing.T, fn func()) (captured string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	old := os.Stdout
	os.Stdout = w

	var (
		wg  sync.WaitGroup
		out strings.Builder
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&out, r)
	}()

	// Restored from a defer so that a t.Fatal inside fn does not leave the
	// rest of the suite writing into a pipe nobody reads.
	defer func() {
		os.Stdout = old
		_ = w.Close()
		wg.Wait()
		_ = r.Close()
		captured = out.String()
	}()
	fn()
	return ""
}
