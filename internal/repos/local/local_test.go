package local

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/domain"
)

func TestPathAllowed(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "nested", "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	sibling := root + "-evil" // shares a textual prefix but is not inside root

	tests := []struct {
		name  string
		paths []string
		path  string
		want  bool
	}{
		{"empty allowlist allows anything", nil, "/anywhere/at/all", true},
		{"root itself", []string{root}, root, true},
		{"inside root", []string{root}, sub, true},
		{"outside root", []string{root}, "/etc", false},
		{"prefix sibling is not inside", []string{root}, sibling, false},
		{"one of several roots", []string{"/opt/a", root}, sub, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Source{paths: tt.paths}
			if got := s.pathAllowed(tt.path); got != tt.want {
				t.Errorf("pathAllowed(%q) with roots %v = %v, want %v", tt.path, tt.paths, got, tt.want)
			}
		})
	}
}

func TestAdd_AllowlistEnforced(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "repo")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir() // exists, but not under the allowlist

	s := New()
	if err := s.Init(context.Background(), map[string]interface{}{"paths": []string{root}}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Add(context.Background(), &domain.AddRequest{Path: inside}); err != nil {
		t.Errorf("Add of allowed path failed: %v", err)
	}
	if _, err := s.Add(context.Background(), &domain.AddRequest{Path: outside}); err == nil {
		t.Error("Add of path outside allowlist should fail, got nil error")
	}
}

func TestAdd_NoAllowlistIsUnrestricted(t *testing.T) {
	dir := t.TempDir()
	s := New() // no Init, empty allowlist
	if _, err := s.Add(context.Background(), &domain.AddRequest{Path: dir}); err != nil {
		t.Errorf("Add without allowlist should succeed, got %v", err)
	}
}

func TestAdd_MissingPath(t *testing.T) {
	s := New()
	if _, err := s.Add(context.Background(), &domain.AddRequest{Path: filepath.Join(t.TempDir(), "does-not-exist")}); err == nil {
		t.Error("Add of nonexistent path should fail")
	}
}

// The walk itself is tested in the repos package; this covers the wiring, that
// the patterns the caller passes reach it and that a local checkout's .git is
// left alone.
func TestGetFiles_HonoursPatternsAndSkipsGit(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"src/main.go",
		"node_modules/left-pad/index.js",
		".git/hooks/bootstrap.py",
		".github/workflows/ci.yml",
	} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	files, err := New().GetFiles(context.Background(), &domain.Repo{Path: dir}, []string{"**/node_modules/**"})
	if err != nil {
		t.Fatalf("GetFiles: %v", err)
	}
	got := make([]string, 0, len(files))
	for _, f := range files {
		got = append(got, filepath.ToSlash(f.Path))
	}
	slices.Sort(got)

	want := []string{".github/workflows/ci.yml", "src/main.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("files = %q, want %q", got, want)
	}
}
