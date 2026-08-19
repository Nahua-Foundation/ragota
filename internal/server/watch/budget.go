package watch

import (
	"log/slog"
	"os"
	"sync"
	"syscall"
)

// A watch costs file descriptors on every BSD-derived system, macOS included,
// because kqueue identifies what it watches by an open fd — and it needs one
// per *entry*, not per directory, since that is how it notices a file appearing
// beside the ones it already knows. Linux's inotify does not: one descriptor
// covers the whole watcher. So the cost is invisible wherever most of this is
// developed and lands on whoever points --source at a real tree.
//
// The arithmetic is not close. macOS caps a process at kern.maxfilesperproc,
// 92160 on a current machine; the benchmark corpus alone is 41874 directories
// holding roughly half a million files.
//
// Unbounded, that is not slow degradation but process-wide failure, because the
// descriptors are shared with everything else. Measured, in order: the listener
// could not open a socket ("listen tcp 127.0.0.1:8080: socket: too many open
// files"), and before the ordering was fixed, os/signal.Notify could not
// allocate its pipe — which is `fatal error: pipe failed`, a runtime throw
// rather than an error any caller could handle. The watcher took the last
// descriptor and the process died at the call that would have let the user
// interrupt it.
//
// Counting directories was the first attempt and it was the wrong unit: a
// budget of "half the limit, in directories" never triggered while the real
// consumption ran an order of magnitude ahead of it. So the budget measures
// what actually runs out — the descriptors this process holds — and stops
// while there are still enough left for the work the watcher exists to serve.

const (
	// reserveFDs stays free for everything that is not a watch: the listener,
	// SQLite, the BM25 segment files a merge opens all at once, the runtime's
	// own pipes. Generous on purpose — a watcher that follows slightly less of
	// a tree is a smaller loss than a server that cannot accept a connection.
	reserveFDs = 8192

	// defaultMaxWatchedDirs is the real bound, and it is a constant rather than
	// a calculation because the calculation cannot be made honest: a watch's
	// cost is the number of entries in the directory, which varies by two
	// orders of magnitude within one tree, so any schedule derived from what
	// the previous directories cost is wrong about the next one. Measured: an
	// adaptive version sailed past a ceiling of 83968 and ended holding 92170
	// descriptors, because it had calibrated on small directories and then
	// walked into large ones.
	//
	// 8192 is chosen against real trees rather than theory — the largest single
	// repository in the benchmark corpus is airflow at 3576 directories, and
	// the corpus entire is 41874, which nobody should be watching. A tree
	// larger than this is one where full index passes are the right mechanism
	// and the watcher is not.
	defaultMaxWatchedDirs = 8192

	// checkEvery samples the descriptor count often enough that the constant
	// above is a bound and not a hope. Small enough that even implausibly large
	// directories cannot cross the reserve between two samples, and cheap
	// because the count it reads is bounded by the same constant.
	checkEvery = 64

	// minWatchBudget applies where the limit is too small to divide usefully.
	// Following a handful of directories still beats following none.
	minWatchBudget = 64
)

// openDescriptors reports how many descriptors this process currently holds.
// /dev/fd is the portable-enough answer on Darwin and Linux both; a platform
// that does not have it reports 0, which turns the sampling into a no-op and
// leaves the hard ceiling below as the only bound.
func openDescriptors() int {
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return 0
	}
	// The ReadDir handle is itself one of the entries it counts; the reserve is
	// far larger than that error.
	return len(entries)
}

// descriptorLimit reports the effective per-process ceiling. Note this is read
// after the Go runtime has already raised the soft limit to the hard one at
// startup, which is why a shell's `ulimit -n` does not appear here.
func descriptorLimit() int {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return minWatchBudget + reserveFDs
	}
	if lim.Cur > 1<<31 {
		return 1 << 31
	}
	return int(lim.Cur)
}

// budget decides whether one more watch may be installed, across every
// repository the watcher follows: a second repository must not be able to spend
// what the first left, and a tree that grows while it is watched must not creep
// past the ceiling either.
type budget struct {
	mu sync.Mutex
	// ceiling is the number of descriptors this process may hold before the
	// watcher stops asking for more.
	ceiling int
	// hardMax, when set, caps the number of watches regardless of descriptors.
	// It exists for tests and for an operator who wants a smaller appetite than
	// the machine allows.
	hardMax int

	granted   int
	skipped   int
	exhausted bool
	warned    bool

	sinceCheck int
}

func newBudget(hardMax int) *budget {
	limit := descriptorLimit()
	ceiling := limit - reserveFDs
	if ceiling < minWatchBudget {
		ceiling = minWatchBudget
	}
	if hardMax <= 0 {
		hardMax = defaultMaxWatchedDirs
	}
	return &budget{ceiling: ceiling, hardMax: hardMax}
}

// take reports whether one more watch may be installed, counting refusals so
// the shortfall can be reported once rather than once per directory.
func (b *budget) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.hardMax > 0 && b.granted >= b.hardMax {
		b.skipped++
		return false
	}
	if b.exhausted {
		b.skipped++
		return false
	}
	// The descriptor check is the second bound, for the machine whose limit is
	// smaller than the constant assumes — a container, or a shell that lowered
	// it. It samples on a fixed short interval rather than a predicted one, for
	// the reason in the comment on defaultMaxWatchedDirs.
	if b.sinceCheck <= 0 {
		b.sinceCheck = checkEvery
		if n := openDescriptors(); n > 0 && n >= b.ceiling {
			b.exhausted = true
			b.skipped++
			return false
		}
	}
	b.sinceCheck--
	b.granted++
	return true
}

// release returns capacity when a watch is dropped, so a directory removed and
// recreated does not slowly consume the budget.
func (b *budget) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.granted > 0 {
		b.granted--
	}
	// A released descriptor may be what put the process back under the ceiling,
	// so the next take is allowed to sample again rather than staying refused
	// forever on one bad moment.
	b.exhausted = false
	b.sinceCheck = 0
}

// report logs once, the first time anything was refused. The alternative is a
// warning per directory, which on a large tree is thousands of identical lines
// burying the one fact worth reading.
func (b *budget) report(repoID string) {
	b.mu.Lock()
	skipped, warned, granted, ceiling := b.skipped, b.warned, b.granted, b.ceiling
	if skipped > 0 {
		b.warned = true
	}
	b.mu.Unlock()

	if skipped == 0 || warned {
		return
	}
	slog.Warn("watch: tree too large to follow completely; changes under the directories left unwatched are picked up by the next full index pass",
		"repo_id", repoID, "watched", granted, "not_watched", skipped, "fd_ceiling", ceiling,
		"remedy", "narrow repos.ignore, point --source at less, or raise the per-process file limit")
}

// stats returns what the budget has granted and refused.
func (b *budget) stats() (granted, skipped, ceiling int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.granted, b.skipped, b.ceiling
}
