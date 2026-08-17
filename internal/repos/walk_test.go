package repos

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// writeTree lays out a throwaway repository from repo-relative slash paths and
// returns its root.
func writeTree(t *testing.T, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range files {
		writeFile(t, filepath.Join(root, filepath.FromSlash(name)), "x\n")
	}
	return root
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// walkPaths returns the sorted slash-separated paths WalkFiles reports.
func walkPaths(t *testing.T, root string, patterns []string) []string {
	t.Helper()
	files, err := WalkFiles(root, patterns)
	if err != nil {
		t.Fatalf("WalkFiles: %v", err)
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, filepath.ToSlash(f.Path))
	}
	slices.Sort(out)
	return out
}

func TestWalkFilesSelectsIndexableFiles(t *testing.T) {
	root := writeTree(t,
		"src/main.go",
		"src/app.ts",
		"README.md",
		"docs/notes.md",
		"assets/logo.png", // no language
		// Excluded by pattern, at the root and nested.
		"node_modules/left-pad/index.js",
		"web/node_modules/react/index.js",
		"web/node_modules/react/lib/deep.js",
		"web/node_modules/.bin/tsc.js",
		"dist/bundle.js",
		// Excluded because ".git" is a whole path component...
		".git/config",
		".git/hooks/setup.py",
		// ...while these three only contain the substring ".git", and were
		// excluded by accident until the walk stopped matching it as one.
		".github/workflows/ci.yml",
		".github/PULL_REQUEST_TEMPLATE.md",
		".gitlab-ci.yml",
	)

	want := []string{
		".github/PULL_REQUEST_TEMPLATE.md",
		".github/workflows/ci.yml",
		".gitlab-ci.yml",
		"README.md",
		"docs/notes.md",
		"src/app.ts",
		"src/main.go",
	}
	got := walkPaths(t, root, []string{"**/node_modules/**", "**/dist/**"})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("files = %q, want %q", got, want)
	}
}

// Only the patterns and the .git directory take files out of the walk: with no
// patterns the excluded trees above come back, which is what makes the test
// above an assertion about the patterns rather than about the tree.
func TestWalkFilesWithoutPatternsKeepsEverythingButGit(t *testing.T) {
	root := writeTree(t,
		"src/main.go",
		"web/node_modules/react/index.js",
		".git/hooks/setup.py",
	)

	want := []string{"src/main.go", "web/node_modules/react/index.js"}
	if got := walkPaths(t, root, nil); !reflect.DeepEqual(got, want) {
		t.Errorf("files = %q, want %q", got, want)
	}
}

// A checkout of a git worktree or a submodule has .git as a file, not a
// directory, so the skip may not assume it can prune.
func TestWalkFilesHandlesGitAsAFile(t *testing.T) {
	root := writeTree(t, "src/main.go")
	writeFile(t, filepath.Join(root, ".git"), "gitdir: /elsewhere/.git/worktrees/x\n")

	if got := walkPaths(t, root, nil); !reflect.DeepEqual(got, []string{"src/main.go"}) {
		t.Errorf("files = %q, want [src/main.go]", got)
	}
}

// The size is read from a separate stat now that the walk no longer stats every
// entry it sees; the field still has to arrive filled in.
func TestWalkFilesReportsLanguageAndSize(t *testing.T) {
	root := t.TempDir()
	const body = "package main\n"
	writeFile(t, filepath.Join(root, "main.go"), body)

	files, err := WalkFiles(root, nil)
	if err != nil {
		t.Fatalf("WalkFiles: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
	if files[0].Language != "go" {
		t.Errorf("language = %q, want go", files[0].Language)
	}
	if files[0].Size != int64(len(body)) {
		t.Errorf("size = %d, want %d", files[0].Size, len(body))
	}
}

// A pattern that happens to match the repository root may not prune it: the
// whole repository would then index as nothing. ShouldIgnore sees "." for the
// root, so "." is the pattern that isolates the case — it matches the root and
// no path below it.
func TestWalkFilesDoesNotPruneTheRoot(t *testing.T) {
	root := writeTree(t, "main.go", "src/main.go")

	want := []string{"main.go", "src/main.go"}
	if got := walkPaths(t, root, []string{"."}); !reflect.DeepEqual(got, want) {
		t.Errorf("files = %q, want %q", got, want)
	}
}
