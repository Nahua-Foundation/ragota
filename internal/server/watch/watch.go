// Package watch keeps the index in step with the working trees it was built
// from: a file written under a watched repository is re-indexed, a new one is
// added, a removed one is dropped.
//
// It is not a second indexing path. Every batch it produces is handed to
// Service.ApplyLocalChanges, which is the incremental pass a pushed commit
// takes — the same ignore handling, the same deletions, the same service
// re-detection and linking. The watcher's whole job is to turn filesystem
// events into the "these paths changed, these are gone" plan that pass already
// accepts, and it deliberately knows nothing about how any of it is indexed.
package watch

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/Nahua-Foundation/ragota/internal/app"
	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/server/progress"
)

// Defaults for Options.
const (
	// DefaultDebounce is how long a repository must be quiet before its
	// accumulated changes are indexed. An editor saving a file writes, renames
	// and chmods it; a formatter run rewrites a package; a branch switch
	// rewrites the tree. Indexing per event would spend the whole interval
	// re-reading files that are about to change again.
	DefaultDebounce = 400 * time.Millisecond

	// DefaultMaxDelay bounds the quiet period. A build tool that rewrites a
	// tree for a minute never goes quiet for a whole debounce interval, and
	// waiting for silence that does not come is indistinguishable from having
	// stopped working.
	DefaultMaxDelay = 5 * time.Second

	// busyRetry is how long to wait before re-offering a batch the service
	// refused because the repository was already index.
	busyRetry = 2 * time.Second
)

// Indexer is the part of the service the watcher drives. It is an interface so
// that the watcher can be tested against a stub, but the only implementation
// is *app.Service.
type Indexer interface {
	// IgnorePatternsFor gives the effective exclusions of one repository:
	// the server's, plus the repository's own .ragota.yaml.
	IgnorePatternsFor(repo *domain.Repo) []string
	// IndexedPathsUnder names the indexed files below a repo-relative
	// directory, which is how a vanished directory becomes a list of deletes.
	IndexedPathsUnder(ctx context.Context, repoID, dir string) ([]string, error)
	// ApplyLocalChanges indexes one batch of working-tree changes.
	ApplyLocalChanges(ctx context.Context, repoID string, files []app.CommitFile) error
}

// Options configure a Watcher. The zero value is usable and takes the
// documented defaults.
type Options struct {
	Debounce time.Duration
	MaxDelay time.Duration
	// Bus, when set, receives what the watcher did. A nil Bus is a no-op.
	Bus *progress.Bus
	// MaxWatchedDirs caps how many directories may be watched at once, across
	// every repository. Zero derives it from the process's descriptor limit,
	// which is what it is really bounded by; see budget.go.
	MaxWatchedDirs int
}

// tree is one watched repository.
type tree struct {
	repo *domain.Repo
	// ignore is replaced, not mutated, when the repository's own .gitignore
	// changes under the watch — the exclusions now depend on a file inside the
	// tree being watched, and a matcher built at Add time would answer with a
	// .gitignore the user has since edited. An atomic pointer because Add
	// walks a tree on the caller's goroutine while the event loop is already
	// delivering events for it.
	ignore atomic.Pointer[config.IgnorePatterns]
	// dirs holds the absolute directories currently watched. A removed path
	// that appears here was a directory, which is the only way to tell a
	// deleted file from a deleted subtree after the fact — by then there is
	// nothing on disk left to stat.
	dirs map[string]bool
}

// batch accumulates one repository's unindexed changes.
type batch struct {
	files map[string]bool // repo-relative paths whose content or existence changed
	dirs  map[string]bool // repo-relative directories that disappeared
	first time.Time       // first event in this batch, for the MaxDelay cap
	last  time.Time       // most recent event, for the quiet period
	after time.Time       // do not attempt before this; set by the busy backoff
}

// Watcher follows the working trees of the repositories added to it.
type Watcher struct {
	idx      Indexer
	fsw      *fsnotify.Watcher
	debounce time.Duration
	maxDelay time.Duration
	bus      *progress.Bus

	// budget bounds how many descriptors the watches may hold, across every
	// repository. See budget.go for why an unbounded watcher is fatal rather
	// than merely wasteful.
	budget *budget

	mu      sync.Mutex
	trees   []*tree
	pending map[string]*batch // repo id -> accumulated changes
}

// New returns a Watcher that pushes changes into idx. Close it when done.
func New(idx Indexer, opts Options) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &Watcher{
		idx:      idx,
		fsw:      fsw,
		debounce: opts.Debounce,
		maxDelay: opts.MaxDelay,
		bus:      opts.Bus,
		budget:   newBudget(opts.MaxWatchedDirs),
		pending:  make(map[string]*batch),
	}
	if w.debounce <= 0 {
		w.debounce = DefaultDebounce
	}
	if w.maxDelay < w.debounce {
		w.maxDelay = max(DefaultMaxDelay, w.debounce)
	}
	return w, nil
}

// Add starts watching one repository: every directory the indexer's own walk
// would descend into gets a watch, and nothing else does.
//
// Failing to watch a directory is reported and skipped, never returned. On the
// platforms where a watch costs a file descriptor per entry, a repository
// large enough to exhaust them would otherwise take down a process that was
// indexing eleven other repositories perfectly well; a subtree that stops
// being followed is worth a warning and the next full pass.
func (w *Watcher) Add(repo *domain.Repo) error {
	if repo == nil || repo.Path == "" {
		return errors.New("watch: repository has no path")
	}
	t := &tree{repo: repo, dirs: make(map[string]bool)}
	t.ignore.Store(config.NewIgnorePatterns(w.idx.IgnorePatternsFor(repo)))
	w.mu.Lock()
	w.trees = append(w.trees, t)
	w.mu.Unlock()

	watched := w.walkTree(t, repo.Path, nil)
	slog.Info("watching repository", "repo_id", repo.ID, "path", repo.Path, "dirs", watched)
	w.budget.report(repo.ID)
	w.bus.Watching(repo.ID, true)
	return nil
}

// walkTree installs a watch on dir and on every directory below it that
// repos.SkipDir would let the indexing walk descend into, and appends the
// indexable files it passes to files (nil to collect none). It returns how
// many directories are now watched.
func (w *Watcher) walkTree(t *tree, dir string, files *[]string) int {
	n := 0
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// A directory that disappeared mid-walk, or one this process may
			// not read. Neither is a reason to abandon the rest of the tree.
			slog.Debug("watch: skipping unreadable path", "path", path, "err", err)
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			// The repository root is exempt from the pattern test, and so is
			// the subtree root handed to this walk: a directory created under
			// an already-accepted parent has been accepted with it.
			if path != t.repo.Path && path != dir && repos.SkipDir(t.repo.Path, path, d.Name(), t.ignore.Load()) {
				return filepath.SkipDir
			}
			// The budget is taken before the watch, not after a failure:
			// finding the limit by hitting it means the process is already out
			// of descriptors, and what fails next is not necessarily the
			// watcher. Skipping the subtree rather than only this directory
			// keeps the walk from spending the whole remaining budget deep in
			// one branch of a tree it cannot cover anyway.
			if !w.budget.take() {
				return filepath.SkipDir
			}
			if werr := w.fsw.Add(path); werr != nil {
				w.budget.release()
				slog.Warn("watch: cannot follow directory", "repo_id", t.repo.ID, "path", path, "err", werr)
				return nil
			}
			w.mu.Lock()
			t.dirs[path] = true
			w.mu.Unlock()
			n++
			return nil
		}
		if files == nil {
			return nil
		}
		rel, ok := t.relIndexable(path)
		if ok {
			*files = append(*files, rel)
		}
		return nil
	})
	if err != nil {
		slog.Warn("watch: walk failed", "repo_id", t.repo.ID, "path", dir, "err", err)
	}
	return n
}

// relIndexable turns an absolute path into the repo-relative one the index
// knows it by, reporting whether the indexer would hold it at all: the same
// two tests the walk applies to a file, so the watcher cannot enqueue
// something a full pass would never have stored.
func (t *tree) relIndexable(abs string) (string, bool) {
	rel, err := filepath.Rel(t.repo.Path, abs)
	if err != nil || rel == "." || rel == ".." {
		return "", false
	}
	if t.ignore.Load().ShouldIgnore(t.repo.Path, abs) {
		return "", false
	}
	if repos.DetectLanguage(rel) == "" {
		return "", false
	}
	return rel, true
}

// Run follows the watched repositories until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	// A ticker rather than a timer reset per event: the readiness test is
	// per-repository (one repository going quiet must not be held up by
	// another still being written to) and it has two clauses, the quiet period
	// and the cap on it. One periodic check evaluates both for every
	// repository; the timer-per-repository version of this is a scheduler.
	tick := time.NewTicker(w.debounce)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.record(ev)
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			// Dropped events on an overflowing kernel queue land here. The
			// index is then behind until the paths change again or a full pass
			// runs, which is worth saying out loud and not worth dying over.
			slog.Warn("watch: event stream error", "err", err)
		case <-tick.C:
			w.flush(ctx)
		}
	}
}

// Close stops watching. Run returns shortly afterwards.
func (w *Watcher) Close() error {
	return w.fsw.Close()
}

// record folds one filesystem event into the pending batch of its repository.
func (w *Watcher) record(ev fsnotify.Event) {
	// Chmod carries no content change, and it is the noisiest op on the
	// platforms that emit it at all.
	if !ev.Has(fsnotify.Create) && !ev.Has(fsnotify.Write) &&
		!ev.Has(fsnotify.Remove) && !ev.Has(fsnotify.Rename) {
		return
	}

	t := w.treeFor(ev.Name)
	if t == nil {
		return
	}

	// A .gitignore edit changes what this repository excludes from here on.
	// Only the matcher is rebuilt: the directories already being watched stay
	// watched, and their files are filtered on the way out instead — dropping
	// and reinstalling watches on a file save is how a watcher spends its
	// descriptor budget, and the next full pass installs the right set anyway.
	if filepath.Base(ev.Name) == ".gitignore" {
		t.ignore.Store(config.NewIgnorePatterns(w.idx.IgnorePatternsFor(t.repo)))
	}

	w.mu.Lock()
	wasDir := t.dirs[ev.Name]
	w.mu.Unlock()

	if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
		// A rename is a remove plus a create: fsnotify reports the old name
		// leaving and, if the new name is also watched, arriving. Nothing here
		// treats the two halves as connected, which is what makes a move out
		// of a watched tree behave like the deletion it is.
		if wasDir {
			w.forgetDir(t, ev.Name)
			if rel, err := filepath.Rel(t.repo.Path, ev.Name); err == nil {
				w.enqueueDir(t.repo.ID, rel)
			}
			return
		}
		if rel, ok := t.relIndexable(ev.Name); ok {
			w.enqueueFile(t.repo.ID, rel)
		}
		return
	}

	// A directory that appears is not one file change but a subtree: a branch
	// switch, an unpacked archive or a move brings its contents with it, and
	// no watch exists for anything inside it yet. Both halves matter — the
	// files that are already there have to be indexed, and the directories
	// have to be followed from now on.
	if fi, err := os.Lstat(ev.Name); err == nil && fi.IsDir() {
		if ev.Name != t.repo.Path && repos.SkipDir(t.repo.Path, ev.Name, filepath.Base(ev.Name), t.ignore.Load()) {
			return
		}
		var found []string
		w.walkTree(t, ev.Name, &found)
		for _, rel := range found {
			w.enqueueFile(t.repo.ID, rel)
		}
		return
	}

	if rel, ok := t.relIndexable(ev.Name); ok {
		w.enqueueFile(t.repo.ID, rel)
	}
}

// treeFor finds the watched repository containing path, preferring the longest
// matching root so that a repository nested inside another claims its own
// files.
func (w *Watcher) treeFor(path string) *tree {
	w.mu.Lock()
	defer w.mu.Unlock()
	var best *tree
	for _, t := range w.trees {
		if !within(t.repo.Path, path) {
			continue
		}
		if best == nil || len(t.repo.Path) > len(best.repo.Path) {
			best = t
		}
	}
	return best
}

// within reports whether path is root or sits under it.
func within(root, path string) bool {
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !hasDotDotPrefix(rel)
}

func hasDotDotPrefix(rel string) bool {
	const dotdot = ".." + string(filepath.Separator)
	return len(rel) >= len(dotdot) && rel[:len(dotdot)] == dotdot
}

// forgetDir drops the watches recorded for a directory and everything below
// it. fsnotify releases the kernel-side watch when the directory goes away;
// this is the bookkeeping that tells a later event it used to be a directory.
func (w *Watcher) forgetDir(t *tree, dir string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for path := range t.dirs {
		if within(dir, path) {
			delete(t.dirs, path)
			_ = w.fsw.Remove(path) // already gone with the directory
		}
	}
}

func (w *Watcher) enqueueFile(repoID, rel string) {
	w.mu.Lock()
	w.batchFor(repoID).files[rel] = true
	w.mu.Unlock()
}

func (w *Watcher) enqueueDir(repoID, rel string) {
	w.mu.Lock()
	w.batchFor(repoID).dirs[rel] = true
	w.mu.Unlock()
}

// batchFor returns the repository's pending batch, starting one if needed, and
// stamps it as active. Callers hold w.mu.
func (w *Watcher) batchFor(repoID string) *batch {
	b, ok := w.pending[repoID]
	if !ok {
		b = &batch{files: map[string]bool{}, dirs: map[string]bool{}, first: time.Now()}
		w.pending[repoID] = b
	}
	b.last = time.Now()
	return b
}

// flush indexes every repository whose batch has settled.
func (w *Watcher) flush(ctx context.Context) {
	now := time.Now()

	w.mu.Lock()
	ready := make(map[string]*batch)
	for id, b := range w.pending {
		if now.Before(b.after) {
			continue
		}
		// Settled means quiet for a debounce interval, or held open past the
		// cap by a stream of events that has not stopped.
		if now.Sub(b.last) < w.debounce && now.Sub(b.first) < w.maxDelay {
			continue
		}
		ready[id] = b
		delete(w.pending, id)
	}
	w.mu.Unlock()

	for id, b := range ready {
		w.apply(ctx, id, b)
	}
}

// apply turns one settled batch into a change set and indexes it.
func (w *Watcher) apply(ctx context.Context, repoID string, b *batch) {
	t := w.treeByID(repoID)
	if t == nil {
		return
	}

	changes, changed, deleted := w.changeSet(ctx, t, b)
	if len(changes) == 0 {
		return
	}

	err := w.idx.ApplyLocalChanges(ctx, repoID, changes)
	if errors.Is(err, app.ErrRepoBusy) {
		// The repository is mid-pass. That pass may have read these files
		// before they changed, so the batch is put back rather than dropped:
		// nothing else will ever come looking for it.
		w.requeue(repoID, b)
		return
	}
	if err != nil {
		slog.Warn("watch: indexing changes failed", "repo_id", repoID, "err", err)
	}
	w.bus.WatchApplied(repoID, changed, deleted, err)
}

// changeSet resolves a batch against the current state of the disk.
//
// The resolution happens here, once, rather than as each event arrives: an
// editor's write-then-rename and a `git checkout` both produce paths whose
// final state is only knowable after the burst has finished, and a path
// created and removed inside one debounce interval must end up described by
// what is on disk now, not by the first event about it.
func (w *Watcher) changeSet(ctx context.Context, t *tree, b *batch) (changes []app.CommitFile, changed, deleted int) {
	for dir := range b.dirs {
		paths, err := w.idx.IndexedPathsUnder(ctx, t.repo.ID, dir)
		if err != nil {
			slog.Warn("watch: cannot list indexed paths of removed directory",
				"repo_id", t.repo.ID, "dir", dir, "err", err)
			continue
		}
		for _, p := range paths {
			changes = append(changes, app.CommitFile{Path: p, Status: "D"})
			deleted++
		}
	}

	for rel := range b.files {
		abs := filepath.Join(t.repo.Path, rel)
		if _, err := os.Lstat(abs); err != nil {
			changes = append(changes, app.CommitFile{Path: rel, Status: "D"})
			deleted++
			continue
		}
		// No content: the pass reads it from disk. Sending what the watcher
		// read would ship whatever the file happened to hold at the moment the
		// event arrived, which is routinely a half-written file.
		changes = append(changes, app.CommitFile{Path: rel, Status: "M"})
		changed++
	}
	return changes, changed, deleted
}

// requeue puts a refused batch back, held off for busyRetry so that a long
// index pass is not re-asked on every tick.
func (w *Watcher) requeue(repoID string, b *batch) {
	w.mu.Lock()
	defer w.mu.Unlock()
	cur, ok := w.pending[repoID]
	if !ok {
		b.after = time.Now().Add(busyRetry)
		w.pending[repoID] = b
		return
	}
	// Events arrived while the batch was out; they belong to the same work.
	for p := range b.files {
		cur.files[p] = true
	}
	for d := range b.dirs {
		cur.dirs[d] = true
	}
	if b.first.Before(cur.first) {
		cur.first = b.first
	}
	cur.after = time.Now().Add(busyRetry)
}

func (w *Watcher) treeByID(repoID string) *tree {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, t := range w.trees {
		if t.repo.ID == repoID {
			return t
		}
	}
	return nil
}
