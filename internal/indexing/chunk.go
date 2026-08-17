package indexing

import (
	"context"
	"fmt"
	"strings"
)

// Chunk represents a code chunk.
type Chunk struct {
	ID         string
	FilePath   string
	Language   string
	StartLine  int
	EndLine    int
	Text       string
	SymbolName string
	SymbolKind string
}

// ChunkInput is a source file handed to a Chunker. It is distinct from
// FileToIndex: a chunker needs only the bytes and how to read them, not the
// hash and bookkeeping an index pass carries.
type ChunkInput struct {
	Path     string
	Language string
	Content  []byte
}

// Chunker is the interface for chunking strategies.
type Chunker interface {
	Chunk(ctx context.Context, f *ChunkInput) ([]*Chunk, error)
}

// ChunkConfig defines chunking configuration.
type ChunkConfig struct {
	Method      string // "window" (symbol-level chunking lives in the vector indexer as "cards")
	WindowLines int
	Overlap     int
}

func (c ChunkConfig) withDefaults() ChunkConfig {
	if c.WindowLines <= 0 {
		c.WindowLines = 60
	}
	if c.Overlap < 0 || c.Overlap >= c.WindowLines {
		c.Overlap = 10
	}
	if c.Method == "" {
		c.Method = "window"
	}
	return c
}

// ForFile returns a Chunker for the given language and config. Symbol-level
// chunking is not done here: the vector indexer builds symbol cards from AST
// units ("cards"), which covers every parsed language.
func ForFile(cfg ChunkConfig, language string) Chunker {
	cfg = cfg.withDefaults()
	return NewWindowChunker(cfg)
}

// WindowChunker implements sliding window chunking.
type WindowChunker struct {
	windowLines int
	overlap     int
}

// NewWindowChunker creates a new window chunker.
func NewWindowChunker(cfg ChunkConfig) *WindowChunker {
	cfg = cfg.withDefaults()
	return &WindowChunker{
		windowLines: cfg.WindowLines,
		overlap:     cfg.Overlap,
	}
}

// Chunk processes a file using window chunking.
func (w *WindowChunker) Chunk(ctx context.Context, f *ChunkInput) ([]*Chunk, error) {
	lines := strings.Split(string(f.Content), "\n")

	var chunks []*Chunk
	chunkNum := 0

	startLine := 0
	for startLine < len(lines) {
		endLine := startLine + w.windowLines
		if endLine > len(lines) {
			endLine = len(lines)
		}

		if startLine >= endLine {
			break
		}

		content := strings.Join(lines[startLine:endLine], "\n")

		chunk := &Chunk{
			ID:        fmt.Sprintf("%s:%d", f.Path, chunkNum),
			FilePath:  f.Path,
			Language:  f.Language,
			StartLine: startLine + 1,
			EndLine:   endLine,
			Text:      content,
		}

		chunks = append(chunks, chunk)
		chunkNum++

		if endLine >= len(lines) {
			break
		}

		nextStartLine := endLine - w.overlap
		if nextStartLine <= startLine || nextStartLine < 0 {
			break
		}
		startLine = nextStartLine
	}

	return chunks, nil
}
