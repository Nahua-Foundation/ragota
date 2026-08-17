package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/indexing"
	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

func TestValidateCommitPath(t *testing.T) {
	valid := []string{
		"main.go",
		"src/app/main.go",
		"./src/main.go",
		"a/../b/main.go", // stays inside the root once cleaned
	}
	for _, p := range valid {
		if err := validateCommitPath(p); err != nil {
			t.Errorf("validateCommitPath(%q) = %v, want nil", p, err)
		}
	}

	// Paths that resolve outside the repository root must be rejected: an
	// unchecked one reaches os.ReadFile and its content becomes searchable.
	invalid := []string{
		"",
		"../secrets.env",
		"../../../../etc/passwd",
		"src/../../outside.go",
		"/etc/passwd",
		`\windows\system32\drivers\etc\hosts`,
	}
	for _, p := range invalid {
		err := validateCommitPath(p)
		if err == nil {
			t.Errorf("validateCommitPath(%q) = nil, want an error", p)
			continue
		}
		if !errors.Is(err, ErrBadRequest) {
			t.Errorf("validateCommitPath(%q) error = %v, want ErrBadRequest", p, err)
		}
	}
}

func TestApplyCommitsRejectsEscapingPath(t *testing.T) {
	svc := &Service{storage: &mockStorage{}}

	_, err := svc.ApplyCommits(t.Context(), "repo1", []CommitEvent{{
		SHA:   "abc",
		Files: []CommitFile{{Path: "../../../etc/passwd", Status: "M"}},
	}})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("ApplyCommits() error = %v, want ErrBadRequest", err)
	}

	_, err = svc.ApplyCommits(t.Context(), "repo1", []CommitEvent{{
		SHA:   "abc",
		Files: []CommitFile{{Path: "ok.go", OldPath: "../escape.go", Status: "R"}},
	}})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("ApplyCommits() with escaping old_path error = %v, want ErrBadRequest", err)
	}

	// The rejection is reported as an invalid path specifically, so the API can
	// answer with the invalid_path code rather than a generic validation error.
	err = validateCommitPath("../secrets.env")
	if !errors.Is(err, ErrInvalidPath) {
		t.Errorf("validateCommitPath error = %v, want ErrInvalidPath", err)
	}
}

// commitTestService builds a Service whose indexers all succeed, over a
// temporary repo directory.
func commitTestService(t *testing.T, st *mockStorage, ignore []string) (*Service, *repos.Repo) {
	t.Helper()
	dir := t.TempDir()
	svc := newTestService(st, map[indexing.IndexType]indexing.Indexer{
		indexing.IndexTypeAST: &mockIndexer{name: "ast", indexType: indexing.IndexTypeAST},
	})
	svc.cfg = &config.Config{Repos: config.ReposConfig{Ignore: ignore}}
	svc.sources = map[repos.SourceType]repos.RepoSource{
		repos.SourceTypeLocal: &mockSource{name: repos.SourceTypeLocal},
	}
	return svc, &repos.Repo{ID: "repo-1", Name: "test", Source: repos.SourceTypeLocal, Path: dir}
}

// TestApplyCommitsSkipsUnreadableFiles: one unreadable file used to abort the
// whole batch after the deletions had already been applied, leaving the index
// matching neither cursor and failing identically on every retry.
func TestApplyCommitsSkipsUnreadableFiles(t *testing.T) {
	st := &mockStorage{}
	svc, repo := commitTestService(t, st, nil)

	// present.go exists on disk; missing.go does not and carries no content.
	if err := os.WriteFile(filepath.Join(repo.Path, "present.go"), []byte("package p\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	unapplied, err := svc.doApplyCommits(context.Background(), repo, []CommitEvent{{
		SHA: "sha-1",
		Files: []CommitFile{
			{Path: "present.go", Status: "M"},
			{Path: "missing.go", Status: "M"},
			{Path: "inline.go", Status: "A", Content: "package q\n"},
		},
	}})
	if err != nil {
		t.Fatalf("doApplyCommits() error = %v, want the batch to continue past one bad file", err)
	}
	if len(unapplied) != 1 || unapplied[0] != "missing.go" {
		t.Errorf("unapplied = %v, want [missing.go]", unapplied)
	}

	stored := st.storedPaths()
	want := map[string]bool{"present.go": true, "inline.go": true}
	if len(stored) != 2 {
		t.Fatalf("stored files = %v, want the two readable ones", stored)
	}
	for _, p := range stored {
		if !want[p] {
			t.Errorf("unexpected stored file %q", p)
		}
	}
}

// TestRunApplyCommitsDoesNotAdvanceCursorOnPartialBatch: advancing the cursor
// past files that never landed makes the index permanently disagree with it,
// because the client will never resend those commits.
func TestRunApplyCommitsDoesNotAdvanceCursorOnPartialBatch(t *testing.T) {
	st := &mockStorage{}
	svc, repo := commitTestService(t, st, nil)

	_ = svc.runApplyCommits(context.Background(), repo, []CommitEvent{{
		SHA:   "sha-2",
		Files: []CommitFile{{Path: "gone.go", Status: "M"}}, // no content, not on disk
	}})

	st.mu.Lock()
	last := st.lastCommit
	statuses := append([]mockStatus(nil), st.statuses...)
	st.mu.Unlock()

	if last != "" {
		t.Errorf("last_commit = %q, want the cursor to stay put after a partial batch", last)
	}
	if len(statuses) != 1 || statuses[0].Status != repos.StatusError {
		t.Fatalf("statuses = %+v, want a single error status", statuses)
	}
	if statuses[0].LastError == "" {
		t.Error("error status carries no message; the client cannot tell what failed")
	}
}

func TestRunApplyCommitsAdvancesCursorOnCleanBatch(t *testing.T) {
	st := &mockStorage{}
	svc, repo := commitTestService(t, st, nil)

	_ = svc.runApplyCommits(context.Background(), repo, []CommitEvent{{
		SHA:   "sha-3",
		Files: []CommitFile{{Path: "ok.go", Status: "A", Content: "package p\n"}},
	}})

	st.mu.Lock()
	last := st.lastCommit
	statuses := append([]mockStatus(nil), st.statuses...)
	st.mu.Unlock()

	if last != "sha-3" {
		t.Errorf("last_commit = %q, want sha-3", last)
	}
	if len(statuses) != 1 || statuses[0].Status != repos.StatusIdle {
		t.Errorf("statuses = %+v, want a single idle status", statuses)
	}
}

// TestApplyCommitsHonoursIgnorePatterns: a push and a full index pass must
// produce the same index, otherwise the two update paths fight each other.
func TestApplyCommitsHonoursIgnorePatterns(t *testing.T) {
	st := &mockStorage{}
	svc, repo := commitTestService(t, st, []string{"**/node_modules/**", "node_modules/**"})

	unapplied, err := svc.doApplyCommits(context.Background(), repo, []CommitEvent{{
		SHA: "sha-4",
		Files: []CommitFile{
			{Path: "src/app.go", Status: "A", Content: "package app\n"},
			{Path: "node_modules/pkg/index.js", Status: "A", Content: "module.exports = {}\n"},
		},
	}})
	if err != nil {
		t.Fatalf("doApplyCommits() error = %v", err)
	}
	if len(unapplied) != 0 {
		t.Errorf("unapplied = %v, want none", unapplied)
	}

	stored := st.storedPaths()
	if len(stored) != 1 || stored[0] != "src/app.go" {
		t.Errorf("stored files = %v, want only src/app.go (node_modules is ignored)", stored)
	}
}

// TestApplyCommitsRecordsPendingCommit: /sync-state needs the in-flight SHA to
// distinguish "being applied" from "lost".
func TestApplyCommitsRecordsPendingCommit(t *testing.T) {
	st := &mockStorage{repo: &repos.Repo{ID: "repo-1", Name: "test", Source: repos.SourceTypeLocal, Path: t.TempDir()}}
	svc := newTestService(st, map[indexing.IndexType]indexing.Indexer{})
	svc.sources = map[repos.SourceType]repos.RepoSource{
		repos.SourceTypeLocal: &mockSource{name: repos.SourceTypeLocal},
	}

	ack, err := svc.ApplyCommits(context.Background(), "repo-1", []CommitEvent{{
		SHA:   "sha-9",
		Files: []CommitFile{{Path: "a.go", Status: "A", Content: "package a\n"}},
	}})
	if err != nil || !ack.Accepted {
		t.Fatalf("ApplyCommits() = %+v, %v", ack, err)
	}
	svc.wg.Wait()

	st.mu.Lock()
	defer st.mu.Unlock()
	// It is cleared again by the terminal status write once the batch lands.
	if st.pendingCommit != "" {
		t.Errorf("pending_commit = %q, want it cleared after the batch finished", st.pendingCommit)
	}
	if st.lastCommit != "sha-9" {
		t.Errorf("last_commit = %q, want sha-9", st.lastCommit)
	}
}

// TestApplyCommitsContinuingInFlightBatchIsBusyNotGap: the cursor only
// advances once the running batch lands, so a back-to-back push must not be
// told to resend or reindex over a race it cannot see.
func TestApplyCommitsContinuingInFlightBatchIsBusyNotGap(t *testing.T) {
	st := &mockStorage{repo: &repos.Repo{
		ID: "repo-1", Name: "test", Source: repos.SourceTypeLocal, Path: t.TempDir(),
		Status: repos.StatusIndexing, LastCommit: "sha-1", PendingCommit: "sha-2",
	}}
	svc := newTestService(st, nil)

	_, err := svc.ApplyCommits(context.Background(), "repo-1", []CommitEvent{{
		SHA:     "sha-3",
		Parents: []string{"sha-2"}, // continues the batch still being applied
		Files:   []CommitFile{{Path: "a.go", Status: "A", Content: "x"}},
	}})
	if !errors.Is(err, ErrRepoBusy) {
		t.Fatalf("ApplyCommits() error = %v, want ErrRepoBusy", err)
	}

	// A genuinely disconnected commit is still a gap.
	ack, err := svc.ApplyCommits(context.Background(), "repo-1", []CommitEvent{{
		SHA:     "sha-9",
		Parents: []string{"unrelated"},
		Files:   []CommitFile{{Path: "a.go", Status: "A", Content: "x"}},
	}})
	if err != nil || ack.Accepted {
		t.Fatalf("ApplyCommits() = %+v, %v; want a rejected batch with no error (commit gap)", ack, err)
	}
}

// distributedCommitService builds a Service in distributed mode over a
// temporary repo directory, with the queue's own storage double.
func distributedCommitService(t *testing.T, st *mockStorage) *Service {
	t.Helper()
	dir := t.TempDir()
	if st.repo == nil {
		st.repo = &repos.Repo{ID: "repo-1", Name: "test", Source: repos.SourceTypeLocal, Path: dir}
	} else if st.repo.Path == "" {
		st.repo.Path = dir
	}
	svc := newTestService(st, map[indexing.IndexType]indexing.Indexer{
		indexing.IndexTypeAST: &mockIndexer{name: "ast", indexType: indexing.IndexTypeAST},
	})
	svc.cfg = &config.Config{Indexes: config.IndexesConfig{Distributed: true}}
	svc.sources = map[repos.SourceType]repos.RepoSource{
		repos.SourceTypeLocal: &mockSource{name: repos.SourceTypeLocal},
	}
	return svc
}

// TestApplyCommitsDistributedQueuesTheBatch is the durability bug: the
// receiving instance used to apply the batch itself, so its death lost work
// that no job described and no cursor reported.
func TestApplyCommitsDistributedQueuesTheBatch(t *testing.T) {
	st := &mockStorage{}
	svc := distributedCommitService(t, st)

	batch := []CommitEvent{{
		SHA:     "sha-2",
		Parents: []string{"sha-1"},
		Files:   []CommitFile{{Path: "a.go", Status: "A", Content: "package a\n"}},
	}}
	ack, err := svc.ApplyCommits(context.Background(), "repo-1", batch)
	if err != nil || !ack.Accepted {
		t.Fatalf("ApplyCommits() = %+v, %v; want accepted", ack, err)
	}
	svc.wg.Wait()

	// The 202 must say the batch is queued, not that it is being indexed here.
	if ack.Status != "queued" || !ack.Queued || ack.JobID == "" || ack.Target != "sha-2" {
		t.Errorf("ack = %+v, want a queued ack carrying the job id and target", ack)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.commitPayloads) != 1 {
		t.Fatalf("queued payloads = %d, want the batch to be persisted with the job", len(st.commitPayloads))
	}
	// The payload must carry the batch itself: the claiming instance has
	// nothing else to reconstruct it from.
	got, err := decodeCommitBatch(st.commitPayloads[0])
	if err != nil {
		t.Fatalf("decode queued payload: %v", err)
	}
	if len(got) != 1 || got[0].SHA != "sha-2" || len(got[0].Files) != 1 ||
		got[0].Files[0].Path != "a.go" || got[0].Files[0].Content != "package a\n" ||
		got[0].Parents[0] != "sha-1" {
		t.Errorf("decoded batch = %+v, want the pushed commits verbatim", got)
	}
	// The work is the queue's now, not this instance's.
	if st.claimCalls != 0 {
		t.Errorf("repo was claimed %d times; a queued batch is claimed by the worker that runs it", st.claimCalls)
	}
	if len(st.storedFiles) != 0 {
		t.Errorf("files were indexed in-process: %v", st.storedPaths())
	}
	// /sync-state must still call the batch "in flight" while it waits.
	if st.pendingCommit != "sha-2" {
		t.Errorf("pending_commit = %q, want sha-2 so a queued batch is not read as lost", st.pendingCommit)
	}
}

// TestApplyCommitsDistributedChecksGapBeforeQueueing: the 409 has to be
// answered synchronously, or the client learns about a hole in its history
// only from a cursor that stops moving.
func TestApplyCommitsDistributedChecksGapBeforeQueueing(t *testing.T) {
	st := &mockStorage{repo: &repos.Repo{
		ID: "repo-1", Name: "test", Source: repos.SourceTypeLocal, LastCommit: "sha-1",
	}}
	svc := distributedCommitService(t, st)

	ack, err := svc.ApplyCommits(context.Background(), "repo-1", []CommitEvent{{
		SHA:     "sha-9",
		Parents: []string{"unrelated"},
		Files:   []CommitFile{{Path: "a.go", Status: "A", Content: "x"}},
	}})
	if err != nil || ack.Accepted {
		t.Fatalf("ApplyCommits() = %+v, %v; want a rejected batch with no error (commit gap)", ack, err)
	}
	if ack.Before != "sha-1" {
		t.Errorf("ack.Before = %q, want the cursor the client must resend from", ack.Before)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.commitPayloads) != 0 {
		t.Errorf("queued payloads = %d, want a rejected batch to leave nothing behind", len(st.commitPayloads))
	}
	if st.pendingCommit != "" {
		t.Errorf("pending_commit = %q, want it untouched by a rejected batch", st.pendingCommit)
	}
}

// TestApplyCommitsDistributedFailedEnqueueIsNotAdvertised: publishing the
// in-flight SHA for a batch that never reached the queue would make
// /sync-state promise work nobody holds.
func TestApplyCommitsDistributedFailedEnqueueIsNotAdvertised(t *testing.T) {
	st := &mockStorage{enqueueCommitErr: errors.New("db down")}
	svc := distributedCommitService(t, st)

	ack, err := svc.ApplyCommits(context.Background(), "repo-1", []CommitEvent{{
		SHA:   "sha-2",
		Files: []CommitFile{{Path: "a.go", Status: "A", Content: "x"}},
	}})
	if err == nil || ack != nil {
		t.Fatalf("ApplyCommits() = %+v, %v; want the enqueue failure surfaced", ack, err)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.pendingCommit != "" {
		t.Errorf("pending_commit = %q, want nothing advertised for a batch that was never queued", st.pendingCommit)
	}
}

// TestRunQueuedJobAppliesCommitBatch: the batch a worker claims is applied and
// advances the cursor exactly as the in-process path did.
func TestRunQueuedJobAppliesCommitBatch(t *testing.T) {
	st := &mockStorage{}
	svc := distributedCommitService(t, st)

	payload, err := encodeCommitBatch([]CommitEvent{{
		SHA:   "sha-2",
		Files: []CommitFile{{Path: "a.go", Status: "A", Content: "package a\n"}},
	}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	job := &storage.IndexJob{ID: "job-7", RepoID: "repo-1", Kind: storage.JobKindCommits,
		Status: storage.JobStatusRunning, Payload: payload}

	svc.runQueuedJob(context.Background(), job, "worker-1", time.Hour)

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.lastCommit != "sha-2" {
		t.Errorf("last_commit = %q, want the cursor to advance to sha-2", st.lastCommit)
	}
	if len(st.completed) != 1 || st.completed[0].JobID != "job-7" || st.completed[0].Error != "" {
		t.Errorf("job results = %+v, want job-7 recorded as successful", st.completed)
	}
	if len(st.storedFiles) != 1 || st.storedFiles[0].Path != "a.go" {
		t.Errorf("stored files = %+v, want the batch's file indexed by the worker", st.storedFiles)
	}
}

// TestRunQueuedJobWaitsForPrecedingBatch: claiming is per job, so a newer
// batch can be claimed while its predecessor is still queued. Applying it
// would reorder history.
func TestRunQueuedJobWaitsForPrecedingBatch(t *testing.T) {
	st := &mockStorage{
		repo:             &repos.Repo{ID: "repo-1", Name: "test", Source: repos.SourceTypeLocal, LastCommit: "sha-1"},
		earlierCommitJob: true,
	}
	svc := distributedCommitService(t, st)

	payload, err := encodeCommitBatch([]CommitEvent{{SHA: "sha-3", Parents: []string{"sha-2"}}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	svc.runQueuedJob(context.Background(), &storage.IndexJob{
		ID: "job-8", RepoID: "repo-1", Kind: storage.JobKindCommits,
		Status: storage.JobStatusRunning, Payload: payload,
	}, "worker-1", time.Hour)

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.released) != 1 || st.released[0] != "job-8" {
		t.Errorf("released jobs = %v, want job-8 back in the queue", st.released)
	}
	if len(st.completed) != 0 {
		t.Errorf("job results = %+v, want none: waiting is not a failure", st.completed)
	}
	if st.claimCalls != 0 {
		t.Errorf("repo was claimed %d times before the batch was applicable", st.claimCalls)
	}
	if st.lastCommit != "" {
		t.Errorf("last_commit = %q, want the cursor untouched", st.lastCommit)
	}
}

// TestRunQueuedJobFailsWhenTheChainIsBroken: with no predecessor left to wait
// for, requeueing forever would block the queue and never tell anyone. The job
// fails, and the in-flight SHA is retracted so /sync-state stops claiming the
// batch is on its way.
func TestRunQueuedJobFailsWhenTheChainIsBroken(t *testing.T) {
	st := &mockStorage{repo: &repos.Repo{
		ID: "repo-1", Name: "test", Source: repos.SourceTypeLocal,
		LastCommit: "sha-1", PendingCommit: "sha-3",
	}}
	svc := distributedCommitService(t, st)

	payload, err := encodeCommitBatch([]CommitEvent{{SHA: "sha-3", Parents: []string{"sha-2"}}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	svc.runQueuedJob(context.Background(), &storage.IndexJob{
		ID: "job-9", RepoID: "repo-1", Kind: storage.JobKindCommits,
		Status: storage.JobStatusRunning, Payload: payload,
	}, "worker-1", time.Hour)

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.released) != 0 {
		t.Errorf("released jobs = %v, want none: nothing precedes this batch", st.released)
	}
	if len(st.completed) != 1 || st.completed[0].Error == "" {
		t.Fatalf("job results = %+v, want job-9 failed with a reason", st.completed)
	}
	if !strings.Contains(st.completed[0].Error, "commit gap") {
		t.Errorf("job error = %q, want it to name the commit gap", st.completed[0].Error)
	}
	if st.pendingCommit != "" {
		t.Errorf("pending_commit = %q, want the dead batch retracted", st.pendingCommit)
	}
}

// TestRunQueuedJobSkipsAlreadyAppliedBatch: a worker that died between
// applying a batch and recording its job leaves the job to be claimed again.
// The cursor already covers it, so it must not be applied twice.
func TestRunQueuedJobSkipsAlreadyAppliedBatch(t *testing.T) {
	st := &mockStorage{repo: &repos.Repo{
		ID: "repo-1", Name: "test", Source: repos.SourceTypeLocal, LastCommit: "sha-2",
	}}
	svc := distributedCommitService(t, st)

	payload, err := encodeCommitBatch([]CommitEvent{{
		SHA: "sha-2", Parents: []string{"sha-1"},
		Files: []CommitFile{{Path: "a.go", Status: "A", Content: "package a\n"}},
	}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	svc.runQueuedJob(context.Background(), &storage.IndexJob{
		ID: "job-10", RepoID: "repo-1", Kind: storage.JobKindCommits,
		Status: storage.JobStatusRunning, Payload: payload,
	}, "worker-1", time.Hour)

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.completed) != 1 || st.completed[0].Error != "" {
		t.Errorf("job results = %+v, want job-10 recorded as done", st.completed)
	}
	if len(st.storedFiles) != 0 {
		t.Errorf("stored files = %v, want the batch not re-applied", st.storedPaths())
	}
	if st.claimCalls != 0 {
		t.Errorf("repo was claimed %d times to re-apply a batch already in the cursor", st.claimCalls)
	}
}

// TestRunQueuedJobRejectsUndecodablePayload: the batch cannot be recovered, so
// the job fails permanently instead of holding the repo hostage.
func TestRunQueuedJobRejectsUndecodablePayload(t *testing.T) {
	st := &mockStorage{}
	svc := distributedCommitService(t, st)

	svc.runQueuedJob(context.Background(), &storage.IndexJob{
		ID: "job-11", RepoID: "repo-1", Kind: storage.JobKindCommits,
		Status: storage.JobStatusRunning, Payload: "{not json",
	}, "worker-1", time.Hour)

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.completed) != 1 || st.completed[0].Error == "" {
		t.Errorf("job results = %+v, want job-11 failed", st.completed)
	}
	if st.claimCalls != 0 {
		t.Errorf("repo was claimed %d times for a job that can never run", st.claimCalls)
	}
}

// TestCommitJobOrder covers the claim-time re-check of the queue order.
func TestCommitJobOrder(t *testing.T) {
	batch := []CommitEvent{{SHA: "sha-2", Parents: []string{"sha-1"}}}

	cases := []struct {
		name string
		repo *repos.Repo
		want commitBatchOrder
	}{
		{"no cursor yet", &repos.Repo{}, commitBatchNext},
		{"continues the cursor", &repos.Repo{LastCommit: "sha-1"}, commitBatchNext},
		{"cursor already past it", &repos.Repo{LastCommit: "sha-2"}, commitBatchApplied},
		{"predecessor missing", &repos.Repo{LastCommit: "sha-0"}, commitBatchTooEarly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commitJobOrder(tc.repo, batch); got != tc.want {
				t.Errorf("commitJobOrder() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestApplyCommitsBusyRepo maps onto the API's 409 repo_busy.
func TestApplyCommitsBusyRepo(t *testing.T) {
	no := false
	st := &mockStorage{claimOK: &no}
	svc := newTestService(st, nil)

	_, err := svc.ApplyCommits(context.Background(), "repo-1", []CommitEvent{{
		SHA:   "sha-1",
		Files: []CommitFile{{Path: "a.go", Status: "A", Content: "x"}},
	}})
	if !errors.Is(err, ErrRepoBusy) {
		t.Fatalf("ApplyCommits() error = %v, want ErrRepoBusy", err)
	}
}
