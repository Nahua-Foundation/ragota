package service

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/repos"
)

// repoWithManifest returns a repo rooted at a temp dir, holding manifest as its
// .ragota.yaml when non-empty.
func repoWithManifest(t *testing.T, id, manifest string) *repos.Repo {
	t.Helper()
	dir := t.TempDir()
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(dir, ".ragota.yaml"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &repos.Repo{ID: id, Name: id, Path: dir}
}

func serviceIgnoring(patterns ...string) *Service {
	cfg := &config.Config{}
	cfg.Repos.Ignore = patterns
	return &Service{cfg: cfg}
}

func TestRepoManifestAddsToServerIgnores(t *testing.T) {
	s := serviceIgnoring("**/node_modules/**")
	repo := repoWithManifest(t, "shop", "ignore:\n  - \"**/generated/**\"\n")

	want := []string{"**/node_modules/**", "**/generated/**"}
	if got := s.IgnorePatternsFor(repo); !reflect.DeepEqual(got, want) {
		t.Errorf("patterns = %q, want %q", got, want)
	}
}

func TestNoManifestLeavesServerIgnores(t *testing.T) {
	s := serviceIgnoring("**/node_modules/**")
	repo := repoWithManifest(t, "shop", "")

	want := []string{"**/node_modules/**"}
	if got := s.IgnorePatternsFor(repo); !reflect.DeepEqual(got, want) {
		t.Errorf("patterns = %q, want %q", got, want)
	}
}

// The manifest is content of the indexed repository. It may narrow what is
// indexed and nothing else — an operator exclusion has to survive whatever the
// repository says, including a repository that tries to name it away.
func TestRepoManifestCannotUndoAServerIgnore(t *testing.T) {
	s := serviceIgnoring("**/node_modules/**")
	repo := repoWithManifest(t, "shop", `
ignore:
  - "!**/node_modules/**"
  - "**/node_modules"
`)

	ig := config.NewIgnorePatterns(s.IgnorePatternsFor(repo))
	if !ig.ShouldIgnore(repo.Path, filepath.Join(repo.Path, "web/node_modules/react/index.js")) {
		t.Error("repository re-enabled a path the server excluded")
	}
}

// A malformed manifest must keep the server's patterns in force. Dropping to
// none would index exactly the tree the operator excluded.
func TestMalformedManifestKeepsServerIgnores(t *testing.T) {
	s := serviceIgnoring("**/node_modules/**")
	repo := repoWithManifest(t, "shop", "ignore:\n  - \"**/x/**\"\n   bad: [\n")

	want := []string{"**/node_modules/**"}
	if got := s.IgnorePatternsFor(repo); !reflect.DeepEqual(got, want) {
		t.Errorf("patterns = %q, want %q", got, want)
	}
}

// One config slice serves every repository indexed, so appending to it in place
// would leak one repository's manifest into the next one's patterns.
func TestManifestDoesNotLeakBetweenRepos(t *testing.T) {
	s := serviceIgnoring("**/node_modules/**")
	shop := repoWithManifest(t, "shop", "ignore: [\"shop-only/**\"]\n")
	crm := repoWithManifest(t, "crm", "ignore: [\"crm-only/**\"]\n")

	gotShop := s.IgnorePatternsFor(shop)
	gotCRM := s.IgnorePatternsFor(crm)

	if want := []string{"**/node_modules/**", "shop-only/**"}; !reflect.DeepEqual(gotShop, want) {
		t.Errorf("shop = %q, want %q", gotShop, want)
	}
	if want := []string{"**/node_modules/**", "crm-only/**"}; !reflect.DeepEqual(gotCRM, want) {
		t.Errorf("crm = %q, want %q", gotCRM, want)
	}
	if want := []string{"**/node_modules/**"}; !reflect.DeepEqual(s.cfg.Repos.Ignore, want) {
		t.Errorf("config mutated: %q, want %q", s.cfg.Repos.Ignore, want)
	}
	// The first result must still read the same after the second call.
	if want := []string{"**/node_modules/**", "shop-only/**"}; !reflect.DeepEqual(gotShop, want) {
		t.Errorf("shop overwritten by crm: %q, want %q", gotShop, want)
	}
}

func TestNilConfigStillReadsTheManifest(t *testing.T) {
	s := &Service{}
	repo := repoWithManifest(t, "shop", "ignore: [\"**/generated/**\"]\n")

	if want := []string{"**/generated/**"}; !reflect.DeepEqual(s.IgnorePatternsFor(repo), want) {
		t.Errorf("patterns = %q, want %q", s.IgnorePatternsFor(repo), want)
	}
}
