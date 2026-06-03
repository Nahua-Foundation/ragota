package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"ragota/internal/store"
)

// TestMax0_Zero tests max0 with zero input.
func TestMax0_Zero(t *testing.T) {
	assert.Equal(t, 0, max0(0))
}

// TestMax0_Positive tests max0 with positive input.
func TestMax0_Positive(t *testing.T) {
	assert.Equal(t, 5, max0(5))
	assert.Equal(t, 100, max0(100))
}

// TestMax0_Negative tests max0 with negative input.
func TestMax0_Negative(t *testing.T) {
	assert.Equal(t, 0, max0(-1))
	assert.Equal(t, 0, max0(-100))
}

// TestNameColumn_EmptyName tests nameColumn with empty name.
func TestNameColumn_EmptyName(t *testing.T) {
	assert.Equal(t, 0, nameColumn("func foo()", ""))
}

// TestNameColumn_EmptySignature tests nameColumn with empty signature.
func TestNameColumn_EmptySignature(t *testing.T) {
	assert.Equal(t, 0, nameColumn("", "foo"))
}

// TestNameColumn_NameInSignature tests nameColumn when name is found.
func TestNameColumn_NameInSignature(t *testing.T) {
	assert.Equal(t, 5, nameColumn("func foo()", "foo"))
	assert.Equal(t, 0, nameColumn("foo()", "foo"))
	assert.Equal(t, 10, nameColumn("def class MyClass:", "MyClass"))
}

// TestNameColumn_NameNotInSignature tests nameColumn when name is not found.
func TestNameColumn_NameNotInSignature(t *testing.T) {
	assert.Equal(t, 0, nameColumn("func bar()", "foo"))
}

// TestNameColumn_NameLongerThanSignature tests edge case where name is longer.
func TestNameColumn_NameLongerThanSignature(t *testing.T) {
	assert.Equal(t, 0, nameColumn("ab", "abc"))
}

// TestNameColumn_MultipleOccurrences tests that first occurrence is returned.
func TestNameColumn_MultipleOccurrences(t *testing.T) {
	// "foo" appears at positions 0 and 8
	assert.Equal(t, 0, nameColumn("foo bar foo", "foo"))
}

// TestIsFuncKind_Function tests isFuncKind with function kind.
func TestIsFuncKind_Function(t *testing.T) {
	assert.True(t, isFuncKind("function"))
}

// TestIsFuncKind_Method tests isFuncKind with method kind.
func TestIsFuncKind_Method(t *testing.T) {
	assert.True(t, isFuncKind("method"))
}

// TestIsFuncKind_Constructor tests isFuncKind with constructor kind.
func TestIsFuncKind_Constructor(t *testing.T) {
	assert.True(t, isFuncKind("constructor"))
}

// TestIsFuncKind_OtherKinds tests isFuncKind with non-function kinds.
func TestIsFuncKind_OtherKinds(t *testing.T) {
	assert.False(t, isFuncKind("class"))
	assert.False(t, isFuncKind("interface"))
	assert.False(t, isFuncKind("struct"))
	assert.False(t, isFuncKind("module"))
	assert.False(t, isFuncKind("variable"))
	assert.False(t, isFuncKind(""))
}

// TestMergeUnits_EmptySlices tests mergeUnits with empty inputs.
func TestMergeUnits_EmptySlices(t *testing.T) {
	result := mergeUnits(nil, nil)
	assert.Empty(t, result)

	result = mergeUnits([]store.ASTUnit{}, []store.ASTUnit{})
	assert.Empty(t, result)
}

// TestMergeUnits_FirstEmpty tests mergeUnits when first slice is empty.
func TestMergeUnits_FirstEmpty(t *testing.T) {
	b := []store.ASTUnit{{ID: 1, Name: "a"}, {ID: 2, Name: "b"}}
	result := mergeUnits(nil, b)
	assert.Len(t, result, 2)
	assert.Equal(t, "a", result[0].Name)
	assert.Equal(t, "b", result[1].Name)
}

// TestMergeUnits_SecondEmpty tests mergeUnits when second slice is empty.
func TestMergeUnits_SecondEmpty(t *testing.T) {
	a := []store.ASTUnit{{ID: 1, Name: "a"}, {ID: 2, Name: "b"}}
	result := mergeUnits(a, nil)
	assert.Len(t, result, 2)
}

// TestMergeUnits_NoDuplicates tests mergeUnits with no duplicates.
func TestMergeUnits_NoDuplicates(t *testing.T) {
	a := []store.ASTUnit{{ID: 1, Name: "a"}, {ID: 2, Name: "b"}}
	b := []store.ASTUnit{{ID: 3, Name: "c"}, {ID: 4, Name: "d"}}
	result := mergeUnits(a, b)
	assert.Len(t, result, 4)
}

// TestMergeUnits_WithDuplicates tests mergeUnits with duplicates.
func TestMergeUnits_WithDuplicates(t *testing.T) {
	a := []store.ASTUnit{{ID: 1, Name: "a"}, {ID: 2, Name: "b"}}
	b := []store.ASTUnit{{ID: 2, Name: "b"}, {ID: 3, Name: "c"}}
	result := mergeUnits(a, b)
	assert.Len(t, result, 3)
	// ID=2 должен быть только один раз
	ids := make(map[int]int)
	for _, u := range result {
		ids[u.ID]++
	}
	assert.Equal(t, 1, ids[2])
}

// TestMergeUnits_DuplicatesWithinFirstSlice tests duplicates in first slice.
func TestMergeUnits_DuplicatesWithinFirstSlice(t *testing.T) {
	a := []store.ASTUnit{{ID: 1, Name: "a"}, {ID: 1, Name: "a"}}
	result := mergeUnits(a, nil)
	assert.Len(t, result, 1)
}

// TestMergeUnits_AllDuplicates tests mergeUnits when all are duplicates.
func TestMergeUnits_AllDuplicates(t *testing.T) {
	a := []store.ASTUnit{{ID: 1, Name: "a"}}
	b := []store.ASTUnit{{ID: 1, Name: "a"}}
	result := mergeUnits(a, b)
	assert.Len(t, result, 1)
}

// TestUriToPath_ValidFileURI tests uriToPath with valid file:// URI.
func TestUriToPath_ValidFileURI(t *testing.T) {
	assert.Equal(t, "/home/user/file.go", uriToPath("file:///home/user/file.go"))
}

// TestUriToPath_EmptyURI tests uriToPath with empty string.
func TestUriToPath_EmptyURI(t *testing.T) {
	assert.Equal(t, "", uriToPath(""))
}

// TestUriToPath_NoFilePrefix tests uriToPath without file:// prefix.
func TestUriToPath_NoFilePrefix(t *testing.T) {
	assert.Equal(t, "/home/user/file.go", uriToPath("/home/user/file.go"))
	assert.Equal(t, "http://example.com", uriToPath("http://example.com"))
}

// TestUriToPath_ShortString tests uriToPath with string shorter than "file://".
func TestUriToPath_ShortString(t *testing.T) {
	assert.Equal(t, "file:", uriToPath("file:"))
	assert.Equal(t, "fi", uriToPath("fi"))
}

// TestUriToPath_ExactFilePrefix tests uriToPath with exact "file://" string.
func TestUriToPath_ExactFilePrefix(t *testing.T) {
	// len("file://") == 7, uri[:7] == "file://", len(uri) > 7 is false
	assert.Equal(t, "file://", uriToPath("file://"))
}
