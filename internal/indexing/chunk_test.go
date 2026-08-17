package indexing

import (
	"context"
	"strings"
	"testing"
)

func TestWindowChunking(t *testing.T) {
	lines := make([]string, 130)
	for i := range lines {
		lines[i] = "line " + string(rune('0'+i%10))
	}
	content := strings.Join(lines, "\n")

	chunker := NewWindowChunker(ChunkConfig{WindowLines: 60, Overlap: 10})
	file := &ChunkInput{Path: "test.go", Language: "go", Content: []byte(content)}

	chunks, err := chunker.Chunk(context.Background(), file)
	if err != nil {
		t.Fatalf("chunk: %v", err)
	}

	if len(chunks) == 0 {
		t.Fatal("expected chunks, got none")
	}

	if chunks[0].StartLine != 1 {
		t.Errorf("first chunk StartLine= %d, want 1", chunks[0].StartLine)
	}

	lastEnd := chunks[0].EndLine
	for i := 1; i < len(chunks); i++ {
		wantStart := lastEnd - chunker.overlap + 1
		if chunks[i].StartLine != wantStart {
			t.Errorf("chunk %d StartLine=%d, want %d", i, chunks[i].StartLine, wantStart)
		}
		lastEnd = chunks[i].EndLine
	}

	if lastEnd != 130 {
		t.Errorf("last chunk EndLine=%d, want 130", lastEnd)
	}
}

func TestShortFile(t *testing.T) {
	content := "line1\nline2\nline3\nline4\nline5"
	chunker := NewWindowChunker(ChunkConfig{WindowLines: 60, Overlap: 10})
	file := &ChunkInput{Path: "short.go", Language: "go", Content: []byte(content)}

	chunks, err := chunker.Chunk(context.Background(), file)
	if err != nil {
		t.Fatalf("chunk: %v", err)
	}

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}

	if chunks[0].StartLine != 1 || chunks[0].EndLine != 5 {
		t.Errorf("StartLine=%d EndLine=%d, want 1,5", chunks[0].StartLine, chunks[0].EndLine)
	}
}

// TestForFileAlwaysWindows pins the surviving contract: chunking only does
// window chunking. Symbol-level chunking moved to the vector indexer's cards
// mode, which covers every parsed language rather than Go alone.
func TestForFileAlwaysWindows(t *testing.T) {
	for _, lang := range []string{"go", "java", "typescript", "python", ""} {
		c := ForFile(ChunkConfig{Method: "window", WindowLines: 20, Overlap: 5}, lang)
		if _, ok := c.(*WindowChunker); !ok {
			t.Errorf("ForFile(%q) = %T, want *WindowChunker", lang, c)
		}
	}
}
