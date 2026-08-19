package gitignore

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestAgreesWithGit is the test that says these are git's rules rather than
// rules that resemble git's: the same tree is put to `git check-ignore` and to
// this package, path by path, and any disagreement is a failure.
//
// It is worth the subprocesses. Every hand-written matcher this replaces was
// plausible in isolation and wrong somewhere — the sibling project's matched a
// pattern against both the base name and the full path, which quietly turns
// every anchored pattern into an unanchored one — and prose about "**" cannot
// catch that. --no-index keeps the comparison to the rules alone: the index is
// empty here, so the tracked-file rescue this package adds on top (which git
// applies elsewhere, by never consulting these rules for tracked content) does
// not enter into it.
func TestAgreesWithGit(t *testing.T) {
	files := map[string]string{
		".gitignore": strings.Join([]string{
			"# comment",
			"*.log",
			"!keep.log",
			"/root-only",
			"anywhere",
			"dir/inside",
			"build/",
			"**/logs",
			"out/**",
			"src/**/gen.go",
			"node_modules/",
			"!node_modules/keep.js",
			"spaced   ",
			"trailing\\ ",
			"\\#hash",
			"a[bc]d",
			"*.py[co]",
			"doc/frotz/",
			"tmp/*/cache",
			"!out/keep.go",
			"?.txt",
			"[!a]bc",
			"deep/**/b/**/c.txt",
			"**/mid/**",
			"logs2/**/*.tmp",
			"weird\\[name",
		}, "\n") + "\n",
		"api/.gitignore":    "!*.gen.go\nprivate/\n*.tmp\n",
		"vendor/.gitignore": "!*\n",
		".git/info/exclude": "scratch/\n*.bak\n",
	}
	paths := []string{
		"a.log", "keep.log", "sub/keep.log", "sub/a.log",
		"root-only", "sub/root-only",
		"anywhere", "sub/deep/anywhere", "anywhere_else",
		"dir/inside", "sub/dir/inside",
		"build", "build/main.go", "sub/build/main.go",
		"logs", "a/b/logs", "out", "out/a.go", "out/deep/a.go",
		"src/gen.go", "src/a/b/gen.go", "other/gen.go",
		"node_modules", "node_modules/keep.js", "node_modules/x/y.js",
		"spaced", "trailing ", "#hash",
		"abd", "acd", "aXd", "x.pyc", "x.pyo", "x.pyx",
		"doc/frotz/f.txt", "a/doc/frotz/f.txt",
		"tmp/a/cache/x", "tmp/cache/x",
		"api/thing.gen.go", "api/v1/thing.gen.go", "api/private/x.go", "api/x.tmp",
		"top.gen.go", "vendor/x.log",
		"scratch/x.go", "x.bak", "sub/x.bak",
		"out/keep.go", "out/sub/keep.go",
		"a.txt", "ab.txt", "x/a.txt",
		"abc", "bbc", "xbc",
		"deep/b/c.txt", "deep/x/b/y/c.txt", "deep/c.txt",
		"mid/x.go", "a/mid/x.go", "a/mid", "mid",
		"logs2/a/b.tmp", "logs2/b.tmp", "logs2/x.go",
		"weird[name",
	}

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
	// Materialise every path so git can tell a directory from a file.
	for _, p := range paths {
		full := filepath.Join(root, filepath.FromSlash(p))
		if isDirPath(p, paths) {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	run := exec.Command("git", "-C", root, "init", "-q")
	run.Env = gitEnv(root)
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	// git init writes its own info/exclude template over the one laid out above.
	if err := os.WriteFile(filepath.Join(root, ".git/info/exclude"), []byte(files[".git/info/exclude"]), 0o600); err != nil {
		t.Fatal(err)
	}

	r := New(root)
	sort.Strings(paths)
	var bad []string
	for _, p := range paths {
		st, err := os.Stat(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			t.Fatal(err)
		}
		mine := r.Ignored(p, st.IsDir())
		theirs := gitSaysIgnored(t, root, p)
		if mine != theirs {
			bad = append(bad, p+": this package says "+verdict(mine)+", git says "+verdict(theirs))
		}
	}
	for _, s := range bad {
		t.Errorf("disagrees with git: %s", s)
	}
	t.Logf("compared %d paths against git", len(paths))
}

func verdict(ignored bool) string {
	if ignored {
		return "ignored"
	}
	return "kept"
}

// isDirPath decides what to create on disk: git needs a real directory to
// apply a directory-only pattern, and these paths are directories because
// something below them is in the list (or because the pattern under test is
// about the directory itself).
func isDirPath(p string, all []string) bool {
	for _, o := range all {
		if o != p && strings.HasPrefix(o, p+"/") {
			return true
		}
	}
	return p == "build" || p == "out" || p == "node_modules"
}

// gitSaysIgnored asks git itself. check-ignore exits 0 when the path is
// excluded and 1 when it is not, so only another status is a real failure.
func gitSaysIgnored(t *testing.T, root, p string) bool {
	t.Helper()
	cmd := exec.Command("git", "-C", root, "check-ignore", "-q", "--no-index", "--", p)
	cmd.Env = gitEnv(root)
	err := cmd.Run()
	if err == nil {
		return true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false
	}
	t.Fatalf("git check-ignore %s: %v", p, err)
	return false
}

// gitEnv keeps a developer's own git configuration — a global excludesfile
// above all — out of the comparison.
func gitEnv(home string) []string {
	return append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, "xdg"),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
}
