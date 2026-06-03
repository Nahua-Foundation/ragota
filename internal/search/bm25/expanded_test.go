package bm25

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ==================== Open edge cases ====================

func TestOpen_ValidPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subdir", "bm25.bleve")
	idx, err := Open(path, 1.2, 0.75)
	require.NoError(t, err)
	defer idx.Close()
	assert.NotNil(t, idx)
}

func TestOpen_ReopenExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bm25.bleve")
	idx, err := Open(path, 1.2, 0.75)
	require.NoError(t, err)
	require.NoError(t, idx.IndexDocs(context.Background(), []Doc{
		{ID: "test", Content: "hello world", Path: "test.go"},
	}))
	require.NoError(t, idx.Close())

	// Reopen
	idx2, err := Open(path, 1.2, 0.75)
	require.NoError(t, err)
	defer idx2.Close()

	cnt, err := idx2.Count(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint64(1), cnt)
}

// ==================== IndexDocs edge cases ====================

func TestIndexDocs_AllEmptyIDs(t *testing.T) {
	idx := openTestIndex(t)
	err := idx.IndexDocs(context.Background(), []Doc{
		{ID: "", Content: "skip1"},
		{ID: "", Content: "skip2"},
	})
	assert.NoError(t, err)
	cnt, _ := idx.Count(context.Background())
	assert.Equal(t, uint64(0), cnt)
}

func TestIndexDocs_MixedValidAndEmpty(t *testing.T) {
	idx := openTestIndex(t)
	err := idx.IndexDocs(context.Background(), []Doc{
		{ID: "valid", Content: "hello"},
		{ID: "", Content: "skip"},
		{ID: "also-valid", Content: "world"},
	})
	assert.NoError(t, err)
	cnt, _ := idx.Count(context.Background())
	assert.Equal(t, uint64(2), cnt)
}

func TestIndexDocs_EmptySlice(t *testing.T) {
	idx := openTestIndex(t)
	err := idx.IndexDocs(context.Background(), []Doc{})
	assert.NoError(t, err)
}

func TestIndexDocs_DuplicateIDs(t *testing.T) {
	idx := openTestIndex(t)
	err := idx.IndexDocs(context.Background(), []Doc{
		{ID: "dup", Content: "version1"},
		{ID: "dup", Content: "version2"},
	})
	assert.NoError(t, err)
	cnt, _ := idx.Count(context.Background())
	// Bleve upserts: last one wins
	assert.Equal(t, uint64(1), cnt)
}

// ==================== Search edge cases ====================

func TestSearch_DefaultLimit(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()
	_ = idx.IndexDocs(ctx, seedDocs())

	// Limit <= 0 should default to 20
	hits, err := idx.Search(ctx, Query{Text: "http", Limit: 0})
	require.NoError(t, err)
	assert.NotEmpty(t, hits)
}

func TestSearch_NegativeLimit(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()
	_ = idx.IndexDocs(ctx, seedDocs())

	hits, err := idx.Search(ctx, Query{Text: "http", Limit: -5})
	require.NoError(t, err)
	assert.NotEmpty(t, hits)
}

func TestSearch_RepoFilter(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()

	docs := []Doc{
		{ID: "r1", Repo: "repo-a", Content: "hello world from repo a", Path: "a.go"},
		{ID: "r2", Repo: "repo-b", Content: "hello world from repo b", Path: "b.go"},
		{ID: "r3", Repo: "repo-c", Content: "hello world from repo c", Path: "c.go"},
	}
	require.NoError(t, idx.IndexDocs(ctx, docs))

	hits, err := idx.Search(ctx, Query{Text: "hello", Repos: []string{"repo-a"}, Limit: 10})
	require.NoError(t, err)
	for _, h := range hits {
		assert.Equal(t, "repo-a", h.Repo)
	}
}

func TestSearch_RepoFilterMultiple(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()

	docs := []Doc{
		{ID: "r1", Repo: "repo-a", Content: "hello world from repo a", Path: "a.go"},
		{ID: "r2", Repo: "repo-b", Content: "hello world from repo b", Path: "b.go"},
		{ID: "r3", Repo: "repo-c", Content: "hello world from repo c", Path: "c.go"},
	}
	require.NoError(t, idx.IndexDocs(ctx, docs))

	hits, err := idx.Search(ctx, Query{Text: "hello", Repos: []string{"repo-a", "repo-b"}, Limit: 10})
	require.NoError(t, err)
	for _, h := range hits {
		assert.Contains(t, []string{"repo-a", "repo-b"}, h.Repo)
	}
}

func TestSearch_RepoFilterWildcard(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()

	docs := []Doc{
		{ID: "r1", Repo: "repo-a", Content: "hello world", Path: "a.go"},
		{ID: "r2", Repo: "repo-b", Content: "hello world", Path: "b.go"},
	}
	require.NoError(t, idx.IndexDocs(ctx, docs))

	// "*" should match all repos
	hits, err := idx.Search(ctx, Query{Text: "hello", Repos: []string{"*"}, Limit: 10})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(hits), 2)
}

func TestSearch_RepoFilterEmptyString(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()

	docs := []Doc{
		{ID: "r1", Repo: "repo-a", Content: "hello world", Path: "a.go"},
	}
	require.NoError(t, idx.IndexDocs(ctx, docs))

	// "" among repos should act as wildcard
	hits, err := idx.Search(ctx, Query{Text: "hello", Repos: []string{""}, Limit: 10})
	require.NoError(t, err)
	assert.NotEmpty(t, hits)
}

func TestSearch_NoMatches(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()
	_ = idx.IndexDocs(ctx, seedDocs())

	hits, err := idx.Search(ctx, Query{Text: "xyznonexistent", Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, hits)
}

func TestSearch_SymbolBoost(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()

	docs := []Doc{
		{ID: "sym", Symbol: "ParseRequest", Content: "this function handles parsing", Path: "a.go"},
		{ID: "nosym", Symbol: "", Content: "ParseRequest is mentioned here in text", Path: "b.go"},
	}
	require.NoError(t, idx.IndexDocs(ctx, docs))

	hits, err := idx.Search(ctx, Query{Text: "ParseRequest", Limit: 10})
	require.NoError(t, err)
	if len(hits) >= 2 {
		// Symbol match should rank higher (boost=2.0)
		assert.Equal(t, "sym", hits[0].ID)
	}
}

func TestSearch_SnippetFromHighlight(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()

	docs := []Doc{
		{ID: "snip", Content: "the quick brown fox jumps over the lazy dog", Path: "test.go"},
	}
	require.NoError(t, idx.IndexDocs(ctx, docs))

	hits, err := idx.Search(ctx, Query{Text: "fox", Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	// Snippet should be populated from highlight or content
	assert.NotEmpty(t, hits[0].Snippet)
}

func TestSearch_LongContentSnippet(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()

	longContent := ""
	for i := 0; i < 300; i++ {
		longContent += "word "
	}
	longContent += "TARGET_TERM "
	for i := 0; i < 100; i++ {
		longContent += "more "
	}

	docs := []Doc{
		{ID: "long", Content: longContent, Path: "long.go"},
	}
	require.NoError(t, idx.IndexDocs(ctx, docs))

	hits, err := idx.Search(ctx, Query{Text: "TARGET_TERM", Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	assert.NotEmpty(t, hits[0].Snippet)
}

// ==================== Delete edge cases ====================

func TestDelete_EmptyIDs(t *testing.T) {
	idx := openTestIndex(t)
	err := idx.Delete(context.Background(), nil)
	assert.NoError(t, err)
}

func TestDelete_NonexistentID(t *testing.T) {
	idx := openTestIndex(t)
	_ = idx.IndexDocs(context.Background(), seedDocs())
	err := idx.Delete(context.Background(), []string{"nonexistent"})
	assert.NoError(t, err)
	cnt, _ := idx.Count(context.Background())
	assert.Equal(t, uint64(4), cnt) // no change
}

// ==================== DeleteByPath edge cases ====================

func TestDeleteByPath_NoMatches(t *testing.T) {
	idx := openTestIndex(t)
	_ = idx.IndexDocs(context.Background(), seedDocs())
	err := idx.DeleteByPath(context.Background(), "nonexistent/path.go")
	assert.NoError(t, err)
	cnt, _ := idx.Count(context.Background())
	assert.Equal(t, uint64(4), cnt) // no change
}

// ==================== Clear ====================

func TestClear(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()
	_ = idx.IndexDocs(ctx, seedDocs())

	cnt, _ := idx.Count(ctx)
	assert.Equal(t, uint64(4), cnt)

	err := idx.Clear(ctx)
	require.NoError(t, err)

	cnt, _ = idx.Count(ctx)
	assert.Equal(t, uint64(0), cnt)
}

func TestClear_EmptyIndex(t *testing.T) {
	idx := openTestIndex(t)
	err := idx.Clear(context.Background())
	assert.NoError(t, err)
}

// ==================== Close edge cases ====================

func TestClose_NilIndex(t *testing.T) {
	var idx *bleveIndex
	err := idx.Close()
	assert.NoError(t, err)
}

func TestDelete_AfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bm25.bleve")
	idx, err := Open(path, 1.2, 0.75)
	require.NoError(t, err)
	require.NoError(t, idx.Close())

	err = idx.Delete(context.Background(), []string{"a"})
	assert.ErrorIs(t, err, ErrClosed)
}

func TestDeleteByPath_AfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bm25.bleve")
	idx, err := Open(path, 1.2, 0.75)
	require.NoError(t, err)
	require.NoError(t, idx.Close())

	err = idx.DeleteByPath(context.Background(), "test.go")
	assert.ErrorIs(t, err, ErrClosed)
}

func TestClear_AfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bm25.bleve")
	idx, err := Open(path, 1.2, 0.75)
	require.NoError(t, err)
	require.NoError(t, idx.Close())

	err = idx.Clear(context.Background())
	assert.ErrorIs(t, err, ErrClosed)
}

// ==================== Doc fields preserved ====================

func TestSearch_AllFieldsPreserved(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()

	docs := []Doc{
		{
			ID:        "full",
			Repo:      "myrepo",
			Path:      "internal/api/handler.go",
			Language:  "go",
			Kind:      "function",
			Symbol:    "HandleRequest",
			Content:   "func HandleRequest processes HTTP requests",
			AstUnitID: 42,
			StartLine: 10,
			EndLine:   25,
		},
	}
	require.NoError(t, idx.IndexDocs(ctx, docs))

	hits, err := idx.Search(ctx, Query{Text: "HandleRequest", Limit: 10})
	require.NoError(t, err)
	require.Len(t, hits, 1)

	h := hits[0]
	assert.Equal(t, "full", h.ID)
	assert.Equal(t, "myrepo", h.Repo)
	assert.Equal(t, "internal/api/handler.go", h.Path)
	assert.Equal(t, "go", h.Language)
	assert.Equal(t, "function", h.Kind)
	assert.Equal(t, "HandleRequest", h.Symbol)
	assert.Equal(t, int64(42), h.AstUnitID)
	assert.Equal(t, 10, h.StartLine)
	assert.Equal(t, 25, h.EndLine)
	assert.NotEmpty(t, h.Snippet)
	assert.Greater(t, h.Score, 0.0)
}

// ==================== Concurrent access ====================

func TestConcurrentSearchAndIndex(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()

	// Seed some data
	require.NoError(t, idx.IndexDocs(ctx, seedDocs()))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10; i++ {
			_, _ = idx.Search(ctx, Query{Text: "http", Limit: 5})
		}
	}()

	for i := 0; i < 10; i++ {
		_ = idx.IndexDocs(ctx, []Doc{{ID: "concurrent", Content: "concurrent content", Path: "c.go"}})
	}

	<-done
}
