//go:build unix

package repos

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestWalkFilesPrunesIgnoredDirectories asserts that an ignored directory is
// never descended into, which the returned file list cannot show: rejecting
// each of its files one by one yields exactly the same list, which is how the
// walk behaved while both filepath.SkipDir branches were unreachable.
//
// The evidence is a directory nested inside the ignored one that may not be
// read. A walk that descends reports the error; a walk that prunes never
// learns the directory is there. The control run without the pattern is what
// makes that a proof rather than an assumption about the permission bits.
func TestWalkFilesPrunesIgnoredDirectories(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads directories regardless of their mode, so an unreadable directory proves nothing")
	}

	root := writeTree(t,
		"src/main.go",
		"web/node_modules/react/index.js",
		"web/node_modules/react/package.json",
		"web/node_modules/left-pad/index.js",
	)
	blocked := filepath.Join(root, "web", "node_modules", "react", "lib")
	writeFile(t, filepath.Join(blocked, "deep.js"), "x\n")
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	// t.TempDir's cleanup cannot remove a directory it may not read.
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })

	if _, err := WalkFiles(root, nil); err == nil {
		t.Fatal("walking the unreadable directory should have failed; without that the test below proves nothing")
	}

	want := []string{"src/main.go"}
	if got := walkPaths(t, root, []string{"**/node_modules/**"}); !reflect.DeepEqual(got, want) {
		t.Errorf("files = %q, want %q", got, want)
	}
}
