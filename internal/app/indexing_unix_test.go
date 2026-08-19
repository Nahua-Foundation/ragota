//go:build unix

package app

import (
	"path/filepath"
	"syscall"
	"testing"
)

// mkfifo creates a named pipe in dir and returns its repo-relative path. A
// fifo is the case that cannot be handled by reading and recovering: opening
// one blocks the scan worker until something else writes to it, so it has to
// be recognized before the file is opened at all.
func mkfifo(t *testing.T, dir string) (string, bool) {
	t.Helper()
	name := "pipe.go"
	if err := syscall.Mkfifo(filepath.Join(dir, name), 0o644); err != nil {
		t.Logf("mkfifo unavailable here: %v", err)
		return "", false
	}
	return name, true
}
