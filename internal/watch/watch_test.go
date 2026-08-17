// The watcher tests drive a real Service over in-memory SQLite with the real
// AST indexer behind it, and assert on what the index holds afterwards.
//
// That is the point of them. A test that waits for an fsnotify event proves
// fsnotify works, which was never in question; what this feature claims is
// that a file saved in an editor becomes a symbol the index can answer with,
// and only the index can be asked whether that happened.
package watch_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/service"
	"github.com/Nahua-Foundation/ragota/internal/setup"
	"github.com/Nahua-Foundation/ragota/internal/status"
	"github.com/Nahua-Foundation/ragota/internal/storage"
	"github.com/Nahua-Foundation/ragota/internal/testutil"
	"github.com/Nahua-Foundation/ragota/internal/watch"
)

// Short intervals so the tests are not mostly sleeping. The policy under test
// is the shape — quiet period, capped — not the production numbers.
const (
	testDebounce = 40 * time.Millisecond
	testMaxDelay = 200 * time.Millisecond
	testTimeout  = 20 * time.Second
)

// fixture is a wired service, an indexed repository and a running watcher.
type fixture struct {
	svc  *service.Service
	repo *repos.Repo
	dir  string
	bus  *status.Bus
}

// newFixture lays out repoFiles under a git-marked directory, indexes it in
// full, and starts a watcher over it.
func newFixture(t *testing.T, repoFiles map[string]string, ignore []string) *fixture {
	t.Helper()
	t.Setenv("RAGOTA_BM25_PATH", t.TempDir())

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range repoFiles {
		writeFile(t, dir, name, body)
	}

	cfg := testutil.TestConfig(t)
	cfg.Repos.Ignore = ignore
	if _, err := setup.ApplySource(cfg, dir); err != nil {
		t.Fatalf("ApplySource() error = %v", err)
	}
	svc, err := setup.Build(context.Background(), cfg)
	if err != nil {
		t.Fatalf("setup.Build() error = %v", err)
	}
	t.Cleanup(func() { _ = svc.Close(context.Background()) })

	bus := status.NewBus(64)
	svc.SetStatusBus(bus)

	ctx := context.Background()
	repo, err := svc.AddRepo(ctx, repos.SourceTypeLocal, &repos.AddRequest{Path: dir})
	if err != nil {
		t.Fatalf("AddRepo() error = %v", err)
	}
	if err := svc.IndexRepoSync(ctx, repo.ID, false); err != nil {
		t.Fatalf("IndexRepoSync() error = %v", err)
	}

	w, err := watch.New(svc, watch.Options{
		Debounce: testDebounce,
		MaxDelay: testMaxDelay,
		Bus:      bus,
	})
	if err != nil {
		t.Fatalf("watch.New() error = %v", err)
	}
	if err := w.Add(repo); err != nil {
		t.Fatalf("Watcher.Add() error = %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(runCtx)
	}()
	t.Cleanup(func() {
		cancel()
		_ = w.Close()
		<-done
	})

	return &fixture{svc: svc, repo: repo, dir: dir, bus: bus}
}

func writeFile(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// hasSymbol asks the index whether the repository defines a symbol by that
// exact name. GetASTUnits rather than Symbols: the latter widens to a
// substring fallback, which would let "Renamed" answer for "Rename".
func (f *fixture) hasSymbol(t *testing.T, name string) bool {
	t.Helper()
	units, err := f.svc.GetASTUnits(context.Background(), storage.QueryOpts{
		RepoID: f.repo.ID, Name: name, Limit: 10,
	})
	if err != nil {
		t.Fatalf("GetASTUnits(%q) error = %v", name, err)
	}
	return len(units) > 0
}

func (f *fixture) indexedPaths(t *testing.T) []string {
	t.Helper()
	paths, err := f.svc.IndexedPathsUnder(context.Background(), f.repo.ID, "")
	if err != nil {
		t.Fatalf("IndexedPathsUnder() error = %v", err)
	}
	return paths
}

// waitFor polls cond until it holds. The watcher is asynchronous by design, so
// every assertion about it is an assertion about a state the index reaches.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", testTimeout, what)
}

func goFile(pkg, fn string) string {
	return "package " + pkg + "\n\nfunc " + fn + "() string { return \"" + fn + "\" }\n"
}

// TestWatcherAppliesCreateModifyAndDelete is the feature: a file that appears
// is indexed, a file that changes is re-indexed, and a file that goes away
// stops being answerable.
func TestWatcherAppliesCreateModifyAndDelete(t *testing.T) {
	f := newFixture(t, map[string]string{
		"src/keep.go": goFile("src", "KeptSymbol"),
		"src/edit.go": goFile("src", "OriginalSymbol"),
	}, nil)

	if !f.hasSymbol(t, "KeptSymbol") || !f.hasSymbol(t, "OriginalSymbol") {
		t.Fatal("the full pass indexed nothing; the watcher assertions below would prove nothing")
	}

	// Create.
	writeFile(t, f.dir, "src/added.go", goFile("src", "AddedSymbol"))
	waitFor(t, "the created file to be indexed", func() bool {
		return f.hasSymbol(t, "AddedSymbol")
	})

	// Modify. The new content must replace the old rather than accumulate
	// beside it, which is what the stale-copy handling in the incremental pass
	// is for.
	writeFile(t, f.dir, "src/edit.go", goFile("src", "RewrittenSymbol"))
	waitFor(t, "the modified file to be re-indexed", func() bool {
		return f.hasSymbol(t, "RewrittenSymbol")
	})
	if f.hasSymbol(t, "OriginalSymbol") {
		t.Error("OriginalSymbol survived the rewrite; the old units were not removed")
	}

	// Delete.
	if err := os.Remove(filepath.Join(f.dir, "src", "keep.go")); err != nil {
		t.Fatal(err)
	}
	// Both halves of the removal, not just the symbols: an incremental pass
	// drops a path's units before its file row, so waiting on the symbol alone
	// would read the index in the middle of one batch.
	waitFor(t, "the deleted file to leave the index", func() bool {
		return !f.hasSymbol(t, "KeptSymbol") &&
			!slices.Contains(f.indexedPaths(t), filepath.Join("src", "keep.go"))
	})

	paths := f.indexedPaths(t)
	want := map[string]bool{
		filepath.Join("src", "added.go"): true,
		filepath.Join("src", "edit.go"):  true,
	}
	if len(paths) != len(want) {
		t.Errorf("indexed paths = %v, want exactly %v", paths, want)
	}
	for _, p := range paths {
		if !want[p] {
			t.Errorf("unexpected indexed path %q", p)
		}
	}
}

// A rename is a remove plus a create, and nothing in the watcher treats the
// two halves as connected — which is what makes a move behave correctly even
// when the destination is outside the watched tree.
func TestWatcherAppliesRename(t *testing.T) {
	f := newFixture(t, map[string]string{
		"src/before.go": goFile("src", "MovedSymbol"),
	}, nil)
	if !f.hasSymbol(t, "MovedSymbol") {
		t.Fatal("the full pass indexed nothing")
	}

	if err := os.Rename(
		filepath.Join(f.dir, "src", "before.go"),
		filepath.Join(f.dir, "src", "after.go"),
	); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the renamed file to be indexed under its new path", func() bool {
		for _, p := range f.indexedPaths(t) {
			if p == filepath.Join("src", "after.go") {
				return true
			}
		}
		return false
	})
	waitFor(t, "the old path to leave the index", func() bool {
		for _, p := range f.indexedPaths(t) {
			if p == filepath.Join("src", "before.go") {
				return false
			}
		}
		return true
	})
	if !f.hasSymbol(t, "MovedSymbol") {
		t.Error("MovedSymbol is gone; the create half of the rename did not land")
	}
}

// A directory that is moved away is reported by the operating system as one
// event about the directory, with nothing left on disk to enumerate. The files
// that were in it still have to leave the index.
func TestWatcherAppliesDirectoryRemoval(t *testing.T) {
	f := newFixture(t, map[string]string{
		"pkg/gone/a.go": goFile("gone", "DoomedA"),
		"pkg/gone/b.go": goFile("gone", "DoomedB"),
		"pkg/stay/c.go": goFile("stay", "SurvivingC"),
	}, nil)
	if !f.hasSymbol(t, "DoomedA") || !f.hasSymbol(t, "SurvivingC") {
		t.Fatal("the full pass indexed nothing")
	}

	// Renamed out of the tree rather than deleted: rm removes the files one by
	// one and would produce an event per file, which is the easy case.
	if err := os.Rename(
		filepath.Join(f.dir, "pkg", "gone"),
		filepath.Join(t.TempDir(), "gone"),
	); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the moved directory's files to leave the index", func() bool {
		return !f.hasSymbol(t, "DoomedA") && !f.hasSymbol(t, "DoomedB")
	})
	if !f.hasSymbol(t, "SurvivingC") {
		t.Error("SurvivingC was removed too; the deletion was not scoped to the directory")
	}
}

// A directory that appears brings its contents with it — a branch switch, an
// unpacked archive, a move — and nothing is watching inside it yet.
func TestWatcherFollowsNewDirectories(t *testing.T) {
	f := newFixture(t, map[string]string{
		"src/root.go": goFile("src", "RootSymbol"),
	}, nil)

	staging := t.TempDir()
	writeFile(t, staging, "arrived/nested/deep.go", goFile("nested", "ArrivedDeepSymbol"))
	if err := os.Rename(
		filepath.Join(staging, "arrived"),
		filepath.Join(f.dir, "arrived"),
	); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the files inside the new directory to be indexed", func() bool {
		return f.hasSymbol(t, "ArrivedDeepSymbol")
	})

	// The subtree is now followed, not merely swept once.
	writeFile(t, f.dir, "arrived/nested/later.go", goFile("nested", "LaterSymbol"))
	waitFor(t, "a file created inside the new directory to be indexed", func() bool {
		return f.hasSymbol(t, "LaterSymbol")
	})
}

// The watcher must exclude exactly what the walker excludes. Indexing what a
// full pass would drop puts entries in the index that the next full pass
// deletes again, and the disagreement is invisible until then.
func TestWatcherHonoursIgnorePatterns(t *testing.T) {
	f := newFixture(t, map[string]string{
		"src/app.go": goFile("src", "AppSymbol"),
	}, []string{"**/node_modules/**"})

	writeFile(t, f.dir, "node_modules/pkg/index.go", goFile("pkg", "VendoredSymbol"))
	writeFile(t, f.dir, "src/trigger.go", goFile("src", "TriggerSymbol"))

	// The trigger is the synchronization point: once it has landed, the batch
	// holding both files has been applied, so a missing VendoredSymbol means
	// excluded rather than not-yet-indexed.
	waitFor(t, "the batch to be applied", func() bool {
		return f.hasSymbol(t, "TriggerSymbol")
	})
	if f.hasSymbol(t, "VendoredSymbol") {
		t.Error("a file under node_modules reached the index; the watcher disagrees with the walker")
	}
}

// The checkout's own .gitignore is part of that same agreement: a build
// directory the repository excludes must not reach the index through the
// watcher either, and it must not cost a watch descriptor.
func TestWatcherHonoursGitignore(t *testing.T) {
	f := newFixture(t, map[string]string{
		".gitignore": "build/\n",
		"src/app.go": goFile("src", "GitignoreAppSymbol"),
	}, nil)

	writeFile(t, f.dir, "build/gen.go", goFile("build", "GeneratedBuildSymbol"))
	writeFile(t, f.dir, "src/trigger.go", goFile("src", "GitignoreTriggerSymbol"))

	waitFor(t, "the batch to be applied", func() bool {
		return f.hasSymbol(t, "GitignoreTriggerSymbol")
	})
	if f.hasSymbol(t, "GeneratedBuildSymbol") {
		t.Error("a file the repository's .gitignore excludes reached the index")
	}
}

// The exclusions now depend on a file inside the tree being watched, so an
// edit to that file has to take effect without a restart — the alternative is
// a user adding a directory to .gitignore and watching it get indexed anyway.
func TestWatcherPicksUpAGitignoreEdit(t *testing.T) {
	f := newFixture(t, map[string]string{
		"src/app.go": goFile("src", "EditAppSymbol"),
	}, nil)

	// Writing the rule and waiting for a file written beside it puts the
	// .gitignore event firmly in the past, without sleeping for it.
	writeFile(t, f.dir, ".gitignore", "secret/\n")
	writeFile(t, f.dir, "src/first.go", goFile("src", "EditFirstSymbol"))
	waitFor(t, "the .gitignore write to be processed", func() bool {
		return f.hasSymbol(t, "EditFirstSymbol")
	})

	writeFile(t, f.dir, "secret/key.go", goFile("secret", "SecretSymbol"))
	writeFile(t, f.dir, "src/second.go", goFile("src", "EditSecondSymbol"))
	waitFor(t, "the batch to be applied", func() bool {
		return f.hasSymbol(t, "EditSecondSymbol")
	})
	if f.hasSymbol(t, "SecretSymbol") {
		t.Error("the watcher is still using the .gitignore it read at startup")
	}
}

// The repository's own .ragota.yaml narrows what is indexed, and the watcher
// reads the same merged pattern set the indexing pass does.
func TestWatcherHonoursTheRepositoryManifest(t *testing.T) {
	f := newFixture(t, map[string]string{
		".ragota.yaml": "ignore:\n  - \"**/generated/**\"\n",
		"src/app.go":   goFile("src", "ManifestAppSymbol"),
	}, nil)

	writeFile(t, f.dir, "src/generated/stub.go", goFile("generated", "GeneratedStubSymbol"))
	writeFile(t, f.dir, "src/hand.go", goFile("src", "HandWrittenSymbol"))

	waitFor(t, "the batch to be applied", func() bool {
		return f.hasSymbol(t, "HandWrittenSymbol")
	})
	if f.hasSymbol(t, "GeneratedStubSymbol") {
		t.Error("a file the repository's .ragota.yaml excludes reached the index")
	}
}

// The status surface has to show what the watcher did, since that is what the
// interactive front end will render.
func TestWatcherPublishesToTheStatusBus(t *testing.T) {
	f := newFixture(t, map[string]string{
		"src/app.go": goFile("src", "BusAppSymbol"),
	}, nil)

	writeFile(t, f.dir, "src/new.go", goFile("src", "BusNewSymbol"))
	// Waiting on the bus rather than on the index: the batch is published once
	// it has been applied, and the units it wrote are visible before that.
	waitFor(t, "the watcher to publish its batch", func() bool {
		return f.bus.Snapshot().Metrics.WatchBatches > 0
	})
	if !f.hasSymbol(t, "BusNewSymbol") {
		t.Error("the published batch did not index the new file")
	}

	snap := f.bus.Snapshot()
	if snap.Metrics.WatchChanged == 0 {
		t.Error("the bus recorded no changed files")
	}
	var watched bool
	for _, r := range snap.Repos {
		if r.ID == f.repo.ID {
			watched = r.Watched
		}
	}
	if !watched {
		t.Error("the repository is not marked as watched")
	}
}
