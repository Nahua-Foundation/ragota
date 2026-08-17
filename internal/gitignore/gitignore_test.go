package gitignore

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// tree lays out a checkout from slash-separated paths: a value is a file's
// content, and a path ending in "/" is an empty directory.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

type want struct {
	path  string
	isDir bool
	want  bool
}

func check(t *testing.T, root string, cases []want) {
	t.Helper()
	r := New(root)
	for _, c := range cases {
		kind := "file"
		if c.isDir {
			kind = "dir"
		}
		if got := r.Ignored(c.path, c.isDir); got != c.want {
			t.Errorf("Ignored(%q, %s) = %v, want %v", c.path, kind, got, c.want)
		}
	}
}

// A pattern with a slash at its start or middle is fixed to the directory of
// the .gitignore that holds it; one without matches at any depth below it.
func TestAnchoring(t *testing.T) {
	root := tree(t, map[string]string{
		".gitignore": "/root-only\nanywhere\ndir/inside\n",
	})
	check(t, root, []want{
		{path: "root-only", want: true},
		{path: "sub/root-only", want: false},
		{path: "anywhere", want: true},
		{path: "sub/deep/anywhere", want: true},
		{path: "dir/inside", want: true},
		{path: "sub/dir/inside", want: false},
		// A prefix of a pattern is not the pattern: "anywhere" must not take
		// "anywhere_else" with it.
		{path: "anywhere_else", want: false},
	})
}

func TestDirectoryOnlyPatterns(t *testing.T) {
	root := tree(t, map[string]string{
		".gitignore": "build/\n",
	})
	check(t, root, []want{
		{path: "build", isDir: true, want: true},
		{path: "build", want: false}, // a *file* called build is not a build directory
		{path: "build/main.go", want: true},
		{path: "sub/build", isDir: true, want: true},
		{path: "sub/build/main.go", want: true},
	})
}

// The three positions ** can appear in, each with a different meaning.
func TestDoubleStar(t *testing.T) {
	root := tree(t, map[string]string{
		".gitignore": "**/logs\nout/**\nsrc/**/gen.go\n",
	})
	check(t, root, []want{
		{path: "logs", isDir: true, want: true},
		{path: "a/b/logs", isDir: true, want: true},
		// "out/**" is everything inside out, not out itself — the distinction
		// keeps a "!out/keep" negation reachable.
		{path: "out", isDir: true, want: false},
		{path: "out/a.go", want: true},
		{path: "out/deep/a.go", want: true},
		{path: "src/gen.go", want: true},
		{path: "src/a/b/gen.go", want: true},
		{path: "other/gen.go", want: false},
	})
}

// Later beats earlier, in both directions, and within one file.
func TestNegationLastMatchWins(t *testing.T) {
	root := tree(t, map[string]string{
		".gitignore": "*.log\n!keep.log\ndebug/*.log\n",
	})
	check(t, root, []want{
		{path: "a.log", want: true},
		{path: "keep.log", want: false},
		{path: "sub/keep.log", want: false},
		{path: "debug/keep.log", want: true}, // the later line takes it back
	})
}

// A .gitignore applies to its own directory and below, and outranks the ones
// above it — including when what it says is "not this one".
func TestNestedFileOverridesParent(t *testing.T) {
	root := tree(t, map[string]string{
		".gitignore":       "*.gen.go\n",
		"api/.gitignore":   "!*.gen.go\nprivate/\n",
		"api/v1/notes.txt": "x",
	})
	check(t, root, []want{
		{path: "top.gen.go", want: true},
		{path: "api/thing.gen.go", want: false},
		{path: "api/v1/thing.gen.go", want: false},
		{path: "other/thing.gen.go", want: true},
		// A nested file governs its own subtree, not the tree above it.
		{path: "api/private", isDir: true, want: true},
		{path: "private", isDir: true, want: false},
	})
}

// git's own exception: a negation cannot re-include a file whose parent
// directory is excluded, because git never looks inside such a directory.
func TestNegationCannotEscapeAnExcludedDirectory(t *testing.T) {
	root := tree(t, map[string]string{
		".gitignore":               "node_modules/\n!node_modules/keep.js\n",
		"nested/.gitignore":        "vendor/\n",
		"nested/vendor/.gitignore": "!important.go\n",
	})
	check(t, root, []want{
		{path: "node_modules", isDir: true, want: true},
		{path: "node_modules/keep.js", want: true},
		{path: "nested/vendor/important.go", want: true},
	})
}

func TestCommentsEscapesAndBlanks(t *testing.T) {
	root := tree(t, map[string]string{
		// "\#" is a file that starts with #, "\!" one that starts with !, and
		// the trailing spaces after "spaced" are not part of the pattern
		// unless a backslash holds them there.
		".gitignore": "# a comment\n\n\\#hash\n\\!bang\nspaced   \ntrailing\\ \n",
	})
	check(t, root, []want{
		{path: "#hash", want: true},
		{path: "!bang", want: true},
		{path: "spaced", want: true},
		{path: "trailing ", want: true},
		{path: "a comment", want: false},
	})
}

// .git/info/exclude is per-checkout and belongs to the same mechanism, and it
// ranks below every .gitignore in the tree.
func TestInfoExclude(t *testing.T) {
	root := tree(t, map[string]string{
		".gitignore":        "*.tmp\n",
		".git/info/exclude": "scratch/\n*.bak\n",
		".git/HEAD":         "ref: refs/heads/main\n",
		"sub/.gitignore":    "!keep.bak\n",
	})
	check(t, root, []want{
		{path: "scratch", isDir: true, want: true},
		{path: "a.bak", want: true},
		{path: "a.tmp", want: true},
		// A .gitignore outranks info/exclude, wherever in the tree it sits.
		{path: "sub/keep.bak", want: false},
	})
}

// A worktree's .git is a file pointing at a per-worktree directory, whose
// info/exclude is not the one in force: "commondir" names the real one.
func TestInfoExcludeThroughAWorktree(t *testing.T) {
	root := tree(t, map[string]string{
		"main/.git/info/exclude":           "*.bak\n",
		"main/.git/worktrees/wt/commondir": "../..\n",
		"main/.git/worktrees/wt/HEAD":      "ref: refs/heads/wt\n",
		"wt/.git":                          "gitdir: ../main/.git/worktrees/wt\n",
	})
	check(t, filepath.Join(root, "wt"), []want{
		{path: "a.bak", want: true},
		{path: "a.go", want: false},
	})
}

// A rule that matches the repository root would index nothing at all, which no
// .gitignore inside that root ever meant.
func TestRootIsNeverExcluded(t *testing.T) {
	root := tree(t, map[string]string{".gitignore": "*\n"})
	check(t, root, []want{
		{path: ".", isDir: true, want: false},
		{path: "", isDir: true, want: false},
		{path: "a.go", want: true},
	})
}

func TestNoRulesIgnoresNothing(t *testing.T) {
	root := tree(t, map[string]string{"main.go": "package main\n"})
	check(t, root, []want{{path: "main.go", want: false}})
}

// A pattern no matcher can parse excludes nothing, rather than something
// arbitrary: too little exclusion is visible and fixable, too much is not.
func TestUnparseablePatternIsDropped(t *testing.T) {
	root := tree(t, map[string]string{".gitignore": "[unclosed\n*.log\n"})
	check(t, root, []want{
		{path: "[unclosed", want: false},
		{path: "a.log", want: true},
	})
}

// git itself never hides tracked content behind an ignore rule, and neither
// may this: the file is in the repository, the user can see it, and an index
// that lacks it answers questions about the repository wrongly.
func TestTrackedFilesAreNotExcluded(t *testing.T) {
	root := tree(t, map[string]string{
		".gitignore":       "vendor/\n*.min.js",
		"vendor/keep.go":   "package vendor\n",
		"vendor/junk.go":   "package vendor\n",
		"app.min.js":       "1\n",
		"untracked.min.js": "1\n",
	})
	gitInit(t, root)
	git(t, root, "add", "-f", "vendor/keep.go", "app.min.js")

	check(t, root, []want{
		// The directory holds something tracked, so the walk has to descend
		// into it — but only what git has escapes the rule.
		{path: "vendor", isDir: true, want: false},
		{path: "vendor/keep.go", want: false},
		{path: "vendor/junk.go", want: true},
		{path: "app.min.js", want: false},
		{path: "untracked.min.js", want: true},
	})
}

// GIT_DIR and friends outrank the -C that names the repository, so a process
// that inherited them from a git hook would otherwise read another
// repository's index and protect the wrong paths — or, if they were merely
// blanked instead of dropped, none at all.
func TestTrackedFilesSurviveAnInheritedGitDir(t *testing.T) {
	root := tree(t, map[string]string{
		".gitignore":     "vendor/\n",
		"vendor/keep.go": "package vendor\n",
	})
	gitInit(t, root)
	git(t, root, "add", "-f", "vendor/keep.go")
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "elsewhere.git"))

	check(t, root, []want{{path: "vendor/keep.go", want: false}})
}

// Nothing is tracked in a directory that is not a checkout, so the rules apply
// as written and no git call is expected to succeed.
func TestPlainDirectoryWithoutGit(t *testing.T) {
	root := tree(t, map[string]string{
		".gitignore":   "build/\n",
		"build/out.go": "package build\n",
		"src/main.go":  "package main\n",
	})
	check(t, root, []want{
		{path: "build", isDir: true, want: true},
		{path: "build/out.go", want: true},
		{path: "src/main.go", want: false},
	})
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	git(t, dir, "init", "-q")
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnv(dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
