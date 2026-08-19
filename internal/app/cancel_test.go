package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/repos/local"
)

// A pass stopped by shutdown must not leave the repository looking broken.
// Quitting the dashboard cancels the context under every index pass in flight;
// recording StatusError for that would persist a lie past the process, and the
// next start would show a repository whose only problem was that somebody
// pressed q — with "context canceled" as its last error, which no reader can
// tell from a real fault.
func TestCancelledIndexLeavesTheRepoIdleNotFailed(t *testing.T) {
	st := &mockStorage{}
	dir := t.TempDir()
	// Something to index, or the pass finishes trivially without ever reaching
	// the context and the test proves nothing.
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc Handler() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := &domain.Repo{ID: "r1", Name: "r1", Path: dir, Source: domain.SourceTypeLocal}
	if err := st.StoreRepo(context.Background(), repo); err != nil {
		t.Fatal(err)
	}

	svc := newTestService(st, nil)
	defer svc.cancelBase()
	// A source has to exist or runIndex fails before it reaches the point this
	// test is about.
	local := local.New()
	if err := local.Init(context.Background(), map[string]interface{}{"paths": []string{repo.Path}}); err != nil {
		t.Fatal(err)
	}
	svc.sources = map[domain.SourceType]repos.RepoSource{domain.SourceTypeLocal: local}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the pass starts already cancelled, as it would mid-shutdown

	err := svc.runIndex(ctx, repo, false)
	if err == nil {
		t.Fatal("runIndex returned nil for a cancelled pass; the caller has to be able to tell")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	got, gerr := st.GetRepo(context.Background(), repo.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.Status == domain.StatusError {
		t.Errorf("status = %q with last_error %q; a cancelled pass is not a failed one",
			got.Status, got.LastError)
	}
	if got.LastError != "" {
		t.Errorf("last_error = %q, want empty: nothing failed", got.LastError)
	}
}
