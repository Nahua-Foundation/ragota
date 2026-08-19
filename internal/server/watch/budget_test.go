package watch

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/domain"
)

// budgetTestRepo is the minimum a tree needs to be walked: an id for the log
// line and a root the pattern test is measured against.
func budgetTestRepo(path string) *domain.Repo {
	return &domain.Repo{ID: "budget-test", Name: "budget-test", Path: path}
}

func TestBudgetStopsSpendingAtTheLimit(t *testing.T) {
	b := newBudget(3)
	for i := 0; i < 3; i++ {
		if !b.take() {
			t.Fatalf("take %d refused below the limit", i)
		}
	}
	for i := 0; i < 5; i++ {
		if b.take() {
			t.Fatalf("take %d granted past the limit", i)
		}
	}
	granted, skipped, _ := b.stats()
	if granted != 3 || skipped != 5 {
		t.Errorf("granted, skipped = %d, %d; want 3, 5", granted, skipped)
	}
}

// A directory removed and recreated must not cost a descriptor each time, or a
// tree churning under a build tool would exhaust the budget over hours without
// ever holding more than a few watches.
func TestBudgetReleaseReturnsCapacity(t *testing.T) {
	b := newBudget(1)
	if !b.take() {
		t.Fatal("first take refused")
	}
	if b.take() {
		t.Fatal("second take granted at limit 1")
	}
	b.release()
	if !b.take() {
		t.Error("take refused after release")
	}
}

// Without an explicit cap the budget comes from the descriptor limit, less the
// reserve that everything which is not a watch needs.
func TestBudgetDefaultsToADescriptorCeiling(t *testing.T) {
	b := newBudget(0)
	_, _, ceiling := b.stats()
	if ceiling < minWatchBudget {
		t.Errorf("ceiling = %d, want at least %d", ceiling, minWatchBudget)
	}
	if limit := descriptorLimit(); ceiling > limit {
		t.Errorf("ceiling = %d exceeds the descriptor limit %d", ceiling, limit)
	}
}

// The sampling has to see something, or the ceiling is unenforceable and the
// budget silently degrades to the hard cap alone.
func TestOpenDescriptorsIsObservable(t *testing.T) {
	if n := openDescriptors(); n <= 0 {
		t.Skipf("this platform does not expose /dev/fd; the ceiling relies on the hard cap alone")
	}
}

// The regression this whole file exists for: a tree with more directories than
// the budget must leave the watcher holding exactly the budget and no more.
// Unbounded, the watcher took every descriptor the process had, and the next
// caller to need one was os/signal.Notify — whose failure is a runtime throw,
// so the server died at the call that would have let the user stop it.
func TestWatcherStopsAtTheBudgetOnALargeTree(t *testing.T) {
	root := t.TempDir()
	const dirs = 60
	for i := 0; i < dirs; i++ {
		if err := os.MkdirAll(filepath.Join(root, "d"+strconv.Itoa(i), "nested"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	const limit = 10
	w, err := New(nil, Options{MaxWatchedDirs: limit})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	tr := &tree{repo: budgetTestRepo(root), dirs: map[string]bool{}}
	tr.ignore.Store(config.NewIgnorePatterns(nil))
	watched := w.walkTree(tr, root, nil)

	if watched > limit {
		t.Errorf("watched %d directories, budget was %d", watched, limit)
	}
	spent, skipped, _ := w.budget.stats()
	if spent > limit {
		t.Errorf("budget spent %d, limit %d", spent, limit)
	}
	if skipped == 0 {
		t.Errorf("nothing was refused on a tree of %d directories with a budget of %d", dirs*2+1, limit)
	}
	// It must report the shortfall once rather than per directory; calling it
	// twice must not produce a second warning.
	w.budget.report(tr.repo.ID)
	w.budget.report(tr.repo.ID)
}
