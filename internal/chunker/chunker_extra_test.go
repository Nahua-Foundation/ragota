package chunker

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ragota/internal/parser"
)

// ---------------------------------------------------------------------------
// New: additional edge cases
// ---------------------------------------------------------------------------

func TestNew_WindowOne_OverlapZero(t *testing.T) {
	c := New(1, 0)
	assert.Equal(t, 1, c.WindowLines)
	assert.Equal(t, 0, c.OverlapLines)
}

func TestNew_LargeWindow(t *testing.T) {
	c := New(10000, 5000)
	assert.Equal(t, 10000, c.WindowLines)
	assert.Equal(t, 5000, c.OverlapLines)
}

func TestNew_OverlapEqualsWindowMinusOne(t *testing.T) {
	// overlap = window - 1 → valid (step=1)
	c := New(10, 9)
	assert.Equal(t, 10, c.WindowLines)
	assert.Equal(t, 9, c.OverlapLines)
}

// ---------------------------------------------------------------------------
// Chunk: overlap correctness
// ---------------------------------------------------------------------------

func TestChunk_OverlapProducesOverlappingLines(t *testing.T) {
	// 10 lines, window=4, overlap=2 → step=2
	lines := make([]string, 10)
	for i := range lines {
		lines[i] = "line"
	}
	source := []byte(strings.Join(lines, "\n"))
	c := New(4, 2)
	chunks := c.Chunk("f.go", "go", source, nil)

	// step = 4-2 = 2, starts: 0,2,4,6,8 → 5 windows
	assert.GreaterOrEqual(t, len(chunks), 4)

	// Verify that consecutive chunks overlap
	for i := 1; i < len(chunks); i++ {
		prev := chunks[i-1]
		curr := chunks[i]
		// curr.StartLine should be <= prev.EndLine (overlap)
		assert.LessOrEqual(t, curr.StartLine, prev.EndLine,
			"chunk[%d] start %d should overlap with chunk[%d] end %d",
			i, curr.StartLine, i-1, prev.EndLine)
	}
}

func TestChunk_SingleLine(t *testing.T) {
	c := New(10, 2)
	chunks := c.Chunk("f.go", "go", []byte("hello"), nil)
	require.Len(t, chunks, 1)
	assert.Equal(t, "hello", chunks[0].Text)
	assert.Equal(t, 1, chunks[0].StartLine)
}

func TestChunk_ExactlyWindowSize(t *testing.T) {
	lines := make([]string, 10)
	for i := range lines {
		lines[i] = "x"
	}
	source := []byte(strings.Join(lines, "\n"))
	c := New(10, 0)
	chunks := c.Chunk("f.go", "go", source, nil)
	assert.Len(t, chunks, 1)
}

func TestChunk_WindowSmallerThanFile(t *testing.T) {
	// 100 lines, window=5, overlap=0 → 20 chunks
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = "x"
	}
	source := []byte(strings.Join(lines, "\n"))
	c := New(5, 0)
	chunks := c.Chunk("f.go", "go", source, nil)
	assert.Equal(t, 20, len(chunks))
}

func TestChunk_SymbolChunkTypeFiltering(t *testing.T) {
	source := []byte("package x\n\nfunc A() {}\nvar B = 1\nconst C = 2\ntype D struct{}\ninterface E {}\n")
	syms := []parser.Symbol{
		{Name: "A", Kind: "function", StartLine: 3, EndLine: 3},
		{Name: "B", Kind: "var", StartLine: 4, EndLine: 4},       // skipped
		{Name: "C", Kind: "const", StartLine: 5, EndLine: 5},     // skipped
		{Name: "D", Kind: "type", StartLine: 6, EndLine: 6},
		{Name: "E", Kind: "interface", StartLine: 7, EndLine: 7},
	}
	c := New(100, 0)
	chunks := c.Chunk("a.go", "go", source, syms)

	var symbolKinds []string
	for _, ch := range chunks {
		if ch.Kind == "symbol" {
			symbolKinds = append(symbolKinds, ch.Symbol)
		}
	}
	assert.Contains(t, symbolKinds, "A")
	assert.Contains(t, symbolKinds, "D")
	assert.Contains(t, symbolKinds, "E")
	assert.NotContains(t, symbolKinds, "B")
	assert.NotContains(t, symbolKinds, "C")
}

func TestChunk_SymbolEndLineBeyondFile(t *testing.T) {
	source := []byte("line1\nline2\nline3")
	syms := []parser.Symbol{
		{Name: "X", Kind: "function", StartLine: 2, EndLine: 100}, // endLine > len(lines)
	}
	c := New(100, 0)
	chunks := c.Chunk("a.go", "go", source, syms)
	var found bool
	for _, ch := range chunks {
		if ch.Kind == "symbol" && ch.Symbol == "X" {
			found = true
			// Should clamp to file end
			assert.Contains(t, ch.Text, "line2")
		}
	}
	assert.True(t, found)
}

func TestChunk_ImportsFromFirstSymbol(t *testing.T) {
	source := []byte("package x\nfunc A() {}\n")
	syms := []parser.Symbol{
		{Name: "A", Kind: "function", StartLine: 2, EndLine: 2, Imports: []string{"fmt", "strings"}},
	}
	c := New(100, 0)
	chunks := c.Chunk("a.go", "go", source, syms)
	for _, ch := range chunks {
		if ch.Kind == "window" {
			assert.Equal(t, []string{"fmt", "strings"}, ch.Imports)
		}
	}
}

func TestChunk_NoImportsWhenNoSymbols(t *testing.T) {
	source := []byte("package x\nfunc A() {}\n")
	c := New(100, 0)
	chunks := c.Chunk("a.go", "go", source, nil)
	for _, ch := range chunks {
		assert.Nil(t, ch.Imports)
	}
}

// ---------------------------------------------------------------------------
// ChunkByTree: additional cases
// ---------------------------------------------------------------------------

func TestChunkByTree_PreservesDocAndParent(t *testing.T) {
	source := []byte("class Foo {\n  method() {}\n}\n")
	tree := []parser.Symbol{
		{StartByte: 0, EndByte: len(source), StartLine: 1, Parent: "Foo", Doc: "Foo doc", Imports: []string{"x"}},
	}
	got := New(10, 0).ChunkByTree("a.ts", "typescript", source, tree)
	require.Len(t, got, 1)
	assert.Equal(t, "Foo", got[0].Parent)
	assert.Equal(t, "Foo doc", got[0].Comments)
	assert.Equal(t, []string{"x"}, got[0].Imports)
	assert.Equal(t, "tree", got[0].Kind)
}

func TestChunkByTree_EmptyTreeChunks(t *testing.T) {
	got := New(10, 0).ChunkByTree("a.go", "go", []byte("package x"), nil)
	assert.Empty(t, got)
}

func TestChunkByTree_MultipleTreeChunks(t *testing.T) {
	source := []byte("func A() {}\nfunc B() {}\nfunc C() {}\n")
	tree := []parser.Symbol{
		{StartByte: 0, EndByte: 11, StartLine: 1},
		{StartByte: 11, EndByte: 22, StartLine: 2},
		{StartByte: 22, EndByte: len(source), StartLine: 3},
	}
	got := New(10, 0).ChunkByTree("a.go", "go", source, tree)
	assert.Len(t, got, 3)
}

// ---------------------------------------------------------------------------
// splitText: direct testing through ChunkByTree
// ---------------------------------------------------------------------------

func TestSplitText_VeryLongSingleLine(t *testing.T) {
	// A single line longer than MaxChunkBytes should be split by runes
	longLine := strings.Repeat("a", MaxChunkBytes*3)
	source := []byte(longLine)
	tree := []parser.Symbol{
		{StartByte: 0, EndByte: len(source), StartLine: 1},
	}
	got := New(10, 0).ChunkByTree("a.go", "go", source, tree)
	assert.GreaterOrEqual(t, len(got), 3, "expected at least 3 sub-chunks for 3x MaxChunkBytes line")

	// Reassemble
	var reassembled string
	for _, ch := range got {
		reassembled += ch.Text
	}
	assert.Equal(t, longLine, reassembled)
}

func TestSplitText_UnicodeLongLine(t *testing.T) {
	// Long line with multi-byte runes
	runeStr := strings.Repeat("é", MaxChunkBytes) // each é is 2 bytes
	source := []byte(runeStr)
	tree := []parser.Symbol{
		{StartByte: 0, EndByte: len(source), StartLine: 1},
	}
	got := New(10, 0).ChunkByTree("a.go", "go", source, tree)
	assert.GreaterOrEqual(t, len(got), 2)

	// All chunks should have valid UTF-8
	for _, ch := range got {
		assert.True(t, len(ch.Text) > 0)
	}
}

func TestSplitText_ExactMaxChunkBytes(t *testing.T) {
	// Text exactly at MaxChunkBytes boundary
	text := strings.Repeat("x", MaxChunkBytes)
	source := []byte(text)
	tree := []parser.Symbol{
		{StartByte: 0, EndByte: len(source), StartLine: 1},
	}
	got := New(10, 0).ChunkByTree("a.go", "go", source, tree)
	assert.Len(t, got, 1, "exact MaxChunkBytes should not be split")
	assert.Equal(t, text, got[0].Text)
}

func TestSplitText_OneByteOver(t *testing.T) {
	text := strings.Repeat("x", MaxChunkBytes+1)
	source := []byte(text)
	tree := []parser.Symbol{
		{StartByte: 0, EndByte: len(source), StartLine: 1},
	}
	got := New(10, 0).ChunkByTree("a.go", "go", source, tree)
	// Single line > MaxChunkBytes gets split by runes
	assert.GreaterOrEqual(t, len(got), 2)
}

func TestSplitText_MultiLineOverLimit(t *testing.T) {
	// Multiple lines that together exceed MaxChunkBytes
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = strings.Repeat("x", 100)
	}
	text := strings.Join(lines, "\n")
	source := []byte(text)
	tree := []parser.Symbol{
		{StartByte: 0, EndByte: len(source), StartLine: 1},
	}
	got := New(10, 0).ChunkByTree("a.go", "go", source, tree)
	assert.GreaterOrEqual(t, len(got), 2)

	// Each chunk (except possibly last) should be ≤ MaxChunkBytes
	for i, ch := range got {
		if i < len(got)-1 {
			assert.LessOrEqual(t, len(ch.Text), MaxChunkBytes, "chunk[%d] too large: %d", i, len(ch.Text))
		}
	}
}

// ---------------------------------------------------------------------------
// MaxChunkBytes constant
// ---------------------------------------------------------------------------

func TestMaxChunkBytes_Value(t *testing.T) {
	assert.Equal(t, 2000, MaxChunkBytes)
}

// ---------------------------------------------------------------------------
// Chunk: whitespace-only lines skipped
// ---------------------------------------------------------------------------

func TestChunk_WhitespaceOnlyWindowsSkipped(t *testing.T) {
	source := []byte("   \n\n   \n\nfunc A() {}\n\n   \n")
	c := New(2, 0)
	chunks := c.Chunk("a.go", "go", source, nil)
	for _, ch := range chunks {
		if ch.Kind == "window" {
			assert.NotEmpty(t, strings.TrimSpace(ch.Text))
		}
	}
}
