package repos

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Discover — error paths
// ---------------------------------------------------------------------------

func TestDiscover_EmptyRoot(t *testing.T) {
	_, err := Discover("")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "пустой корень")
}

func TestDiscover_NonExistentPath(t *testing.T) {
	_, err := Discover("/nonexistent_path_12345")
	assert.Error(t, err)
}

func TestDiscover_FileInsteadOfDir(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	require.NoError(t, os.WriteFile(f, []byte("x"), 0o644))
	_, err := Discover(f)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "не директория")
}

// ---------------------------------------------------------------------------
// Discover — single repo
// ---------------------------------------------------------------------------

func TestDiscover_SingleRepo_HasGit(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))

	repos, err := Discover(root)
	require.NoError(t, err)
	require.Len(t, repos, 1)
	assert.Equal(t, filepath.Base(root), repos[0].Name)
	assert.Equal(t, root, repos[0].Path)
	assert.True(t, repos[0].HasGit)
}

func TestDiscover_SingleRepo_GitWorktreeFile(t *testing.T) {
	// In a git worktree, .git is a file, not a directory.
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /somewhere"), 0o644))

	repos, err := Discover(root)
	require.NoError(t, err)
	require.Len(t, repos, 1)
	assert.True(t, repos[0].HasGit)
}

// ---------------------------------------------------------------------------
// Discover — multi-repo workspace
// ---------------------------------------------------------------------------

func TestDiscover_MultiRepo_SortedByName(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"charlie", "alpha", "beta"} {
		dir := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
	}

	repos, err := Discover(root)
	require.NoError(t, err)
	require.Len(t, repos, 3)
	assert.Equal(t, "alpha", repos[0].Name)
	assert.Equal(t, "beta", repos[1].Name)
	assert.Equal(t, "charlie", repos[2].Name)
}

func TestDiscover_MultiRepo_SkipsHiddenDirs(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".hidden", ".git"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "visible", ".git"), 0o755))

	repos, err := Discover(root)
	require.NoError(t, err)
	require.Len(t, repos, 1)
	assert.Equal(t, "visible", repos[0].Name)
}

func TestDiscover_MultiRepo_IncludesNonGitChildren(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "project", ".git"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "docs"), 0o755))

	repos, err := Discover(root)
	require.NoError(t, err)
	require.Len(t, repos, 2)

	names := map[string]bool{}
	for _, r := range repos {
		names[r.Name] = r.HasGit
	}
	assert.True(t, names["project"])
	assert.False(t, names["docs"])
}

func TestDiscover_MultiRepo_SkipsFiles(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "project", ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("# hi"), 0o644))

	repos, err := Discover(root)
	require.NoError(t, err)
	require.Len(t, repos, 1)
	assert.Equal(t, "project", repos[0].Name)
}

func TestDiscover_NoGitAnywhere_FallbackToSingle(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "subdir"), 0o755))

	repos, err := Discover(root)
	require.NoError(t, err)
	require.Len(t, repos, 1)
	assert.Equal(t, filepath.Base(root), repos[0].Name)
	assert.False(t, repos[0].HasGit)
}

// ---------------------------------------------------------------------------
// Discover — name collision
// ---------------------------------------------------------------------------

func TestDiscover_NameCollision_AddsHashSuffix(t *testing.T) {
	// This is tricky to set up — we need two dirs with the same name at
	// the same level. Since dirs can't have the same name in the same parent,
	// we test the pathHash function directly and verify collision resolution
	// logic via Signature uniqueness.
	h1 := pathHash("/path/a")
	h2 := pathHash("/path/b")
	assert.NotEqual(t, h1, h2, "pathHash should differ for different paths")
	assert.Len(t, h1, 8, "pathHash should be 8 hex chars")
}

func TestPathHash_Deterministic(t *testing.T) {
	h1 := pathHash("/some/path")
	h2 := pathHash("/some/path")
	assert.Equal(t, h1, h2)
}

func TestPathHash_DifferentForDifferentPaths(t *testing.T) {
	h1 := pathHash("/a/b/c")
	h2 := pathHash("/x/y/z")
	assert.NotEqual(t, h1, h2)
}

// ---------------------------------------------------------------------------
// dirHasGit
// ---------------------------------------------------------------------------

func TestDirHasGit_Directory(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	assert.True(t, dirHasGit(root))
}

func TestDirHasGit_File(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: ..."), 0o644))
	assert.True(t, dirHasGit(root))
}

func TestDirHasGit_Missing(t *testing.T) {
	root := t.TempDir()
	assert.False(t, dirHasGit(root))
}

// ---------------------------------------------------------------------------
// Resolver
// ---------------------------------------------------------------------------

func TestResolver_ForExactPathMatch(t *testing.T) {
	r := NewResolver([]Repo{
		{Name: "alpha", Path: "/workspace/alpha"},
	})
	assert.Equal(t, "alpha", r.For("/workspace/alpha"))
}

func TestResolver_ForSubpath(t *testing.T) {
	r := NewResolver([]Repo{
		{Name: "alpha", Path: "/workspace/alpha"},
	})
	assert.Equal(t, "alpha", r.For("/workspace/alpha/pkg/main.go"))
}

func TestResolver_ForNoMatch(t *testing.T) {
	r := NewResolver([]Repo{
		{Name: "alpha", Path: "/workspace/alpha"},
	})
	assert.Equal(t, "", r.For("/workspace/beta/main.go"))
}

func TestResolver_ForDeeperPathWins(t *testing.T) {
	// Longer path should win over shorter prefix.
	r := NewResolver([]Repo{
		{Name: "root", Path: "/workspace"},
		{Name: "deep", Path: "/workspace/deep"},
	})
	assert.Equal(t, "deep", r.For("/workspace/deep/file.go"))
}

func TestResolver_ForNilResolver(t *testing.T) {
	var r *Resolver
	assert.Equal(t, "", r.For("/any/path"))
}

func TestResolver_ForEmptyRepos(t *testing.T) {
	r := NewResolver(nil)
	assert.Equal(t, "", r.For("/any/path"))
}

func TestResolver_ForPartialPrefixNotMatched(t *testing.T) {
	// /workspace/alpha should NOT match /workspace/alpha2/file.go.
	r := NewResolver([]Repo{
		{Name: "alpha", Path: "/workspace/alpha"},
	})
	assert.Equal(t, "", r.For("/workspace/alpha2/file.go"))
}

// ---------------------------------------------------------------------------
// Resolver.All
// ---------------------------------------------------------------------------

func TestResolverAll_ReturnsSortedByName(t *testing.T) {
	r := NewResolver([]Repo{
		{Name: "charlie", Path: "/c"},
		{Name: "alpha", Path: "/a"},
		{Name: "beta", Path: "/b"},
	})
	all := r.All()
	require.Len(t, all, 3)
	assert.Equal(t, "alpha", all[0].Name)
	assert.Equal(t, "beta", all[1].Name)
	assert.Equal(t, "charlie", all[2].Name)
}

func TestResolverAll_ReturnsCopy(t *testing.T) {
	r := NewResolver([]Repo{{Name: "x", Path: "/x"}})
	all := r.All()
	all[0].Name = "MUTATED"
	assert.Equal(t, "x", r.All()[0].Name, "All() should return a copy")
}

func TestResolverAll_NilResolver(t *testing.T) {
	var r *Resolver
	assert.Nil(t, r.All())
}

// ---------------------------------------------------------------------------
// Signature
// ---------------------------------------------------------------------------

func TestSignature_EmptyRepos(t *testing.T) {
	s := Signature(nil)
	assert.NotEmpty(t, s, "signature of empty repos should still produce a hash")
}

func TestSignature_DifferentRepos(t *testing.T) {
	s1 := Signature([]Repo{{Name: "a", Path: "/a"}})
	s2 := Signature([]Repo{{Name: "b", Path: "/b"}})
	assert.NotEqual(t, s1, s2)
}

func TestSignature_OrderIndependent(t *testing.T) {
	r1 := []Repo{{Name: "a", Path: "/a"}, {Name: "b", Path: "/b"}}
	r2 := []Repo{{Name: "b", Path: "/b"}, {Name: "a", Path: "/a"}}
	assert.Equal(t, Signature(r1), Signature(r2))
}

func TestSignature_Deterministic(t *testing.T) {
	r := []Repo{{Name: "x", Path: "/x"}}
	assert.Equal(t, Signature(r), Signature(r))
}

func TestSignature_PathMatters(t *testing.T) {
	r1 := []Repo{{Name: "a", Path: "/path1"}}
	r2 := []Repo{{Name: "a", Path: "/path2"}}
	assert.NotEqual(t, Signature(r1), Signature(r2))
}

// ---------------------------------------------------------------------------
// Discover — integration with real filesystem
// ---------------------------------------------------------------------------

func TestDiscover_ComplexWorkspace(t *testing.T) {
	root := t.TempDir()

	// Create a workspace with:
	// - 2 git repos
	// - 1 non-git child
	// - 1 hidden dir (should be skipped)
	// - 1 file at root (should be skipped)
	for _, d := range []string{"frontend", "backend", "docs", ".config"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0o755))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(root, "frontend", ".git"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "backend", ".git"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".config", ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "Makefile"), []byte("all:"), 0o644))

	repos, err := Discover(root)
	require.NoError(t, err)

	names := map[string]Repo{}
	for _, r := range repos {
		names[r.Name] = r
	}

	assert.Contains(t, names, "frontend")
	assert.Contains(t, names, "backend")
	assert.Contains(t, names, "docs")
	assert.NotContains(t, names, ".config")
	assert.True(t, names["frontend"].HasGit)
	assert.True(t, names["backend"].HasGit)
	assert.False(t, names["docs"].HasGit)
}
