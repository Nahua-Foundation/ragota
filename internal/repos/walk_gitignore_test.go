package repos

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/config"
)

// withGitignore sets repos.use_gitignore for one test and puts it back.
func withGitignore(t *testing.T, on bool) {
	t.Helper()
	prev := config.UseGitignore()
	config.SetUseGitignore(on)
	t.Cleanup(func() { config.SetUseGitignore(prev) })
}

// The walk leaves out what the checkout leaves out, with no configured
// patterns at all: this is the case that indexed a benchmark corpus of twelve
// foreign repositories because they sat in a gitignored directory.
func TestWalkFilesAppliesGitignore(t *testing.T) {
	withGitignore(t, true)
	root := writeTree(t,
		"src/main.go",
		"src/generated/api.go",
		"build/out.go",
		"web/node_modules/react/index.js",
		"notes.md",
		"vendor/lib/dep.go",
		"vendor/keep.go",
	)
	writeFile(t, filepath.Join(root, ".gitignore"), "build/\nnode_modules/\nvendor/\n!vendor/keep.go\n")
	writeFile(t, filepath.Join(root, "src", ".gitignore"), "generated/\n")

	want := []string{"notes.md", "src/main.go"}
	if got := walkPaths(t, root, nil); !reflect.DeepEqual(got, want) {
		t.Errorf("files = %q, want %q", got, want)
	}
}

// The switch is what a repository whose .gitignore hides something worth
// indexing has: with it off, the same tree indexes as it did before.
func TestWalkFilesGitignoreCanBeTurnedOff(t *testing.T) {
	withGitignore(t, false)
	root := writeTree(t, "src/main.go", "build/out.go")
	writeFile(t, filepath.Join(root, ".gitignore"), "build/\n")

	want := []string{"build/out.go", "src/main.go"}
	if got := walkPaths(t, root, nil); !reflect.DeepEqual(got, want) {
		t.Errorf("files = %q, want %q", got, want)
	}
}

// The configured patterns are the operator's word and they are final: a
// .gitignore may exclude more, never less. Both a negation and — see
// TestWalkFilesKeepsTrackedFiles — a file being tracked stop at that line.
func TestWalkFilesConfiguredPatternsOutrankGitignore(t *testing.T) {
	withGitignore(t, true)
	root := writeTree(t, "src/main.go", "web/node_modules/react/index.js")
	// The repository insists node_modules is fine. The server disagrees.
	writeFile(t, filepath.Join(root, ".gitignore"), "!node_modules/\n")

	want := []string{"src/main.go"}
	if got := walkPaths(t, root, []string{"**/node_modules/**"}); !reflect.DeepEqual(got, want) {
		t.Errorf("files = %q, want %q", got, want)
	}
}

// A file git has in its index is code the user can see in the checkout, so no
// .gitignore rule may take it out of the index — the walk has to descend into
// the excluded directory to reach it, and leave its untracked neighbours out.
func TestWalkFilesKeepsTrackedFiles(t *testing.T) {
	withGitignore(t, true)
	root := writeTree(t, "src/main.go", "vendor/keep.go", "vendor/junk.go")
	writeFile(t, filepath.Join(root, ".gitignore"), "vendor/\n")
	gitAdd(t, root, "vendor/keep.go")

	want := []string{"src/main.go", "vendor/keep.go"}
	if got := walkPaths(t, root, nil); !reflect.DeepEqual(got, want) {
		t.Errorf("files = %q, want %q", got, want)
	}
}

// A real repository, its real .gitignore, and the directory a Maven build
// leaves behind. The corpus checkouts are pristine — nothing has ever been
// built in them — so the generated tree has to be materialised for the case to
// exist at all; everything else about the tree is the repository's own.
func TestWalkFilesExcludesACorpusRepositorysOwnBuildOutput(t *testing.T) {
	withGitignore(t, true)
	root := copyCorpusRepo(t, "petclinic")

	// petclinic's .gitignore excludes "target/" (where Maven writes) and
	// "generated/", both unanchored, so they match at any depth.
	writeFile(t, filepath.Join(root, "spring-petclinic-customers-service", "target", "generated-sources", "Stub.java"), "class Stub {}\n")
	writeFile(t, filepath.Join(root, "generated", "Api.java"), "class Api {}\n")

	got := walkPaths(t, root, nil)
	for _, p := range got {
		if strings.Contains("/"+p, "/target/") || strings.HasPrefix(p, "generated/") {
			t.Fatalf("%q reached the walk; the repository's .gitignore excludes it", p)
		}
	}
	// Not a vacuous pass: the tree is really there, and the walk really found
	// the repository's sources.
	const real = "spring-petclinic-config-server/src/main/java/org/springframework/samples/petclinic/config/ConfigServerApplication.java"
	if !slices.Contains(got, real) {
		t.Fatalf("the repository's own sources are missing from the walk (%d files)", len(got))
	}
	withGitignore(t, false)
	if off := walkPaths(t, root, nil); len(off) != len(got)+2 {
		t.Errorf("with .gitignore off the walk found %d files, want %d — the two generated files", len(off), len(got)+2)
	}
}

// gitAdd makes a checkout out of root and puts paths in its index, skipping the
// test when there is no git to do it with. -f because the point of the exercise
// is a file the repository's own .gitignore matches.
func gitAdd(t *testing.T, root string, paths ...string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	env := append(os.Environ(), "HOME="+root, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	for _, args := range [][]string{{"init", "-q"}, append([]string{"add", "-f"}, paths...)} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// copyCorpusRepo copies one benchmark-corpus checkout into a temporary
// directory, or skips. The corpus is other people's source, cloned by hand
// (`make corpus-clone`) and never written to, so the test works on a copy —
// and only ever one repository of the twelve.
func copyCorpusRepo(t *testing.T, name string) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("..", "..", "_corpus", name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Skipf("no benchmark corpus at %s (make corpus-clone)", src)
	}
	dst := t.TempDir()
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Skipf("cannot copy %s: %v", src, err)
	}
	return dst
}
