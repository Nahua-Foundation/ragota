package fileutil

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Matcher: edge cases
// ---------------------------------------------------------------------------

func TestNewMatcher_NilPatterns(t *testing.T) {
	m := NewMatcher(nil)
	assert.False(t, m.IsIgnored("anything", false))
	assert.False(t, m.IsIgnored(".git", true))
}

func TestNewMatcher_EmptyPatterns(t *testing.T) {
	m := NewMatcher([]string{})
	assert.False(t, m.IsIgnored("vendor", true))
}

func TestMatcher_IsIgnored_DotAndEmpty(t *testing.T) {
	m := NewMatcher([]string{"vendor", "*.log"})
	assert.False(t, m.IsIgnored(".", true))
	assert.False(t, m.IsIgnored("", false))
}

func TestMatcher_IsIgnored_GlobPatterns(t *testing.T) {
	m := NewMatcher([]string{"*.log", "test_*", "build-??"})

	assert.True(t, m.IsIgnored("app.log", false))
	assert.True(t, m.IsIgnored("debug.log", false))
	assert.False(t, m.IsIgnored("app.txt", false))

	assert.True(t, m.IsIgnored("test_main.go", false))
	assert.True(t, m.IsIgnored("test_utils.go", false))
	assert.False(t, m.IsIgnored("main_test.go", false))

	assert.True(t, m.IsIgnored("build-01", true))
	assert.True(t, m.IsIgnored("build-ab", true))
	assert.False(t, m.IsIgnored("build-001", true)) // 3 chars, not 2
}

func TestMatcher_IsIgnored_PathComponentMatching(t *testing.T) {
	m := NewMatcher([]string{"vendor"})

	// Direct match at any depth
	assert.True(t, m.IsIgnored("vendor", true))
	assert.True(t, m.IsIgnored("a/vendor", true))
	assert.True(t, m.IsIgnored("a/b/c/vendor", true))

	// Files under vendor
	assert.True(t, m.IsIgnored("vendor/pkg/file.go", false))
	assert.True(t, m.IsIgnored("a/b/vendor/file.go", false))

	// Not matching partial names
	assert.False(t, m.IsIgnored("myvendor", true))
	assert.False(t, m.IsIgnored("vendor-tools", true))
}

func TestMatcher_IsIgnored_ExactGlobOnRelPath(t *testing.T) {
	m := NewMatcher([]string{"src/*.test.go"})
	assert.True(t, m.IsIgnored("src/foo.test.go", false))
	assert.False(t, m.IsIgnored("src/sub/foo.test.go", false)) // * doesn't cross /
}

func TestMatcher_IsIgnored_MultiplePatterns(t *testing.T) {
	m := NewMatcher([]string{".git", "node_modules", "dist", "*.min.js", "*.map"})

	tests := []struct {
		rel     string
		isDir   bool
		ignored bool
	}{
		{".git", true, true},
		{".git/HEAD", false, true},
		{"node_modules", true, true},
		{"node_modules/react/index.js", false, true},
		{"dist", true, true},
		{"dist/app.js", false, true},
		{"jquery.min.js", false, true},
		{"app.min.js", false, true},
		{"main.js", false, false},
		{"app.js.map", false, true},
		{"src/app.js", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			assert.Equal(t, tt.ignored, m.IsIgnored(tt.rel, tt.isDir),
				"IsIgnored(%q, %v)", tt.rel, tt.isDir)
		})
	}
}

func TestMatcher_PatternsCloned(t *testing.T) {
	patterns := []string{"vendor"}
	m := NewMatcher(patterns)
	patterns[0] = "changed" // should not affect matcher
	assert.True(t, m.IsIgnored("vendor", true))
}

// ---------------------------------------------------------------------------
// LanguageByExt: comprehensive
// ---------------------------------------------------------------------------

func TestLanguageByExt_AllSupported(t *testing.T) {
	tests := map[string]string{
		".go":    "go",
		".ts":    "typescript",
		".tsx":   "typescript",
		".js":    "javascript",
		".jsx":   "javascript",
		".mjs":   "javascript",
		".cjs":   "javascript",
		".py":    "python",
		".java":  "java",
		".proto": "proto",
		".md":    "text",
		".rst":   "text",
		".txt":   "text",
		".json":  "json",
		".yaml":  "yaml",
		".yml":   "yaml",
		".toml":  "toml",
	}
	for ext, want := range tests {
		t.Run(ext, func(t *testing.T) {
			assert.Equal(t, want, LanguageByExt(ext))
		})
	}
}

func TestLanguageByExt_CaseInsensitive(t *testing.T) {
	assert.Equal(t, "go", LanguageByExt(".GO"))
	assert.Equal(t, "go", LanguageByExt(".Go"))
	assert.Equal(t, "python", LanguageByExt(".PY"))
	assert.Equal(t, "java", LanguageByExt(".JAVA"))
	assert.Equal(t, "typescript", LanguageByExt(".TS"))
	assert.Equal(t, "typescript", LanguageByExt(".TSX"))
}

func TestLanguageByExt_Unknown(t *testing.T) {
	assert.Equal(t, "", LanguageByExt(".rs"))
	assert.Equal(t, "", LanguageByExt(".cpp"))
	assert.Equal(t, "", LanguageByExt(".rb"))
	assert.Equal(t, "", LanguageByExt(""))
	assert.Equal(t, "", LanguageByExt("go")) // no dot
}

// ---------------------------------------------------------------------------
// HashFile / HashBytes
// ---------------------------------------------------------------------------

func TestHashBytes_Empty(t *testing.T) {
	h := HashBytes([]byte{})
	assert.NotEmpty(t, h)
	assert.Len(t, h, 40) // sha1 hex = 40 chars
}

func TestHashBytes_Deterministic(t *testing.T) {
	data := []byte("deterministic test data")
	h1 := HashBytes(data)
	h2 := HashBytes(data)
	assert.Equal(t, h1, h2)
}

func TestHashBytes_DifferentInputs(t *testing.T) {
	h1 := HashBytes([]byte("input1"))
	h2 := HashBytes([]byte("input2"))
	assert.NotEqual(t, h1, h2)
}

func TestHashFile_NonExistent(t *testing.T) {
	_, err := HashFile("/nonexistent/path/file.txt")
	assert.Error(t, err)
}

func TestHashFile_EmptyFile(t *testing.T) {
	f, err := os.CreateTemp("", "hashfile_empty_*")
	require.NoError(t, err)
	defer os.Remove(f.Name())
	f.Close()

	h, err := HashFile(f.Name())
	require.NoError(t, err)
	assert.Equal(t, HashBytes([]byte{}), h)
}

func TestHashFile_LargeFile(t *testing.T) {
	f, err := os.CreateTemp("", "hashfile_large_*")
	require.NoError(t, err)
	defer os.Remove(f.Name())

	data := strings.Repeat("x", 100000)
	_, err = f.WriteString(data)
	require.NoError(t, err)
	f.Close()

	h, err := HashFile(f.Name())
	require.NoError(t, err)
	assert.Equal(t, HashBytes([]byte(data)), h)
}

// ---------------------------------------------------------------------------
// SecureJoin: edge cases
// ---------------------------------------------------------------------------

func TestSecureJoin_TraversalVariants(t *testing.T) {
	root := t.TempDir()

	tests := []struct {
		rel     string
		wantErr bool
	}{
		{"../etc/passwd", true},
		{"../../etc/passwd", true},
		{"sub/../../etc/passwd", true},
		{"./sub/file.go", false},
		{"sub/file.go", false},
		{".", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			_, err := SecureJoin(root, tt.rel)
			if tt.wantErr {
				assert.Error(t, err, "SecureJoin(%q, %q) should error", root, tt.rel)
			} else {
				assert.NoError(t, err, "SecureJoin(%q, %q) should succeed", root, tt.rel)
			}
		})
	}
}

func TestSecureJoin_AbsolutePathInsideRoot(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "sub", "file.go")
	got, err := SecureJoin(root, inside)
	require.NoError(t, err)
	assert.Equal(t, inside, got)
}

func TestSecureJoin_AbsolutePathOutsideRoot(t *testing.T) {
	root := t.TempDir()
	_, err := SecureJoin(root, "/etc/passwd")
	assert.Error(t, err)
}

func TestSecureJoin_DotDotSlash(t *testing.T) {
	root := t.TempDir()
	_, err := SecureJoin(root, "a/b/../../../etc/passwd")
	assert.Error(t, err)
}

func TestSecureJoin_CleanPath(t *testing.T) {
	root := t.TempDir()
	got, err := SecureJoin(root, "a/../a/file.go")
	require.NoError(t, err)
	assert.Contains(t, got, "a/file.go")
}

// ---------------------------------------------------------------------------
// WalkFiles: additional cases
// ---------------------------------------------------------------------------

func writeFileHelper(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func TestWalkFiles_DeepDirectoryStructure(t *testing.T) {
	root := t.TempDir()
	writeFileHelper(t, filepath.Join(root, "a", "b", "c", "d.go"), "x")
	writeFileHelper(t, filepath.Join(root, "a", "b", "e.go"), "x")
	writeFileHelper(t, filepath.Join(root, "f.go"), "x")

	var got []string
	err := WalkFiles(root, nil, []string{".go"}, func(_, rel string, _ fs.FileInfo) error {
		got = append(got, filepath.ToSlash(rel))
		return nil
	})
	require.NoError(t, err)
	sort.Strings(got)
	assert.Equal(t, []string{"a/b/c/d.go", "a/b/e.go", "f.go"}, got)
}

func TestWalkFiles_IgnoredDirSkipsAllContents(t *testing.T) {
	root := t.TempDir()
	writeFileHelper(t, filepath.Join(root, "vendor", "a.go"), "x")
	writeFileHelper(t, filepath.Join(root, "vendor", "sub", "b.go"), "x")
	writeFileHelper(t, filepath.Join(root, "main.go"), "x")

	m := NewMatcher([]string{"vendor"})
	var got []string
	err := WalkFiles(root, m, []string{".go"}, func(_, rel string, _ fs.FileInfo) error {
		got = append(got, filepath.ToSlash(rel))
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"main.go"}, got)
}

func TestWalkFiles_EmptyDir(t *testing.T) {
	root := t.TempDir()
	var n int
	err := WalkFiles(root, nil, nil, func(_, _ string, _ fs.FileInfo) error {
		n++
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestWalkFiles_MultipleExtensions(t *testing.T) {
	root := t.TempDir()
	writeFileHelper(t, filepath.Join(root, "a.go"), "x")
	writeFileHelper(t, filepath.Join(root, "b.py"), "x")
	writeFileHelper(t, filepath.Join(root, "c.ts"), "x")
	writeFileHelper(t, filepath.Join(root, "d.txt"), "x")

	var got []string
	err := WalkFiles(root, nil, []string{".go", ".py"}, func(_, rel string, _ fs.FileInfo) error {
		got = append(got, filepath.ToSlash(rel))
		return nil
	})
	require.NoError(t, err)
	sort.Strings(got)
	assert.Equal(t, []string{"a.go", "b.py"}, got)
}

func TestWalkFiles_CallbackError(t *testing.T) {
	root := t.TempDir()
	writeFileHelper(t, filepath.Join(root, "a.go"), "x")
	writeFileHelper(t, filepath.Join(root, "b.go"), "x")

	expectedErr := os.ErrPermission
	var count int
	err := WalkFiles(root, nil, []string{".go"}, func(_, _ string, _ fs.FileInfo) error {
		count++
		return expectedErr
	})
	assert.ErrorIs(t, err, expectedErr)
	assert.Equal(t, 1, count, "should stop after first error")
}

func TestWalkFiles_NonExistentRoot(t *testing.T) {
	var called bool
	err := WalkFiles("/nonexistent/dir/12345", nil, nil, func(_, _ string, _ fs.FileInfo) error {
		called = true
		return nil
	})
	// WalkFiles swallows the root-level error from WalkDir (returns nil),
	// but the callback should never be called.
	assert.NoError(t, err)
	assert.False(t, called, "callback should not be called for non-existent root")
}

func TestWalkFiles_IgnoredFilesNotMatched(t *testing.T) {
	root := t.TempDir()
	writeFileHelper(t, filepath.Join(root, "main.go"), "x")
	writeFileHelper(t, filepath.Join(root, "main.pb.go"), "x")
	writeFileHelper(t, filepath.Join(root, "test_grpc.pb.go"), "x")

	m := NewMatcher([]string{"*.pb.go"})
	var got []string
	err := WalkFiles(root, m, []string{".go"}, func(_, rel string, _ fs.FileInfo) error {
		got = append(got, filepath.ToSlash(rel))
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"main.go"}, got)
}

func TestWalkFiles_HiddenFiles(t *testing.T) {
	root := t.TempDir()
	writeFileHelper(t, filepath.Join(root, ".hidden.go"), "x")
	writeFileHelper(t, filepath.Join(root, "visible.go"), "x")

	var got []string
	err := WalkFiles(root, nil, []string{".go"}, func(_, rel string, _ fs.FileInfo) error {
		got = append(got, filepath.ToSlash(rel))
		return nil
	})
	require.NoError(t, err)
	sort.Strings(got)
	assert.Equal(t, []string{".hidden.go", "visible.go"}, got)
}

func TestWalkFiles_HiddenDirIgnored(t *testing.T) {
	root := t.TempDir()
	writeFileHelper(t, filepath.Join(root, ".git", "config"), "x")
	writeFileHelper(t, filepath.Join(root, "main.go"), "x")

	m := NewMatcher([]string{".git"})
	var got []string
	err := WalkFiles(root, m, nil, func(_, rel string, _ fs.FileInfo) error {
		got = append(got, filepath.ToSlash(rel))
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"main.go"}, got)
}
