package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/domain"
)

func TestValidateRemoteURL(t *testing.T) {
	tests := []struct {
		url string
		ok  bool
	}{
		{"https://github.com/acme/widgets.git", true},
		{"http://internal.example/repo.git", true},
		{"git://example.com/repo.git", true},
		{"ssh://git@example.com/repo.git", true},
		{"git@github.com:acme/widgets.git", true},
		{"", false},
		{"--upload-pack=touch /tmp/pwned", false}, // the option-injection class
		{"-oProxyCommand=evil", false},
		{"https://example.com/ repo", false}, // whitespace
		{"file:///etc/passwd", false},        // local transport
		{"ext::sh -c 'touch pwned'", false},  // git ext helper
		{"/var/lib/secrets", false},          // bare local path
	}
	for _, tt := range tests {
		err := validateRemoteURL(tt.url)
		if tt.ok && err != nil {
			t.Errorf("validateRemoteURL(%q) = %v, want nil", tt.url, err)
		}
		if !tt.ok && err == nil {
			t.Errorf("validateRemoteURL(%q) = nil, want error", tt.url)
		}
	}
}

func TestValidateBranch(t *testing.T) {
	tests := []struct {
		branch string
		ok     bool
	}{
		{"", true},
		{"main", true},
		{"release/2.0", true},
		{"feature-x", true},
		{"--upload-pack=evil", false},
		{"--flag", false},
		{"has space", false},
		{"weird~ref", false},
	}
	for _, tt := range tests {
		err := validateBranch(tt.branch)
		if tt.ok && err != nil {
			t.Errorf("validateBranch(%q) = %v, want nil", tt.branch, err)
		}
		if !tt.ok && err == nil {
			t.Errorf("validateBranch(%q) = nil, want error", tt.branch)
		}
	}
}

func TestScpLikeURL(t *testing.T) {
	yes := []string{"git@github.com:acme/repo.git", "host:path"}
	no := []string{"https://github.com/a/b", "no-colon-here", ":leading", "/local/path"}
	for _, u := range yes {
		if !scpLikeURL(u) {
			t.Errorf("scpLikeURL(%q) = false, want true", u)
		}
	}
	for _, u := range no {
		if scpLikeURL(u) {
			t.Errorf("scpLikeURL(%q) = true, want false", u)
		}
	}
}

func TestGetTokenForURL(t *testing.T) {
	s := New(&Config{Auth: &Auth{GitHubToken: "gh", GitLabToken: "gl"}})
	if got := s.getTokenForURL("https://github.com/a/b"); got != "gh" {
		t.Errorf("github token = %q, want gh", got)
	}
	if got := s.getTokenForURL("https://gitlab.com/a/b"); got != "gl" {
		t.Errorf("gitlab token = %q, want gl", got)
	}
	if got := s.getTokenForURL("https://example.com/a/b"); got != "" {
		t.Errorf("third-party token = %q, want empty", got)
	}
}

func TestAdd_RejectsInjectionURL(t *testing.T) {
	work := t.TempDir()
	s := New(&Config{WorkDir: work})
	_, err := s.Add(context.Background(), &domain.AddRequest{Name: "x", URL: "--upload-pack=touch pwned"})
	if err == nil {
		t.Fatal("Add with injection URL should fail")
	}
	if !strings.Contains(err.Error(), "invalid url") {
		t.Errorf("error = %v, want invalid url", err)
	}
	// Nothing should have been created on disk.
	if entries, _ := os.ReadDir(work); len(entries) != 0 {
		t.Errorf("work dir should be empty, has %d entries", len(entries))
	}
}

// --- integration against a local repository (no network) ---

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// makeLocalRepo builds a one-commit repository on branch "main" and returns its
// path, usable as a clone/ls-remote source over a local path.
func makeLocalRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitCmd(t, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "README.md")
	gitCmd(t, dir, "commit", "-m", "initial")
	return dir
}

func TestDetectDefaultBranch_Local(t *testing.T) {
	src := makeLocalRepo(t)
	s := New(&Config{})
	if got := s.detectDefaultBranch(context.Background(), src); got != "main" {
		t.Errorf("detectDefaultBranch = %q, want main", got)
	}
}

// TestDetectDefaultBranch_NonMain guards the parser fix: a repository whose
// default branch is not "main" must be detected as such, not silently reported
// empty and left to the "main" fallback (which would clone a missing branch).
func TestDetectDefaultBranch_NonMain(t *testing.T) {
	dir := t.TempDir()
	gitCmd(t, dir, "init", "-b", "trunk")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "f.txt")
	gitCmd(t, dir, "commit", "-m", "initial")

	s := New(&Config{})
	if got := s.detectDefaultBranch(context.Background(), dir); got != "trunk" {
		t.Errorf("detectDefaultBranch = %q, want trunk", got)
	}
}

func TestClonePull_Local(t *testing.T) {
	src := makeLocalRepo(t)
	s := New(&Config{})
	dest := filepath.Join(t.TempDir(), "cloned")

	if err := s.clone(context.Background(), src, dest, "main"); err != nil {
		t.Fatalf("clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Fatalf("cloned tree missing README.md: %v", err)
	}

	// A second commit upstream should arrive via pull without error.
	if err := os.WriteFile(filepath.Join(src, "second.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "add", "second.txt")
	gitCmd(t, src, "commit", "-m", "second")

	repo := &domain.Repo{URL: src, Path: dest}
	if err := s.Update(context.Background(), repo); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "second.txt")); err != nil {
		t.Errorf("pull did not fetch new commit: %v", err)
	}
}

// A clone always carries a real .git directory, so this is where the component
// match earns its keep: nothing under .git may be indexed, while .github — which
// merely contains the substring — must be.
func TestGetFiles_SkipsGitButNotGitHub(t *testing.T) {
	src := makeLocalRepo(t)
	s := New(&Config{})
	dest := filepath.Join(t.TempDir(), "cloned")
	if err := s.clone(context.Background(), src, dest, "main"); err != nil {
		t.Fatalf("clone: %v", err)
	}

	writeFile(t, filepath.Join(dest, ".git", "hooks", "bootstrap.py"), "print(1)\n")
	writeFile(t, filepath.Join(dest, ".github", "workflows", "ci.yml"), "on: push\n")

	files, err := s.GetFiles(context.Background(), &domain.Repo{Path: dest}, nil)
	if err != nil {
		t.Fatalf("GetFiles: %v", err)
	}
	got := make([]string, 0, len(files))
	for _, f := range files {
		got = append(got, filepath.ToSlash(f.Path))
	}
	slices.Sort(got)

	want := []string{".github/workflows/ci.yml", "README.md"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("files = %q, want %q", got, want)
	}
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

func TestCloneAtomic_LeavesNothingOnBadBranch(t *testing.T) {
	src := makeLocalRepo(t)
	s := New(&Config{})
	dest := filepath.Join(t.TempDir(), "repo")

	// A nonexistent branch makes the clone fail; the destination must not exist
	// afterwards and no temp clone dir may be left behind.
	if err := s.cloneAtomic(context.Background(), src, dest, "no-such-branch"); err == nil {
		t.Fatal("clone of missing branch should fail")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("destination should not exist after failed clone")
	}
	entries, _ := os.ReadDir(filepath.Dir(dest))
	if len(entries) != 0 {
		t.Errorf("temp clone dir leaked: %v", entries)
	}
}
