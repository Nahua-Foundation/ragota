package testutil

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestdataPath(t *testing.T, rel string) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	return filepath.Join(root, "testdata", rel)
}
