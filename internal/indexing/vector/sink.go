package vector

import "context"

// Doc — документ для лексического индекса, определённый продюсером.
// Содержит все поля, необходимые для полнотекстового поиска.
type Doc struct {
	ID        string
	Repo      string
	Path      string
	Language  string
	Kind      string
	Symbol    string
	Content   string
	StartLine int
	EndLine   int
}

// SearchResult — результат поиска в лексическом индексе.
type SearchResult struct {
	ID        string
	Score     float64
	Repo      string
	Path      string
	Language  string
	Kind      string
	Symbol    string
	Snippet   string
	StartLine int
	EndLine   int
}

// SearchQuery — параметры поиска.
type SearchQuery struct {
	Text     string
	Language string
	Kind     string
	Repos    []string
	Limit    int
}

// WriteSink — интерфейс поискового индекса (BM25 и аналоги).
// Определяется продюсером данных (indexing/vector), а не потребителем (search/bm25).
type WriteSink interface {
	IndexDocs(ctx context.Context, docs []Doc) error
	DeleteByPath(ctx context.Context, path string) error
	Clear(ctx context.Context) error
	Count(ctx context.Context) (uint64, error)
	Search(ctx context.Context, q SearchQuery) ([]SearchResult, error)
}
