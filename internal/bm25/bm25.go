// Package bm25 — лексический индекс на базе Bleve (BM25-ranker).
//
// Используется в hybrid retrieval вместе с векторным поиском. Bleve выбран
// вместо упомянутого в ТЗ Tantivy потому, что это нативный Go (CGO/sidecar
// не требуются), а BM25 в нём поддерживается из коробки.
package bm25

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search/query"
)

// ErrClosed возвращается при работе с уже закрытым индексом.
var ErrClosed = errors.New("bm25: index is closed")

// Doc — документ для индексации. ID должен быть стабильным (например,
// "<file>:<ast_unit_id>" либо "<file>:<chunk_idx>").
type Doc struct {
	ID        string `json:"id"`
	Repo      string `json:"repo"` // имя репы (multi-repo workspace), "" = single-root
	Path      string `json:"path"`
	Language  string `json:"language"`
	Kind      string `json:"kind"`   // ast unit kind (function/class/...) либо "chunk" для текстовых чанков
	Symbol    string `json:"symbol"` // qualified name (если ast-unit)
	Content   string `json:"content"`
	AstUnitID int64  `json:"ast_unit_id"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// Hit — результат BM25-поиска.
type Hit struct {
	ID        string  `json:"id"`
	Score     float64 `json:"score"`
	Repo      string  `json:"repo"`
	Path      string  `json:"path"`
	Language  string  `json:"language"`
	Kind      string  `json:"kind"`
	Symbol    string  `json:"symbol"`
	AstUnitID int64   `json:"ast_unit_id"`
	StartLine int     `json:"start_line"`
	EndLine   int     `json:"end_line"`
	Snippet   string  `json:"snippet"`
}

// Query — параметры BM25-поиска.
type Query struct {
	Text     string
	Language string   // фильтр
	Kind     string   // фильтр (function/class/...)
	Repos    []string // фильтр по репам (multi-repo). nil/пусто = все репы.
	Limit    int
}

// Index — интерфейс лексического индекса.
type Index interface {
	IndexDocs(ctx context.Context, docs []Doc) error
	Delete(ctx context.Context, ids []string) error
	DeleteByPath(ctx context.Context, path string) error
	// Clear полностью очищает индекс (для batch-переиндексации).
	Clear(ctx context.Context) error
	Search(ctx context.Context, q Query) ([]Hit, error)
	Count(ctx context.Context) (uint64, error)
	Close() error
}

// bleveIndex — реализация поверх github.com/blevesearch/bleve/v2.
type bleveIndex struct {
	idx  bleve.Index
	path string
}

// Open открывает (или создаёт) индекс по указанному пути. k1, b сейчас
// игнорируются: Bleve использует собственный bm25-ranker с дефолтными
// параметрами; их кастомизация требует подмены similarity-функции и
// выходит за рамки текущей реализации.
func Open(path string, k1, b float64) (Index, error) {
	if path == "" {
		return nil, errors.New("bm25: empty index path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("bm25: mkdir: %w", err)
	}

	var idx bleve.Index
	if _, err := os.Stat(path); err == nil {
		i, err := bleve.Open(path)
		if err != nil {
			return nil, fmt.Errorf("bm25: open: %w", err)
		}
		idx = i
	} else {
		m := buildMapping()
		i, err := bleve.New(path, m)
		if err != nil {
			return nil, fmt.Errorf("bm25: new: %w", err)
		}
		idx = i
	}

	return &bleveIndex{idx: idx, path: path}, nil
}

func buildMapping() mapping.IndexMapping {
	im := bleve.NewIndexMapping()

	docMap := bleve.NewDocumentMapping()

	contentField := bleve.NewTextFieldMapping()
	contentField.Analyzer = "standard"
	contentField.Store = true
	docMap.AddFieldMappingsAt("content", contentField)

	symbolField := bleve.NewTextFieldMapping()
	symbolField.Analyzer = "standard"
	symbolField.Store = true
	docMap.AddFieldMappingsAt("symbol", symbolField)

	keywordField := bleve.NewTextFieldMapping()
	keywordField.Analyzer = "keyword"
	keywordField.Store = true
	for _, name := range []string{"path", "language", "kind", "id", "repo"} {
		docMap.AddFieldMappingsAt(name, keywordField)
	}

	num := bleve.NewNumericFieldMapping()
	num.Store = true
	docMap.AddFieldMappingsAt("ast_unit_id", num)
	docMap.AddFieldMappingsAt("start_line", num)
	docMap.AddFieldMappingsAt("end_line", num)

	im.DefaultMapping = docMap
	return im
}

func (b *bleveIndex) IndexDocs(ctx context.Context, docs []Doc) error {
	if b == nil || b.idx == nil {
		return ErrClosed
	}
	if len(docs) == 0 {
		return nil
	}
	batch := b.idx.NewBatch()
	for _, d := range docs {
		if d.ID == "" {
			continue
		}
		if err := batch.Index(d.ID, d); err != nil {
			return fmt.Errorf("bm25: batch add: %w", err)
		}
	}
	return b.idx.Batch(batch)
}

func (b *bleveIndex) Delete(ctx context.Context, ids []string) error {
	if b == nil || b.idx == nil {
		return ErrClosed
	}
	if len(ids) == 0 {
		return nil
	}
	batch := b.idx.NewBatch()
	for _, id := range ids {
		batch.Delete(id)
	}
	return b.idx.Batch(batch)
}

func (b *bleveIndex) DeleteByPath(ctx context.Context, path string) error {
	if b == nil || b.idx == nil {
		return ErrClosed
	}
	q := bleve.NewTermQuery(path)
	q.SetField("path")
	req := bleve.NewSearchRequest(q)
	req.Size = 10000
	req.Fields = []string{"id"}
	res, err := b.idx.Search(req)
	if err != nil {
		return fmt.Errorf("bm25: search-by-path: %w", err)
	}
	ids := make([]string, 0, len(res.Hits))
	for _, h := range res.Hits {
		ids = append(ids, h.ID)
	}
	return b.Delete(ctx, ids)
}

func (b *bleveIndex) Clear(ctx context.Context) error {
	if b == nil || b.idx == nil {
		return ErrClosed
	}
	// Удаляем все документы — ищем всё и удаляем батчем.
	q := bleve.NewMatchAllQuery()
	req := bleve.NewSearchRequest(q)
	req.Size = 100000
	req.Fields = []string{"id"}
	res, err := b.idx.Search(req)
	if err != nil {
		return fmt.Errorf("bm25: clear search: %w", err)
	}
	ids := make([]string, 0, len(res.Hits))
	for _, h := range res.Hits {
		ids = append(ids, h.ID)
	}
	return b.Delete(ctx, ids)
}

func (b *bleveIndex) Search(ctx context.Context, q Query) ([]Hit, error) {
	if b == nil || b.idx == nil {
		return nil, ErrClosed
	}
	if q.Text == "" {
		return nil, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}

	parts := []query.Query{}
	mq := bleve.NewMatchQuery(q.Text)
	mq.SetField("content")
	mq.SetBoost(1.0)
	sq := bleve.NewMatchQuery(q.Text)
	sq.SetField("symbol")
	sq.SetBoost(2.0)
	disj := bleve.NewDisjunctionQuery(mq, sq)
	parts = append(parts, disj)

	if q.Language != "" {
		f := bleve.NewTermQuery(q.Language)
		f.SetField("language")
		parts = append(parts, f)
	}
	if q.Kind != "" {
		f := bleve.NewTermQuery(q.Kind)
		f.SetField("kind")
		parts = append(parts, f)
	}
	if len(q.Repos) > 0 {
		repoParts := make([]query.Query, 0, len(q.Repos))
		for _, r := range q.Repos {
			if r == "" || r == "*" {
				repoParts = nil // "*" среди значений отключает фильтр
				break
			}
			t := bleve.NewTermQuery(r)
			t.SetField("repo")
			repoParts = append(repoParts, t)
		}
		if len(repoParts) > 0 {
			parts = append(parts, bleve.NewDisjunctionQuery(repoParts...))
		}
	}

	req := bleve.NewSearchRequest(bleve.NewConjunctionQuery(parts...))
	req.Size = limit
	req.Fields = []string{"repo", "path", "language", "kind", "symbol", "ast_unit_id", "start_line", "end_line", "content"}
	req.Highlight = bleve.NewHighlight()

	res, err := b.idx.SearchInContext(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("bm25: search: %w", err)
	}
	out := make([]Hit, 0, len(res.Hits))
	for _, h := range res.Hits {
		hit := Hit{ID: h.ID, Score: h.Score}
		hit.Repo, _ = h.Fields["repo"].(string)
		hit.Path, _ = h.Fields["path"].(string)
		hit.Language, _ = h.Fields["language"].(string)
		hit.Kind, _ = h.Fields["kind"].(string)
		hit.Symbol, _ = h.Fields["symbol"].(string)
		if v, ok := h.Fields["ast_unit_id"].(float64); ok {
			hit.AstUnitID = int64(v)
		}
		if v, ok := h.Fields["start_line"].(float64); ok {
			hit.StartLine = int(v)
		}
		if v, ok := h.Fields["end_line"].(float64); ok {
			hit.EndLine = int(v)
		}
		if frags, ok := h.Fragments["content"]; ok && len(frags) > 0 {
			hit.Snippet = frags[0]
		} else if c, ok := h.Fields["content"].(string); ok {
			if len(c) > 240 {
				c = c[:240] + "..."
			}
			hit.Snippet = c
		}
		out = append(out, hit)
	}
	return out, nil
}

func (b *bleveIndex) Count(ctx context.Context) (uint64, error) {
	if b == nil || b.idx == nil {
		return 0, ErrClosed
	}
	return b.idx.DocCount()
}

func (b *bleveIndex) Close() error {
	if b == nil || b.idx == nil {
		return nil
	}
	err := b.idx.Close()
	b.idx = nil
	return err
}
