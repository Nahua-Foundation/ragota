// Package status is the in-process surface an interactive front end reads:
// which repository is being indexed and how far along it is, when a pass
// started and finished, what the watcher just did, and the recent warnings and
// errors.
//
// A Bus is optional wherever it is published to. Every method is a no-op on a
// nil receiver, so the server — which normally runs without one — costs
// nothing, and no publisher needs a nil check around its call. That is the
// whole reason the publishers take *Bus rather than an interface: an interface
// holding a nil *Bus is not nil, and the first publish would panic.
//
// Nothing here talks to the network or to disk. The log is a ring buffer and
// the repository table has one entry per registered repository, so a process
// that runs for weeks holds a bounded amount of it.
package status

import (
	"log/slog"
	"sort"
	"sync"
	"time"
)

// DefaultLogCapacity is how many log entries a Bus keeps when NewBus is given
// no capacity of its own.
const DefaultLogCapacity = 512

// Phase is what a repository is doing. It is deliberately the same vocabulary
// as repos.Status plus "queued", which only this surface can see: the storage
// row goes straight from idle to indexing, while a front end wants to show the
// repositories waiting their turn.
type Phase string

// Repository phases reported by Snapshot.
const (
	PhaseQueued   Phase = "queued"
	PhaseIndexing Phase = "indexing"
	PhaseIdle     Phase = "idle"
	PhaseError    Phase = "error"
)

// RepoState is everything the surface knows about one repository.
//
// Files/Total describe the pass currently running and are reset when one
// starts; Indexed/Failed describe the last pass that finished. A front end
// that renders a progress bar wants the first pair, one that renders a result
// column wants the second, and conflating them made "0 files" mean both "not
// started" and "finished with nothing to do".
type RepoState struct {
	ID      string
	Name    string
	Path    string
	Phase   Phase
	Watched bool // a filesystem watcher is following this repository

	Files int // files handed to the indexers so far in the running pass
	Total int // files the running pass will visit; 0 until it is known

	Indexed int // files indexed by the last finished pass
	Failed  int // files that pass could not index

	StartedAt  time.Time // when the running (or last) pass started
	FinishedAt time.Time // when the last pass finished; zero while one runs
	Err        string    // failure of the last pass, empty when it succeeded
}

// Entry is one line of the bounded log. Level uses slog's levels rather than a
// parallel vocabulary, because most entries arrive from a slog handler.
type Entry struct {
	At     time.Time
	Level  slog.Level
	Msg    string
	RepoID string // empty for process-level entries
}

// Metrics are the process-wide counters a front end shows as a header line.
// They only ever go up, so a front end may render deltas between snapshots.
type Metrics struct {
	Repos        int   // repositories registered with the bus
	Passes       int   // index passes that finished, successfully or not
	FilesIndexed int64 // files indexed across every finished pass
	WatchBatches int   // debounced batches the watcher applied
	WatchChanged int64 // files the watcher re-indexed or added
	WatchDeleted int64 // files the watcher removed from the index
	Warnings     int
	Errors       int
}

// Snapshot is a consistent copy of the whole surface, safe to read without
// holding anything. Repos is sorted by name and Log runs oldest first.
type Snapshot struct {
	StartedAt time.Time
	Repos     []RepoState
	// Dormant counts the registered repositories the run is not about, which
	// are deliberately absent from Repos. A front end shows the working set and
	// says how much of the index it is leaving out; see ReposDormant.
	Dormant int
	Log     []Entry
	Metrics Metrics
}

// Bus collects status from the indexing path and the watcher and hands it out
// as snapshots. It is safe for concurrent publish and read.
type Bus struct {
	startedAt time.Time

	mu      sync.Mutex
	repos   map[string]*RepoState
	order   []string // registration order, so ties in Snapshot's sort are stable
	dormant int      // registered repositories outside the working set
	log     []Entry  // ring buffer; len is the capacity once it has filled
	logNext int      // index the next entry is written to
	logLen  int      // entries currently held
	metrics Metrics
	subs    map[int]chan struct{}
	nextSub int
}

// NewBus returns a Bus keeping the last logCapacity log entries. A capacity of
// zero or less means DefaultLogCapacity.
func NewBus(logCapacity int) *Bus {
	if logCapacity <= 0 {
		logCapacity = DefaultLogCapacity
	}
	return &Bus{
		startedAt: time.Now(),
		repos:     make(map[string]*RepoState),
		log:       make([]Entry, logCapacity),
		subs:      make(map[int]chan struct{}),
	}
}

// RepoRegistered announces a repository the surface should track. Calling it
// again for the same id updates the name and path without disturbing the
// phase, so a re-registration does not erase a pass in flight.
func (b *Bus) RepoRegistered(id, name, path string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	st := b.state(id)
	st.Name, st.Path = name, path
	b.mu.Unlock()
	b.notify()
}

// ReposDormant records how many registered repositories are outside the working
// set the run is about.
//
// A count rather than rows, and deliberately: a dormant repository is one
// nothing will happen to, so a row for it would sit in the table forever
// looking like it was waiting its turn. The count is here so that a front end
// can say how much of the index it is not showing, which is the one thing about
// them worth a line.
func (b *Bus) ReposDormant(n int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.dormant = n
	b.mu.Unlock()
	b.notify()
}

// IndexQueued marks a repository as waiting for an index pass.
func (b *Bus) IndexQueued(id string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.state(id).Phase = PhaseQueued
	b.mu.Unlock()
	b.notify()
}

// IndexStarted marks the beginning of a pass over total files; pass 0 when the
// count is not yet known and report it later with IndexProgress.
func (b *Bus) IndexStarted(id string, total int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	st := b.state(id)
	st.Phase = PhaseIndexing
	st.Files, st.Total = 0, total
	st.StartedAt, st.FinishedAt = time.Now(), time.Time{}
	st.Err = ""
	name := st.Name
	b.appendLog(Entry{At: time.Now(), Level: slog.LevelInfo, RepoID: id,
		Msg: "indexing " + name})
	b.mu.Unlock()
	b.notify()
}

// IndexProgress reports that files of total have been handed to the indexers.
// A total of 0 leaves the previously reported one in place.
func (b *Bus) IndexProgress(id string, files, total int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	st := b.state(id)
	st.Files = files
	if total > 0 {
		st.Total = total
	}
	b.mu.Unlock()
	b.notify()
}

// IndexFinished records how a pass ended. err nil means it succeeded, which is
// not the same as "nothing failed": a pass reports the files it could not
// index through failed and still returns an error describing them.
func (b *Bus) IndexFinished(id string, indexed, failed int, err error) {
	if b == nil {
		return
	}
	now := time.Now()
	b.mu.Lock()
	st := b.state(id)
	st.Indexed, st.Failed = indexed, failed
	st.FinishedAt = now
	if st.Total == 0 {
		st.Total = indexed + failed
	}
	st.Files = st.Total
	level := slog.LevelInfo
	msg := "indexed " + st.Name
	if err != nil {
		st.Phase, st.Err = PhaseError, err.Error()
		level, msg = slog.LevelError, "index failed: "+err.Error()
	} else {
		st.Phase, st.Err = PhaseIdle, ""
	}
	b.metrics.Passes++
	b.metrics.FilesIndexed += int64(indexed)
	b.appendLog(Entry{At: now, Level: level, RepoID: id, Msg: msg})
	b.mu.Unlock()
	b.notify()
}

// Watching says whether a filesystem watcher is following a repository.
func (b *Bus) Watching(id string, on bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.state(id).Watched = on
	b.mu.Unlock()
	b.notify()
}

// WatchApplied records one debounced batch the watcher pushed into the index.
func (b *Bus) WatchApplied(id string, changed, deleted int, err error) {
	if b == nil {
		return
	}
	now := time.Now()
	b.mu.Lock()
	st := b.state(id)
	b.metrics.WatchBatches++
	b.metrics.WatchChanged += int64(changed)
	b.metrics.WatchDeleted += int64(deleted)
	level, msg := slog.LevelInfo, watchMsg(st.Name, changed, deleted)
	if err != nil {
		level, msg = slog.LevelError, "watch: "+err.Error()
		st.Phase, st.Err = PhaseError, err.Error()
	} else {
		st.Phase, st.Err = PhaseIdle, ""
		st.FinishedAt = now
	}
	b.appendLog(Entry{At: now, Level: level, RepoID: id, Msg: msg})
	b.mu.Unlock()
	b.notify()
}

func watchMsg(name string, changed, deleted int) string {
	return "watch " + name + ": " +
		itoa(changed) + " changed, " + itoa(deleted) + " deleted"
}

// itoa avoids pulling strconv in for two call sites in a package whose whole
// point is to stay small; the counts are file counts, always non-negative.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// Log appends one entry. repoID may be empty for process-level messages.
func (b *Bus) Log(level slog.Level, repoID, msg string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.appendLog(Entry{At: time.Now(), Level: level, RepoID: repoID, Msg: msg})
	b.mu.Unlock()
	b.notify()
}

// Snapshot returns a copy of the whole surface.
func (b *Bus) Snapshot() Snapshot {
	if b == nil {
		return Snapshot{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	snap := Snapshot{StartedAt: b.startedAt, Dormant: b.dormant, Metrics: b.metrics}
	snap.Metrics.Repos = len(b.repos)
	snap.Repos = make([]RepoState, 0, len(b.repos))
	for _, id := range b.order {
		if st, ok := b.repos[id]; ok {
			snap.Repos = append(snap.Repos, *st)
		}
	}
	sort.SliceStable(snap.Repos, func(i, j int) bool { return snap.Repos[i].Name < snap.Repos[j].Name })

	snap.Log = make([]Entry, 0, b.logLen)
	start := (b.logNext - b.logLen + len(b.log)) % len(b.log)
	for i := 0; i < b.logLen; i++ {
		snap.Log = append(snap.Log, b.log[(start+i)%len(b.log)])
	}
	return snap
}

// Subscribe returns a channel that receives a value whenever the surface
// changes, and a function that cancels the subscription. Sends are coalescing
// and never block: a subscriber that is redrawing misses no change, it just
// sees several of them as one wakeup.
//
// On a nil Bus the channel is nil, which blocks forever in a select rather
// than spinning — a reader with no bus has nothing to wait for.
func (b *Bus) Subscribe() (<-chan struct{}, func()) {
	if b == nil {
		return nil, func() {}
	}
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	id := b.nextSub
	b.nextSub++
	b.subs[id] = ch
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if sub, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(sub)
		}
		b.mu.Unlock()
	}
}

// state returns the entry for id, creating it on first mention. Callers hold
// b.mu.
func (b *Bus) state(id string) *RepoState {
	if st, ok := b.repos[id]; ok {
		return st
	}
	st := &RepoState{ID: id, Name: id, Phase: PhaseIdle}
	b.repos[id] = st
	b.order = append(b.order, id)
	return st
}

// appendLog writes one entry into the ring and counts it. Callers hold b.mu.
func (b *Bus) appendLog(e Entry) {
	b.log[b.logNext] = e
	b.logNext = (b.logNext + 1) % len(b.log)
	if b.logLen < len(b.log) {
		b.logLen++
	}
	switch {
	case e.Level >= slog.LevelError:
		b.metrics.Errors++
	case e.Level >= slog.LevelWarn:
		b.metrics.Warnings++
	}
}

// notify wakes every subscriber. It runs outside b.mu so that a subscriber
// calling Snapshot from its wakeup cannot deadlock against the publisher.
func (b *Bus) notify() {
	b.mu.Lock()
	subs := make([]chan struct{}, 0, len(b.subs))
	for _, ch := range b.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
