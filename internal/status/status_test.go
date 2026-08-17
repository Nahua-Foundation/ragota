package status

import (
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// A nil Bus is the server's normal state, so every method has to work on one.
// If any of them needed a guard at the call site, the call site that forgets it
// is a panic in production and nowhere else.
func TestNilBusIsANoOp(t *testing.T) {
	var b *Bus

	b.RepoRegistered("id", "name", "/path")
	b.ReposDormant(3)
	b.IndexQueued("id")
	b.IndexStarted("id", 10)
	b.IndexProgress("id", 5, 10)
	b.IndexFinished("id", 5, 0, nil)
	b.Watching("id", true)
	b.WatchApplied("id", 1, 2, errors.New("boom"))
	b.Log(slog.LevelWarn, "id", "message")

	if snap := b.Snapshot(); len(snap.Repos) != 0 || len(snap.Log) != 0 {
		t.Errorf("Snapshot() of a nil bus = %+v, want the zero value", snap)
	}
	ch, cancel := b.Subscribe()
	if ch != nil {
		t.Error("Subscribe() on a nil bus returned a channel; a nil one blocks instead of spinning")
	}
	cancel()
}

// The repositories outside the working set are a count and never rows: a front
// end shows what the run is about and says how much of the index that leaves
// out. Publishing them as repositories would put a row in the table that
// nothing ever happens to.
func TestDormantIsACountAndNotRows(t *testing.T) {
	b := NewBus(8)
	b.RepoRegistered("r1", "alpha", "/src/alpha")
	b.ReposDormant(19)

	snap := b.Snapshot()
	if snap.Dormant != 19 {
		t.Errorf("Dormant = %d, want 19", snap.Dormant)
	}
	if len(snap.Repos) != 1 || snap.Metrics.Repos != 1 {
		t.Errorf("repos = %+v (metric %d), want only the one that was registered",
			snap.Repos, snap.Metrics.Repos)
	}

	b.ReposDormant(0)
	if snap := b.Snapshot(); snap.Dormant != 0 {
		t.Errorf("Dormant = %d after the set widened, want 0", snap.Dormant)
	}
}

func TestIndexLifecycle(t *testing.T) {
	b := NewBus(8)
	b.RepoRegistered("r1", "alpha", "/src/alpha")

	b.IndexQueued("r1")
	if got := repoState(t, b, "r1").Phase; got != PhaseQueued {
		t.Errorf("phase = %q, want %q", got, PhaseQueued)
	}

	b.IndexStarted("r1", 100)
	st := repoState(t, b, "r1")
	if st.Phase != PhaseIndexing || st.Total != 100 || st.Files != 0 {
		t.Errorf("after IndexStarted: %+v, want indexing 0/100", st)
	}
	if st.StartedAt.IsZero() {
		t.Error("StartedAt was not set")
	}

	b.IndexProgress("r1", 40, 100)
	if got := repoState(t, b, "r1").Files; got != 40 {
		t.Errorf("files = %d, want 40", got)
	}

	b.IndexFinished("r1", 98, 2, nil)
	st = repoState(t, b, "r1")
	if st.Phase != PhaseIdle || st.Indexed != 98 || st.Failed != 2 {
		t.Errorf("after IndexFinished: %+v, want idle 98/2", st)
	}
	if st.FinishedAt.IsZero() {
		t.Error("FinishedAt was not set")
	}

	m := b.Snapshot().Metrics
	if m.Passes != 1 || m.FilesIndexed != 98 || m.Repos != 1 {
		t.Errorf("metrics = %+v, want 1 pass, 98 files, 1 repo", m)
	}
}

func TestIndexFailureIsRecorded(t *testing.T) {
	b := NewBus(8)
	b.RepoRegistered("r1", "alpha", "/src/alpha")
	b.IndexStarted("r1", 3)
	b.IndexFinished("r1", 0, 3, errors.New("embedder unreachable"))

	st := repoState(t, b, "r1")
	if st.Phase != PhaseError {
		t.Errorf("phase = %q, want %q", st.Phase, PhaseError)
	}
	if st.Err != "embedder unreachable" {
		t.Errorf("err = %q, want the failure text", st.Err)
	}
	if got := b.Snapshot().Metrics.Errors; got != 1 {
		t.Errorf("errors = %d, want 1", got)
	}
}

// A pass that starts again must describe the new pass, not carry the old one's
// result in the fields a progress bar reads.
func TestIndexStartedResetsTheRunningCounters(t *testing.T) {
	b := NewBus(8)
	b.IndexStarted("r1", 10)
	b.IndexFinished("r1", 10, 0, errors.New("failed late"))
	b.IndexStarted("r1", 20)

	st := repoState(t, b, "r1")
	if st.Files != 0 || st.Total != 20 || st.Err != "" || !st.FinishedAt.IsZero() {
		t.Errorf("after a restart: %+v, want a clean 0/20 with no error", st)
	}
}

// The log is bounded: a process that runs for weeks must not accumulate one.
func TestLogRingDropsTheOldest(t *testing.T) {
	b := NewBus(3)
	for _, msg := range []string{"one", "two", "three", "four", "five"} {
		b.Log(slog.LevelInfo, "", msg)
	}

	log := b.Snapshot().Log
	if len(log) != 3 {
		t.Fatalf("log holds %d entries, want the capacity of 3", len(log))
	}
	for i, want := range []string{"three", "four", "five"} {
		if log[i].Msg != want {
			t.Errorf("log[%d] = %q, want %q (oldest first)", i, log[i].Msg, want)
		}
	}
}

func TestWatchAppliedCounts(t *testing.T) {
	b := NewBus(8)
	b.RepoRegistered("r1", "alpha", "/src/alpha")
	b.Watching("r1", true)
	b.WatchApplied("r1", 3, 1, nil)
	b.WatchApplied("r1", 2, 0, nil)

	if !repoState(t, b, "r1").Watched {
		t.Error("the repository is not marked as watched")
	}
	m := b.Snapshot().Metrics
	if m.WatchBatches != 2 || m.WatchChanged != 5 || m.WatchDeleted != 1 {
		t.Errorf("metrics = %+v, want 2 batches, 5 changed, 1 deleted", m)
	}
}

func TestSubscribeWakesOnChange(t *testing.T) {
	b := NewBus(8)
	ch, cancel := b.Subscribe()
	defer cancel()

	b.RepoRegistered("r1", "alpha", "/src/alpha")
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("no wakeup after a publish")
	}

	// Coalescing: a burst must not block the publisher on a subscriber that is
	// busy redrawing.
	for i := 0; i < 100; i++ {
		b.IndexProgress("r1", i, 100)
	}
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("no wakeup after a burst")
	}

	cancel()
	if _, ok := <-ch; ok {
		t.Error("the channel is still open after the subscription was cancelled")
	}
}

// Publishing and reading run concurrently in the real process: the indexer, the
// watcher and a front end's redraw loop all touch the bus at once.
func TestConcurrentPublishAndRead(t *testing.T) {
	b := NewBus(16)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				b.IndexProgress("r1", i, 200)
				b.Log(slog.LevelWarn, "r1", "noise")
				b.WatchApplied("r1", 1, 0, nil)
			}
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 400; i++ {
			_ = b.Snapshot()
		}
	}()
	wg.Wait()

	if got := b.Snapshot().Metrics.WatchBatches; got != 800 {
		t.Errorf("watch batches = %d, want 800", got)
	}
}

func repoState(t *testing.T, b *Bus, id string) RepoState {
	t.Helper()
	for _, r := range b.Snapshot().Repos {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no state for repository %q", id)
	return RepoState{}
}
