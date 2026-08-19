package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The configured patterns keep the semantics they were measured with. The
// asymmetry is deliberate and documented in README and config.example.yaml:
// "dir/**" is a string-prefix match anchored at the repository root, while
// "**/dir/**" matches whole path components at any depth. Adding .gitignore
// handling on top must not have moved either of them.
func TestConfiguredPatternSemantics(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"node_modules/**", "node_modules/x.js", true},
		{"node_modules/**", "web/node_modules/x.js", false},
		{"node_modules/**", "node_modules_old/x.js", true}, // prefix, not a component
		{"**/node_modules/**", "node_modules/x.js", true},
		{"**/node_modules/**", "web/node_modules/x.js", true},
		{"**/node_modules/**", "node_modules_old/x.js", false},
		{"**/*.min.js", "web/app.min.js", true},
		{"*.min.js", "web/app.min.js", false},
		{"*.min.js", "app.min.js", true},
		{".git/**", ".git/config", true},
	}
	root := t.TempDir()
	for _, c := range cases {
		i := NewIgnorePatternsWithGitignore([]string{c.pattern}, false)
		if got := i.ShouldIgnore(root, filepath.Join(root, filepath.FromSlash(c.path))); got != c.want {
			t.Errorf("pattern %q vs %q = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

// A directory-only .gitignore pattern needs to know which one it is looking
// at, and only the caller does.
func TestShouldIgnoreDirIsAskedSeparately(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".gitignore"), "build/\n")
	i := NewIgnorePatternsWithGitignore(nil, true)

	if !i.ShouldIgnoreDir(root, filepath.Join(root, "build")) {
		t.Error("the build directory should be ignored")
	}
	if i.ShouldIgnore(root, filepath.Join(root, "build")) {
		t.Error("a file named build is not the build directory")
	}
	if !i.ShouldIgnore(root, filepath.Join(root, "build", "out.go")) {
		t.Error("a file under the build directory should be ignored")
	}
}

// The order the two sources compose in, stated as a test: the configured
// patterns exclude on their own, and .gitignore is consulted only for what
// they keep — so a "!" in a repository's .gitignore cannot argue with
// repos.ignore.
func TestConfiguredPatternsOutrankGitignore(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".gitignore"), "!vendor/\nsecrets/\n")
	i := NewIgnorePatternsWithGitignore([]string{"**/vendor/**"}, true)

	if !i.ShouldIgnore(root, filepath.Join(root, "vendor", "lib.go")) {
		t.Error("repos.ignore excludes vendor; a .gitignore negation may not re-include it")
	}
	if !i.ShouldIgnore(root, filepath.Join(root, "secrets", "key.go")) {
		t.Error(".gitignore excludes secrets, and nothing in the config keeps it")
	}
	if i.ShouldIgnore(root, filepath.Join(root, "main.go")) {
		t.Error("nothing excludes main.go")
	}
}

func TestGitignoreOffIgnoresOnlyTheConfiguredPatterns(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".gitignore"), "build/\n")
	i := NewIgnorePatternsWithGitignore(nil, false)

	if i.ShouldIgnoreDir(root, filepath.Join(root, "build")) {
		t.Error("with the switch off nothing in .gitignore applies")
	}
}

// The default is on: a run that never says anything about it gets git's view
// of what is not code. Loading a config is what puts the setting in force.
func TestUseGitignoreDefaultsOnAndFollowsTheConfig(t *testing.T) {
	t.Cleanup(func() { SetUseGitignore(true) })

	cfg := &Config{}
	cfg.applyDefaults()
	if cfg.Repos.UseGitignore == nil || !*cfg.Repos.UseGitignore {
		t.Fatalf("repos.use_gitignore = %v, want true", cfg.Repos.UseGitignore)
	}
	if !UseGitignore() {
		t.Error("applying a config with the default left the process setting off")
	}

	off := false
	cfg = &Config{Repos: ReposConfig{UseGitignore: &off}}
	cfg.applyDefaults()
	if UseGitignore() {
		t.Error("repos.use_gitignore: false did not reach the matchers")
	}
	if NewIgnorePatterns(nil).gitignore {
		t.Error("a matcher built afterwards still consults .gitignore")
	}
}

// The environment wins over the file, for the run that has no file: a
// zero-configuration `--source DIR` is exactly the one that meets a .gitignore
// hiding something the user wanted indexed.
func TestUseGitignoreEnvOverride(t *testing.T) {
	t.Cleanup(func() { SetUseGitignore(true) })
	t.Setenv("RAGOTA_USE_GITIGNORE", "0")

	on := true
	cfg := &Config{Repos: ReposConfig{UseGitignore: &on}}
	cfg.applyDefaults()
	if *cfg.Repos.UseGitignore || UseGitignore() {
		t.Error("RAGOTA_USE_GITIGNORE=0 did not turn .gitignore handling off")
	}

	// A value nobody can read as a boolean leaves the configured setting alone
	// rather than quietly meaning "false".
	t.Setenv("RAGOTA_USE_GITIGNORE", "maybe")
	cfg = &Config{Repos: ReposConfig{UseGitignore: &on}}
	cfg.applyDefaults()
	if !*cfg.Repos.UseGitignore {
		t.Error("an unparseable override should leave the config in force")
	}
}
