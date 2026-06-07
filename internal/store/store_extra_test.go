package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func openMem(t *testing.T) *SQLite {
	t.Helper()
	st, err := Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func ensureFile(t *testing.T, st *SQLite, path, lang string) {
	t.Helper()
	require.NoError(t, st.EnsureFile(context.Background(), path, lang))
}

func seedUnits(t *testing.T, st *SQLite, filePath string, units []ASTUnit) map[string]int {
	t.Helper()
	ensureFile(t, st, filePath, units[0].Language)
	ids, err := st.ReplaceASTUnits(context.Background(), filePath, units)
	require.NoError(t, err)
	return ids
}

// ---------------------------------------------------------------------------
// Open / OpenFresh / Close
// ---------------------------------------------------------------------------

func TestOpen_MemoryDB(t *testing.T) {
	st := openMem(t)
	assert.NotNil(t, st.GetDBForTests())
}

func TestOpenFresh_NewDB(t *testing.T) {
	st, err := OpenFresh(":memory:", "sig-1")
	require.NoError(t, err)
	defer st.Close()
	assert.NotNil(t, st)
}

func TestClose(t *testing.T) {
	st, err := Open(":memory:")
	require.NoError(t, err)
	require.NoError(t, st.Close())
}

// ---------------------------------------------------------------------------
// File CRUD
// ---------------------------------------------------------------------------

func TestGetFile_NotFound(t *testing.T) {
	st := openMem(t)
	got, err := st.GetFile(context.Background(), "/no/such/file")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestUpsertFile_AndGetFile(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	f := FileRow{
		Path:     "/a.go",
		Language: "go",
		Hash:     "abc123",
		Size:     100,
		ModTime:  time.Now(),
	}
	syms := []SymbolRow{
		{Name: "Foo", Kind: "function", StartLine: 1, EndLine: 5},
		{Name: "Bar", Kind: "function", StartLine: 10, EndLine: 15},
	}
	require.NoError(t, st.UpsertFile(ctx, f, syms))
	got, err := st.GetFile(ctx, "/a.go")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "go", got.Language)
	assert.Equal(t, "abc123", got.Hash)
	assert.Equal(t, int64(100), got.Size)
	assert.Equal(t, 2, got.Symbols)
}

func TestUpsertFile_UpdateExisting(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	f := FileRow{Path: "/a.go", Language: "go", Hash: "v1", Size: 50, ModTime: time.Now()}
	require.NoError(t, st.UpsertFile(ctx, f, nil))
	f.Hash = "v2"
	f.Size = 200
	require.NoError(t, st.UpsertFile(ctx, f, nil))
	got, _ := st.GetFile(ctx, "/a.go")
	assert.Equal(t, "v2", got.Hash)
	assert.Equal(t, int64(200), got.Size)
}

func TestEnsureFile(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	require.NoError(t, st.EnsureFile(ctx, "/x.py", "py"))
	got, err := st.GetFile(ctx, "/x.py")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "py", got.Language)
}

func TestEnsureFile_Idempotent(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	require.NoError(t, st.EnsureFile(ctx, "/x.py", "py"))
	require.NoError(t, st.EnsureFile(ctx, "/x.py", "py"))
}

func TestDeleteFile(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ensureFile(t, st, "/del.go", "go")
	require.NoError(t, st.DeleteFile(ctx, "/del.go"))
	got, _ := st.GetFile(ctx, "/del.go")
	assert.Nil(t, got)
}

func TestDeleteFile_NonExistent(t *testing.T) {
	st := openMem(t)
	err := st.DeleteFile(context.Background(), "/no/such")
	assert.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Hash operations
// ---------------------------------------------------------------------------

func TestGetFileHash_Empty(t *testing.T) {
	st := openMem(t)
	hash, err := st.GetFileHash(context.Background(), "/no.go")
	require.NoError(t, err)
	assert.Empty(t, hash)
}

func TestUpdateFileHash(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	require.NoError(t, st.UpdateFileHash(ctx, "/new.go", "hash123"))
	hash, err := st.GetFileHash(ctx, "/new.go")
	require.NoError(t, err)
	assert.Equal(t, "hash123", hash)
}

func TestUpdateVectorHash_NewFile(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	require.NoError(t, st.UpdateVectorHash(ctx, "/vec.go", "vh1"))
	got, err := st.GetFile(ctx, "/vec.go")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "vh1", got.VecHash)
}

func TestUpdateVectorHash_ExistingFile(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ensureFile(t, st, "/vec.go", "go")
	require.NoError(t, st.UpdateVectorHash(ctx, "/vec.go", "vh2"))
	got, _ := st.GetFile(ctx, "/vec.go")
	assert.Equal(t, "vh2", got.VecHash)
}

func TestHasFileHashes_EmptyDB(t *testing.T) {
	st := openMem(t)
	has, err := st.HasFileHashes(context.Background())
	require.NoError(t, err)
	assert.False(t, has)
}

func TestHasFileHashes_WithHash(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	require.NoError(t, st.UpdateFileHash(ctx, "/a.go", "h"))
	has, err := st.HasFileHashes(ctx)
	require.NoError(t, err)
	assert.True(t, has)
}

func TestHasVecHashes_EmptyDB(t *testing.T) {
	st := openMem(t)
	has, err := st.HasVecHashes(context.Background())
	require.NoError(t, err)
	assert.False(t, has)
}

func TestHasVecHashes_WithHash(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	require.NoError(t, st.UpdateVectorHash(ctx, "/a.go", "vh"))
	has, err := st.HasVecHashes(ctx)
	require.NoError(t, err)
	assert.True(t, has)
}

func TestResetFileHashes(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	require.NoError(t, st.UpdateFileHash(ctx, "/a.go", "h1"))
	require.NoError(t, st.UpdateFileHash(ctx, "/b.go", "h2"))
	require.NoError(t, st.ResetFileHashes(ctx))
	has, _ := st.HasFileHashes(ctx)
	assert.False(t, has)
}

func TestResetVecHashes(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	require.NoError(t, st.UpdateVectorHash(ctx, "/a.go", "vh1"))
	require.NoError(t, st.UpdateVectorHash(ctx, "/b.go", "vh2"))
	require.NoError(t, st.ResetVecHashes(ctx))
	has, _ := st.HasVecHashes(ctx)
	assert.False(t, has)
}

// ---------------------------------------------------------------------------
// SearchSymbols
// ---------------------------------------------------------------------------

func TestSearchSymbols_EmptyDB(t *testing.T) {
	st := openMem(t)
	got, err := st.SearchSymbols(context.Background(), "foo", "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestSearchSymbols_Basic(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	f := FileRow{Path: "/s.go", Language: "go", ModTime: time.Now()}
	require.NoError(t, st.UpsertFile(ctx, f, []SymbolRow{
		{Name: "Foo", Kind: "function", StartLine: 1, EndLine: 5},
		{Name: "FooBar", Kind: "function", StartLine: 10, EndLine: 15},
		{Name: "Baz", Kind: "struct", StartLine: 20, EndLine: 25},
	}))
	hits, err := st.SearchSymbols(ctx, "Foo", "", "", 10)
	require.NoError(t, err)
	assert.Len(t, hits, 2)
}

func TestSearchSymbols_WithKindFilter(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	f := FileRow{Path: "/s.go", Language: "go", ModTime: time.Now()}
	require.NoError(t, st.UpsertFile(ctx, f, []SymbolRow{
		{Name: "Foo", Kind: "function", StartLine: 1, EndLine: 5},
		{Name: "Foo", Kind: "struct", StartLine: 10, EndLine: 15},
	}))
	hits, err := st.SearchSymbols(ctx, "Foo", "function", "", 10)
	require.NoError(t, err)
	assert.Len(t, hits, 1)
	assert.Equal(t, "function", hits[0].Kind)
}

func TestSearchSymbols_WithLanguageFilter(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	f1 := FileRow{Path: "/a.go", Language: "go", ModTime: time.Now()}
	f2 := FileRow{Path: "/b.py", Language: "py", ModTime: time.Now()}
	require.NoError(t, st.UpsertFile(ctx, f1, []SymbolRow{{Name: "Save", Kind: "function"}}))
	require.NoError(t, st.UpsertFile(ctx, f2, []SymbolRow{{Name: "Save", Kind: "function"}}))
	hits, err := st.SearchSymbols(ctx, "Save", "", "go", 10)
	require.NoError(t, err)
	assert.Len(t, hits, 1)
}

func TestSearchSymbols_DefaultLimit(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	f := FileRow{Path: "/s.go", Language: "go", ModTime: time.Now()}
	syms := make([]SymbolRow, 100)
	for i := range syms {
		syms[i] = SymbolRow{Name: "Match", Kind: "function"}
	}
	require.NoError(t, st.UpsertFile(ctx, f, syms))
	hits, err := st.SearchSymbols(ctx, "Match", "", "", 0)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(hits), 50)
}

// ---------------------------------------------------------------------------
// SymbolsByFile
// ---------------------------------------------------------------------------

func TestSymbolsByFile_Empty(t *testing.T) {
	st := openMem(t)
	got, err := st.SymbolsByFile(context.Background(), "/no.go")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestSymbolsByFile_Ordered(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	f := FileRow{Path: "/o.go", Language: "go", ModTime: time.Now()}
	require.NoError(t, st.UpsertFile(ctx, f, []SymbolRow{
		{Name: "B", Kind: "function", StartByte: 50},
		{Name: "A", Kind: "function", StartByte: 0},
	}))
	syms, err := st.SymbolsByFile(ctx, "/o.go")
	require.NoError(t, err)
	require.Len(t, syms, 2)
	assert.Equal(t, "A", syms[0].Name)
	assert.Equal(t, "B", syms[1].Name)
}

// ---------------------------------------------------------------------------
// Stats / GraphStats
// ---------------------------------------------------------------------------

func TestStats_Empty(t *testing.T) {
	st := openMem(t)
	s, err := st.Stats(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, s.Files)
	assert.Equal(t, 0, s.Symbols)
}

func TestStats_AfterUpsert(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	require.NoError(t, st.UpsertFile(ctx, FileRow{Path: "/a.go", Language: "go", ModTime: time.Now()},
		[]SymbolRow{{Name: "X", Kind: "function"}}))
	s, err := st.Stats(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, s.Files)
	assert.Equal(t, 1, s.Symbols)
}

func TestGraphStats_Empty(t *testing.T) {
	st := openMem(t)
	s, err := st.GraphStats(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, s.Units)
	assert.Equal(t, 0, s.Edges)
}

func TestGraphStats_AfterSeed(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	seedUnits(t, st, "/g.go", []ASTUnit{
		{FilePath: "/g.go", Language: "go", Kind: "function", Name: "A", StartLine: 1, EndLine: 2},
		{FilePath: "/g.go", Language: "go", Kind: "function", Name: "B", StartLine: 3, EndLine: 4},
	})
	s, err := st.GraphStats(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, s.Units)
	assert.Equal(t, 0, s.Edges)
}

// ---------------------------------------------------------------------------
// ReplaceASTUnits
// ---------------------------------------------------------------------------

func TestReplaceASTUnits_EmptySlice(t *testing.T) {
	st := openMem(t)
	ids, err := st.ReplaceASTUnits(context.Background(), "/empty.go", nil)
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestReplaceASTUnits_ReturnsNameMap(t *testing.T) {
	st := openMem(t)
	ids := seedUnits(t, st, "/m.go", []ASTUnit{
		{FilePath: "/m.go", Language: "go", Kind: "function", Name: "Foo", Qualified: "pkg.Foo"},
		{FilePath: "/m.go", Language: "go", Kind: "function", Name: "Bar"},
	})
	assert.Contains(t, ids, "pkg.Foo")
	assert.Contains(t, ids, "Bar")
	assert.NotZero(t, ids["pkg.Foo"])
	assert.NotZero(t, ids["Bar"])
}

func TestReplaceASTUnits_ReplacesOld(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ids1 := seedUnits(t, st, "/r.go", []ASTUnit{
		{FilePath: "/r.go", Language: "go", Kind: "function", Name: "Old"},
	})
	oldID := ids1["Old"]
	u, _ := st.GetASTUnit(ctx, oldID)
	require.NotNil(t, u)

	_ = seedUnits(t, st, "/r.go", []ASTUnit{
		{FilePath: "/r.go", Language: "go", Kind: "function", Name: "New"},
	})
	u2, _ := st.GetASTUnit(ctx, oldID)
	assert.Nil(t, u2, "old unit should be deleted")
}

// ---------------------------------------------------------------------------
// GetASTUnit
// ---------------------------------------------------------------------------

func TestGetASTUnit_NotFound(t *testing.T) {
	st := openMem(t)
	u, err := st.GetASTUnit(context.Background(), 99999)
	require.NoError(t, err)
	assert.Nil(t, u)
}

func TestGetASTUnit_Fields(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ids := seedUnits(t, st, "/f.go", []ASTUnit{
		{
			Repo: "myrepo", FilePath: "/f.go", Language: "go", Kind: "method",
			Name: "Save", Qualified: "pkg.Svc.Save", Signature: "func Save()",
			Doc: "Save persists data", Hash: "h1",
			StartLine: 1, EndLine: 10, StartByte: 0, EndByte: 100,
			NameStartLine: 1, NameStartCol: 5,
		},
	})
	u, err := st.GetASTUnit(ctx, ids["pkg.Svc.Save"])
	require.NoError(t, err)
	require.NotNil(t, u)
	assert.Equal(t, "myrepo", u.Repo)
	assert.Equal(t, "method", u.Kind)
	assert.Equal(t, "Save", u.Name)
	assert.Equal(t, "pkg.Svc.Save", u.Qualified)
	assert.Equal(t, "func Save()", u.Signature)
	assert.Equal(t, "Save persists data", u.Doc)
	assert.Equal(t, "h1", u.Hash)
	assert.Equal(t, 1, u.NameStartLine)
	assert.Equal(t, 5, u.NameStartCol)
}

// ---------------------------------------------------------------------------
// FindASTUnits edge cases
// ---------------------------------------------------------------------------

func TestFindASTUnits_CaseInsensitive(t *testing.T) {
	st := openMem(t)
	seedUnits(t, st, "/ci.go", []ASTUnit{
		{FilePath: "/ci.go", Language: "go", Kind: "function", Name: "MyFunc"},
	})
	hits, err := st.FindASTUnits(context.Background(), "myfunc", "", "", "", 10)
	require.NoError(t, err)
	assert.NotEmpty(t, hits)
	assert.Equal(t, "MyFunc", hits[0].Name)
}

func TestFindASTUnits_WildcardRepo(t *testing.T) {
	st := openMem(t)
	seedUnits(t, st, "/w.go", []ASTUnit{
		{Repo: "r1", FilePath: "/w.go", Language: "go", Kind: "function", Name: "X"},
	})
	hits, err := st.FindASTUnits(context.Background(), "X", "", "", "*", 10)
	require.NoError(t, err)
	assert.Len(t, hits, 1)
}

func TestFindASTUnits_RepoFilter(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ensureFile(t, st, "/r1.go", "go")
	ensureFile(t, st, "/r2.go", "go")
	_, _ = st.ReplaceASTUnits(ctx, "/r1.go", []ASTUnit{
		{Repo: "alpha", FilePath: "/r1.go", Language: "go", Kind: "function", Name: "Shared"},
	})
	_, _ = st.ReplaceASTUnits(ctx, "/r2.go", []ASTUnit{
		{Repo: "beta", FilePath: "/r2.go", Language: "go", Kind: "function", Name: "Shared"},
	})
	hits, err := st.FindASTUnits(ctx, "Shared", "", "", "alpha", 10)
	require.NoError(t, err)
	assert.Len(t, hits, 1)
	assert.Equal(t, "alpha", hits[0].Repo)
}

func TestFindASTUnits_LIKEFallback(t *testing.T) {
	st := openMem(t)
	seedUnits(t, st, "/like.go", []ASTUnit{
		{FilePath: "/like.go", Language: "go", Kind: "function", Name: "LongFunctionName"},
	})
	hits, err := st.FindASTUnits(context.Background(), "Function", "", "", "", 10)
	require.NoError(t, err)
	assert.NotEmpty(t, hits)
}

// ---------------------------------------------------------------------------
// UpdateASTParents
// ---------------------------------------------------------------------------

func TestUpdateASTParents_Nil(t *testing.T) {
	st := openMem(t)
	err := st.UpdateASTParents(context.Background(), nil)
	assert.NoError(t, err)
}

func TestUpdateASTParents_Empty(t *testing.T) {
	st := openMem(t)
	err := st.UpdateASTParents(context.Background(), map[int]int{})
	assert.NoError(t, err)
}

func TestUpdateASTParents_SelfReference(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ids := seedUnits(t, st, "/self.go", []ASTUnit{
		{FilePath: "/self.go", Language: "go", Kind: "function", Name: "X"},
	})
	require.NoError(t, st.UpdateASTParents(ctx, map[int]int{ids["X"]: ids["X"]}))
	u, _ := st.GetASTUnit(ctx, ids["X"])
	assert.False(t, u.ParentID.Valid)
}

// ---------------------------------------------------------------------------
// ChildrenOf
// ---------------------------------------------------------------------------

func TestChildrenOf_Empty(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ids := seedUnits(t, st, "/ch.go", []ASTUnit{
		{FilePath: "/ch.go", Language: "go", Kind: "function", Name: "P"},
	})
	kids, err := st.ChildrenOf(ctx, ids["P"])
	require.NoError(t, err)
	assert.Empty(t, kids)
}

func TestChildrenOf_WithChildren(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ids := seedUnits(t, st, "/ch.go", []ASTUnit{
		{FilePath: "/ch.go", Language: "go", Kind: "struct", Name: "S"},
		{FilePath: "/ch.go", Language: "go", Kind: "method", Name: "M1"},
		{FilePath: "/ch.go", Language: "go", Kind: "method", Name: "M2"},
	})
	require.NoError(t, st.UpdateASTParents(ctx, map[int]int{
		ids["M1"]: ids["S"],
		ids["M2"]: ids["S"],
	}))
	kids, err := st.ChildrenOf(ctx, ids["S"])
	require.NoError(t, err)
	assert.Len(t, kids, 2)
}

// ---------------------------------------------------------------------------
// Edges
// ---------------------------------------------------------------------------

func TestReplaceEdges_EmptyFile(t *testing.T) {
	st := openMem(t)
	err := st.ReplaceEdges(context.Background(), "/empty.go", nil)
	assert.NoError(t, err)
}

func TestEdgesFrom_NoKind(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ids := seedUnits(t, st, "/ef.go", []ASTUnit{
		{FilePath: "/ef.go", Language: "go", Kind: "function", Name: "A"},
		{FilePath: "/ef.go", Language: "go", Kind: "function", Name: "B"},
	})
	require.NoError(t, st.ReplaceEdges(ctx, "/ef.go", []Edge{
		{SrcID: ids["A"], DstID: ids["B"], Kind: "call"},
		{SrcID: ids["A"], DstID: ids["B"], Kind: "import"},
	}))
	all, err := st.EdgesFrom(ctx, ids["A"], "")
	require.NoError(t, err)
	assert.Len(t, all, 2)
}

func TestEdgesTo_NoKind(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ids := seedUnits(t, st, "/et.go", []ASTUnit{
		{FilePath: "/et.go", Language: "go", Kind: "function", Name: "A"},
		{FilePath: "/et.go", Language: "go", Kind: "function", Name: "B"},
	})
	require.NoError(t, st.ReplaceEdges(ctx, "/et.go", []Edge{
		{SrcID: ids["A"], DstID: ids["B"], Kind: "call"},
	}))
	all, err := st.EdgesTo(ctx, ids["B"], "")
	require.NoError(t, err)
	assert.Len(t, all, 1)
}

func TestEdgesByDstName_SuffixMatch(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ids := seedUnits(t, st, "/dn.go", []ASTUnit{
		{FilePath: "/dn.go", Language: "go", Kind: "function", Name: "A"},
	})
	require.NoError(t, st.ReplaceEdges(ctx, "/dn.go", []Edge{
		{SrcID: ids["A"], DstID: 0, Kind: "call", DstName: "pkg.sub.Target"},
	}))
	hits, err := st.EdgesByDstName(ctx, "Target", "")
	require.NoError(t, err)
	assert.Len(t, hits, 1)
}

func TestEdgesByDstName_ExactMatch(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ids := seedUnits(t, st, "/dne.go", []ASTUnit{
		{FilePath: "/dne.go", Language: "go", Kind: "function", Name: "A"},
	})
	require.NoError(t, st.ReplaceEdges(ctx, "/dne.go", []Edge{
		{SrcID: ids["A"], DstID: 0, Kind: "call", DstName: "Target"},
	}))
	hits, err := st.EdgesByDstName(ctx, "Target", "")
	require.NoError(t, err)
	assert.Len(t, hits, 1)
}

func TestEdgesByDstNameForLang_FallbackToBase(t *testing.T) {
	// When lang="" and repo="", should fallback to EdgesByDstName.
	st := openMem(t)
	ctx := context.Background()
	ids := seedUnits(t, st, "/fb.go", []ASTUnit{
		{FilePath: "/fb.go", Language: "go", Kind: "function", Name: "A"},
	})
	require.NoError(t, st.ReplaceEdges(ctx, "/fb.go", []Edge{
		{SrcID: ids["A"], DstID: 0, Kind: "call", DstName: "Target"},
	}))
	hits, err := st.EdgesByDstNameForLangRepo(ctx, "Target", "", "", "")
	require.NoError(t, err)
	assert.Len(t, hits, 1)
}

func TestEdgesByDstNameForLangRepo_StarRepo(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ids := seedUnits(t, st, "/sr.go", []ASTUnit{
		{Repo: "r1", FilePath: "/sr.go", Language: "go", Kind: "function", Name: "A"},
	})
	require.NoError(t, st.ReplaceEdges(ctx, "/sr.go", []Edge{
		{Repo: "r1", SrcID: ids["A"], DstID: 0, Kind: "call", DstName: "Target"},
	}))
	hits, err := st.EdgesByDstNameForLangRepo(ctx, "Target", "", "go", "*")
	require.NoError(t, err)
	assert.Len(t, hits, 1)
}

// ---------------------------------------------------------------------------
// ResolvePendingEdges advanced
// ---------------------------------------------------------------------------

func TestResolvePendingEdges_QualifiedMatch(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ids := seedUnits(t, st, "/qp.go", []ASTUnit{
		{FilePath: "/qp.go", Language: "go", Kind: "function", Name: "caller"},
		{FilePath: "/qp.go", Language: "go", Kind: "function", Name: "target", Qualified: "pkg.target"},
	})
	require.NoError(t, st.ReplaceEdges(ctx, "/qp.go", []Edge{
		{SrcID: ids["caller"], DstID: 0, Kind: "call", DstName: "pkg.target"},
	}))
	n, err := st.ResolvePendingEdges(ctx, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestResolvePendingEdges_LocalNameMatch(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ids := seedUnits(t, st, "/ln.go", []ASTUnit{
		{FilePath: "/ln.go", Language: "go", Kind: "function", Name: "caller"},
		{FilePath: "/ln.go", Language: "go", Kind: "function", Name: "helper"},
	})
	require.NoError(t, st.ReplaceEdges(ctx, "/ln.go", []Edge{
		{SrcID: ids["caller"], DstID: 0, Kind: "call", DstName: "helper"},
	}))
	n, err := st.ResolvePendingEdges(ctx, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestResolvePendingEdges_EmptyDstName(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ids := seedUnits(t, st, "/en.go", []ASTUnit{
		{FilePath: "/en.go", Language: "go", Kind: "function", Name: "caller"},
	})
	require.NoError(t, st.ReplaceEdges(ctx, "/en.go", []Edge{
		{SrcID: ids["caller"], DstID: 0, Kind: "call", DstName: ""},
	}))
	n, err := st.ResolvePendingEdges(ctx, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestResolvePendingEdges_DoesNotCrossLanguages(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ensureFile(t, st, "/go.go", "go")
	ensureFile(t, st, "/py.py", "py")
	idsGo, _ := st.ReplaceASTUnits(ctx, "/go.go", []ASTUnit{
		{FilePath: "/go.go", Language: "go", Kind: "function", Name: "caller"},
	})
	idsPy, _ := st.ReplaceASTUnits(ctx, "/py.py", []ASTUnit{
		{FilePath: "/py.py", Language: "py", Kind: "function", Name: "target"},
	})
	require.NoError(t, st.ReplaceEdges(ctx, "/go.go", []Edge{
		{SrcID: idsGo["caller"], DstID: 0, Kind: "call", DstName: "target"},
	}))
	n, err := st.ResolvePendingEdges(ctx, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "Go caller should not resolve to Python target")
	_ = idsPy
}

func TestResolvePendingEdges_ReceiverMethodResolution(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()

	// Создаём структуру и метод (как в Go/TS/Java)
	ids := seedUnits(t, st, "/receiver.go", []ASTUnit{
		{FilePath: "/receiver.go", Language: "go", Kind: "struct", Name: "NDM", Qualified: "token.NDM"},
		{FilePath: "/receiver.go", Language: "go", Kind: "method", Name: "loadEmission", Qualified: "token.NDM.loadEmission", ParentID: sql.NullInt64{Int64: 1, Valid: true}},
		{FilePath: "/receiver.go", Language: "go", Kind: "function", Name: "caller"},
	})

	// Создаём edge с receiver.method (как это делает tree-sitter/go/ast)
	require.NoError(t, st.ReplaceEdges(ctx, "/receiver.go", []Edge{
		{SrcID: ids["caller"], DstID: 0, Kind: "call", DstName: "NDM.loadEmission"},
	}))

	// Pass 5 должен разрешить этот edge
	n, err := st.ResolvePendingEdges(ctx, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "receiver.method edge должен быть разрешён")

	// Проверяем, что edge разрешён правильно (используем qualified name как ключ)
	loadEmissionID := ids["token.NDM.loadEmission"]
	edges, err := st.EdgesTo(ctx, loadEmissionID, "call")
	require.NoError(t, err)
	assert.Len(t, edges, 1)
	assert.Equal(t, "NDM.loadEmission", edges[0].DstName)
}

func TestResolvePendingEdges_ReceiverMethod_DifferentFiles(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()

	// Структура в одном файле
	ids1 := seedUnits(t, st, "/struct.go", []ASTUnit{
		{FilePath: "/struct.go", Language: "go", Kind: "struct", Name: "Service", Qualified: "pkg.Service"},
	})

	// Метод в другом файле (но с правильным parent_id)
	ids2 := seedUnits(t, st, "/method.go", []ASTUnit{
		{FilePath: "/method.go", Language: "go", Kind: "method", Name: "process", Qualified: "pkg.Service.process", ParentID: sql.NullInt64{Int64: 1, Valid: true}},
	})

	// Вызывающая функция
	ids3 := seedUnits(t, st, "/caller.go", []ASTUnit{
		{FilePath: "/caller.go", Language: "go", Kind: "function", Name: "main"},
	})

	// Edge с receiver.method
	require.NoError(t, st.ReplaceEdges(ctx, "/caller.go", []Edge{
		{SrcID: ids3["main"], DstID: 0, Kind: "call", DstName: "Service.process"},
	}))

	n, err := st.ResolvePendingEdges(ctx, nil)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, 1, "receiver.method edge должен быть разрешён")

	// Проверяем, что edge был разрешён
	processID := ids2["pkg.Service.process"]
	edges, err := st.EdgesTo(ctx, processID, "call")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(edges), 1, "должен быть хотя бы один edge к методу process")

	_ = ids1
}

// ---------------------------------------------------------------------------
// ExpandNeighbors
// ---------------------------------------------------------------------------

func TestExpandNeighbors_DepthZero(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ids := seedUnits(t, st, "/d0.go", []ASTUnit{
		{FilePath: "/d0.go", Language: "go", Kind: "function", Name: "A"},
	})
	nodes, edges, err := st.ExpandNeighbors(ctx, ids["A"], 0, nil)
	require.NoError(t, err)
	assert.Len(t, nodes, 1)
	assert.Empty(t, edges)
}

func TestExpandNeighbors_NonExistentNode(t *testing.T) {
	st := openMem(t)
	nodes, edges, err := st.ExpandNeighbors(context.Background(), 99999, 1, nil)
	require.NoError(t, err)
	assert.Empty(t, nodes)
	assert.Empty(t, edges)
}

func TestExpandNeighbors_DeduplicatesNodes(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ids := seedUnits(t, st, "/dedup.go", []ASTUnit{
		{FilePath: "/dedup.go", Language: "go", Kind: "function", Name: "A"},
		{FilePath: "/dedup.go", Language: "go", Kind: "function", Name: "B"},
	})
	require.NoError(t, st.ReplaceEdges(ctx, "/dedup.go", []Edge{
		{SrcID: ids["A"], DstID: ids["B"], Kind: "call"},
		{SrcID: ids["A"], DstID: ids["B"], Kind: "reference"},
	}))
	nodes, _, err := st.ExpandNeighbors(ctx, ids["A"], 2, nil)
	require.NoError(t, err)
	// Should be exactly 2 nodes (A and B), not 3.
	assert.Len(t, nodes, 2)
}

// ---------------------------------------------------------------------------
// TraverseGraph
// ---------------------------------------------------------------------------

func TestTraverseGraph_DirectedOnly(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ids := seedUnits(t, st, "/tg.go", []ASTUnit{
		{FilePath: "/tg.go", Language: "go", Kind: "function", Name: "A"},
		{FilePath: "/tg.go", Language: "go", Kind: "function", Name: "B"},
		{FilePath: "/tg.go", Language: "go", Kind: "function", Name: "C"},
	})
	// A→B, C→A: TraverseGraph from A should find B but NOT C.
	require.NoError(t, st.ReplaceEdges(ctx, "/tg.go", []Edge{
		{SrcID: ids["A"], DstID: ids["B"], Kind: "call"},
		{SrcID: ids["C"], DstID: ids["A"], Kind: "call"},
	}))
	nodes, _, err := st.TraverseGraph(ctx, ids["A"], 1, nil)
	require.NoError(t, err)
	nodeIDs := map[int]bool{}
	for _, n := range nodes {
		nodeIDs[n.ID] = true
	}
	assert.True(t, nodeIDs[ids["A"]])
	assert.True(t, nodeIDs[ids["B"]])
	assert.False(t, nodeIDs[ids["C"]])
}

func TestTraverseGraph_KindFilter(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ids := seedUnits(t, st, "/kf.go", []ASTUnit{
		{FilePath: "/kf.go", Language: "go", Kind: "function", Name: "A"},
		{FilePath: "/kf.go", Language: "go", Kind: "function", Name: "B"},
		{FilePath: "/kf.go", Language: "go", Kind: "function", Name: "C"},
	})
	require.NoError(t, st.ReplaceEdges(ctx, "/kf.go", []Edge{
		{SrcID: ids["A"], DstID: ids["B"], Kind: "call"},
		{SrcID: ids["A"], DstID: ids["C"], Kind: "import"},
	}))
	nodes, _, err := st.TraverseGraph(ctx, ids["A"], 1, []string{"import"})
	require.NoError(t, err)
	nodeIDs := map[int]bool{}
	for _, n := range nodes {
		nodeIDs[n.ID] = true
	}
	assert.True(t, nodeIDs[ids["A"]])
	assert.True(t, nodeIDs[ids["C"]])
	assert.False(t, nodeIDs[ids["B"]])
}

func TestTraverseGraph_DepthZero(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ids := seedUnits(t, st, "/dz.go", []ASTUnit{
		{FilePath: "/dz.go", Language: "go", Kind: "function", Name: "A"},
	})
	nodes, _, err := st.TraverseGraph(ctx, ids["A"], 0, nil)
	require.NoError(t, err)
	assert.Len(t, nodes, 1)
}

func TestTraverseGraph_CircularEdges(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ids := seedUnits(t, st, "/circ.go", []ASTUnit{
		{FilePath: "/circ.go", Language: "go", Kind: "function", Name: "A"},
		{FilePath: "/circ.go", Language: "go", Kind: "function", Name: "B"},
	})
	require.NoError(t, st.ReplaceEdges(ctx, "/circ.go", []Edge{
		{SrcID: ids["A"], DstID: ids["B"], Kind: "call"},
		{SrcID: ids["B"], DstID: ids["A"], Kind: "call"},
	}))
	// Should not hang, visited set prevents infinite loop.
	nodes, _, err := st.TraverseGraph(ctx, ids["A"], 5, nil)
	require.NoError(t, err)
	assert.Len(t, nodes, 2)
}

// ---------------------------------------------------------------------------
// EmbedMeta
// ---------------------------------------------------------------------------

func TestGetEmbedMeta_NotFound(t *testing.T) {
	st := openMem(t)
	got, err := st.GetEmbedMeta(context.Background(), "missing")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestSetEmbedMeta_Upsert(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	require.NoError(t, st.SetEmbedMeta(ctx, EmbedMeta{Collection: "c", Model: "m1", Dim: 128}))
	got, _ := st.GetEmbedMeta(ctx, "c")
	require.NotNil(t, got)
	assert.Equal(t, "m1", got.Model)
	assert.Equal(t, 128, got.Dim)

	require.NoError(t, st.SetEmbedMeta(ctx, EmbedMeta{Collection: "c", Model: "m2", Dim: 256}))
	got2, _ := st.GetEmbedMeta(ctx, "c")
	assert.Equal(t, "m2", got2.Model)
	assert.Equal(t, 256, got2.Dim)
}

func TestSetEmbedMeta_MultipleCollections(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	require.NoError(t, st.SetEmbedMeta(ctx, EmbedMeta{Collection: "code", Model: "m1", Dim: 1024}))
	require.NoError(t, st.SetEmbedMeta(ctx, EmbedMeta{Collection: "text", Model: "m2", Dim: 768}))
	c, _ := st.GetEmbedMeta(ctx, "code")
	txt, _ := st.GetEmbedMeta(ctx, "text")
	assert.Equal(t, "m1", c.Model)
	assert.Equal(t, "m2", txt.Model)
}

// ---------------------------------------------------------------------------
// ListASTUnitsByFile
// ---------------------------------------------------------------------------

func TestListASTUnitsByFile_Empty(t *testing.T) {
	st := openMem(t)
	units, err := st.ListASTUnitsByFile(context.Background(), "/no.go")
	require.NoError(t, err)
	assert.Empty(t, units)
}

func TestListASTUnitsByFile_Ordered(t *testing.T) {
	st := openMem(t)
	seedUnits(t, st, "/ord.go", []ASTUnit{
		{FilePath: "/ord.go", Language: "go", Kind: "function", Name: "B", StartByte: 100},
		{FilePath: "/ord.go", Language: "go", Kind: "function", Name: "A", StartByte: 0},
	})
	units, err := st.ListASTUnitsByFile(context.Background(), "/ord.go")
	require.NoError(t, err)
	require.Len(t, units, 2)
	assert.Equal(t, "A", units[0].Name)
	assert.Equal(t, "B", units[1].Name)
}

// ---------------------------------------------------------------------------
// NullParentID
// ---------------------------------------------------------------------------

func TestNullParentID_RoundTrip(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ids := seedUnits(t, st, "/null.go", []ASTUnit{
		{FilePath: "/null.go", Language: "go", Kind: "function", Name: "X",
			ParentID: sql.NullInt64{Valid: false}},
	})
	u, err := st.GetASTUnit(ctx, ids["X"])
	require.NoError(t, err)
	assert.False(t, u.ParentID.Valid)
}

func TestParentID_ValidRoundTrip(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ensureFile(t, st, "/p.go", "go")
	_, err := st.ReplaceASTUnits(ctx, "/p.go", []ASTUnit{
		{FilePath: "/p.go", Language: "go", Kind: "struct", Name: "S"},
		{FilePath: "/p.go", Language: "go", Kind: "method", Name: "M"},
	})
	require.NoError(t, err)
	units, _ := st.ListASTUnitsByFile(ctx, "/p.go")
	require.Len(t, units, 2)
	sID, mID := units[0].ID, units[1].ID
	require.NoError(t, st.UpdateASTParents(ctx, map[int]int{mID: sID}))
	u, _ := st.GetASTUnit(ctx, mID)
	assert.True(t, u.ParentID.Valid)
	assert.Equal(t, int64(sID), u.ParentID.Int64)
}

// ---------------------------------------------------------------------------
// UpsertFile replaces symbols atomically
// ---------------------------------------------------------------------------

func TestUpsertFile_ReplacesSymbols(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	f := FileRow{Path: "/rs.go", Language: "go", ModTime: time.Now()}
	require.NoError(t, st.UpsertFile(ctx, f, []SymbolRow{
		{Name: "Old1", Kind: "function"},
		{Name: "Old2", Kind: "function"},
	}))
	syms1, _ := st.SymbolsByFile(ctx, "/rs.go")
	assert.Len(t, syms1, 2)

	require.NoError(t, st.UpsertFile(ctx, f, []SymbolRow{
		{Name: "New1", Kind: "function"},
	}))
	syms2, _ := st.SymbolsByFile(ctx, "/rs.go")
	assert.Len(t, syms2, 1)
	assert.Equal(t, "New1", syms2[0].Name)
}

// ---------------------------------------------------------------------------
// ReplaceEdges cleans old edges for file
// ---------------------------------------------------------------------------

func TestReplaceEdges_CleansOld(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ids := seedUnits(t, st, "/ce.go", []ASTUnit{
		{FilePath: "/ce.go", Language: "go", Kind: "function", Name: "A"},
		{FilePath: "/ce.go", Language: "go", Kind: "function", Name: "B"},
	})
	require.NoError(t, st.ReplaceEdges(ctx, "/ce.go", []Edge{
		{SrcID: ids["A"], DstID: ids["B"], Kind: "call"},
		{SrcID: ids["A"], DstID: ids["B"], Kind: "import"},
	}))
	before, _ := st.EdgesFrom(ctx, ids["A"], "")
	assert.Len(t, before, 2)

	require.NoError(t, st.ReplaceEdges(ctx, "/ce.go", []Edge{
		{SrcID: ids["A"], DstID: ids["B"], Kind: "reference"},
	}))
	after, _ := st.EdgesFrom(ctx, ids["A"], "")
	assert.Len(t, after, 1)
	assert.Equal(t, "reference", after[0].Kind)
}

// ---------------------------------------------------------------------------
// ExpandNeighbors with kinds filter
// ---------------------------------------------------------------------------

func TestExpandNeighbors_KindsFilter(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ids := seedUnits(t, st, "/ek.go", []ASTUnit{
		{FilePath: "/ek.go", Language: "go", Kind: "function", Name: "A"},
		{FilePath: "/ek.go", Language: "go", Kind: "function", Name: "B"},
		{FilePath: "/ek.go", Language: "go", Kind: "function", Name: "C"},
	})
	require.NoError(t, st.ReplaceEdges(ctx, "/ek.go", []Edge{
		{SrcID: ids["A"], DstID: ids["B"], Kind: "call"},
		{SrcID: ids["A"], DstID: ids["C"], Kind: "import"},
	}))
	nodes, _, err := st.ExpandNeighbors(ctx, ids["A"], 1, []string{"call"})
	require.NoError(t, err)
	nodeIDs := map[int]bool{}
	for _, n := range nodes {
		nodeIDs[n.ID] = true
	}
	assert.True(t, nodeIDs[ids["B"]])
	assert.False(t, nodeIDs[ids["C"]])
}

// ---------------------------------------------------------------------------
// TraverseGraph deep chain
// ---------------------------------------------------------------------------

func TestTraverseGraph_DeepChain(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	ids := seedUnits(t, st, "/dc.go", []ASTUnit{
		{FilePath: "/dc.go", Language: "go", Kind: "function", Name: "A"},
		{FilePath: "/dc.go", Language: "go", Kind: "function", Name: "B"},
		{FilePath: "/dc.go", Language: "go", Kind: "function", Name: "C"},
		{FilePath: "/dc.go", Language: "go", Kind: "function", Name: "D"},
	})
	require.NoError(t, st.ReplaceEdges(ctx, "/dc.go", []Edge{
		{SrcID: ids["A"], DstID: ids["B"], Kind: "call"},
		{SrcID: ids["B"], DstID: ids["C"], Kind: "call"},
		{SrcID: ids["C"], DstID: ids["D"], Kind: "call"},
	}))
	// depth=1: A,B
	nodes1, _, _ := st.TraverseGraph(ctx, ids["A"], 1, nil)
	assert.Len(t, nodes1, 2)
	// depth=2: A,B,C
	nodes2, _, _ := st.TraverseGraph(ctx, ids["A"], 2, nil)
	assert.Len(t, nodes2, 3)
	// depth=3: A,B,C,D
	nodes3, _, _ := st.TraverseGraph(ctx, ids["A"], 3, nil)
	assert.Len(t, nodes3, 4)
}
