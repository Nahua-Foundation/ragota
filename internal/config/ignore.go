package config

import (
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/Nahua-Foundation/ragota/pkg/gitignore"
)

// noGitignore turns the checkout's own .gitignore rules off for every matcher
// built from here on. It is stored inverted so that the zero value — a process
// that never loaded a config, a test that builds a Config literal — is the
// documented default of "on".
//
// A process-wide value rather than a parameter, because the three places that
// build a matcher must agree: the indexing walk, the file watcher, and the
// incremental pass a pushed commit takes. If a push applied different
// exclusions from a full index pass, the index would gain files on every push
// and lose them again on the next full pass, which is the drift the shared
// predicate in repos.SkipDir exists to prevent. (The commit path builds its
// matcher inside the service package, which is why the setting travels this
// way rather than through IgnorePatternsFor's []string.)
var noGitignore atomic.Bool

// SetUseGitignore records repos.use_gitignore for the matchers built after it.
// applyDefaults calls it, so loading a config is enough; nothing else should
// need to.
func SetUseGitignore(v bool) { noGitignore.Store(!v) }

// UseGitignore reports the setting in force.
func UseGitignore() bool { return !noGitignore.Load() }

// IgnorePatterns decides what a repository's traversal leaves out. Two
// independent sources of exclusion compose in it:
//
//   - the configured glob patterns — repos.ignore plus the repository's own
//     .ragota.yaml, already concatenated by the caller;
//   - the checkout's own rules — its .gitignore files and .git/info/exclude —
//     unless repos.use_gitignore is off.
//
// The order is settled and one-directional: a path the configured patterns
// exclude is excluded, full stop. The .gitignore rules are consulted only for
// paths those patterns keep, so they can only ever exclude more. Neither a
// "!pattern" in a .gitignore nor a file being tracked re-includes something an
// operator wrote repos.ignore to keep out — the operator's list is the one
// place in this system that says what the server will not index, and a
// repository must not be able to argue with it.
type IgnorePatterns struct {
	patterns  []string
	gitignore bool

	// One lock for the whole matcher. The verdict caches were unsynchronised
	// while only the single-threaded walk used them; the watcher adds a
	// repository from one goroutine while its event loop asks about paths in
	// another, which is a data race on a plain map.
	mu    sync.Mutex
	files map[string]map[string]bool // repo -> repo-relative path -> ignored
	dirs  map[string]map[string]bool // the same, for paths known to be directories
	rules map[string]*gitignore.Rules
}

// NewIgnorePatterns creates a new ignore patterns manager, honouring the
// repos.use_gitignore setting in force when it is called.
func NewIgnorePatterns(patterns []string) *IgnorePatterns {
	return NewIgnorePatternsWithGitignore(patterns, UseGitignore())
}

// NewIgnorePatternsWithGitignore is NewIgnorePatterns with the .gitignore
// handling stated outright, for callers that must not depend on process-wide
// state — tests, mainly.
func NewIgnorePatternsWithGitignore(patterns []string, useGitignore bool) *IgnorePatterns {
	return &IgnorePatterns{
		patterns:  patterns,
		gitignore: useGitignore,
		files:     make(map[string]map[string]bool),
		dirs:      make(map[string]map[string]bool),
		rules:     make(map[string]*gitignore.Rules),
	}
}

// ShouldIgnore returns true if the path should be ignored. The path is treated
// as a file: "build/" in a .gitignore matches a directory and never a file, so
// a caller holding a directory has to say so with ShouldIgnoreDir.
func (i *IgnorePatterns) ShouldIgnore(repoPath, filePath string) bool {
	return i.shouldIgnore(repoPath, filePath, false)
}

// ShouldIgnoreDir is ShouldIgnore for a path known to be a directory.
func (i *IgnorePatterns) ShouldIgnoreDir(repoPath, dirPath string) bool {
	return i.shouldIgnore(repoPath, dirPath, true)
}

func (i *IgnorePatterns) shouldIgnore(repoPath, filePath string, isDir bool) bool {
	if len(i.patterns) == 0 && !i.gitignore {
		return false
	}

	// Normalize path to be relative to repo root
	relPath, err := filepath.Rel(repoPath, filePath)
	if err != nil {
		return false
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	cache := i.files
	if isDir {
		// Separate caches: the same relative path cannot be both a file and a
		// directory in one tree, but the two questions have different answers
		// and nothing here guarantees which one was asked first.
		cache = i.dirs
	}
	repoCache, ok := cache[repoPath]
	if !ok {
		repoCache = make(map[string]bool)
		cache[repoPath] = repoCache
	}
	if cached, ok := repoCache[relPath]; ok {
		return cached
	}

	ignored := false
	for _, pattern := range i.patterns {
		if matchPattern(pattern, relPath) {
			ignored = true
			break
		}
	}
	if !ignored && i.gitignore {
		ignored = i.rulesFor(repoPath).Ignored(filepath.ToSlash(relPath), isDir)
	}

	repoCache[relPath] = ignored
	return ignored
}

// rulesFor returns the checkout's rules, loaded lazily and kept for the life
// of the matcher. Callers hold i.mu.
func (i *IgnorePatterns) rulesFor(repoPath string) *gitignore.Rules {
	r, ok := i.rules[repoPath]
	if !ok {
		r = gitignore.New(repoPath)
		i.rules[repoPath] = r
	}
	return r
}

// matchPattern matches a path against a pattern.
func matchPattern(pattern, path string) bool {
	// Handle **/pattern (matches in any subdirectory)
	if strings.HasPrefix(pattern, "**/") {
		pattern = pattern[3:] // Remove **/
		// Check if path contains the pattern
		pathParts := strings.Split(path, string(filepath.Separator))
		for _, part := range pathParts {
			matched, _ := doublestar.Match(pattern, part)
			if matched {
				return true
			}
		}
		return false
	}

	// Handle pattern/** (matches this pattern anywhere)
	if strings.HasSuffix(pattern, "/**") {
		prefix := pattern[:len(pattern)-3] // Remove /**
		return strings.HasPrefix(path, prefix)
	}

	// Handle ** (matches anything)
	if pattern == "**" || pattern == "*.*" {
		return true
	}

	// Standard glob matching
	matched, _ := doublestar.Match(pattern, path)
	return matched
}
