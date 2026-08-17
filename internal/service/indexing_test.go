package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/indexing"
	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/repos/local"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// newLocalTestService builds a Service over a real local repo source, which is
// what the scan path needs: the file listing comes from an Lstat walk, so
// dangling symlinks and non-regular files reach scanFile exactly as they do in
// a real repository.
func newLocalTestService(t *testing.T, st storage.Storage, indexers map[indexing.IndexType]indexing.Indexer) *Service {
	t.Helper()
	src := local.New()
	if err := src.Init(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	svc := newTestService(st, indexers)
	svc.sources = map[repos.SourceType]repos.RepoSource{repos.SourceTypeLocal: src}
	return svc
}

// TestDoIndexSkipsUnreadableFiles: one file nobody can read must not take the
// repository with it. argo-cd ships a deliberately dangling symlink in test
// data, and it left all 1536 of its other files unindexed.
func TestDoIndexSkipsUnreadableFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "good.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "link-to-nowhere", "nowhere.go"), filepath.Join(dir, "dangling.go")); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		if err := os.WriteFile(filepath.Join(dir, "locked.go"), []byte("package main\n"), 0o000); err != nil {
			t.Fatal(err)
		}
	}

	st := &mockStorage{}
	svc := newLocalTestService(t, st, nil)
	repo := &repos.Repo{ID: "r1", Name: "t", Source: repos.SourceTypeLocal, Path: dir}

	if err := svc.doIndex(ctx, repo, false); err != nil {
		t.Fatalf("doIndex() error = %v, want the unreadable files to be skipped", err)
	}
	got := st.storedPaths()
	if len(got) != 1 || got[0] != "good.go" {
		t.Errorf("indexed files = %v, want only good.go", got)
	}
}

// TestScanFileClassifiesUnreadablePaths pins the classification itself: every
// one of these is a property of the file, never of the run.
func TestScanFileClassifiesUnreadablePaths(t *testing.T) {
	dir := t.TempDir()

	// A file that vanished between the listing and the scan.
	vanished := "gone.go"

	dangling := "dangling.go"
	if err := os.Symlink(filepath.Join(dir, "nowhere", "nowhere.go"), filepath.Join(dir, dangling)); err != nil {
		t.Fatal(err)
	}

	locked := "locked.go"
	if err := os.WriteFile(filepath.Join(dir, locked), []byte("package main\n"), 0o000); err != nil {
		t.Fatal(err)
	}

	paths := []string{vanished, dangling}
	if os.Geteuid() != 0 {
		// Root reads a 0000 file regardless, so the case says nothing there.
		paths = append(paths, locked)
	}
	if fifo, ok := mkfifo(t, dir); ok {
		paths = append(paths, fifo)
	}

	svc := newTestService(&mockStorage{}, nil)
	repo := &repos.Repo{ID: "r1", Path: dir}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			got := svc.scanFile(repo, &repos.RepoFile{Path: path, Language: "go"}, nil)
			if got.unreadable == nil {
				t.Fatalf("scanFile(%s).unreadable = nil, want the file to be skipped", path)
			}
			if !strings.Contains(got.unreadable.Error(), path) {
				t.Errorf("scanFile(%s).unreadable = %v, want the path in the message", path, got.unreadable)
			}
		})
	}
}

// TestIndexStorageFailureIsFatal is the other half of the same change: a store
// that stopped answering says nothing about which files are indexed, and
// treating that like an unreadable file would silently re-index (or skip) the
// whole repository while hiding a broken database. The check is now one
// snapshot per pass instead of one query per file, so the failure surfaces
// from doIndex.
func TestIndexStorageFailureIsFatal(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "good.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dbDown := errors.New("connection refused")
	st := &mockStorage{filesByRepoErr: dbDown}
	svc := newLocalTestService(t, st, nil)
	repo := &repos.Repo{ID: "r1", Name: "t", Source: repos.SourceTypeLocal, Path: dir}

	if err := svc.doIndex(ctx, repo, false); !errors.Is(err, dbDown) {
		t.Fatalf("doIndex() error = %v, want %v", err, dbDown)
	}
	if got := st.storedPaths(); len(got) != 0 {
		t.Errorf("stored files = %v, want none: a pass that cannot read the index must not record any", got)
	}
}

// TestScanFileUnknownPathIsNotAFailure: a file with no stored row is the
// normal first-index case.
func TestScanFileUnknownPathIsNotAFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "good.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := newTestService(&mockStorage{}, nil)
	known := map[string]*storage.File{"other.go": {Path: "other.go", Indexed: true}}
	got := svc.scanFile(&repos.Repo{ID: "r1", Path: dir}, &repos.RepoFile{Path: "good.go", Language: "go"}, known)
	if got.unreadable != nil || got.skipped {
		t.Fatalf("scanFile() = %+v, want the file queued for indexing", got)
	}
	if got.toIndex == nil || got.toIndex.Path != "good.go" {
		t.Fatalf("scanFile().toIndex = %+v, want good.go", got.toIndex)
	}
}

// TestScanFileSkipsUnchangedIndexedFile pins the one thing the snapshot is
// for: a file whose stored row matches the content hash is not re-indexed.
func TestScanFileSkipsUnchangedIndexedFile(t *testing.T) {
	dir := t.TempDir()
	content := []byte("package main\n")
	if err := os.WriteFile(filepath.Join(dir, "good.go"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	hash := repos.HashContent(content)

	svc := newTestService(&mockStorage{}, nil)
	f := &repos.RepoFile{Path: "good.go", Language: "go"}
	repo := &repos.Repo{ID: "r1", Path: dir}

	indexed := map[string]*storage.File{"good.go": {Path: "good.go", Hash: hash, Indexed: true}}
	if got := svc.scanFile(repo, f, indexed); !got.skipped {
		t.Errorf("scanFile() = %+v, want the unchanged file skipped", got)
	}
	stale := map[string]*storage.File{"good.go": {Path: "good.go", Hash: "other", Indexed: true}}
	if got := svc.scanFile(repo, f, stale); got.skipped {
		t.Error("scanFile() skipped a file whose stored hash differs")
	}
	unfinished := map[string]*storage.File{"good.go": {Path: "good.go", Hash: hash}}
	if got := svc.scanFile(repo, f, unfinished); got.skipped {
		t.Error("scanFile() skipped a file whose stored row is not marked indexed")
	}
	// A forced pass asks nothing.
	if got := svc.scanFile(repo, f, nil); got.skipped {
		t.Error("scanFile(nil snapshot) skipped a file; a forced pass must re-index everything")
	}
}
