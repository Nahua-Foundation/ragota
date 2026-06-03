package chunker

import (
	"strings"
	"testing"

	"ragota/internal/indexing/parser"
)

// makeLines собирает source из n строк "lineN", разделённых '\n'.
func makeLines(n int) string {
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = "line"
	}
	return strings.Join(parts, "\n")
}

// ---------------------------------------------------------------------------
// New: дефолты и валидация параметров.
// ---------------------------------------------------------------------------

func TestNew_DefaultsOnInvalidParams(t *testing.T) {
	cases := []struct {
		name            string
		window, overlap int
		wantWindow      int
		wantOverlap     int
	}{
		// window<=0 → 60; overlap=0 валиден (>=0 и <60), оставляется как есть.
		{"both zero → window default, overlap stays 0", 0, 0, 60, 0},
		// window<=0 → 60; overlap=4 валиден → оставляется.
		{"negative window → default 60, overlap kept", -5, 4, 60, 4},
		{"valid values preserved", 40, 5, 40, 5},
		// overlap >= window → window/6.
		{"overlap >= window → window/6", 30, 30, 30, 5},
		// overlap < 0 → window/6.
		{"overlap negative → window/6", 30, -1, 30, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New(tc.window, tc.overlap)
			if c.WindowLines != tc.wantWindow || c.OverlapLines != tc.wantOverlap {
				t.Errorf("New(%d, %d) = {%d, %d}, want {%d, %d}",
					tc.window, tc.overlap, c.WindowLines, c.OverlapLines,
					tc.wantWindow, tc.wantOverlap)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Chunk: оконное разбиение и символьные чанки.
// ---------------------------------------------------------------------------

func TestChunk_WindowedSplitWithOverlap(t *testing.T) {
	// 25 строк, окно 10, overlap 2 → step=8 → старты: 0,8,16,24 → 4 окна.
	source := []byte(makeLines(25))
	c := New(10, 2)

	chunks := c.Chunk("a.go", "go", source, nil)
	if len(chunks) != 4 {
		t.Fatalf("expected 4 windowed chunks, got %d", len(chunks))
	}
	for i, ch := range chunks {
		if ch.Kind != "window" {
			t.Errorf("chunk[%d].Kind = %q, want \"window\"", i, ch.Kind)
		}
		if ch.Path != "a.go" || ch.Language != "go" {
			t.Errorf("chunk[%d] meta wrong: %+v", i, ch)
		}
		if ch.StartLine < 1 {
			t.Errorf("chunk[%d] start line must be 1-based, got %d", i, ch.StartLine)
		}
	}
	// Первое окно начинается с 1-й строки (1-based).
	if chunks[0].StartLine != 1 {
		t.Errorf("first chunk should start at line 1, got %d", chunks[0].StartLine)
	}
}

func TestChunk_EmptySourceProducesNoChunks(t *testing.T) {
	c := New(10, 2)
	if got := c.Chunk("a.go", "go", []byte(""), nil); len(got) != 0 {
		t.Errorf("empty source should produce no chunks, got %d", len(got))
	}
	if got := c.Chunk("a.go", "go", []byte("\n\n\n"), nil); len(got) != 0 {
		t.Errorf("whitespace-only source should produce no chunks, got %d", len(got))
	}
}

func TestChunk_AddsSymbolChunks(t *testing.T) {
	source := []byte("package x\n\nfunc Foo() {}\nfunc Bar() {}\n")
	syms := []parser.Symbol{
		{Name: "Foo", Kind: "function", StartLine: 3, EndLine: 3, Imports: []string{"fmt"}, Doc: "// Foo doc"},
		{Name: "Bar", Kind: "function", StartLine: 4, EndLine: 4, Parent: "X"},
		{Name: "skipMe", Kind: "variable", StartLine: 1, EndLine: 1}, // не семантический — должен быть пропущен
	}
	c := New(100, 0) // одно окно покрывает весь файл
	chunks := c.Chunk("a.go", "go", source, syms)

	var symbolChunks []Chunk
	for _, ch := range chunks {
		if ch.Kind == "symbol" {
			symbolChunks = append(symbolChunks, ch)
		}
	}
	if len(symbolChunks) != 2 {
		t.Fatalf("expected 2 symbol chunks (Foo, Bar), got %d", len(symbolChunks))
	}

	byName := map[string]Chunk{}
	for _, ch := range symbolChunks {
		byName[ch.Symbol] = ch
	}
	foo, ok := byName["Foo"]
	if !ok {
		t.Fatal("missing Foo symbol chunk")
	}
	if foo.Comments != "// Foo doc" {
		t.Errorf("Foo.Comments = %q, want '// Foo doc'", foo.Comments)
	}
	if len(foo.Imports) != 1 || foo.Imports[0] != "fmt" {
		t.Errorf("Foo.Imports = %v, want [fmt]", foo.Imports)
	}
	bar, ok := byName["Bar"]
	if !ok {
		t.Fatal("missing Bar symbol chunk")
	}
	if bar.Parent != "X" {
		t.Errorf("Bar.Parent = %q, want X", bar.Parent)
	}
}

func TestChunk_SkipsSymbolsWithInvalidLineRange(t *testing.T) {
	source := []byte("package x\nfunc Foo() {}\n")
	syms := []parser.Symbol{
		{Name: "Bad1", Kind: "function", StartLine: 0, EndLine: 1},    // StartLine < 1
		{Name: "Bad2", Kind: "function", StartLine: 5, EndLine: 4},    // End < Start
		{Name: "Bad3", Kind: "function", StartLine: 99, EndLine: 100}, // вне диапазона
		{Name: "Good", Kind: "function", StartLine: 2, EndLine: 2},
	}
	chunks := New(100, 0).Chunk("a.go", "go", source, syms)
	var got []string
	for _, ch := range chunks {
		if ch.Kind == "symbol" {
			got = append(got, ch.Symbol)
		}
	}
	if len(got) != 1 || got[0] != "Good" {
		t.Errorf("expected only [Good] symbol chunk, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// splitText через ChunkByTree — самый простой способ протестировать большие
// чанки (используется напрямую в ChunkByTree, без оконной обвязки).
// ---------------------------------------------------------------------------

func TestChunkByTree_RespectsMaxChunkBytes(t *testing.T) {
	// Один tree-чанк размером > MaxChunkBytes должен быть разбит на несколько.
	// Используем многострочный текст, чтобы splitText мог резать по строкам.
	line := strings.Repeat("x", 200)         // 200-байтовая строка
	bigText := strings.Repeat(line+"\n", 30) // ~6 КБ, заведомо > MaxChunkBytes (2000)
	source := []byte(bigText)
	tree := []parser.Symbol{
		{StartByte: 0, EndByte: len(source), StartLine: 1, Doc: "doc"},
	}
	c := New(10, 2)
	got := c.ChunkByTree("big.go", "go", source, tree)
	if len(got) < 2 {
		t.Fatalf("expected at least 2 sub-chunks for oversized tree chunk, got %d", len(got))
	}
	for i, ch := range got {
		if ch.Kind != "tree" {
			t.Errorf("chunk[%d].Kind = %q, want \"tree\"", i, ch.Kind)
		}
		// MaxChunkBytes — мягкий порог: при отдельной строке длиннее лимита
		// splitText может породить чанк такой длины. Но в нашем кейсе строки
		// по 200 байт, так что каждый чанк должен укладываться в ~MaxChunkBytes.
		if len(ch.Text) > MaxChunkBytes+len(line) {
			t.Errorf("chunk[%d] unexpectedly large: %d bytes", i, len(ch.Text))
		}
	}
}

func TestChunkByTree_SkipsInvalidByteRanges(t *testing.T) {
	source := []byte("hello world")
	tree := []parser.Symbol{
		{StartByte: -1, EndByte: 5},                        // negative
		{StartByte: 0, EndByte: 1000},                      // out of range
		{StartByte: 5, EndByte: 5},                         // empty
		{StartByte: 0, EndByte: len(source), StartLine: 1}, // valid
	}
	got := New(10, 2).ChunkByTree("a.go", "go", source, tree)
	if len(got) != 1 {
		t.Fatalf("expected 1 valid chunk, got %d: %+v", len(got), got)
	}
	if got[0].Text != "hello world" {
		t.Errorf("chunk text mismatch: %q", got[0].Text)
	}
}

func TestChunkByTree_SkipsWhitespaceOnly(t *testing.T) {
	source := []byte("   \n\t\n  ")
	tree := []parser.Symbol{{StartByte: 0, EndByte: len(source), StartLine: 1}}
	if got := New(10, 2).ChunkByTree("a.go", "go", source, tree); len(got) != 0 {
		t.Errorf("whitespace-only tree chunk should be skipped, got %d", len(got))
	}
}
