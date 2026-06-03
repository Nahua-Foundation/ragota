// Tests для util-функций: parseRepoParam, repoMatcher, filterUnitsByRepo,
// filterEdgesByRepo, jsonResult, errorToResult.

package mcp

import (
	"testing"

	"ragota/internal/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// parseRepoParam
// ---------------------------------------------------------------------------

func TestParseRepoParam_Empty(t *testing.T) {
	assert.Nil(t, parseRepoParam(""))
}

func TestParseRepoParam_Wildcard(t *testing.T) {
	assert.Nil(t, parseRepoParam("*"))
}

func TestParseRepoParam_WhitespaceOnly(t *testing.T) {
	assert.Nil(t, parseRepoParam("   "))
}

func TestParseRepoParam_WildcardWithSpaces(t *testing.T) {
	assert.Nil(t, parseRepoParam("  *  "))
}

func TestParseRepoParam_SingleName(t *testing.T) {
	result := parseRepoParam("myrepo")
	assert.Equal(t, "myrepo", result)
}

func TestParseRepoParam_JSONArray(t *testing.T) {
	result := parseRepoParam(`["repo1","repo2"]`)
	arr, ok := result.([]string)
	require.True(t, ok, "expected []string, got %T", result)
	assert.Equal(t, []string{"repo1", "repo2"}, arr)
}

func TestParseRepoParam_JSONArraySingleElement(t *testing.T) {
	result := parseRepoParam(`["only"]`)
	assert.Equal(t, "only", result)
}

func TestParseRepoParam_JSONArrayWithWildcard(t *testing.T) {
	result := parseRepoParam(`["repo1","*"]`)
	assert.Nil(t, result, "wildcard in array should cancel filter")
}

func TestParseRepoParam_JSONArrayEmpty(t *testing.T) {
	result := parseRepoParam(`[]`)
	assert.Nil(t, result)
}

func TestParseRepoParam_InvalidJSONArray(t *testing.T) {
	result := parseRepoParam("[not-json")
	assert.Equal(t, "[not-json", result)
}

func TestParseRepoParam_CSV(t *testing.T) {
	result := parseRepoParam("a,b,c")
	arr, ok := result.([]string)
	require.True(t, ok)
	assert.Equal(t, []string{"a", "b", "c"}, arr)
}

func TestParseRepoParam_CSVWithSpaces(t *testing.T) {
	result := parseRepoParam(" a , b , c ")
	arr, ok := result.([]string)
	require.True(t, ok)
	assert.Equal(t, []string{"a", "b", "c"}, arr)
}

func TestParseRepoParam_CSVWithEmptyParts(t *testing.T) {
	result := parseRepoParam("a,,b,")
	arr, ok := result.([]string)
	require.True(t, ok)
	assert.Equal(t, []string{"a", "b"}, arr)
}

func TestParseRepoParam_CSVSingleValue(t *testing.T) {
	result := parseRepoParam("only,")
	assert.Equal(t, "only", result)
}

// ---------------------------------------------------------------------------
// normalizeRepoList
// ---------------------------------------------------------------------------

func TestNormalizeRepoList_Empty(t *testing.T) {
	assert.Nil(t, normalizeRepoList(nil))
	assert.Nil(t, normalizeRepoList([]string{}))
}

func TestNormalizeRepoList_SingleElement(t *testing.T) {
	result := normalizeRepoList([]string{"myrepo"})
	assert.Equal(t, "myrepo", result)
}

func TestNormalizeRepoList_MultipleElements(t *testing.T) {
	result := normalizeRepoList([]string{"a", "b", "c"})
	arr, ok := result.([]string)
	require.True(t, ok)
	assert.Equal(t, []string{"a", "b", "c"}, arr)
}

func TestNormalizeRepoList_WithWildcard(t *testing.T) {
	assert.Nil(t, normalizeRepoList([]string{"a", "*"}))
	assert.Nil(t, normalizeRepoList([]string{"*"}))
}

func TestNormalizeRepoList_AllWildcards(t *testing.T) {
	assert.Nil(t, normalizeRepoList([]string{"*", "*"}))
}

// ---------------------------------------------------------------------------
// parseRepoListParam
// ---------------------------------------------------------------------------

func TestParseRepoListParam_Empty(t *testing.T) {
	assert.Nil(t, parseRepoListParam(""))
}

func TestParseRepoListParam_Wildcard(t *testing.T) {
	assert.Nil(t, parseRepoListParam("*"))
}

func TestParseRepoListParam_SingleName(t *testing.T) {
	result := parseRepoListParam("repo1")
	assert.Equal(t, []string{"repo1"}, result)
}

func TestParseRepoListParam_MultipleNames(t *testing.T) {
	result := parseRepoListParam("a,b,c")
	assert.Equal(t, []string{"a", "b", "c"}, result)
}

func TestParseRepoListParam_JSONArray(t *testing.T) {
	result := parseRepoListParam(`["x","y"]`)
	assert.Equal(t, []string{"x", "y"}, result)
}

func TestParseRepoListParam_CSVWithWildcard(t *testing.T) {
	result := parseRepoListParam("a,*")
	assert.Nil(t, result)
}

// ---------------------------------------------------------------------------
// repoMatcher
// ---------------------------------------------------------------------------

func TestRepoMatcher_Nil(t *testing.T) {
	fn, ok := repoMatcher(nil)
	assert.False(t, ok)
	assert.Nil(t, fn)
}

func TestRepoMatcher_EmptyString(t *testing.T) {
	fn, ok := repoMatcher("")
	assert.False(t, ok)
	assert.Nil(t, fn)
}

func TestRepoMatcher_WildcardString(t *testing.T) {
	fn, ok := repoMatcher("*")
	assert.False(t, ok)
	assert.Nil(t, fn)
}

func TestRepoMatcher_SingleString(t *testing.T) {
	fn, ok := repoMatcher("myrepo")
	require.True(t, ok)
	require.NotNil(t, fn)
	assert.True(t, fn("myrepo"))
	assert.False(t, fn("other"))
	assert.False(t, fn(""))
}

func TestRepoMatcher_EmptySlice(t *testing.T) {
	fn, ok := repoMatcher([]string{})
	assert.False(t, ok)
	assert.Nil(t, fn)
}

func TestRepoMatcher_StringSlice(t *testing.T) {
	fn, ok := repoMatcher([]string{"a", "b"})
	require.True(t, ok)
	require.NotNil(t, fn)
	assert.True(t, fn("a"))
	assert.True(t, fn("b"))
	assert.False(t, fn("c"))
	assert.False(t, fn(""))
}

func TestRepoMatcher_SliceWithWildcard(t *testing.T) {
	fn, ok := repoMatcher([]string{"a", "*"})
	assert.False(t, ok, "wildcard in slice should disable filter")
	assert.Nil(t, fn)
}

func TestRepoMatcher_UnknownType(t *testing.T) {
	fn, ok := repoMatcher(42)
	assert.False(t, ok)
	assert.Nil(t, fn)
}

func TestRepoMatcher_BoolType(t *testing.T) {
	fn, ok := repoMatcher(true)
	assert.False(t, ok)
	assert.Nil(t, fn)
}

// ---------------------------------------------------------------------------
// filterUnitsByRepo
// ---------------------------------------------------------------------------

func makeUnits(repos ...string) []store.ASTUnit {
	units := make([]store.ASTUnit, len(repos))
	for i, r := range repos {
		units[i] = store.ASTUnit{ID: i + 1, Repo: r, Name: "unit_" + r}
	}
	return units
}

func TestFilterUnitsByRepo_NilFilter(t *testing.T) {
	units := makeUnits("a", "b", "c")
	result := filterUnitsByRepo(units, nil)
	assert.Equal(t, units, result, "nil filter should return all units")
}

func TestFilterUnitsByRepo_WildcardFilter(t *testing.T) {
	units := makeUnits("a", "b")
	result := filterUnitsByRepo(units, "*")
	assert.Equal(t, units, result, "wildcard should return all units")
}

func TestFilterUnitsByRepo_SingleRepo(t *testing.T) {
	units := makeUnits("a", "b", "a", "c")
	result := filterUnitsByRepo(units, "a")
	require.Len(t, result, 2)
	assert.Equal(t, "a", result[0].Repo)
	assert.Equal(t, "a", result[1].Repo)
}

func TestFilterUnitsByRepo_MultipleRepos(t *testing.T) {
	units := makeUnits("a", "b", "c", "d")
	result := filterUnitsByRepo(units, []string{"a", "c"})
	require.Len(t, result, 2)
	assert.Equal(t, "a", result[0].Repo)
	assert.Equal(t, "c", result[1].Repo)
}

func TestFilterUnitsByRepo_NoMatch(t *testing.T) {
	units := makeUnits("a", "b")
	result := filterUnitsByRepo(units, "nonexistent")
	assert.Empty(t, result)
}

func TestFilterUnitsByRepo_EmptyInput(t *testing.T) {
	result := filterUnitsByRepo(nil, "a")
	assert.Empty(t, result)
	result = filterUnitsByRepo([]store.ASTUnit{}, "a")
	assert.Empty(t, result)
}

func TestFilterUnitsByRepo_EmptyRepoField(t *testing.T) {
	units := makeUnits("", "a", "")
	result := filterUnitsByRepo(units, "a")
	require.Len(t, result, 1)
	assert.Equal(t, "a", result[0].Repo)
}

// ---------------------------------------------------------------------------
// filterEdgesByRepo
// ---------------------------------------------------------------------------

func makeEdges(repos ...string) []store.Edge {
	edges := make([]store.Edge, len(repos))
	for i, r := range repos {
		edges[i] = store.Edge{ID: i + 1, Repo: r, Kind: "call"}
	}
	return edges
}

func TestFilterEdgesByRepo_NilFilter(t *testing.T) {
	edges := makeEdges("a", "b")
	result := filterEdgesByRepo(edges, nil)
	assert.Equal(t, edges, result)
}

func TestFilterEdgesByRepo_WildcardFilter(t *testing.T) {
	edges := makeEdges("a", "b")
	result := filterEdgesByRepo(edges, "*")
	assert.Equal(t, edges, result)
}

func TestFilterEdgesByRepo_SingleRepo(t *testing.T) {
	edges := makeEdges("a", "b", "a")
	result := filterEdgesByRepo(edges, "a")
	require.Len(t, result, 2)
	assert.Equal(t, "a", result[0].Repo)
	assert.Equal(t, "a", result[1].Repo)
}

func TestFilterEdgesByRepo_MultipleRepos(t *testing.T) {
	edges := makeEdges("a", "b", "c")
	result := filterEdgesByRepo(edges, []string{"b", "c"})
	require.Len(t, result, 2)
}

func TestFilterEdgesByRepo_NoMatch(t *testing.T) {
	edges := makeEdges("a", "b")
	result := filterEdgesByRepo(edges, "nonexistent")
	assert.Empty(t, result)
}

func TestFilterEdgesByRepo_EmptyInput(t *testing.T) {
	result := filterEdgesByRepo(nil, "a")
	assert.Empty(t, result)
}

// ---------------------------------------------------------------------------
// jsonResult
// ---------------------------------------------------------------------------

func TestJsonResult_Nil(t *testing.T) {
	res, err := jsonResult(nil)
	require.NoError(t, err)
	assert.Equal(t, "[]", contentText(res.Content[0]))
}

func TestJsonResult_NilSlice(t *testing.T) {
	var nilSlice []store.ASTUnit
	res, err := jsonResult(nilSlice)
	require.NoError(t, err)
	assert.Equal(t, "[]", contentText(res.Content[0]))
}

func TestJsonResult_EmptySlice(t *testing.T) {
	res, err := jsonResult([]store.ASTUnit{})
	require.NoError(t, err)
	assert.Equal(t, "[]", contentText(res.Content[0]))
}

func TestJsonResult_NonNilSlice(t *testing.T) {
	units := []store.ASTUnit{{ID: 1, Name: "foo"}}
	res, err := jsonResult(units)
	require.NoError(t, err)
	text := contentText(res.Content[0])
	assert.Contains(t, text, "foo")
}

func TestJsonResult_Struct(t *testing.T) {
	u := store.ASTUnit{ID: 42, Name: "bar", Kind: "function"}
	res, err := jsonResult(u)
	require.NoError(t, err)
	text := contentText(res.Content[0])
	assert.Contains(t, text, "bar")
	assert.Contains(t, text, "function")
}

func TestJsonResult_Map(t *testing.T) {
	m := map[string]any{"key": "value", "num": 42}
	res, err := jsonResult(m)
	require.NoError(t, err)
	text := contentText(res.Content[0])
	assert.Contains(t, text, "key")
	assert.Contains(t, text, "value")
}

// ---------------------------------------------------------------------------
// errorToResult
// ---------------------------------------------------------------------------

func TestErrorToResult_Format(t *testing.T) {
	res, err := errorToResult("tool_name", assert.AnError)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.IsError)
	text := contentText(res.Content[0])
	assert.Contains(t, text, "tool_name")
}

func TestErrorToResult_NilError(t *testing.T) {
	res, err := errorToResult("tool", nil)
	require.NoError(t, err)
	require.NotNil(t, res)
	text := contentText(res.Content[0])
	assert.Contains(t, text, "tool")
}

// ---------------------------------------------------------------------------
// parseCSVParam
// ---------------------------------------------------------------------------

func TestParseCSVParam(t *testing.T) {
	assert.Nil(t, parseCSVParam(""))
	assert.Nil(t, parseCSVParam("  "))
	assert.Equal(t, []string{"a"}, parseCSVParam("a"))
	assert.Equal(t, []string{"a", "b"}, parseCSVParam("a,b"))
	assert.Equal(t, []string{"a", "b"}, parseCSVParam(" a , b "))
}
