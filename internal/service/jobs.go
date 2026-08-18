package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// IndexJobState reports the queue state of one indexing job. It backs the
// job_id a distributed-mode /index response hands out: without it a client
// only ever sees "202 indexing" and cannot tell a queued job from a running
// one.
func (s *Service) IndexJobState(ctx context.Context, jobID string) (*storage.IndexJob, error) {
	if s.storage == nil {
		return nil, fmt.Errorf("storage not available")
	}
	return s.storage.GetIndexJob(ctx, jobID)
}

// maxJobListLimit caps a job listing: the queue keeps terminal jobs, so an
// unbounded page would grow with the repository's push history.
const maxJobListLimit = 200

// RepoJobs lists the queue entries of one repository, newest first. The repo
// is read first so an unknown id is a 404 rather than an empty list, which a
// client cannot tell from "nothing queued".
func (s *Service) RepoJobs(ctx context.Context, repoID string, limit int) ([]*storage.IndexJob, error) {
	if s.storage == nil {
		return nil, fmt.Errorf("storage not available")
	}
	if _, err := s.storage.GetRepo(ctx, repoID); err != nil {
		return nil, err
	}
	if limit > maxJobListLimit {
		limit = maxJobListLimit
	}
	return s.storage.ListIndexJobs(ctx, repoID, limit)
}

// RepoJob returns one job of a repository. A job belonging to another repo is
// reported as missing: the id alone must not be a way to read across repos.
func (s *Service) RepoJob(ctx context.Context, repoID, jobID string) (*storage.IndexJob, error) {
	if s.storage == nil {
		return nil, fmt.Errorf("storage not available")
	}
	job, err := s.storage.GetIndexJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job.RepoID != repoID {
		return nil, storage.ErrNotFound
	}
	return job, nil
}

// Ready probes the dependencies a request actually needs, so /ready can fail
// while /health (pure liveness) still succeeds. It touches the metadata store
// and, when configured, the vector store — a cheap round-trip each, not a full
// query.
func (s *Service) Ready(ctx context.Context) error {
	if s.storage == nil {
		return fmt.Errorf("storage not available")
	}
	if err := s.storage.Init(ctx); err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	if _, err := s.storage.CountASTUnits(ctx); err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	if vs := s.storage.VectorStore(); vs != nil {
		if _, err := vs.Stats(ctx); err != nil {
			return fmt.Errorf("vector store: %w", err)
		}
	}
	return nil
}

// --- the distributed job worker ---

// distributed reports whether the shared indexing job queue is enabled.
func (s *Service) distributed() bool {
	return s.cfg != nil && s.cfg.Indexes.Distributed
}

// repoClaimTTLSeconds bounds how long an indexing claim keeps a repo
// locked before another pass may take it over.
const repoClaimTTLSeconds = storage.DefaultRepoClaimTTLSeconds

var hostname = func() string {
	h, _ := os.Hostname()
	if h == "" {
		h = "unknown"
	}
	return h
}()

// ownerID identifies this Service instance as the holder of a repo or job
// claim. The pointer distinguishes instances that share a process (tests, and
// any embedding that builds more than one Service); host and pid make the
// value meaningful in logs and unique across processes.
func (s *Service) ownerID() string {
	return fmt.Sprintf("%s-%d-%p", hostname, os.Getpid(), s)
}

// recoverStuckRepos releases indexing claims left behind by a process that
// died mid-run. Without it a repo stays in "indexing" forever: nothing clears
// the status at runtime, so every later index or commit request is rejected as
// busy and, in distributed mode, jobs are burned in a loop.
//
// In single-instance mode every claim in the database belongs to a previous
// life of this process, so all of them are released; with the shared queue
// another instance may legitimately hold one, so only expired claims are.
func (s *Service) recoverStuckRepos() {
	if s.storage == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.baseCtx), terminalWriteTimeout)
	defer cancel()

	n, err := s.storage.ResetStuckRepos(ctx, !s.distributed())
	if err != nil {
		slog.Error("reset stuck repos failed", "err", err)
		return
	}
	if n > 0 {
		slog.Warn("reset repos left in indexing state", "count", n)
	}
}

// startJobPoller runs startup recovery and launches the background worker loop
// for distributed indexing. New calls it on every construction path, which is
// why the recovery lives here: the worker loop itself is a no-op unless
// indexes.distributed is enabled, but a repo left claimed by a crashed run has
// to be released whatever the mode.
func (s *Service) startJobPoller() {
	s.recoverStuckRepos()

	if !s.distributed() {
		return
	}
	poll := time.Duration(s.cfg.Indexes.JobPollSeconds) * time.Second
	if poll <= 0 {
		poll = 3 * time.Second
	}
	staleSec := int64(s.cfg.Indexes.StaleJobSeconds)
	if staleSec <= 0 {
		staleSec = 120
	}
	workerID := s.ownerID()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(poll)
		defer ticker.Stop()
		slog.Info("distributed index worker started", "worker_id", workerID, "poll", poll, "stale_after_sec", staleSec)
		for {
			select {
			case <-s.baseCtx.Done():
				return
			case <-ticker.C:
				s.pollIndexJobs(workerID, staleSec, poll)
			}
		}
	}()
}

// pollIndexJobs performs one worker tick: requeue stale jobs, claim the next
// pending job (if any) and execute it.
func (s *Service) pollIndexJobs(workerID string, staleSec int64, heartbeatEvery time.Duration) {
	ctx := s.baseCtx

	if n, err := s.storage.RequeueStaleIndexJobs(ctx, staleSec); err != nil {
		slog.Error("requeue stale index jobs failed", "err", err)
	} else if n > 0 {
		slog.Warn("requeued stale index jobs", "count", n)
	}

	job, err := s.storage.ClaimNextIndexJob(ctx, workerID)
	if errors.Is(err, storage.ErrNotFound) {
		return
	}
	if err != nil {
		slog.Error("claim index job failed", "err", err)
		return
	}
	s.runQueuedJob(ctx, job, workerID, heartbeatEvery)
}

// jobBookkeeper holds the queue-side bookkeeping of one claimed job. The
// writes are detached from ctx for the same reason the repo status is: Close
// cancels ctx, and a job left "running" is only recovered by the stale-job
// sweep on some other instance.
type jobBookkeeper struct {
	s        *Service
	job      *storage.IndexJob
	workerID string
	ctx      context.Context
}

// complete records the job's outcome ("" = success).
func (b jobBookkeeper) complete(jobErr string) {
	bookCtx, cancel := terminalCtx(b.ctx)
	defer cancel()
	cerr := b.s.storage.CompleteIndexJob(bookCtx, b.job.ID, b.workerID, jobErr)
	if errors.Is(cerr, storage.ErrNotFound) {
		// The job was requeued (a slow heartbeat) and re-claimed elsewhere.
		// Its new owner decides the outcome; overwriting it here is how a
		// successful run used to be recorded as a failure.
		slog.Warn("index job no longer owned by this worker; result discarded",
			"job_id", b.job.ID, "worker_id", b.workerID, "job_err", jobErr)
		return
	}
	if cerr != nil {
		slog.Error("complete index job failed", "job_id", b.job.ID, "err", cerr)
	}
}

// release returns the job to the queue; it is the answer to a retry condition
// (the repo is busy, or an earlier batch has to land first), not a failure.
func (b jobBookkeeper) release(reason string) {
	bookCtx, cancel := terminalCtx(b.ctx)
	defer cancel()
	slog.Info("index job released", "job_id", b.job.ID, "repo_id", b.job.RepoID, "reason", reason)
	if rerr := b.s.storage.ReleaseIndexJob(bookCtx, b.job.ID, b.workerID); rerr != nil {
		slog.Warn("release index job failed", "job_id", b.job.ID, "err", rerr)
	}
}

// runQueuedJob executes one claimed job: it claims the repo, heartbeats while
// the work runs and records the job result. Both job kinds share this
// scaffolding — what differs is only the work itself.
func (s *Service) runQueuedJob(ctx context.Context, job *storage.IndexJob, workerID string, heartbeatEvery time.Duration) {
	book := jobBookkeeper{s: s, job: job, workerID: workerID, ctx: ctx}

	// Decoding before the repo is claimed: an undecodable payload is a
	// permanent failure, and claiming the repo for it would only block the
	// batches that can still be applied.
	var commits []CommitEvent
	if job.Kind == storage.JobKindCommits {
		var derr error
		if commits, derr = decodeCommitBatch(job.Payload); derr != nil {
			book.complete(derr.Error())
			return
		}
	}

	repo, err := s.storage.GetRepo(ctx, job.RepoID)
	if err != nil {
		book.complete(fmt.Sprintf("get repo %s: %v", job.RepoID, err))
		return
	}

	if job.Kind == storage.JobKindCommits {
		target := commits[len(commits)-1].SHA
		switch commitJobOrder(repo, commits) {
		case commitBatchApplied:
			// A worker died between applying the batch and recording the job.
			// The cursor already covers it, so re-applying would be busywork.
			s.clearPendingCommit(ctx, repo, target)
			book.complete("")
			return
		case commitBatchTooEarly:
			// Claiming is per job, so a newer batch can be claimed while its
			// predecessor is still queued: applying it here would reorder
			// history. Waiting is only right while that predecessor still
			// exists — otherwise the chain is broken and the job would block
			// the queue forever, so it fails and says so.
			earlier, herr := s.storage.HasPendingCommitJobBefore(ctx, job.RepoID, job.ID)
			if herr != nil {
				slog.Warn("check earlier commit jobs failed", "job_id", job.ID, "err", herr)
				book.release("preceding batch state unknown")
				return
			}
			if earlier {
				book.release("waiting for the preceding commit batch")
				return
			}
			s.clearPendingCommit(ctx, repo, target)
			book.complete(fmt.Sprintf(
				"commit gap: batch %s..%s does not continue cursor %q and no preceding batch is queued; resend from the cursor or reindex",
				commits[0].SHA, target, repo.LastCommit))
			return
		}
	}

	claimed, err := s.storage.ClaimRepoForIndexing(ctx, job.RepoID, workerID, repoClaimTTLSeconds)
	if err != nil {
		book.complete(fmt.Sprintf("claim repo %s: %v", job.RepoID, err))
		return
	}
	if !claimed {
		// Another pass is indexing this repo. That is a retry condition, not a
		// job failure: release the job so it runs once the repo frees up.
		book.release("repo busy")
		return
	}

	// Heartbeat while the work runs so other instances don't requeue the job.
	hbCtx, hbCancel := context.WithCancel(ctx)
	var hbWg sync.WaitGroup
	hbWg.Add(1)
	go func() {
		defer hbWg.Done()
		ticker := time.NewTicker(heartbeatEvery)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				if herr := s.storage.HeartbeatIndexJob(hbCtx, job.ID, workerID); herr != nil && !errors.Is(herr, context.Canceled) {
					slog.Warn("index job heartbeat failed", "job_id", job.ID, "err", herr)
				}
			}
		}
	}()

	var runErr error
	if job.Kind == storage.JobKindCommits {
		target := commits[len(commits)-1].SHA
		// Republished under this worker: the repo claim cleared it, and
		// /sync-state has to keep saying which batch is in flight.
		if perr := s.storage.SetRepoPendingCommit(ctx, job.RepoID, target); perr != nil {
			slog.Warn("record pending commit failed", "repo_id", job.RepoID, "err", perr)
		}
		slog.Info("commit job claimed", "job_id", job.ID, "repo_id", job.RepoID,
			"commits", len(commits), "target", target)
		runErr = s.runApplyCommits(ctx, repo, commits)
	} else {
		slog.Info("index job claimed", "job_id", job.ID, "repo_id", job.RepoID, "force", job.Force)
		runErr = s.runIndex(ctx, repo, job.Force)
	}

	hbCancel()
	hbWg.Wait()

	jobErr := ""
	if runErr != nil {
		jobErr = runErr.Error()
	}
	book.complete(jobErr)
}
