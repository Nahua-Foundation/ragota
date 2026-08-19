package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/index"
	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// CommitFile is one file change inside a commit pushed by an external client.
// The JSON tags are load-bearing: a queued batch is stored in this form and
// decoded by whichever instance later claims the job.
type CommitFile struct {
	Path    string `json:"path"`               // path after the change
	OldPath string `json:"old_path,omitempty"` // previous path, set for renames (status R)
	Content string `json:"content,omitempty"`  // new content; empty means "read from disk"
	Status  string `json:"status"`             // A (added) | M (modified) | D (deleted) | R (renamed)
}

// CommitEvent is one commit with its file changes, in commit order.
type CommitEvent struct {
	SHA     string       `json:"sha"`
	Parents []string     `json:"parents,omitempty"`
	Files   []CommitFile `json:"files,omitempty"`
}

// CommitAck describes what a POST /commits actually did, which differs between
// modes exactly as IndexAck does: the batch has either started applying in
// this instance or is only queued, and a queued batch is followed by job id.
type CommitAck struct {
	Accepted bool   // false = commit gap; nothing was queued or applied
	Status   string // indexing | queued
	Queued   bool   // true when the batch is only enqueued
	JobID    string // distributed mode
	Target   string // SHA the batch advances the cursor to
	Before   string // cursor before this batch
}

// ApplyCommits incrementally indexes a batch of commits pushed by an external
// client (the source of truth about the repository history).
//
// It returns Accepted=false (and no error) when the repo has a commit cursor
// and the first commit does not reference it in Parents — a gap in the commit
// chain. The caller is expected to surface this as a 409 with the current
// cursor so the client can resend the missing range or request a full reindex;
// nothing is indexed in that case.
//
// On success the changes are applied in the background (like IndexRepo): only
// the affected paths are re-indexed or removed, then services are re-detected,
// the linker runs and the commit cursor advances to the last SHA.
//
// In distributed mode the batch is not applied by this instance at all: it is
// queued as a commit job (payload included) and any instance's worker may pick
// it up, so an instance that dies after answering 202 does not take the batch
// with it. The gap check still runs here, before the batch is accepted, so a
// client learns about a hole in the chain from the response rather than from a
// cursor that stops moving.
func (s *Service) ApplyCommits(ctx context.Context, repoID string, commits []CommitEvent) (*CommitAck, error) {
	if len(commits) == 0 {
		return nil, fmt.Errorf("%w: commits are required", ErrBadRequest)
	}
	for _, c := range commits {
		if c.SHA == "" {
			return nil, fmt.Errorf("%w: commit sha is required", ErrBadRequest)
		}
		for _, f := range c.Files {
			if err := validateCommitPath(f.Path); err != nil {
				return nil, err
			}
			if f.OldPath != "" {
				if err := validateCommitPath(f.OldPath); err != nil {
					return nil, err
				}
			}
		}
	}

	repo, err := s.store.GetRepo(ctx, repoID)
	if err != nil {
		return nil, err
	}

	// Commit-gap check: with a known cursor, the first pushed commit must be a
	// direct child of it.
	if repo.LastCommit != "" && !hasParent(commits[0].Parents, repo.LastCommit) {
		// Unless it continues the batch still being applied. The cursor only
		// advances once that batch lands, so a client pushing back-to-back
		// would otherwise be told to resend or reindex over a race it cannot
		// see. "Busy, retry" is the truthful answer.
		if repo.PendingCommit != "" && hasParent(commits[0].Parents, repo.PendingCommit) {
			return nil, fmt.Errorf("%w: %s is applying %s", ErrRepoBusy, repoID, repo.PendingCommit)
		}
		return &CommitAck{Accepted: false, Before: repo.LastCommit}, nil
	}

	lastSHA := commits[len(commits)-1].SHA

	if s.distributed() {
		payload, merr := encodeCommitBatch(commits)
		if merr != nil {
			return nil, merr
		}
		job, jerr := s.store.EnqueueCommitJob(ctx, repoID, payload)
		if jerr != nil {
			return nil, fmt.Errorf("enqueue commit job: %w", jerr)
		}
		// Published only after the batch is durable. The other order would
		// advertise an in-flight batch that a failed enqueue never queued, and
		// a client trusting /sync-state would wait for work nobody holds.
		if perr := s.store.SetRepoPendingCommit(ctx, repoID, lastSHA); perr != nil {
			slog.Warn("record pending commit failed", "repo_id", repoID, "err", perr)
		}
		slog.Info("commit job enqueued", "repo_id", repoID, "job_id", job.ID,
			"commits", len(commits), "target", lastSHA, "payload_bytes", len(payload))
		return &CommitAck{
			Accepted: true, Status: "queued", Queued: true, JobID: job.ID,
			Target: lastSHA, Before: repo.LastCommit,
		}, nil
	}

	claimed, err := s.store.ClaimRepoForIndexing(ctx, repoID, s.ownerID(), repoClaimTTLSeconds)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return nil, fmt.Errorf("%w: %s", ErrRepoBusy, repoID)
	}

	// Publish the target SHA before the work starts so /sync-state can tell
	// "this batch is in flight" from "the push was lost".
	if perr := s.store.SetRepoPendingCommit(ctx, repoID, lastSHA); perr != nil {
		slog.Warn("record pending commit failed", "repo_id", repoID, "err", perr)
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		_ = s.runApplyCommits(s.baseCtx, repo, commits)
	}()
	return &CommitAck{Accepted: true, Status: "indexing", Target: lastSHA, Before: repo.LastCommit}, nil
}

// encodeCommitBatch serializes a batch for the job queue.
//
// The payload lives in the job row rather than in the accepting instance's
// memory or a side channel: it has to outlive the process that received the
// push, be readable by whichever instance claims the job, and be committed
// atomically with the queue entry — otherwise "the job exists" and "the batch
// exists" can disagree, which is the failure this replaces. The batch cannot
// be reconstructed from the repository either: pushed content is the client's,
// not necessarily anything on disk. The cost is a row as large as the push
// (the endpoint caps it at 64 MiB), which is why the payload is dropped when
// the job reaches a terminal state and never selected by the read paths.
func encodeCommitBatch(commits []CommitEvent) (string, error) {
	b, err := json.Marshal(commits)
	if err != nil {
		return "", fmt.Errorf("encode commit batch: %w", err)
	}
	return string(b), nil
}

// decodeCommitBatch reads back a payload written by encodeCommitBatch.
func decodeCommitBatch(payload string) ([]CommitEvent, error) {
	var commits []CommitEvent
	if err := json.Unmarshal([]byte(payload), &commits); err != nil {
		return nil, fmt.Errorf("decode commit batch: %w", err)
	}
	if len(commits) == 0 {
		return nil, fmt.Errorf("decode commit batch: empty batch")
	}
	return commits, nil
}

func hasParent(parents []string, sha string) bool {
	for _, p := range parents {
		if p == sha {
			return true
		}
	}
	return false
}

// runApplyCommits executes one partial index pass over the commits' paths and
// records the resulting repo status and commit cursor. It returns the failure
// it recorded (nil on a clean batch) so a queued run can report the same thing
// as the job result.
//
// The cursor only advances when the whole batch landed. Advancing it past
// files that failed would make the index permanently disagree with the cursor:
// the client would never resend those commits, and a non-forced pass skips
// nothing it has not stored a hash for.
func (s *Service) runApplyCommits(ctx context.Context, repo *domain.Repo, commits []CommitEvent) error {
	unapplied, err := s.doApplyCommits(ctx, repo, commits)

	// Detached from ctx: Close cancels the run's context, and a dropped
	// terminal write leaves the repo claimed as "indexing" forever.
	statusCtx, cancel := terminalCtx(ctx)
	defer cancel()

	if err != nil {
		slog.Error("apply commits failed", "repo_id", repo.ID, "err", err)
		if uerr := s.store.UpdateRepoStatus(statusCtx, repo.ID, domain.StatusError, err.Error(), repo.IndexedAt); uerr != nil {
			slog.Error("update repo status to error failed", "repo_id", repo.ID, "err", uerr)
		}
		return err
	}
	if len(unapplied) > 0 {
		msg := fmt.Sprintf("commit batch partially applied; %d path(s) left unindexed: %s",
			len(unapplied), strings.Join(unapplied, ", "))
		slog.Error("apply commits incomplete", "repo_id", repo.ID, "unapplied", unapplied)
		if uerr := s.store.UpdateRepoStatus(statusCtx, repo.ID, domain.StatusError, msg, repo.IndexedAt); uerr != nil {
			slog.Error("update repo status to error failed", "repo_id", repo.ID, "err", uerr)
		}
		return errors.New(msg)
	}

	lastSHA := commits[len(commits)-1].SHA
	if uerr := s.store.UpdateRepoLastCommit(statusCtx, repo.ID, lastSHA); uerr != nil {
		slog.Error("update repo last commit failed", "repo_id", repo.ID, "err", uerr)
	}
	if uerr := s.store.UpdateRepoStatus(statusCtx, repo.ID, domain.StatusIdle, "", time.Now().Unix()); uerr != nil {
		slog.Error("update repo status to idle failed", "repo_id", repo.ID, "err", uerr)
	}
	return nil
}

// ErrInvalidPath marks a rejected repo-relative path. It wraps ErrBadRequest,
// so existing 400 handling still applies while callers can report the more
// specific "invalid_path" code.
var ErrInvalidPath = fmt.Errorf("%w: invalid path", ErrBadRequest)

// validateCommitPath rejects paths that would resolve outside the repository
// root. Pushed paths reach os.ReadFile when the client sends no content, so an
// unchecked "../" would let a caller pull any file the process can read into
// the index — and out again through search.
func validateCommitPath(path string) error {
	if path == "" {
		return fmt.Errorf("%w: commit file path is required", ErrInvalidPath)
	}
	if filepath.IsAbs(path) || strings.HasPrefix(path, "/") || strings.HasPrefix(path, `\`) {
		return fmt.Errorf("%w: commit file path must be relative to the repo root: %s", ErrInvalidPath, path)
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: commit file path escapes the repo root: %s", ErrInvalidPath, path)
	}
	return nil
}

// commitPlanEntry is the merged final action for one path across all commits.
type commitPlanEntry struct {
	delete  bool
	content string // new content, if pushed by the client
}

// mergeCommitPlan flattens the ordered commits into a final per-path action:
// the last change to a path wins; a rename deletes the old path and upserts
// the new one.
func mergeCommitPlan(commits []CommitEvent) map[string]commitPlanEntry {
	plan := make(map[string]commitPlanEntry)
	for _, c := range commits {
		for _, f := range c.Files {
			switch f.Status {
			case "D":
				plan[f.Path] = commitPlanEntry{delete: true}
			case "R":
				if f.OldPath != "" && f.OldPath != f.Path {
					plan[f.OldPath] = commitPlanEntry{delete: true}
				}
				plan[f.Path] = commitPlanEntry{content: f.Content}
			default: // A | M
				plan[f.Path] = commitPlanEntry{content: f.Content}
			}
		}
	}
	return plan
}

// doApplyCommits applies one merged commit batch. It returns the paths the
// batch could not apply (unreadable files, per-file indexer failures); an
// error is reserved for failures that affect the batch as a whole.
func (s *Service) doApplyCommits(ctx context.Context, repo *domain.Repo, commits []CommitEvent) ([]string, error) {
	start := time.Now()
	defer func() {
		applyCommitsSeconds.Observe(time.Since(start).Seconds())
	}()
	return s.applyPlan(ctx, repo, mergeCommitPlan(commits), fmt.Sprintf("%d commit(s)", len(commits)))
}

// applyPlan is the incremental index pass: given the final action for each
// changed path, it removes what has to go, indexes what has to be re-read, and
// brings the derived data (services, edges) back in step.
//
// It is separate from doApplyCommits only because a commit batch is not the
// only thing that can produce such a plan — a filesystem watcher produces the
// same one from a working tree (see ApplyLocalChanges). Everything that makes
// an incremental pass agree with a full one lives here rather than in either
// caller, so there is exactly one implementation of it to be right.
//
// origin names what produced the plan and appears only in the log line.
//
// It returns the paths the pass could not apply (unreadable files, per-file
// indexer failures); an error is reserved for failures that affect the pass as
// a whole.
func (s *Service) applyPlan(ctx context.Context, repo *domain.Repo, plan map[string]commitPlanEntry, origin string) ([]string, error) {
	// A push must produce the same index as a full pass over the same tree, so
	// the same ignore patterns apply here too — otherwise a push indexes
	// node_modules and the next full index deletes it again. That includes the
	// repository's own .ragota.yaml patterns, which is why this reads the
	// per-repo set rather than the server's.
	ignore := config.NewIgnorePatterns(s.IgnorePatternsFor(repo))

	var deletes []string
	needsSourceUpdate := false
	for path, entry := range plan {
		if entry.delete {
			deletes = append(deletes, path)
			continue
		}
		if ignore.ShouldIgnore(repo.Path, filepath.Join(repo.Path, path)) {
			// An ignored path may have been indexed before the pattern was
			// added; drop it rather than leaving a stale copy behind.
			deletes = append(deletes, path)
			continue
		}
		if entry.content == "" {
			needsSourceUpdate = true
		}
	}

	// Some upserts carry no content: sync the working tree once so the files
	// can be read from disk.
	if needsSourceUpdate {
		if source, ok := s.sources[repo.Source]; ok {
			if err := source.Update(ctx, repo); err != nil {
				return nil, fmt.Errorf("update source: %w", err)
			}
		}
	}

	// Read every new file before touching the index. Deleting first and then
	// failing to read left the index matching neither the old nor the new
	// cursor, and the retry failed the same way; an unreadable file is now
	// skipped and reported instead of aborting the batch.
	var toIndex []*index.FileToIndex
	var processed []*store.File
	var unapplied []string
	for path, entry := range plan {
		if entry.delete || ignore.ShouldIgnore(repo.Path, filepath.Join(repo.Path, path)) {
			continue
		}
		lang := repos.DetectLanguage(path)
		if lang == "" {
			continue // unsupported language, nothing to index
		}
		content := []byte(entry.content)
		if entry.content == "" {
			data, err := os.ReadFile(filepath.Join(repo.Path, path))
			if err != nil {
				slog.Error("commit file unreadable; skipped", "repo_id", repo.ID, "path", path, "err", err)
				unapplied = append(unapplied, path)
				continue
			}
			content = data
		}
		hash := repos.HashContent(content)
		toIndex = append(toIndex, &index.FileToIndex{
			Path:     path,
			Hash:     hash,
			Language: lang,
			Content:  content,
		})
		processed = append(processed, &store.File{
			RepoID:   repo.ID,
			Path:     path,
			Hash:     hash,
			Language: lang,
			Size:     int64(len(content)),
			Indexed:  true,
		})
	}

	if err := s.removeIndexedPaths(ctx, repo.ID, deletes); err != nil {
		return nil, err
	}

	failed, err := s.indexFileSet(ctx, repo, toIndex, processed, true, nil, nil)
	if err != nil {
		return nil, err
	}
	for path := range failed {
		unapplied = append(unapplied, path)
	}
	sort.Strings(unapplied)

	if err := s.detectServices(ctx, repo); err != nil {
		return nil, fmt.Errorf("detect services: %w", err)
	}

	if err := s.linkRepo(ctx, repo); err != nil {
		return nil, err
	}

	slog.Info("applied changes", "repo_id", repo.ID, "origin", origin,
		"indexed", len(toIndex)-len(failed), "deleted", len(deletes), "unapplied", len(unapplied))
	return unapplied, nil
}

// ApplyLocalChanges indexes a set of working-tree changes that no commit
// describes — the ones a filesystem watcher observes. Files carries the same
// shape a pushed commit does (path, status, optional content), because it is
// the same information: what changed, and whether it still exists.
//
// It is ApplyCommits without the commit chain, and shares everything below
// that: the merged plan, the ignore patterns including the repository's own,
// the deletions, the stale-copy handling, the service re-detection and the
// linker (see applyPlan). What it does not share is the cursor. last_commit is
// the contract with a client that pushes real history, and the gap check
// against it is the only way such a client learns it has missed a range;
// satisfying that check from a working tree would mean inventing SHAs, and a
// cursor advanced past an invented SHA answers the question with a lie. A
// watched tree has no history to be ahead or behind of.
//
// The repository is claimed for the duration, so a watcher batch and a full
// pass cannot run over each other. A caller that gets ErrRepoBusy should retry
// with the same paths rather than drop them: the pass holding the claim may
// have started before the change reached disk, in which case nothing else will
// ever pick it up.
func (s *Service) ApplyLocalChanges(ctx context.Context, repoID string, files []CommitFile) error {
	if len(files) == 0 {
		return nil
	}
	for _, f := range files {
		if err := validateCommitPath(f.Path); err != nil {
			return err
		}
		if f.OldPath != "" {
			if err := validateCommitPath(f.OldPath); err != nil {
				return err
			}
		}
	}

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

	unapplied, err := s.applyPlan(ctx, repo, mergeCommitPlan([]CommitEvent{{Files: files}}), "watch")

	// Detached from ctx for the reason runApplyCommits detaches: Close cancels
	// the context this runs under, and a dropped terminal write leaves the repo
	// claimed as "indexing" with nothing left to clear it.
	statusCtx, cancel := terminalCtx(ctx)
	defer cancel()

	if err == nil && len(unapplied) > 0 {
		err = fmt.Errorf("%d path(s) left unindexed: %s", len(unapplied), strings.Join(unapplied, ", "))
	}
	if err != nil {
		slog.Error("apply local changes failed", "repo_id", repo.ID, "err", err)
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

// clearPendingCommit retracts the in-flight SHA of a batch that is not going
// to be applied by this job. Leaving it set is exactly the "in flight" answer
// /sync-state must not give about a batch nobody holds any more.
func (s *Service) clearPendingCommit(ctx context.Context, repo *domain.Repo, target string) {
	if repo.PendingCommit != target {
		return
	}
	bookCtx, cancel := terminalCtx(ctx)
	defer cancel()
	if err := s.store.SetRepoPendingCommit(bookCtx, repo.ID, ""); err != nil {
		slog.Warn("clear pending commit failed", "repo_id", repo.ID, "err", err)
	}
}

// commitBatchOrder says how a claimed commit batch relates to the repo's
// cursor.
type commitBatchOrder int

const (
	commitBatchNext     commitBatchOrder = iota // continues the cursor: apply it
	commitBatchApplied                          // the cursor already covers it
	commitBatchTooEarly                         // an earlier batch has to land first
)

// commitJobOrder re-checks at claim time what ApplyCommits checked at accept
// time. The queue is ordered but claiming is not: with two batches queued for
// one repo, two workers can claim both at once, and only the cursor says which
// one may run.
func commitJobOrder(repo *domain.Repo, commits []CommitEvent) commitBatchOrder {
	if repo.LastCommit == "" || hasParent(commits[0].Parents, repo.LastCommit) {
		return commitBatchNext
	}
	for _, c := range commits {
		if c.SHA == repo.LastCommit {
			return commitBatchApplied
		}
	}
	return commitBatchTooEarly
}
