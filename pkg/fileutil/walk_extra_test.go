package fileutil

import (
	"crypto/sha1"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// helpers ---------------------------------------------------------------

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// WalkFiles -------------------------------------------------------------

func TestWalkFiles_FiltersByExtensionAndIgnore(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.go"), "package a")
	writeFile(t, filepath.Join(root, "b.txt"), "hello")
	writeFile(t, filepath.Join(root, "vendor", "v.go"), "package v")
	writeFile(t, filepath.Join(root, "sub", "c.go"), "package c")
	writeFile(t, filepath.Join(root, "sub", "d.pb.go"), "package d")

	m := NewMatcher([]string{"vendor", "*.pb.go"})

	var got []string
	err := WalkFiles(root, m, []string{".go"}, func(abs, rel string, info fs.FileInfo) error {
		if info == nil {
			t.Errorf("nil info for %s", rel)
		}
		got = append(got, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("WalkFiles: %v", err)
	}
	sort.Strings(got)
	want := []string{"a.go", "sub/c.go"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestWalkFiles_NoExtFilter_TakesAllFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.go"), "x")
	writeFile(t, filepath.Join(root, "b.txt"), "y")

	var n int
	err := WalkFiles(root, nil, nil, func(_, _ string, _ fs.FileInfo) error {
		n++
		return nil
	})
	if err != nil {
		t.Fatalf("WalkFiles: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 files, got %d", n)
	}
}

func TestWalkFiles_NilMatcher(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "x.go"), "x")
	var n int
	err := WalkFiles(root, nil, []string{".go"}, func(_, _ string, _ fs.FileInfo) error {
		n++
		return nil
	})
	if err != nil || n != 1 {
		t.Fatalf("WalkFiles: err=%v n=%d", err, n)
	}
}

func TestWalkFiles_CaseInsensitiveExt(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "X.GO"), "x")
	var n int
	_ = WalkFiles(root, nil, []string{".go"}, func(_, _ string, _ fs.FileInfo) error {
		n++
		return nil
	})
	if n != 1 {
		t.Errorf("case-insensitive ext: expected 1, got %d", n)
	}
}

// Hash ------------------------------------------------------------------

func TestHashBytes_Stable(t *testing.T) {
	data := []byte("hello")
	h := sha1.Sum(data)
	want := hex.EncodeToString(h[:])
	if got := HashBytes(data); got != want {
		t.Errorf("HashBytes = %s, want %s", got, want)
	}
	if HashBytes(nil) == HashBytes([]byte("x")) {
		t.Error("HashBytes(nil) must differ from non-empty")
	}
}

func TestHashFile(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "f.txt")
	writeFile(t, p, "abc")
	got, err := HashFile(p)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}
	want := HashBytes([]byte("abc"))
	if got != want {
		t.Errorf("HashFile = %s, want %s", got, want)
	}

	if _, err := HashFile(filepath.Join(root, "does-not-exist")); err == nil {
		t.Error("expected error on missing file")
	}
}

// SecureJoin ------------------------------------------------------------

func TestSecureJoin(t *testing.T) {
	root := t.TempDir()
	abs, err := SecureJoin(root, "sub/file.go")
	if err != nil {
		t.Fatalf("SecureJoin: %v", err)
	}
	if !strings.HasPrefix(abs, root) {
		t.Errorf("result %q must be inside %q", abs, root)
	}

	// Equal to root => OK.
	if _, err := SecureJoin(root, "."); err != nil {
		t.Errorf("SecureJoin root: %v", err)
	}

	// Path traversal must fail.
	if _, err := SecureJoin(root, "../etc/passwd"); err == nil {
		t.Error("expected traversal error for ../etc/passwd")
	}

	// Absolute path outside root must fail.
	if _, err := SecureJoin(root, "/etc/passwd"); err == nil {
		t.Error("expected traversal error for absolute outside path")
	}

	// Absolute path inside root must succeed.
	inside := filepath.Join(root, "x", "y.go")
	got, err := SecureJoin(root, inside)
	if err != nil {
		t.Fatalf("SecureJoin abs inside: %v", err)
	}
	if got != inside {
		t.Errorf("got %s, want %s", got, inside)
	}
}
