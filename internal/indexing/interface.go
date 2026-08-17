package indexing

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// ErrIndexDamaged reports that an index could not answer because its on-disk
// data is unreadable. Rebuilding it (a forced reindex) is the only repair.
//
// It lives here, not with the indexer that raises it, so the search service and
// the HTTP layer can recognise it without depending on a particular indexer.
var ErrIndexDamaged = errors.New("index segment is unreadable")

// IndexType represents the type of indexer.
type IndexType string

const (
	IndexTypeAST    IndexType = "ast"
	IndexTypeVector IndexType = "vector"
	IndexTypeBM25   IndexType = "bm25"
	IndexTypeCustom IndexType = "custom"
)

// Indexer is the interface for all indexers (indexing operations only).
type Indexer interface {
	// Name returns the indexer name.
	Name() string

	// Type returns the indexer type.
	Type() IndexType

	// Init initializes the indexer with configuration.
	Init(ctx context.Context, config map[string]interface{}) error

	// Index indexes a repository's files.
	Index(ctx context.Context, req *IndexRequest) (*IndexResult, error)

	// Remove removes indexed data for files in a repo.
	Remove(ctx context.Context, repoID string, paths []string) error

	// Stats returns indexer statistics.
	Stats(ctx context.Context) (*IndexerStats, error)

	// Close closes the indexer and releases resources.
	Close() error
}

// Searcher is the interface for search operations.
type Searcher interface {
	Search(ctx context.Context, q *SearchQuery) (*SearchResult, error)
}

// Compactor is implemented by an indexer whose answers depend on how its
// storage happens to be laid out and not only on what was indexed — a
// distinction that matters because layout is decided by background work, so
// two passes over the same sources otherwise disagree. A full pass calls
// Compact once it has written everything, which settles the layout before
// anything reads it. Indexers that do not implement it have nothing to settle.
type Compactor interface {
	Compact(ctx context.Context) error
}

// IndexRequest is a request to index a repository.
type IndexRequest struct {
	RepoID   string
	RepoPath string
	RepoName string
	Files    []*FileToIndex
	Force    bool // Re-index even if hash matches

	// Annotations carry text about a symbol that the indexer cannot derive
	// from the file: an LLM-written line saying what the symbol does, for code
	// that documents itself only in its own vocabulary. Keyed by
	// AnnotationKey(file path, symbol start line); an indexer that has no use
	// for them ignores the map.
	Annotations map[string]string
}

// AnnotationKey keys an annotation to the symbol it describes.
func AnnotationKey(path string, startLine int) string {
	return path + ":" + strconv.Itoa(startLine)
}

// FileToIndex represents a file to be indexed.
type FileToIndex struct {
	Path     string
	Hash     string
	Language string
	Content  []byte // Nil if not needed (e.g., for re-indexing by hash)
}

// IndexResult is the result of an indexing operation.
type IndexResult struct {
	FilesIndexed int
	FilesSkipped int
	FilesFailed  int
	Duration     time.Duration
	Errors       []string // File paths that failed

	// Coverage counts, per contract kind (storage.ContractKindHTTP and
	// friends), the call sites this run recognized as outbound contracts and
	// how many of them produced an edge. It describes one Index call only —
	// a pass indexes in batches, and summing them is the caller's job.
	//
	// A kind present with {0, 0} means the indexer looked and found nothing;
	// a kind that is absent means it did not look. An indexer that cannot
	// report leaves the map nil, which is not the same answer as an empty one.
	Coverage map[string]storage.CoverageCounts
}

// ContractCoverage implements the coverage reporter the service layer consumes
// (see internal/service/coverage.go).
func (r *IndexResult) ContractCoverage() map[string]storage.CoverageCounts {
	return r.Coverage
}

// SearchQuery is a search query.
type SearchQuery struct {
	Query  string
	RepoID string
	Repos  []string // Search in specific repos
	Limit  int
	Filter map[string]interface{} // see ParseFilters for the understood keys
	Vector []float32              // For vector search (already embedded)

	// Intent names what the query text is asking for: "" or "auto" lets the
	// service detect it from the phrasing, "callers" asks for the code that
	// calls/uses the described symbol, "none" forces plain text retrieval.
	// Individual searchers ignore it — the service layer acts on it after
	// retrieval (see service.promoteCallers).
	Intent string
}

// Filter keys understood by the searchers. Singular and plural spellings are
// equivalent; values may be a string or a list of strings.
const (
	FilterLanguage   = "language"
	FilterLanguages  = "languages"
	FilterKind       = "kind"
	FilterKinds      = "kinds"
	FilterPath       = "path"
	FilterPathPrefix = "path_prefix"
)

// Filters is the parsed, normalized form of SearchQuery.Filter.
type Filters struct {
	Languages  []string
	Kinds      []string
	PathPrefix string
}

// Empty reports whether no filter is set.
func (f Filters) Empty() bool {
	return len(f.Languages) == 0 && len(f.Kinds) == 0 && f.PathPrefix == ""
}

// Match reports whether a hit's attributes satisfy the filters. Attributes
// that the caller could not determine (empty strings) never match a filter
// that is set, so an unannotated document is dropped rather than smuggled in.
func (f Filters) Match(language, kind, path string) bool {
	if len(f.Languages) > 0 && !containsFold(f.Languages, language) {
		return false
	}
	if len(f.Kinds) > 0 && !containsFold(f.Kinds, kind) {
		return false
	}
	if f.PathPrefix != "" && !strings.HasPrefix(path, f.PathPrefix) {
		return false
	}
	return true
}

func containsFold(list []string, v string) bool {
	if v == "" {
		return false
	}
	for _, item := range list {
		if strings.EqualFold(item, v) {
			return true
		}
	}
	return false
}

// ParseFilters normalizes a raw filter map into Filters. Unknown keys and
// values of unexpected types are ignored — a filter that cannot be understood
// must not silently narrow the result set.
func ParseFilters(raw map[string]interface{}) Filters {
	var f Filters
	for key, value := range raw {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case FilterLanguage, FilterLanguages:
			f.Languages = append(f.Languages, toStrings(value)...)
		case FilterKind, FilterKinds:
			f.Kinds = append(f.Kinds, toStrings(value)...)
		case FilterPath, FilterPathPrefix:
			if vs := toStrings(value); len(vs) > 0 {
				f.PathPrefix = vs[0]
			}
		}
	}
	sort.Strings(f.Languages)
	sort.Strings(f.Kinds)
	return f
}

func toStrings(value interface{}) []string {
	var out []string
	switch v := value.(type) {
	case string:
		if s := strings.TrimSpace(v); s != "" {
			out = append(out, s)
		}
	case []string:
		for _, s := range v {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	case []interface{}:
		for _, item := range v {
			out = append(out, toStrings(item)...)
		}
	}
	return out
}

// SearchResult is the result of a search query.
type SearchResult struct {
	Hits  []*Hit
	Total int
	Query string
	// Mode is the search mode that actually ran, filled in by the layer that
	// resolves it — an empty request mode means the default, and only that
	// layer knows what the default is. A single searcher leaves it empty: it is
	// one retrieval channel, not a mode.
	Mode     string
	Duration time.Duration
	Metadata map[string]interface{}
}

// Hit represents a single search result.
type Hit struct {
	RepoID   string  `json:"repo_id"`
	FilePath string  `json:"file_path"`
	Path     string  `json:"path"` // For display (relative to repo root)
	Line     int     `json:"line"`
	EndLine  int     `json:"end_line"`
	Symbol   string  `json:"symbol,omitempty"`
	Kind     string  `json:"kind,omitempty"`
	Language string  `json:"language,omitempty"`
	Score    float32 `json:"score"`
	Snippet  string  `json:"snippet,omitempty"`
	Reason   string  `json:"reason,omitempty"` // Why this result matched
}

// Key returns a unique key for this hit (for merging results).
func (h *Hit) Key() string {
	return fmt.Sprintf("%s:%s:%d:%d", h.RepoID, h.FilePath, h.Line, h.EndLine)
}

// Range returns the hit's line span, normalized so that the end is never
// before the start (indexers that only know a start line leave EndLine zero).
func (h *Hit) Range() (int, int) {
	start, end := h.Line, h.EndLine
	if end < start {
		end = start
	}
	return start, end
}

// Overlaps reports whether two hits cover the same file and their line ranges
// intersect.
func (h *Hit) Overlaps(other *Hit) bool {
	if h.RepoID != other.RepoID || h.FilePath != other.FilePath {
		return false
	}
	s1, e1 := h.Range()
	s2, e2 := other.Range()
	return s1 <= e2 && s2 <= e1
}

// IndexerStats holds statistics for an indexer.
type IndexerStats struct {
	// Total number of documents indexed
	Documents int64
	// Total size of indexed content in bytes
	SizeBytes int64
	// Number of repositories indexed
	Repos int
	// Indexer-specific stats
	Specific map[string]interface{}
}
