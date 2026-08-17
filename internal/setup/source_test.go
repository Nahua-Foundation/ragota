// The registration tests live in the external test package because they need
// a fully wired Service, which internal/testutil builds by calling setup.Build
// — a dependency the in-package tests cannot take without a cycle.
package setup_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/service"
	"github.com/Nahua-Foundation/ragota/internal/setup"
	"github.com/Nahua-Foundation/ragota/internal/testutil"
)

func TestApplySourceAddsToTheLocalPaths(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}

	abs, err := setup.ApplySource(cfg, dir)
	if err != nil {
		t.Fatalf("ApplySource() error = %v", err)
	}
	if abs != dir {
		t.Errorf("ApplySource() = %q, want %q", abs, dir)
	}
	if cfg.Repos.Sources.Local == nil || !cfg.Repos.Sources.Local.Enabled {
		t.Fatal("ApplySource() left the local source disabled")
	}
	if !slices.Equal(cfg.Repos.Sources.Local.Paths, []string{dir}) {
		t.Errorf("paths = %v, want [%s]", cfg.Repos.Sources.Local.Paths, dir)
	}
}

// --source composes with configuration rather than replacing it: a config file
// that already restricts the indexer to a set of roots keeps every one.
func TestApplySourceKeepsConfiguredPaths(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{Repos: config.ReposConfig{
		Sources: config.ReposSourcesConfig{
			Local: &config.LocalSourceConfig{Enabled: true, Paths: []string{"/srv/code"}},
		},
	}}

	if _, err := setup.ApplySource(cfg, dir); err != nil {
		t.Fatalf("ApplySource() error = %v", err)
	}
	if !slices.Equal(cfg.Repos.Sources.Local.Paths, []string{"/srv/code", dir}) {
		t.Errorf("paths = %v, want the configured root kept and %q added",
			cfg.Repos.Sources.Local.Paths, dir)
	}

	// Naming the same directory twice must not grow the list; the allowlist is
	// a set.
	if _, err := setup.ApplySource(cfg, dir); err != nil {
		t.Fatalf("ApplySource() error = %v", err)
	}
	if got := len(cfg.Repos.Sources.Local.Paths); got != 2 {
		t.Errorf("paths after a repeat = %d entries (%v), want 2",
			got, cfg.Repos.Sources.Local.Paths)
	}
}

// buildService wires a real Service over in-memory SQLite, with root as the
// local source's only allowlisted path — the state a --source run reaches
// before it registers anything.
func buildService(t *testing.T, root string) *service.Service {
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

// TestDiscoverAndRegisterIsIdempotent is the property that makes --source safe
// in a shell alias: the second run must find the same repositories already
// registered rather than adding a second copy of each.
func TestDiscoverAndRegisterIsIdempotent(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	svc := buildService(t, root)
	ctx := context.Background()

	first, err := setup.DiscoverAndRegister(ctx, svc, root, nil)
	if err != nil {
		t.Fatalf("DiscoverAndRegister() error = %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("registered %d repositories, want 2", len(first))
	}

	second, err := setup.DiscoverAndRegister(ctx, svc, root, nil)
	if err != nil {
		t.Fatalf("DiscoverAndRegister() second run error = %v", err)
	}

	firstIDs := ids(first)
	if !slices.Equal(ids(second), firstIDs) {
		t.Errorf("second run produced ids %v, want the same %v", ids(second), firstIDs)
	}

	stored, err := svc.ListRepos(ctx)
	if err != nil {
		t.Fatalf("ListRepos() error = %v", err)
	}
	if len(stored) != 2 {
		t.Errorf("after two runs the store holds %d repositories, want 2", len(stored))
	}
	if !slices.Equal(ids(stored), firstIDs) {
		t.Errorf("stored ids = %v, want %v", ids(stored), firstIDs)
	}
}

// A nil bus must be usable everywhere a real one is: the server runs without
// one, so a publish site that needs a guard is a publish site that will be
// missing one.
func TestDiscoverAndRegisterWithoutABusDoesNotPanic(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	svc := buildService(t, root)
	got, err := setup.DiscoverAndRegister(context.Background(), svc, root, nil)
	if err != nil {
		t.Fatalf("DiscoverAndRegister() error = %v", err)
	}
	// No .git anywhere, so the root itself is the repository.
	if len(got) != 1 || got[0].Path != root {
		t.Errorf("registered %v, want the root %q as one repository", got, root)
	}
}

// ActivateOnly is the half of --source that says which repositories the run is
// about. Over a real tree of a dozen projects, because that is where the
// difference between "the twenty in the database" and "the one I asked about"
// is the whole point: registering them all leaves them all active, and naming
// one narrows the set to it without unregistering the rest.
func TestActivateOnlyNarrowsTheWorkingSet(t *testing.T) {
	root := corpusRoot(t)
	svc := buildService(t, root)
	ctx := context.Background()

	found, err := setup.DiscoverAndRegister(ctx, svc, root, nil)
	if err != nil {
		t.Fatalf("DiscoverAndRegister() error = %v", err)
	}
	if len(found) < 2 {
		t.Fatalf("the corpus registered %d repositories, want several", len(found))
	}
	active, err := svc.ActiveRepos(ctx)
	if err != nil {
		t.Fatalf("ActiveRepos() error = %v", err)
	}
	if !slices.Equal(ids(active), ids(found)) {
		t.Errorf("after registration %d of %d repositories are active, want all of them",
			len(active), len(found))
	}

	if err := setup.ActivateOnly(ctx, svc, found[:1]); err != nil {
		t.Fatalf("ActivateOnly() error = %v", err)
	}
	active, err = svc.ActiveRepos(ctx)
	if err != nil {
		t.Fatalf("ActiveRepos() error = %v", err)
	}
	if !slices.Equal(ids(active), ids(found[:1])) {
		t.Errorf("active = %v, want only %v", ids(active), ids(found[:1]))
	}

	stored, err := svc.ListRepos(ctx)
	if err != nil {
		t.Fatalf("ListRepos() error = %v", err)
	}
	if len(stored) != len(found) {
		t.Errorf("the store holds %d repositories, want the %d that were registered",
			len(stored), len(found))
	}
	for _, r := range stored {
		if r.ID != found[0].ID && r.Active {
			t.Errorf("%s stayed active outside the working set", r.Name)
		}
	}
}

// corpusRoot is the benchmark corpus, or a skip. It is gitignored and cloned by
// hand (`make corpus-clone`), so a checkout without it is normal.
//
// Discovery is the only thing pointed at it. It stops at each repository root
// rather than descending into one, so this reads a dozen directory entries and
// a dozen .git markers — nothing here walks the fifteen gigabytes below them.
func corpusRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "_corpus"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Skipf("no benchmark corpus at %s (make corpus-clone)", root)
	}
	return root
}

func ids(list []*repos.Repo) []string {
	out := make([]string, 0, len(list))
	for _, r := range list {
		out = append(out, r.ID)
	}
	slices.Sort(out)
	return out
}
