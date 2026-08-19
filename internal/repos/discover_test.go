package repos

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// mkRepo creates dir and marks it as a git checkout.
func mkRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDiscoverFindsRepositoriesUnderTheRoot(t *testing.T) {
	root := t.TempDir()
	want := []string{
		mkRepo(t, filepath.Join(root, "alpha")),
		mkRepo(t, filepath.Join(root, "beta")),
	}
	// A plain directory beside them contributes nothing on its own.
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := Discover(root, DefaultDiscoveryDepth)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("Discover() = %v, want %v", got, want)
	}
}

// A directory that is itself a repository must not need a wrapper: `--source
// ./my-project` is the single-project case and the common one.
func TestDiscoverAcceptsTheRootItself(t *testing.T) {
	root := mkRepo(t, t.TempDir())
	// A nested checkout (a submodule, a vendored clone) belongs to the outer
	// repository and must not become a second one.
	mkRepo(t, filepath.Join(root, "third_party", "lib"))

	got, err := Discover(root, DefaultDiscoveryDepth)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if !slices.Equal(got, []string{root}) {
		t.Errorf("Discover() = %v, want just the root %q", got, root)
	}
}

func TestDiscoverHonoursTheDepthBound(t *testing.T) {
	root := t.TempDir()
	shallow := mkRepo(t, filepath.Join(root, "org", "repo"))           // depth 2
	deep := mkRepo(t, filepath.Join(root, "a", "b", "c", "d", "repo")) // depth 5

	got, err := Discover(root, 2)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if !slices.Equal(got, []string{shallow}) {
		t.Errorf("Discover(depth 2) = %v, want just %q", got, shallow)
	}
	if slices.Contains(got, deep) {
		t.Errorf("Discover(depth 2) reached %q, which is 5 levels down", deep)
	}
}

// Dot directories are configuration and caches. Descending into .git alone
// would cost more than the whole rest of the scan.
func TestDiscoverSkipsDotDirectories(t *testing.T) {
	root := t.TempDir()
	mkRepo(t, filepath.Join(root, ".cache", "vendored"))
	visible := mkRepo(t, filepath.Join(root, "visible"))

	got, err := Discover(root, DefaultDiscoveryDepth)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if !slices.Equal(got, []string{visible}) {
		t.Errorf("Discover() = %v, want just %q", got, visible)
	}
}

// Nothing version-controlled anywhere below: the directory someone pointed the
// indexer at is taken to be the thing they meant.
func TestDiscoverFallsBackToTheRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Discover(root, DefaultDiscoveryDepth)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if !slices.Equal(got, []string{root}) {
		t.Errorf("Discover() = %v, want the root %q as the single repository", got, root)
	}
}

func TestDiscoverRejectsAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(file, DefaultDiscoveryDepth); err == nil {
		t.Error("Discover() of a file returned no error")
	}
	if _, err := Discover(filepath.Join(t.TempDir(), "missing"), DefaultDiscoveryDepth); err == nil {
		t.Error("Discover() of a missing path returned no error")
	}
}

// TestDiscoverOverTheBenchmarkCorpus runs the scan over the real tree the
// benchmarks use — twelve checkouts of production repositories under one
// directory, which is the layout --source exists for.
//
// It reads one directory and stats twelve entries: the scan stops at each
// repository root it recognizes, so it never descends into 2 GB of source. That
// is the property being checked as much as the count is.
func TestDiscoverOverTheBenchmarkCorpus(t *testing.T) {
	corpus := filepath.Join("..", "..", "_corpus")
	entries, err := os.ReadDir(corpus)
	if err != nil {
		t.Skipf("benchmark corpus not present (%v); run 'make corpus-clone'", err)
	}

	var expected []string
	for _, e := range entries {
		if e.IsDir() && e.Name()[0] != '.' && IsRepoRoot(filepath.Join(corpus, e.Name())) {
			abs, err := filepath.Abs(filepath.Join(corpus, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			expected = append(expected, abs)
		}
	}
	if len(expected) == 0 {
		t.Skip("benchmark corpus holds no checkouts")
	}
	slices.Sort(expected)

	got, err := Discover(corpus, DefaultDiscoveryDepth)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if !slices.Equal(got, expected) {
		t.Errorf("Discover() found %d repositories, want %d\ngot:  %v\nwant: %v",
			len(got), len(expected), got, expected)
	}
	// Every result is a repository root and none contains another: had the scan
	// carried on past one, a vendored checkout inside it would show up here.
	for _, p := range got {
		if !IsRepoRoot(p) {
			t.Errorf("%q is not a repository root", p)
		}
		for _, other := range got {
			if other != p && strings.HasPrefix(p, other+string(filepath.Separator)) {
				t.Errorf("%q sits inside %q; the scan descended past a repository", p, other)
			}
		}
	}
}
