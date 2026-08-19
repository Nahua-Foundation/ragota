// Package gitignore answers the question git answers with `git check-ignore`:
// does this checkout's own configuration exclude this path?
//
// Every developer reads "gitignored" as "not my code", so an indexer that has
// its own pattern list and nothing else surprises people in a way that is hard
// to attribute: it indexes build output, virtualenvs, vendored trees and — the
// case that produced this package — a benchmark corpus of twelve foreign
// repositories sitting inside the repository being indexed.
//
// The rules implemented here are git's, not a glob list that resembles them:
// one .gitignore per directory rather than one per repository, later patterns
// beating earlier ones so that a negation can re-include, anchoring by whether
// the pattern contains a slash, directory-only patterns, and .git/info/exclude.
// A half-implementation is worse than none, because the thing it gets wrong is
// always "excluded something the user can see is tracked".
//
// Two rules from git are load-bearing and easy to miss:
//
//   - A file cannot be re-included if one of its parent directories is
//     excluded. Git never looks inside an excluded directory, so a negation
//     nested under one has nothing to override.
//   - Ignore rules never apply to tracked content. `git check-ignore` refuses
//     to answer for a tracked path at all unless told --no-index, because the
//     file is in the repository and git shows it in every listing; hiding it
//     from an index would be hiding code the user can see. Tracked paths are
//     therefore rescued here, and only from these rules — never from the
//     operator's configured repos.ignore patterns, which mean what they say.
//
// A submodule is walked as an ordinary directory, as it always has been here,
// so the parent's rules and the parent's index are what apply inside it. Git
// would treat it as its own repository; the difference shows only for a
// submodule holding a file the superproject's .gitignore matches.
//
// Matching is case-sensitive, whatever the filesystem is. Git flips to
// case-insensitive matching when core.ignoreCase is set, which `git init` does
// on macOS and Windows; the difference only shows for a pattern whose case
// does not match the file's, and it errs towards indexing a file rather than
// hiding one.
//
// The user's global excludesfile (core.excludesFile, ~/.config/git/ignore) is
// deliberately not read. It is machine configuration for one developer's
// editor and OS droppings, not a property of the repository: honouring it
// would make the same commit index differently depending on whose home
// directory the server process happens to run with, and a daemon or container
// has nobody's. Whatever belongs there for the whole team belongs in a
// .gitignore anyway.
package gitignore

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar/v4"
)

// trackedTimeout bounds the `git ls-files` call that loads the tracked set.
// It reads an index file — 47k paths in tens of milliseconds on the largest
// repository in the benchmark corpus — so a call that has not finished by now
// is a repository in a state git itself is stuck on, and the walk must not
// hang behind it.
const trackedTimeout = 30 * time.Second

// Rules is the ignore state of one checkout, loaded lazily as paths are
// queried: a .gitignore is read the first time a path inside its directory is
// asked about, and the tracked set only when a rule is about to exclude
// something. A repository whose rules never match anything costs one failed
// open per directory.
//
// A Rules is safe for concurrent use. The walker is single-threaded but the
// file watcher is not, and both share this through config.IgnorePatterns.
type Rules struct {
	root string

	mu sync.Mutex
	// dirs maps a repo-relative directory ("" for the root) to the patterns of
	// its .gitignore. Presence in the map means "already looked", so a
	// directory without one caches as a nil slice rather than being reopened.
	dirs map[string][]pattern
	// verdicts memoises directory decisions *before* the tracked-file rescue,
	// which is what the parent-directory rule needs: a directory kept only
	// because it holds tracked files is still an excluded directory for the
	// untracked files beside them.
	verdicts map[string]bool

	exclude       []pattern // .git/info/exclude
	excludeLoaded bool

	tracked       map[string]struct{} // tracked paths and their parent directories
	trackedLoaded bool
}

// New returns the rules of the checkout at root. It reads nothing yet.
func New(root string) *Rules {
	return &Rules{
		root:     filepath.Clean(root),
		dirs:     make(map[string][]pattern),
		verdicts: make(map[string]bool),
	}
}

// Ignored reports whether the checkout's own rules exclude rel, a
// slash-separated path relative to the repository root. isDir has to be
// supplied by the caller because "foo/" matches a directory and never a file,
// and the callers all know which one they are holding.
//
// The repository root itself is never excluded: a rule that matched it would
// index nothing at all, which is never what a .gitignore inside it meant.
func (r *Rules) Ignored(rel string, isDir bool) bool {
	rel = strings.Trim(filepath.ToSlash(rel), "/")
	if rel == "" || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// The parent chain decides first: git does not descend into an excluded
	// directory, so no pattern below one — negation included — is ever read.
	if parent := path.Dir(rel); parent != "." && r.dirExcluded(parent) {
		return !r.isTracked(rel)
	}
	if !r.match(rel, isDir) {
		return false
	}
	return !r.isTracked(rel)
}

// dirExcluded is the memoised verdict for a directory, parents included.
func (r *Rules) dirExcluded(dir string) bool {
	if v, ok := r.verdicts[dir]; ok {
		return v
	}
	v := false
	if parent := path.Dir(dir); parent != "." {
		v = r.dirExcluded(parent)
	}
	if !v {
		v = r.match(dir, true)
	}
	r.verdicts[dir] = v
	return v
}

// match applies the pattern files that govern rel, without the parent rule and
// without the tracked-file rescue.
//
// Precedence is git's: the .gitignore in the path's own directory outranks the
// ones above it, the last matching line in a file outranks the earlier ones,
// and .git/info/exclude ranks below every .gitignore in the tree. Scanning
// from the deepest file upward and each file bottom-up makes the first match
// found the winning one.
func (r *Rules) match(rel string, isDir bool) bool {
	for dir := path.Dir(rel); ; dir = path.Dir(dir) {
		base := dir
		if base == "." {
			base = ""
		}
		if v, ok := matchIn(r.patternsFor(base), relTo(base, rel), isDir); ok {
			return v
		}
		if dir == "." {
			break
		}
	}
	v, _ := matchIn(r.infoExclude(), rel, isDir)
	return v
}

// matchIn reports the verdict of the last matching pattern in one file, and
// whether any matched at all.
func matchIn(pats []pattern, rel string, isDir bool) (ignored, matched bool) {
	for i := len(pats) - 1; i >= 0; i-- {
		p := pats[i]
		if p.dirOnly && !isDir {
			continue
		}
		if ok, err := doublestar.Match(p.glob, rel); err == nil && ok {
			return !p.negate, true
		}
	}
	return false, false
}

// relTo re-expresses a repo-relative path relative to the directory whose
// .gitignore is being applied, because that is what its patterns are anchored
// against.
func relTo(base, rel string) string {
	if base == "" {
		return rel
	}
	return strings.TrimPrefix(rel, base+"/")
}

// patternsFor returns the patterns of dir's own .gitignore, reading it once.
func (r *Rules) patternsFor(dir string) []pattern {
	if pats, ok := r.dirs[dir]; ok {
		return pats
	}
	data, err := os.ReadFile(filepath.Join(r.root, filepath.FromSlash(dir), ".gitignore"))
	var pats []pattern
	if err == nil {
		pats = parse(data)
	}
	r.dirs[dir] = pats
	return pats
}

// infoExclude returns the patterns of .git/info/exclude, the per-checkout list
// that belongs to the same mechanism as .gitignore but is not committed.
func (r *Rules) infoExclude() []pattern {
	if r.excludeLoaded {
		return r.exclude
	}
	r.excludeLoaded = true
	if dir, ok := r.gitDir(); ok {
		if data, err := os.ReadFile(filepath.Join(dir, "info", "exclude")); err == nil {
			r.exclude = parse(data)
		}
	}
	return r.exclude
}

// gitDir resolves the repository's git directory at root, or reports that root
// is not the top of a checkout.
//
// .git is a file rather than a directory in a worktree or a submodule; in a
// worktree it points at a per-worktree directory whose info/exclude is not the
// one in force, so "commondir" is followed to the shared one — the same file
// git itself reads.
func (r *Rules) gitDir() (string, bool) {
	dot := filepath.Join(r.root, ".git")
	info, err := os.Stat(dot)
	if err != nil {
		return "", false
	}
	dir := dot
	if !info.IsDir() {
		data, err := os.ReadFile(dot)
		if err != nil {
			return "", false
		}
		ptr := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir:"))
		if ptr == "" {
			return "", false
		}
		if !filepath.IsAbs(ptr) {
			ptr = filepath.Join(r.root, ptr)
		}
		dir = filepath.Clean(ptr)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "commondir")); err == nil {
		common := strings.TrimSpace(string(data))
		if common != "" {
			if !filepath.IsAbs(common) {
				common = filepath.Join(dir, common)
			}
			dir = filepath.Clean(common)
		}
	}
	return dir, true
}

// isCheckout reports whether root looks like a repository git would work in,
// which is what makes a failed `git ls-files` worth reporting. HEAD rather
// than the directory alone: an empty .git is what a fixture that only needs to
// look like a repository leaves behind, and warning about those would train
// everyone to ignore the warning that matters.
func (r *Rules) isCheckout() bool {
	dir, ok := r.gitDir()
	if !ok {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "HEAD"))
	return err == nil
}

// isTracked reports whether git has this path in its index, or — for a
// directory — whether it holds anything git has.
//
// This is the guard that keeps the feature honest. A repository that
// force-added a file its own .gitignore matches is showing that file to
// everyone who checks the tree out, and an index that silently lacks it
// answers questions about that repository wrongly — measured over the twelve
// repositories of the benchmark corpus, this keeps 23 files that the rules
// alone would have dropped (7 in consul, all under an ignored vendor
// directory, 16 in grafana). The directory entries matter for the same reason:
// without them the walk would prune an ignored directory before ever reaching
// the tracked files inside it.
func (r *Rules) isTracked(rel string) bool {
	r.loadTracked()
	if r.tracked == nil {
		return false
	}
	_, ok := r.tracked[rel]
	return ok
}

// loadTracked asks git for the index contents, once.
//
// Reading the index through git rather than parsing .git/index keeps the
// authority on "what is tracked" in one place — the same program the user runs
// — and it is cheap: the largest repository in the benchmark corpus lists its
// 47k paths in about 40ms, once per repository per walk. `git ls-files` prints
// paths relative to the directory it runs in, which is exactly the form the
// rest of this package uses.
func (r *Rules) loadTracked() {
	if r.trackedLoaded {
		return
	}
	r.trackedLoaded = true

	ctx, cancel := context.WithTimeout(context.Background(), trackedTimeout)
	defer cancel()
	// -z because git quotes and escapes unusual path names otherwise, and a
	// misparsed name would silently drop a file out of the protected set.
	cmd := exec.CommandContext(ctx, "git", "-C", r.root, "ls-files", "-z", "--cached")
	cmd.Env = envWithoutGitLocation()
	out, err := cmd.Output()
	if err != nil {
		// A directory that is not a checkout has nothing tracked and nothing to
		// protect, which is the common case for a plain source tree and not
		// worth a line in the log. A checkout whose git cannot be run is worth
		// one: the exclusions below are then unverified against the index.
		if r.isCheckout() {
			slog.Warn("gitignore: cannot read the git index; tracked files are not protected from .gitignore",
				"path", r.root, "err", err)
		}
		return
	}

	tracked := make(map[string]struct{})
	for _, name := range bytes.Split(out, []byte{0}) {
		p := string(name)
		if p == "" {
			continue
		}
		tracked[p] = struct{}{}
		for dir := path.Dir(p); dir != "." && dir != "/"; dir = path.Dir(dir) {
			if _, ok := tracked[dir]; ok {
				break // this parent chain is already in
			}
			tracked[dir] = struct{}{}
		}
	}
	r.tracked = tracked
}

// envWithoutGitLocation is the process environment with the variables that
// point git at a repository removed. They outrank the -C above, so a process
// that inherited them — one started from a git hook, say — would read some
// other repository's index and protect the wrong paths. They have to be
// dropped rather than set empty: git reads GIT_DIR="" as a repository named
// "" and fails.
func envWithoutGitLocation() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "GIT_DIR="),
			strings.HasPrefix(kv, "GIT_WORK_TREE="),
			strings.HasPrefix(kv, "GIT_INDEX_FILE="),
			strings.HasPrefix(kv, "GIT_COMMON_DIR="):
			continue
		}
		out = append(out, kv)
	}
	return out
}

// pattern is one line of a .gitignore, translated into the glob syntax
// doublestar matches, relative to the directory the file governs.
type pattern struct {
	glob    string
	negate  bool
	dirOnly bool
}

// parse turns the body of a .gitignore into patterns, dropping the lines git
// drops (blank, comment) and the ones no matcher can use.
func parse(data []byte) []pattern {
	var pats []pattern
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if p, ok := parseLine(sc.Text()); ok {
			pats = append(pats, p)
		}
	}
	return pats
}

// parseLine translates one .gitignore line, reporting whether anything is left
// of it.
func parseLine(line string) (pattern, bool) {
	line = strings.TrimRight(line, "\r")
	// Trailing spaces are not part of a pattern unless escaped — a rule that
	// exists because text editors add them.
	line = trimUnescapedTrailingSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return pattern{}, false // "\#" keeps its meaning: only a bare # comments
	}

	var p pattern
	if strings.HasPrefix(line, "!") {
		p.negate = true
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") && !escaped(line, len(line)-1) {
		p.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	if line == "" {
		return pattern{}, false
	}

	// A slash anywhere but at the end anchors the pattern to the directory of
	// the file it came from; without one it matches at any depth below it.
	anchored := strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	if line == "" {
		return pattern{}, false
	}
	if !anchored {
		line = "**/" + line
	}
	// "abc/**" matches everything *inside* abc, not abc itself — the
	// distinction matters because excluding the directory would also make
	// "!abc/keep" unreachable, and git keeps that negation working. doublestar
	// lets a trailing "**" match zero segments, so one segment is required.
	if strings.HasSuffix(line, "/**") {
		line += "/*"
	}
	// Unparseable patterns (an unclosed bracket, say) are dropped rather than
	// guessed at: excluding nothing is recoverable, excluding the wrong
	// subtree is not.
	if !doublestar.ValidatePattern(line) {
		return pattern{}, false
	}
	p.glob = line
	return p, true
}

// trimUnescapedTrailingSpace removes the trailing whitespace git ignores,
// stopping at a space that a backslash protects.
func trimUnescapedTrailingSpace(s string) string {
	end := len(s)
	for end > 0 && (s[end-1] == ' ' || s[end-1] == '\t') {
		if escaped(s, end-1) {
			break
		}
		end--
	}
	return s[:end]
}

// escaped reports whether the byte at i is preceded by an odd number of
// backslashes, i.e. whether it is a literal rather than syntax.
func escaped(s string, i int) bool {
	n := 0
	for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
		n++
	}
	return n%2 == 1
}
