package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/index"
	"github.com/Nahua-Foundation/ragota/internal/lsp"
	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// IndexAck describes what a POST /index actually did, which differs between
// modes: in-process indexing has already started, whereas in distributed mode
// only a queue entry exists and any instance may pick it up later.
type IndexAck struct {
	Status     string `json:"status"`                // indexing | queued
	Queued     bool   `json:"queued"`                // true when the work is only enqueued
	JobID      string `json:"job_id,omitempty"`      // distributed mode
	JobStatus  string `json:"job_status,omitempty"`  // pending | running
	Force      bool   `json:"force"`                 // effective force flag (merged with a queued job)
	QueuedAt   int64  `json:"queued_at,omitempty"`   // unix seconds
	ClaimedBy  string `json:"claimed_by,omitempty"`  // worker that already picked the job up
	RepoStatus string `json:"repo_status,omitempty"` // repo status at the time of the ack
}

// IndexRepo indexes all files in a repository in the background and returns
// immediately. It is the error-only form of StartIndex.
func (s *Service) IndexRepo(ctx context.Context, repoID string, force bool) error {
	_, err := s.StartIndex(ctx, repoID, force)
	return err
}

// StartIndex starts (or enqueues) a full index pass and reports what happened.
// The repo is claimed atomically, so concurrent index requests cannot race.
// In distributed mode the request is enqueued into the shared job queue
// instead, and any instance's worker may pick it up.
func (s *Service) StartIndex(ctx context.Context, repoID string, force bool) (*IndexAck, error) {
	repo, err := s.store.GetRepo(ctx, repoID)
	if err != nil {
		return nil, err
	}

	if s.distributed() {
		job, err := s.store.EnqueueIndexJob(ctx, repoID, force)
		if err != nil {
			return nil, fmt.Errorf("enqueue index job: %w", err)
		}
		slog.Info("index job enqueued", "repo_id", repoID, "job_id", job.ID, "force", job.Force)
		return &IndexAck{
			Status:     "queued",
			Queued:     true,
			JobID:      job.ID,
			JobStatus:  job.Status,
			Force:      job.Force,
			QueuedAt:   job.CreatedAt,
			ClaimedBy:  job.ClaimedBy,
			RepoStatus: string(repo.Status),
		}, nil
	}

	claimed, err := s.store.ClaimRepoForIndexing(ctx, repoID, s.ownerID(), repoClaimTTLSeconds)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return nil, fmt.Errorf("%w: %s", ErrRepoBusy, repoID)
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		_ = s.runIndex(s.baseCtx, repo, force)
	}()

	return &IndexAck{Status: "indexing", Force: force, RepoStatus: string(domain.StatusIndexing)}, nil
}

// IndexRepoSync runs a full index pass and waits for it, unlike IndexRepo
// which returns as soon as the pass has been started or queued.
//
// It exists for a bulk load — `--source` registering a directory of
// repositories — where the point is to run the passes one after another. The
// asynchronous form would start every pass at once, and a pass holds a window
// of file contents in memory for as long as its indexers are working through
// it, so concurrent passes multiply peak memory to arrive no sooner.
//
// It is single-instance only by construction: it does the work here rather
// than enqueueing it, which is what "wait for it" has to mean.
func (s *Service) IndexRepoSync(ctx context.Context, repoID string, force bool) error {
	repo, err := s.store.GetRepo(ctx, repoID)
	if err != nil {
		return err
	}
	claimed, err := s.store.ClaimRepoForIndexing(ctx, repoID, s.ownerID(), repoClaimTTLSeconds)
	if err != nil {
		return err
	}
	if !claimed {
		return fmt.Errorf("%w: %s", ErrRepoBusy, repoID)
	}
	return s.runIndex(ctx, repo, force)
}

// runIndex executes one full index pass, records the resulting repo status
// and returns the indexing error (nil on success).
func (s *Service) runIndex(ctx context.Context, repo *domain.Repo, force bool) error {
	err := s.doIndex(ctx, repo, force)

	// The terminal status write is detached from ctx on purpose. Close cancels
	// the base context that indexing runs under, so writing the final status
	// through it would silently drop the write and leave the repo claimed as
	// "indexing" forever — the state nothing else clears at runtime.
	statusCtx, cancel := terminalCtx(ctx)
	defer cancel()

	// A cancelled pass was interrupted, not broken, and the difference outlives
	// the process: recording StatusError would leave a repository looking
	// failed — with "context canceled" as its last error — because somebody
	// stopped the server. It goes back to idle so the next pass picks it up as
	// the ordinary work it is.
	if errors.Is(err, context.Canceled) {
		slog.Info("index cancelled", "repo_id", repo.ID)
		if uerr := s.store.UpdateRepoStatus(statusCtx, repo.ID, domain.StatusIdle, "", repo.IndexedAt); uerr != nil {
			slog.Error("update repo status to idle failed", "repo_id", repo.ID, "err", uerr)
		}
		return err
	}
	if err != nil {
		slog.Error("index failed", "repo_id", repo.ID, "err", err)
		if uerr := s.store.UpdateRepoStatus(statusCtx, repo.ID, domain.StatusError, err.Error(), repo.IndexedAt); uerr != nil {
			slog.Error("update repo status to error failed", "repo_id", repo.ID, "err", uerr)
		}
		return err
	}
	if uerr := s.store.UpdateRepoStatus(statusCtx, repo.ID, domain.StatusIdle, "", time.Now().Unix()); uerr != nil {
		slog.Error("update repo status to idle failed", "repo_id", repo.ID, "err", uerr)
	}
	return nil
}

func (s *Service) doIndex(ctx context.Context, repo *domain.Repo, force bool) (retErr error) {
	indexStart := time.Now()
	indexed := 0
	failed := map[string]bool{}
	defer func() {
		indexRepoSeconds.Observe(time.Since(indexStart).Seconds())
		// Every way out of the pass comes through here, including the early
		// returns above the counters: a front end that stopped being told about
		// a repository would show it indexing forever.
		s.bus.IndexFinished(repo.ID, indexed-len(failed), len(failed), retErr)
	}()

	source, ok := s.sources[repo.Source]
	if !ok {
		return fmt.Errorf("source not available for repo %s", repo.ID)
	}

	files, err := source.GetFiles(ctx, repo, s.IgnorePatternsFor(repo))
	if err != nil {
		return fmt.Errorf("get files: %w", err)
	}
	// Published after the walk rather than at the top of the pass: the total is
	// what a progress bar needs, and until the tree has been walked there is no
	// honest number to give it.
	s.bus.IndexStarted(repo.ID, len(files))

	// One snapshot of what is already indexed serves the whole pass: the stale
	// sweep and the per-file "has this changed?" check both read it. The pass
	// holds the repo's indexing claim, so nothing else writes these rows while
	// it runs — which is what makes a snapshot as good as the per-file lookup
	// it replaces, at one round-trip instead of one per file.
	stored, err := s.store.GetFilesByRepo(ctx, repo.ID)
	if err != nil {
		// Not knowing what is indexed is a property of the store, not of any
		// file: swallowing it re-indexes the whole repository on every pass and
		// hides a database that has stopped answering.
		return fmt.Errorf("get indexed files: %w", err)
	}
	if err := s.removeStaleFiles(ctx, repo.ID, stored, files); err != nil {
		return fmt.Errorf("remove stale files: %w", err)
	}
	// nil means "re-index everything": a forced pass asks no questions.
	var known map[string]*store.File
	if !force {
		known = make(map[string]*store.File, len(stored))
		for _, f := range stored {
			known[f.Path] = f
		}
	}

	skipped := 0
	unreadable := 0
	var coverage coverageAccumulator
	// Summaries need file content, which the batch loop otherwise releases;
	// keep only as many candidates as the generator will actually consume.
	var summaryFiles []*index.FileToIndex
	spent := indexerTimes{}

	// Files are scanned and indexed in batches rather than all at once: the
	// scan holds every file's content in memory, so a whole-repo pass peaked at
	// several gigabytes on repositories with tens of thousands of files.
	// Batching bounds that to one window at a time.
	for start := 0; start < len(files); start += indexBatchFiles {
		end := min(start+indexBatchFiles, len(files))
		batch := files[start:end]

		scanned := make([]scannedFile, len(batch))
		jobs := make(chan int)
		var wg sync.WaitGroup
		for w := 0; w < s.indexWorkers(); w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range jobs {
					scanned[i] = s.scanFile(repo, batch[i], known)
				}
			}()
		}
		for i := range batch {
			jobs <- i
		}
		close(jobs)
		wg.Wait()

		var toIndex []*index.FileToIndex
		var processed []*store.File
		for i := range scanned {
			switch {
			case scanned[i].unreadable != nil:
				unreadable++
				slog.Warn("skipping unreadable file",
					"repo_id", repo.ID, "path", batch[i].Path, "err", scanned[i].unreadable)
				continue
			case scanned[i].skipped:
				skipped++
				continue
			}
			toIndex = append(toIndex, scanned[i].toIndex)
			processed = append(processed, scanned[i].processed)
		}

		batchFailed, err := s.indexFileSet(ctx, repo, toIndex, processed, force, &coverage, spent)
		if err != nil {
			return err
		}
		for path := range batchFailed {
			failed[path] = true
		}
		indexed += len(toIndex)
		summaryFiles = s.collectSummaryFiles(summaryFiles, toIndex, batchFailed)
		// Per batch, not per file: the batch is the unit that has actually
		// reached the indexers, and a per-file publish would wake a front end
		// tens of thousands of times for a redraw it cannot show anyway.
		s.bus.IndexProgress(repo.ID, end, len(files))
	}

	if unreadable > 0 {
		indexUnreadableFiles.Add(float64(unreadable))
	}

	// The indexers have had every file this pass will give them, so the ones
	// that score against their own storage layout can settle it before the
	// first query reads them. This is ahead of the summary passes below on
	// purpose: those write to the vector store, which has no layout to settle,
	// and they are best-effort — a pass whose summaries failed should still
	// leave a reproducible keyword index. A pass that indexed nothing left the
	// layout alone.
	if indexed > 0 {
		s.compactIndexes(ctx)
	}
	// The summary describes a whole repository, so only a pass that handed
	// every file to the indexers may write it. An incremental pass skips the
	// files whose hash is unchanged, and its counters would report their
	// contracts as having disappeared; the previous summary, with the older
	// updated_at that says how old it is, is the better answer.
	s.storeCoverage(ctx, repo.ID, &coverage, skipped == 0)

	if s.enrich.ReconEnabled() {
		// Recon is best-effort enrichment: log, don't fail the index.
		if err := s.enrich.ReconRepo(ctx, repo); err != nil {
			slog.Warn("recon pass", "repo_id", repo.ID, "err", err)
		}
	}

	if err := s.detectServices(ctx, repo); err != nil {
		return fmt.Errorf("detect services: %w", err)
	}

	if err := s.enrich.SummarizeRepo(ctx, repo, summaryFiles); err != nil {
		// Summaries are best-effort enrichment: log, don't fail the index.
		slog.Warn("summarize repo", "repo_id", repo.ID, "err", err)
	}

	// Boundary-symbol summaries run after the indexers, because they select
	// their candidates from the contract edges the AST pass just stored, and
	// before linking, which does not affect them either way.
	if err := s.enrich.SummarizeSymbols(ctx, repo); err != nil {
		slog.Warn("summarize symbols", "repo_id", repo.ID, "err", err)
	}

	if err := s.linkRepo(ctx, repo); err != nil {
		return err
	}

	slog.Info("index repo", "repo_id", repo.ID,
		"indexed", indexed-len(failed), "skipped", skipped, "failed", len(failed), "unreadable", unreadable,
		"indexers", spent.log(), "total_sec", int(time.Since(indexStart).Seconds()))
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d files could not be indexed (see logs); they stay unindexed and will be retried", len(failed), indexed)
	}
	return nil
}

// collectSummaryFiles keeps the leading successfully indexed files whose
// content the summary pass may still need. It is bounded, so batching does not
// reintroduce the whole-repo-in-memory behaviour it removed.
func (s *Service) collectSummaryFiles(acc []*index.FileToIndex, batch []*index.FileToIndex, failed map[string]bool) []*index.FileToIndex {
	limit := s.enrich.SummaryFileBudget()
	if limit <= 0 {
		return nil // no summary pass will run, so keep nothing for it
	}
	for _, f := range batch {
		if len(acc) >= limit {
			return acc
		}
		if !failed[f.Path] {
			acc = append(acc, f)
		}
	}
	return acc
}

// indexFileSet runs every configured indexer over the given files and stores
// the file rows of those that succeeded. It is the shared "index this list of
// files" step used by both full indexing (doIndex) and partial commit-based
// index.
//
// It returns the set of paths that at least one indexer failed on. Those files
// deliberately get no file row: the stored hash is what makes a later
// non-forced pass skip a file, so recording a failed file as indexed loses it
// permanently — a broken embedder would silently empty the index one pass at a
// time.
//
// The indexers run concurrently. They write to independent backends (the SQL
// store, the Bleve index, the vector store) and there was never an order
// between them — the map they come from iterates randomly — so the only thing
// serializing them was the loop, which left the machine idle: the AST
// indexer's store stage is one goroutine waiting on SQLite while the BM25
// indexer's parse stage has nothing to do but wait for it.
//
// cov, when non-nil, accumulates the contract coverage the indexers report for
// this batch; a caller that indexes only part of a repository passes nil,
// since those counters would not describe the repository. spent, when non-nil,
// accumulates how long each indexer took.
func (s *Service) indexFileSet(ctx context.Context, repo *domain.Repo, toIndex []*index.FileToIndex, processed []*store.File, force bool, cov *coverageAccumulator, spent indexerTimes) (map[string]bool, error) {
	if len(toIndex) == 0 {
		return nil, nil
	}

	runs := withFiles(s.indexerRuns(), toIndex)
	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func(r *indexerRun) {
			defer wg.Done()
			started := time.Now()
			r.res, r.err = r.idx.Index(ctx, &index.IndexRequest{
				RepoID:   repo.ID,
				RepoPath: repo.Path,
				RepoName: repo.Name,
				Files:    r.files,
				Force:    force,
			})
			r.took = time.Since(started)
		}(&runs[i])
	}
	wg.Wait()

	failed := make(map[string]bool)
	for i := range runs {
		r := &runs[i]
		spent.add(r.typ, r.took)
		if r.err != nil {
			return nil, fmt.Errorf("%s indexer: %w", r.typ, r.err)
		}
		if cov != nil {
			// A nil result satisfies the reporter interface as a typed nil, so
			// it is handed over untyped: the accumulator must fall through to
			// the indexer rather than dereference it.
			var res any
			if r.res != nil {
				res = r.res
			}
			cov.collect(r.idx, res)
		}
		if r.res == nil || r.res.FilesFailed == 0 {
			continue
		}
		attributed := attributeFailures(r.res.Errors, toIndex, failed)
		if attributed < r.res.FilesFailed {
			// A failure we cannot pin on a file makes the whole batch suspect;
			// marking any of it indexed could be the loss described above.
			for _, f := range toIndex {
				failed[f.Path] = true
			}
		}
		slog.Warn("indexer reported file failures",
			"indexer", r.typ, "repo_id", repo.ID,
			"files_failed", r.res.FilesFailed, "attributed", attributed, "errors", r.res.Errors)
	}

	ok := processed[:0:0]
	for _, sf := range processed {
		if !failed[sf.Path] {
			ok = append(ok, sf)
		}
	}
	if err := s.store.BatchStoreFiles(ctx, ok); err != nil {
		return nil, fmt.Errorf("store files: %w", err)
	}
	if len(failed) > 0 {
		slog.Warn("files left unindexed and not marked as indexed",
			"repo_id", repo.ID, "failed", len(failed), "stored", len(ok))
	}
	return failed, nil
}

// indexerRun is one indexer's slot in a batch fan-out.
type indexerRun struct {
	typ   index.IndexType
	idx   index.Indexer
	files []*index.FileToIndex
	res   *index.IndexResult
	err   error
	took  time.Duration
}

// CompactIndexes settles the index layout on demand.
//
// A pass compacts what it indexed, which is right for a server that indexes one
// repository at a time and wrong for a bulk load: filling an index with twelve
// repositories back to back pays twelve whole-index rewrites to arrive at the
// layout the last one would have produced alone. Such a loader turns
// indexes.bm25.no_compact on, indexes everything, and calls this once — the
// same final layout for one rewrite instead of twelve.
//
// It reports how long each indexer took, because the caller waiting on it is
// usually the one deciding whether to keep doing it this way.
func (s *Service) CompactIndexes(ctx context.Context) map[string]int64 {
	took := map[string]int64{}
	for _, r := range s.indexerRuns() {
		c, ok := r.idx.(index.Compactor)
		if !ok {
			continue
		}
		started := time.Now()
		if err := c.Compact(ctx); err != nil {
			slog.Warn("compact index", "indexer", r.typ, "err", err)
			continue
		}
		took[string(r.typ)] = time.Since(started).Milliseconds()
	}
	return took
}

// compactIndexes settles the storage layout at the end of a full pass, so that
// what a query scores depends on what was indexed and not on how far a
// background merger happened to get. An incremental commit pass writes a
// handful of files and would pay a whole-index rewrite for them, which is why a
// repository kept current by commits alone stays as reproducible as its last
// full pass and no more.
//
// Failing to compact leaves a correct index that scores less reproducibly, so
// it is logged rather than failing the pass that produced it.
func (s *Service) compactIndexes(ctx context.Context) {
	for _, r := range s.indexerRuns() {
		c, ok := r.idx.(index.Compactor)
		if !ok {
			continue
		}
		// indexes.bm25.no_compact turns the automatic settle off. It says when
		// compaction happens, not whether it can: a bulk loader still asks for
		// it once at the end (see CompactIndexes).
		if a, ok := r.idx.(interface{ AutoCompact() bool }); ok && !a.AutoCompact() {
			continue
		}
		started := time.Now()
		if err := c.Compact(ctx); err != nil {
			slog.Warn("compact index", "indexer", r.typ, "err", err)
			continue
		}
		slog.Debug("compacted index", "indexer", r.typ, "took_ms", time.Since(started).Milliseconds())
	}
}

// indexerRuns prepares one slot per configured indexer, in a stable order.
func (s *Service) indexerRuns() []indexerRun {
	runs := make([]indexerRun, 0, len(s.indexers))
	for typ, idx := range s.indexers {
		runs = append(runs, indexerRun{typ: typ, idx: idx})
	}
	// Map iteration order is random; a stable order makes the reported error
	// of a batch that upset several indexers reproducible.
	sort.Slice(runs, func(i, j int) bool { return runs[i].typ < runs[j].typ })
	return runs
}

// withFiles gives every run its own view of the batch.
//
// The indexers write back into FileToIndex.Content when it arrives empty, so
// sharing the values across a concurrent fan-out would be a data race even
// though every current caller fills Content in. The copies are shallow: the
// file bytes stay shared and read-only, and are not duplicated.
func withFiles(runs []indexerRun, files []*index.FileToIndex) []indexerRun {
	for i := range runs {
		copies := make([]index.FileToIndex, len(files))
		view := make([]*index.FileToIndex, len(files))
		for j, f := range files {
			copies[j] = *f
			view[j] = &copies[j]
		}
		runs[i].files = view
	}
	return runs
}

// indexerTimes accumulates how long each indexer spent over a whole pass. The
// batches run every indexer, so only the sum says which one a slow pass was
// waiting for.
type indexerTimes map[index.IndexType]time.Duration

func (t indexerTimes) add(typ index.IndexType, d time.Duration) {
	if t != nil {
		t[typ] += d
	}
}

// log renders the tally as "ast=41s bm25=63s", sorted for stable log lines.
func (t indexerTimes) log() string {
	types := make([]string, 0, len(t))
	for typ := range t {
		types = append(types, string(typ))
	}
	sort.Strings(types)
	var b strings.Builder
	for i, typ := range types {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s=%ds", typ, int(t[index.IndexType(typ)].Seconds()))
	}
	return b.String()
}

// attributeFailures maps an indexer's error strings back onto the batch's
// paths and records them in failed. Indexers format each entry as
// "<path>: <message>". It returns how many entries were attributed, so the
// caller can tell a fully understood failure set from a partial one.
func attributeFailures(errs []string, batch []*index.FileToIndex, failed map[string]bool) int {
	known := make(map[string]bool, len(batch))
	for _, f := range batch {
		known[f.Path] = true
	}
	attributed := 0
	for _, e := range errs {
		path, _, ok := strings.Cut(e, ": ")
		if !ok || !known[path] {
			continue
		}
		failed[path] = true
		attributed++
	}
	return attributed
}

// linkRepo runs the global linking pass: it resolves this repo's local edges
// and re-links cross-repo contract edges (gRPC/HTTP/Kafka) across all indexed
// repos.
func (s *Service) linkRepo(ctx context.Context, repo *domain.Repo) error {
	linkStart := time.Now()
	s.linkMu.Lock()
	linkStats, err := s.linker.RunWithStats(ctx, repo.ID)
	s.linkMu.Unlock()
	linkSeconds.Observe(time.Since(linkStart).Seconds())
	if linkStats != nil {
		linkErrors.Add(float64(linkStats.Errors))
		linkResolved.Add(float64(linkStats.ResolvedLocal + linkStats.ResolvedContracts))
	}
	if err != nil {
		return fmt.Errorf("link edges: %w", err)
	}
	s.refineCalls(ctx, repo)
	return nil
}

// callRefiner corrects a repository's call edges from language-server
// evidence. It is an interface so the service does not depend on a running
// language server to be constructed or tested.
type callRefiner interface {
	RefineRepo(ctx context.Context, repoID, repoPath string) (*lsp.CallStats, error)
}

// SetCallRefiner installs the language-server call-edge pass. Passing nil
// disables it.
func (s *Service) SetCallRefiner(r callRefiner) { s.callRefiner = r }

// refineCalls runs the language-server call-edge pass over one repository,
// after the name-based linker has had its say. It is best-effort enrichment:
// an unreachable server leaves the name-matched graph exactly as it was, which
// is the graph every deployment without language servers runs on.
func (s *Service) refineCalls(ctx context.Context, repo *domain.Repo) {
	if s.callRefiner == nil {
		return
	}
	stats, err := s.callRefiner.RefineRepo(ctx, repo.ID, repo.Path)
	if err != nil {
		slog.Warn("lsp: call-edge pass failed", "repo_id", repo.ID, "err", err)
		return
	}
	if stats != nil {
		slog.Info("lsp call pass", append([]any{"repo_id", repo.ID}, stats.Log()...)...)
	}
}

// maxIndexWorkers caps the indexing worker-pool size.
const maxIndexWorkers = 32

// resolveWorkers returns the effective worker-pool size for n configured
// workers: values <= 0 fall back to runtime.NumCPU(), capped at maxIndexWorkers.
func resolveWorkers(n int) int {
	if n <= 0 {
		n = runtime.NumCPU()
	}
	if n > maxIndexWorkers {
		n = maxIndexWorkers
	}
	if n < 1 {
		n = 1
	}
	return n
}

// indexWorkers returns the worker-pool size configured via indexes.workers.
func (s *Service) indexWorkers() int {
	workers := 0
	if s.cfg != nil {
		workers = s.cfg.Indexes.Workers
	}
	return resolveWorkers(workers)
}

// scannedFile is the result of the read+hash+compare stage for a single file.
// indexBatchFiles is how many files one scan+index window holds. The scan
// keeps each file's content in memory until its batch is indexed, so this is
// the knob that bounds peak memory on a large repository — and, now that the
// indexers run concurrently, the window is live in each of them at once. 256
// was measured too: it costs ~14% of the peak RSS of a 12-repository pass and
// buys nothing overall (669s against 655s), so the size the memory bound was
// designed around is kept.
const indexBatchFiles = 512

type scannedFile struct {
	toIndex   *index.FileToIndex
	processed *store.File
	skipped   bool
	// unreadable holds the reason a file could not be taken in — a dangling
	// symlink, a permission error, a file that vanished mid-scan, something
	// that is not a regular file. Such a file is skipped and counted; it is
	// not the run's problem, and one of them (a dangling symlink in argo-cd's
	// test data) used to leave the other 1535 files unindexed.
	unreadable error
}

// scanFile reads a file, hashes its content and compares the hash with the
// stored one to decide whether the file needs re-index. known is the pass's
// snapshot of the repo's stored file rows; nil forces every file through.
func (s *Service) scanFile(repo *domain.Repo, f *domain.RepoFile, known map[string]*store.File) scannedFile {
	full := filepath.Join(repo.Path, f.Path)
	// Stat before opening: the listing comes from an Lstat walk, so a dangling
	// symlink is still on it, and opening a fifo or a device node would block
	// this worker until something else writes to it.
	info, err := os.Stat(full)
	switch {
	case err != nil:
		return scannedFile{unreadable: fmt.Errorf("stat file %s: %w", f.Path, err)}
	case !info.Mode().IsRegular():
		return scannedFile{unreadable: fmt.Errorf("file %s is not a regular file (%s)", f.Path, info.Mode().Type())}
	}

	content, err := os.ReadFile(full)
	if err != nil {
		return scannedFile{unreadable: fmt.Errorf("read file %s: %w", f.Path, err)}
	}
	hash := repos.HashContent(content)

	if stored := known[f.Path]; stored != nil && stored.Indexed && stored.Hash == hash {
		return scannedFile{skipped: true}
	}

	return scannedFile{
		toIndex: &index.FileToIndex{
			Path:     f.Path,
			Hash:     hash,
			Language: f.Language,
			Content:  content,
		},
		processed: &store.File{
			RepoID:   repo.ID,
			Path:     f.Path,
			Hash:     hash,
			Language: f.Language,
			Size:     f.Size,
			Indexed:  true,
		},
	}
}

// removeStaleFiles drops indexed data for files that no longer exist in the
// repository working tree. stored is the pass's snapshot of the repo's file
// rows.
func (s *Service) removeStaleFiles(ctx context.Context, repoID string, stored []*store.File, current []*domain.RepoFile) error {
	if len(stored) == 0 {
		return nil
	}
	present := make(map[string]bool, len(current))
	for _, f := range current {
		present[f.Path] = true
	}
	var stale []string
	for _, f := range stored {
		if !present[f.Path] {
			stale = append(stale, f.Path)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	if err := s.removeIndexedPaths(ctx, repoID, stale); err != nil {
		return err
	}
	slog.Info("removed stale files", "repo_id", repoID, "count", len(stale))
	return nil
}

// removeIndexedPaths drops all indexed data (indexer documents, AST units,
// edges and file rows) for the given paths of a repo.
func (s *Service) removeIndexedPaths(ctx context.Context, repoID string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	for _, idx := range s.indexers {
		if err := idx.Remove(ctx, repoID, paths); err != nil {
			return fmt.Errorf("remove from indexer %s: %w", idx.Name(), err)
		}
	}
	if err := s.store.DeleteASTUnitsByFiles(ctx, repoID, paths); err != nil {
		return err
	}
	if err := s.store.DeleteEdgesByFiles(ctx, repoID, paths); err != nil {
		return err
	}
	return s.store.DeleteFilesByPaths(ctx, repoID, paths)
}

// IndexedPathsUnder returns the repo-relative paths of the indexed files below
// dir, which is itself repo-relative; "" or "." means the whole repository.
//
// It exists for the filesystem watcher. A directory that is moved or deleted
// wholesale is reported by the operating system as one event about the
// directory and none about the files that were inside it, and by the time the
// event is read there is nothing left on disk to enumerate. What is indexed is
// the surviving record of what used to be there, so the deletions are derived
// from it rather than from a shadow copy of the tree the watcher would have to
// keep in memory and keep correct.
func (s *Service) IndexedPathsUnder(ctx context.Context, repoID, dir string) ([]string, error) {
	files, err := s.store.GetFilesByRepo(ctx, repoID)
	if err != nil {
		return nil, err
	}
	prefix := ""
	if dir != "" && dir != "." {
		prefix = filepath.Clean(dir) + string(filepath.Separator)
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		if prefix == "" || strings.HasPrefix(f.Path, prefix) {
			out = append(out, f.Path)
		}
	}
	return out, nil
}

// detectServices refreshes the repo's service units (kind "service").
func (s *Service) detectServices(ctx context.Context, repo *domain.Repo) error {
	services, err := repos.DetectServices(repo.Path, repo.Name)
	if err != nil {
		return err
	}
	// LLM hints from the recon pass (if it ran) fill in services the
	// heuristics missed; detected services keep priority per root.
	if hints := s.enrich.ReconHints(ctx, repo); len(hints) > 0 {
		services = repos.MergeServiceHints(services, hints)
	}
	if err := s.store.DeleteASTUnitsByKind(ctx, repo.ID, store.KindService); err != nil {
		return err
	}
	for _, sv := range services {
		unit := &domain.ASTUnit{
			RepoID:    repo.ID,
			FilePath:  sv.Manifest,
			Kind:      store.KindService,
			Name:      sv.Name,
			Qualified: "service:" + repo.Name + "/" + sv.Name,
			Signature: "root:" + sv.Root, // legacy convention, kept for readers of old data
			Doc:       sv.DetectedBy,     // legacy convention, kept for readers of old data
			Meta:      store.EncodeUnitMeta(&store.UnitMeta{Root: sv.Root, DetectedBy: sv.DetectedBy}),
			StartLine: 1,
			EndLine:   1,
		}
		if err := s.store.StoreASTUnit(ctx, unit); err != nil {
			return err
		}
	}
	slog.Info("detected services", "repo_id", repo.ID, "count", len(services))
	return nil
}

func (s *Service) ignorePatterns() []string {
	if s.cfg == nil {
		return nil
	}
	return s.cfg.Repos.Ignore
}

// IgnorePatternsFor returns the patterns in force for one repository: the
// server's, plus whatever the repository excludes for itself in .ragota.yaml.
//
// The repo's patterns are appended, never substituted. The manifest is content
// of the indexed repository, so it may only narrow what is indexed — a
// repository does not get to re-enable a path the operator excluded. A
// manifest that fails to parse leaves the server's patterns in force and is
// reported; dropping to no patterns at all would index the tree it excludes.
func (s *Service) IgnorePatternsFor(repo *domain.Repo) []string {
	server := s.ignorePatterns()
	m, err := repos.LoadManifest(repo.Path)
	if err != nil {
		slog.Warn("repository manifest", "repo_id", repo.ID, "err", err)
		return server
	}
	if m == nil || len(m.Ignore) == 0 {
		return server
	}
	// Copy: the config's slice is shared by every repository indexed.
	out := make([]string, 0, len(server)+len(m.Ignore))
	return append(append(out, server...), m.Ignore...)
}
