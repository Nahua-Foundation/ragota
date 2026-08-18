package vector

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/indexing"
)

// textCapturingEmbedder records every text it is asked to embed.
type textCapturingEmbedder struct {
	mu    sync.Mutex
	texts []string
}

func (e *textCapturingEmbedder) Name() string { return "capturing" }
func (e *textCapturingEmbedder) Dim() int     { return 2 }

func (e *textCapturingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.texts = append(e.texts, texts...)
	e.mu.Unlock()
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

// The query — and only the query — is wrapped when QueryInstruction is set.
// Documents embed bare at index time; that asymmetry is what instruction-aware
// models are trained on, and it is why the key needs no reindex.
func TestSearchWrapsQueryWithInstruction(t *testing.T) {
	const instruction = "Given a code search query, retrieve the most relevant code"

	emb := &textCapturingEmbedder{}
	idx := New(&Config{Embedder: emb, Storage: &memVecStore{}, QueryInstruction: instruction})

	if _, err := idx.Search(context.Background(), &indexing.SearchQuery{Query: "who charges the card", Limit: 5}); err != nil {
		t.Fatalf("Search: %v", err)
	}

	want := "Instruct: " + instruction + "\nQuery: who charges the card"
	if len(emb.texts) != 1 || emb.texts[0] != want {
		t.Fatalf("embedded %q, want %q", emb.texts, want)
	}
}

// Without the key the query goes to the embedder untouched — the pre-existing
// behaviour, pinned so the wrap can never become unconditional.
func TestSearchEmbedsBareQueryByDefault(t *testing.T) {
	emb := &textCapturingEmbedder{}
	idx := New(&Config{Embedder: emb, Storage: &memVecStore{}})

	if _, err := idx.Search(context.Background(), &indexing.SearchQuery{Query: "who charges the card", Limit: 5}); err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(emb.texts) != 1 || emb.texts[0] != "who charges the card" {
		t.Fatalf("embedded %q, want the bare query", emb.texts)
	}
	if strings.Contains(emb.texts[0], "Instruct:") {
		t.Fatalf("bare query gained an instruction: %q", emb.texts[0])
	}
}
