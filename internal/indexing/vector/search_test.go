package vector

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ragota/internal/indexing/chunker"
)

func TestCombinedText_NoComments(t *testing.T) {
	ch := chunker.Chunk{Text: "func hello() {}", Comments: ""}
	assert.Equal(t, "func hello() {}", combinedText(ch))
}

func TestCombinedText_WithComments(t *testing.T) {
	ch := chunker.Chunk{Text: "func hello() {}", Comments: "// greets the world"}
	assert.Equal(t, "// greets the world\nfunc hello() {}", combinedText(ch))
}

func TestCombinedText_EmptyText(t *testing.T) {
	ch := chunker.Chunk{Text: "", Comments: ""}
	assert.Equal(t, "", combinedText(ch))
}

func TestCombinedText_EmptyTextWithComments(t *testing.T) {
	ch := chunker.Chunk{Text: "", Comments: "// only comment"}
	assert.Equal(t, "// only comment\n", combinedText(ch))
}

func TestCombinedText_MultilineComments(t *testing.T) {
	ch := chunker.Chunk{
		Text:     "body",
		Comments: "// line1\n// line2\n// line3",
	}
	expected := "// line1\n// line2\n// line3\nbody"
	assert.Equal(t, expected, combinedText(ch))
}

func TestChunkID_Deterministic(t *testing.T) {
	id1 := chunkID("/foo/bar.go", 0)
	id2 := chunkID("/foo/bar.go", 0)
	assert.Equal(t, id1, id2)
}

func TestChunkID_DifferentFiles(t *testing.T) {
	id1 := chunkID("/foo/a.go", 0)
	id2 := chunkID("/foo/b.go", 0)
	assert.NotEqual(t, id1, id2)
}

func TestChunkID_DifferentIndices(t *testing.T) {
	id1 := chunkID("/foo/a.go", 0)
	id2 := chunkID("/foo/a.go", 1)
	assert.NotEqual(t, id1, id2)
}

func TestChunkID_Format(t *testing.T) {
	id := chunkID("/foo/bar.go", 42)
	parts := strings.Split(id, "-")
	require.Len(t, parts, 5)
	assert.Len(t, parts[0], 8)
	assert.Len(t, parts[1], 4)
	assert.Len(t, parts[2], 4)
	assert.Len(t, parts[3], 4)
	assert.Len(t, parts[4], 12)
}

func TestChunkID_MatchesSHA1(t *testing.T) {
	file := "/test/file.py"
	idx := 5
	h := sha1.Sum([]byte(fmt.Sprintf("%s#%d", file, idx)))
	hexStr := hex.EncodeToString(h[:16])
	expected := fmt.Sprintf("%s-%s-%s-%s-%s", hexStr[0:8], hexStr[8:12], hexStr[12:16], hexStr[16:20], hexStr[20:32])
	assert.Equal(t, expected, chunkID(file, idx))
}

func TestChunkID_EmptyFile(t *testing.T) {
	id := chunkID("", 0)
	assert.NotEmpty(t, id)
	parts := strings.Split(id, "-")
	assert.Len(t, parts, 5)
}

func TestBuildFilter_Nil(t *testing.T) {
	assert.Nil(t, buildFilter(nil))
}

func TestBuildFilter_Empty(t *testing.T) {
	assert.Nil(t, buildFilter(map[string]any{}))
}

func TestBuildFilter_SingleKey(t *testing.T) {
	f := buildFilter(map[string]any{"language": "go"})
	require.NotNil(t, f)
	must, ok := f["must"].([]map[string]any)
	require.True(t, ok)
	assert.Len(t, must, 1)
	assert.Equal(t, "language", must[0]["key"])
}

func TestBuildFilter_RepoWildcard(t *testing.T) {
	f := buildFilter(map[string]any{"repo": "*"})
	assert.Nil(t, f)
}

func TestBuildFilter_RepoEmpty(t *testing.T) {
	f := buildFilter(map[string]any{"repo": ""})
	assert.Nil(t, f)
}

func TestBuildFilter_RepoSingle(t *testing.T) {
	f := buildFilter(map[string]any{"repo": "myrepo"})
	require.NotNil(t, f)
	must := f["must"].([]map[string]any)
	assert.Len(t, must, 1)
	assert.Equal(t, "repo", must[0]["key"])
}

func TestBuildFilter_RepoSliceSingle(t *testing.T) {
	f := buildFilter(map[string]any{"repo": []string{"alpha"}})
	require.NotNil(t, f)
	must := f["must"].([]map[string]any)
	require.Len(t, must, 1)
	match := must[0]["match"].(map[string]any)
	assert.Equal(t, "alpha", match["value"])
}

func TestBuildFilter_RepoSliceMultiple(t *testing.T) {
	f := buildFilter(map[string]any{"repo": []string{"alpha", "beta"}})
	require.NotNil(t, f)
	must := f["must"].([]map[string]any)
	require.Len(t, must, 1)
	match := must[0]["match"].(map[string]any)
	anyVals, ok := match["any"]
	require.True(t, ok)
	assert.Contains(t, anyVals, "alpha")
	assert.Contains(t, anyVals, "beta")
}

func TestBuildFilter_RepoSliceWithWildcard(t *testing.T) {
	f := buildFilter(map[string]any{"repo": []string{"alpha", "*"}})
	require.NotNil(t, f)
	must := f["must"].([]map[string]any)
	require.Len(t, must, 1)
	match := must[0]["match"].(map[string]any)
	assert.Equal(t, "alpha", match["value"])
}

func TestBuildFilter_RepoSliceAllWildcards(t *testing.T) {
	f := buildFilter(map[string]any{"repo": []string{"*", ""}})
	assert.Nil(t, f)
}

func TestBuildFilter_MultipleKeys(t *testing.T) {
	f := buildFilter(map[string]any{"language": "go", "kind": "function"})
	require.NotNil(t, f)
	must := f["must"].([]map[string]any)
	assert.Len(t, must, 2)
}

func TestBuildFilter_UnknownType(t *testing.T) {
	f := buildFilter(map[string]any{"repo": 42})
	assert.Nil(t, f)
}

func TestBuildFilter_MixedRepoAndOther(t *testing.T) {
	f := buildFilter(map[string]any{"repo": "myrepo", "language": "go"})
	require.NotNil(t, f)
	must := f["must"].([]map[string]any)
	assert.Len(t, must, 2)
}

func TestRepoMatchCondition_StringEmpty(t *testing.T) {
	assert.Nil(t, repoMatchCondition(""))
}

func TestRepoMatchCondition_StringWildcard(t *testing.T) {
	assert.Nil(t, repoMatchCondition("*"))
}

func TestRepoMatchCondition_StringName(t *testing.T) {
	cond := repoMatchCondition("myrepo")
	require.NotNil(t, cond)
	assert.Equal(t, "repo", cond["key"])
	match := cond["match"].(map[string]any)
	assert.Equal(t, "myrepo", match["value"])
}

func TestRepoMatchCondition_SliceSingle(t *testing.T) {
	cond := repoMatchCondition([]string{"alpha"})
	require.NotNil(t, cond)
	match := cond["match"].(map[string]any)
	assert.Equal(t, "alpha", match["value"])
}

func TestRepoMatchCondition_SliceMultiple(t *testing.T) {
	cond := repoMatchCondition([]string{"a", "b", "c"})
	require.NotNil(t, cond)
	match := cond["match"].(map[string]any)
	anyVals := match["any"].([]string)
	assert.Len(t, anyVals, 3)
}

func TestRepoMatchCondition_SliceAllWildcards(t *testing.T) {
	assert.Nil(t, repoMatchCondition([]string{"*", "", "*"}))
}

func TestRepoMatchCondition_UnsupportedType(t *testing.T) {
	assert.Nil(t, repoMatchCondition(42))
	assert.Nil(t, repoMatchCondition(nil))
	assert.Nil(t, repoMatchCondition(map[string]string{"a": "b"}))
}

func TestRepoMatchCondition_EmptySlice(t *testing.T) {
	assert.Nil(t, repoMatchCondition([]string{}))
}

func TestRepoMatchCondition_SliceWithMixedWildcard(t *testing.T) {
	cond := repoMatchCondition([]string{"a", "*", "b"})
	require.NotNil(t, cond)
	match := cond["match"].(map[string]any)
	anyVals := match["any"].([]string)
	assert.Len(t, anyVals, 2)
	assert.Contains(t, anyVals, "a")
	assert.Contains(t, anyVals, "b")
}
