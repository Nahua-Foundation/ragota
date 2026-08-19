package bm25

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/index"
	"github.com/Nahua-Foundation/ragota/internal/index/ast"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/custom"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/keyword"
	regexpchar "github.com/blevesearch/bleve/v2/analysis/char/regexp"
	"github.com/blevesearch/bleve/v2/analysis/token/camelcase"
	"github.com/blevesearch/bleve/v2/analysis/token/lowercase"
	"github.com/blevesearch/bleve/v2/analysis/tokenizer/unicode"
	"github.com/blevesearch/bleve/v2/index/scorch"
	"github.com/blevesearch/bleve/v2/index/scorch/mergeplan"
	"github.com/blevesearch/bleve/v2/mapping"
	bleveSearch "github.com/blevesearch/bleve/v2/search"
	"github.com/blevesearch/bleve/v2/search/query"
	bleveIndexAPI "github.com/blevesearch/bleve_index_api"
)

// Compile-time interface assertions.
var _ index.Indexer = (*Indexer)(nil)
var _ index.Searcher = (*Indexer)(nil)

// Indexer implements index.Indexer and index.Searcher for BM25 keyword search.
type Indexer struct {
	index   bleve.Index
	path    string
	chunker *index.WindowChunker
	// compact says whether a finished index pass canonicalises the segment
	// layout; see Compact for why a score depends on it.
	compact bool
	// compactMu serialises compactions: bleve refuses a second force merge
	// while one is running, and two repositories can finish a pass at once.
	compactMu sync.Mutex
	// splitIdentifiers and indexPaths say whether this index carries the
	// code-aware view of its text and the searchable view of its paths. Both
	// are read from the index on disk rather than from the config, because
	// indexing and querying have to agree about a field that only a reindex
	// can add; see New.
	splitIdentifiers bool
	indexPaths       bool
	// pathBoost weights the path clause; it is query-time, so unlike the two
	// above it is the config's to decide on every start.
	pathBoost float64

	closeOnce sync.Once
	closeErr  error
}

// Config is the BM25 indexer configuration.
type Config struct {
	Path string // Path to store the Bleve index
	K1   float64
	B    float64
	// NoCompact leaves the segment layout as indexing happened to leave it.
	// It makes scores depend on how many segments the background merger got
	// through, so it is off by default; see Compact.
	NoCompact bool
	// SplitIdentifiers indexes a second, code-aware view of every chunk; see
	// codeAnalyzer. It shapes the index, so it applies when one is created and
	// a running index keeps whatever it was built with.
	SplitIdentifiers bool
	// IndexPaths makes a document's own path searchable text; see
	// pathTextField. It shapes the index the same way.
	IndexPaths bool
	// PathBoost weights the path clause against the text (0 or less means
	// defaultPathBoost). It is query-time: changing it needs no reindex.
	PathBoost float64
}

// applyBM25Params points bleve at the BM25 scoring model and installs k1/b.
// Bleve exposes these multipliers only as package-level variables, so they are
// process-wide: the last indexer created wins. With a single BM25 index per
// process (the only configuration this service builds) that is exact.
func applyBM25Params(k1, b float64) {
	if k1 > 0 {
		bleveSearch.BM25_k1 = k1
	}
	if b > 0 {
		bleveSearch.BM25_b = b
	}
}

// New creates a new BM25 indexer.
func New(cfg *Config) (*Indexer, error) {
	// Ensure directory exists
	if err := os.MkdirAll(cfg.Path, 0755); err != nil {
		return nil, fmt.Errorf("create directory: %w", err)
	}

	applyBM25Params(cfg.K1, cfg.B)

	// Open or create index
	idx, err := bleve.Open(cfg.Path)
	switch {
	case err == nil:
	case errors.Is(err, bleve.ErrorIndexPathDoesNotExist):
		mapping, mErr := buildMapping(cfg.SplitIdentifiers, cfg.IndexPaths)
		if mErr != nil {
			return nil, mErr
		}
		if idx, err = bleve.New(cfg.Path, mapping); err != nil {
			return nil, fmt.Errorf("create index at %s: %w", cfg.Path, err)
		}
	case (errors.Is(err, bleve.ErrorIndexMetaMissing) || errors.Is(err, bleve.ErrorIndexMetaCorrupt)) && dirIsEmpty(cfg.Path):
		// An existing but empty directory is the normal case for a freshly
		// mounted volume, and bleve.New accepts it.
		mapping, mErr := buildMapping(cfg.SplitIdentifiers, cfg.IndexPaths)
		if mErr != nil {
			return nil, mErr
		}
		if idx, err = bleve.New(cfg.Path, mapping); err != nil {
			return nil, fmt.Errorf("create index at %s: %w", cfg.Path, err)
		}
	case errors.Is(err, bleve.ErrorIndexMetaMissing), errors.Is(err, bleve.ErrorIndexMetaCorrupt):
		// The directory exists but its metadata does not, so bleve.New cannot
		// take it over — it refuses a non-empty path. Recreating silently would
		// also throw away an index that is merely half-written by another
		// instance starting at the same moment. Name the situation and the
		// repair instead of failing with "path already exists", which describes
		// the symptom of our own fallback rather than the cause.
		return nil, fmt.Errorf(
			"%w: %s exists but its metadata is missing or corrupt — remove the directory and reindex, "+
				"or point indexes.bm25.path elsewhere if another instance owns it", ErrIndexDamaged, cfg.Path)
	default:
		return nil, fmt.Errorf("open index at %s: %w", cfg.Path, err)
	}

	// A mapping is written once, when the index is created, and bleve.Open
	// takes it from disk — so what the index actually carries is the only
	// answer both indexing and querying can be built on. Reading the config
	// here instead would mean asking a query for a field no document has, and
	// calling the result a measurement.
	split := indexHasField(idx, codeSplitField)
	paths := indexHasField(idx, pathTextField)
	warnIndexShape("split_identifiers", cfg.SplitIdentifiers, split, cfg.Path)
	warnIndexShape("index_paths", cfg.IndexPaths, paths, cfg.Path)

	pathBoost := cfg.PathBoost
	if pathBoost <= 0 {
		pathBoost = defaultPathBoost
	}

	return &Indexer{
		index:            idx,
		path:             cfg.Path,
		chunker:          index.NewWindowChunker(index.ChunkConfig{}),
		compact:          !cfg.NoCompact,
		splitIdentifiers: split,
		indexPaths:       paths,
		pathBoost:        pathBoost,
	}, nil
}

// indexHasField reports whether an index was built with a field, by asking the
// mapping it carries.
func indexHasField(idx bleve.Index, field string) bool {
	im, ok := idx.Mapping().(*mapping.IndexMappingImpl)
	if !ok || im.DefaultMapping == nil {
		return false
	}
	_, ok = im.DefaultMapping.Properties[field]
	return ok
}

// warnIndexShape reports a setting that shapes the index disagreeing with the
// index on disk. It is a warning rather than an error because the disagreement
// is legitimate for one run — the setting takes effect on the next forced
// reindex — and silence is what would make it expensive.
func warnIndexShape(setting string, configured, onDisk bool, path string) {
	if configured == onDisk {
		return
	}
	slog.Warn("indexes.bm25."+setting+" does not match the index on disk; it shapes the index, so it takes effect on the next forced reindex",
		"configured", configured, "index", onDisk, "path", path)
}

// Name returns the indexer name.
func (i *Indexer) Name() string {
	return "bm25"
}

// Type returns the indexer type.
func (i *Indexer) Type() index.IndexType {
	return index.IndexTypeBM25
}

// Init initializes the indexer.
func (i *Indexer) Init(ctx context.Context, config map[string]interface{}) error {
	// Already initialized in New
	return nil
}

// chunkedFile is a file whose chunks are cut and waiting to be labelled with
// the symbol that covers them.
type chunkedFile struct {
	file   *index.FileToIndex
	chunks []*index.Chunk
}

// Index indexes a repository's files.
//
// Chunking and symbol annotation are separate passes because the AST indexer
// runs over the same window at the same time and publishes the symbols of
// every file it parses. Cutting all the chunks first gives it that head start,
// and the annotation pass then takes what has been published rather than
// parsing the same bytes again. Nothing is waited for: a file whose symbols
// have not arrived is parsed here, exactly as the whole window used to be.
func (i *Indexer) Index(ctx context.Context, req *index.IndexRequest) (*index.IndexResult, error) {
	start := time.Now()

	result := &index.IndexResult{}
	batch := i.index.NewBatch()

	chunked := make([]chunkedFile, 0, len(req.Files))
	rewritten := make([]string, 0, len(req.Files))
	for _, file := range req.Files {
		if file.Content == nil {
			content, err := readFileContent(req.RepoPath, file.Path)
			if err != nil {
				result.FilesFailed++
				result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", file.Path, err))
				continue
			}
			file.Content = content
		}

		// A file that was read is re-indexed whether or not it chunks, so what
		// it already has in the index is stale either way.
		rewritten = append(rewritten, file.Path)

		// Chunk the content
		chunks, err := i.chunker.Chunk(ctx, &index.ChunkInput{Path: file.Path, Language: file.Language, Content: file.Content})
		if err != nil {
			result.FilesFailed++
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", file.Path, err))
			continue
		}

		chunked = append(chunked, chunkedFile{file: file, chunks: chunks})
		result.FilesIndexed++
	}

	// One query for the window, so a failure belongs to no single file. Leaving
	// it unattributed is deliberate: the service marks a window it cannot pin a
	// failure on as unindexed and repeats it, which is the right answer when
	// the only casualty is a shrunken file that may still carry chunks it no
	// longer has.
	if err := i.dropStaleChunks(batch, req.RepoID, rewritten, chunked); err != nil {
		result.FilesFailed++
		result.Errors = append(result.Errors, fmt.Sprintf("stale chunk scan: %v", err))
	}

	// First sweep: the files whose symbols are already published cost nothing.
	// The rest are held back so the producer keeps running while they wait,
	// and re-checked in the second sweep before anything is parsed here.
	var unlabelled []chunkedFile
	for _, cf := range chunked {
		syms, ok := index.SharedSymbols.Take(req.RepoID, cf.file.Path, cf.file.Hash)
		if !ok {
			unlabelled = append(unlabelled, cf)
			continue
		}
		symbolsReused.Inc()
		if err := i.indexChunks(batch, req.RepoID, cf, syms); err != nil {
			return nil, err
		}
	}
	for _, cf := range unlabelled {
		syms, ok := index.SharedSymbols.Take(req.RepoID, cf.file.Path, cf.file.Hash)
		switch {
		case ok:
			symbolsReused.Inc()
		case index.SymbolsAnnotated(cf.file.Language):
			syms = symbolUnits(cf.file.Path, cf.file.Language, cf.file.Content)
			symbolsParsed.Inc()
		}
		if err := i.indexChunks(batch, req.RepoID, cf, syms); err != nil {
			return nil, err
		}
	}

	// Execute batch
	if err := i.index.Batch(batch); err != nil {
		return nil, fmt.Errorf("batch index: %w", err)
	}

	result.Duration = time.Since(start)
	return result, nil
}

// indexChunks adds one file's chunks to the batch, each labelled with the
// symbol that covers most of it.
func (i *Indexer) indexChunks(batch *bleve.Batch, repoID string, cf chunkedFile, syms []index.Symbol) error {
	for chunkIdx, chunk := range cf.chunks {
		doc := buildDocument(repoID, cf.file.Path, chunk.Text, cf.file.Language, chunk.StartLine, chunk.EndLine)
		if symbol, kind := dominantUnit(syms, chunk.StartLine, chunk.EndLine); symbol != "" {
			doc["symbol"] = symbol
			doc["kind"] = kind
		}
		if i.splitIdentifiers {
			// The same text a second time, for the analyser to take apart.
			doc[codeSplitField] = chunk.Text
		}
		if i.indexPaths {
			doc[pathTextField] = cf.file.Path
		}

		docID := chunkDocID(repoID, cf.file.Path, chunkIdx)
		if err := batch.Index(docID, doc); err != nil {
			return fmt.Errorf("index chunk %s: %w", docID, err)
		}
	}
	return nil
}

// chunkDocID is the id of one chunk of a file. The ordinal runs from zero
// without gaps, which is what lets a re-index overwrite the chunks a file keeps
// and delete only the tail a file that shrank leaves behind.
func chunkDocID(repoID, path string, ordinal int) string {
	return fmt.Sprintf("%s:%s:chunk%d", repoID, path, ordinal)
}

// AutoCompact reports whether a finished index pass should settle the layout
// on its own. indexes.bm25.no_compact turns that off — it is a statement about
// when compaction happens, not about whether this index can be compacted, so an
// explicit Compact still does the work (see app.CompactIndexes).
func (i *Indexer) AutoCompact() bool { return i.compact }

// Compact rewrites the index into a single segment, which is what makes a
// score a function of the indexed content rather than of the run that indexed
// it.
//
// Bleve scores BM25 against an average document length it derives as
// ceil(FieldCardinality(field) / DocCount()) (search/searcher/search_term.go).
// DocCount is the live document count and depends on nothing else, but
// FieldCardinality is a *sum over segments* of each segment's distinct-term
// count (index/scorch/snapshot_index.go, newIndexSnapshotFieldDict): a term
// present in eight segments is counted eight times. The value therefore says
// as much about how the documents were split into segments as about the
// corpus, and avgDocLength appears in both the IDF and the length-normalising
// denominator — so every score in the index moves with it.
//
// Segment layout is not a property of the input. Scorch persists and merges on
// its own goroutines, so two passes over the same sources with the same binary
// end in different layouts, and a merge landing while the index sits idle
// changes the scores of an index nobody wrote to: measured on this repository's
// own sources, 1067 chunks scored 0.872 with two segments and 0.757 a tenth of
// a second later with one. That is the noise floor a re-indexing A/B was
// paying, and it is why the eval harness could not tell a ranking change from
// a merge.
//
// One segment is one segment however it was reached, and its dictionary holds
// each term once, so the count becomes the corpus's true distinct-term count.
// The merge costs about 4% of the time the pass that filled the index spent,
// and returns in microseconds when the index is already compact.
func (i *Indexer) Compact(ctx context.Context) error {
	// bleve.Index is an interface over several index types; only scorch merges
	// segments, and only scorch is what buildMapping produces. An upsidedown
	// index has nothing to canonicalise, so this is not an error.
	adv, err := i.index.Advanced()
	if err != nil {
		return fmt.Errorf("advanced index: %w", err)
	}
	sc, ok := adv.(*scorch.Scorch)
	if !ok {
		return nil
	}

	// Bleve rejects a force merge while one is running, and two repositories
	// can finish a pass at the same moment. Waiting is what the caller means;
	// the second merge then finds one segment and returns immediately.
	i.compactMu.Lock()
	defer i.compactMu.Unlock()

	// An index that is already one segment has nothing to canonicalise, and
	// asking anyway is not free. Scorch's background epochs advance only when
	// its merger and persister loops run, and on an idle, freshly reopened
	// index they never do — ForceMerge and the settle wait below then both
	// stall against a cycle that is not coming. Observed as admin/compact
	// holding its caller for 27 minutes over an index compacted hours
	// earlier. The segment count answers without waiting on anything.
	if file, mem, counted := i.rootSegments(); counted && file <= 1 && mem == 0 {
		return nil
	}

	start := time.Now()
	if err := sc.ForceMerge(ctx, &mergeplan.SingleSegmentMergePlanOptions); err != nil {
		return fmt.Errorf("compact index at %s: %w", i.path, err)
	}
	err = i.awaitSteadyState(ctx)
	compactSeconds.Observe(time.Since(start).Seconds())
	return err
}

// compactSettleTimeout bounds the wait for a compaction to reach disk. It is
// generous because the write is proportional to the index, and exceeding it
// costs only reproducibility across a restart — never correctness.
const compactSettleTimeout = 10 * time.Minute

// awaitSteadyState waits for the merge to be written and recorded.
//
// ForceMerge returns once the merged segment is in the root snapshot, which is
// enough for this process: every search from here on reads the compacted
// layout. It is not enough for the next process. Persisting the new root is
// the persister's own goroutine's job, and a close before it gets there leaves
// the last recorded snapshot pointing at the segments the merge replaced — so
// reopening the index reads back the layout that was just compacted away, and
// with it the scores that layout produces.
//
// Bleve reports both halves as index_bgthreads_active, which is false once the
// merger and the persister have caught up with the current root.
func (i *Indexer) awaitSteadyState(ctx context.Context) error {
	deadline := time.NewTimer(compactSettleTimeout)
	defer deadline.Stop()

	// Short enough that a small index is not held up by the poll, and the
	// backoff keeps a large one from spinning while it writes.
	wait := time.Millisecond
	for {
		if settled, ok := i.steady(); settled || !ok {
			// A build of bleve that stops reporting the signal leaves nothing
			// to wait for; the in-memory layout is compacted either way.
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			slog.Warn("bm25 compaction did not reach disk within the timeout; the index is compacted "+
				"for this process but reopening it may read back the layout before the merge",
				"path", i.path, "timeout", compactSettleTimeout)
			compactUnsettled.Inc()
			return nil
		case <-time.After(wait):
			if wait < 100*time.Millisecond {
				wait *= 2
			}
		}
	}
}

// rootSegments reports how many file and in-memory segments the current root
// snapshot holds, and whether the running bleve build reports the numbers at
// all.
func (i *Indexer) rootSegments() (file, mem uint64, reported bool) {
	stats, ok := i.index.StatsMap()["index"].(map[string]interface{})
	if !ok {
		return 0, 0, false
	}
	file, fok := stats["TotFileSegmentsAtRoot"].(uint64)
	mem, mok := stats["TotMemorySegmentsAtRoot"].(uint64)
	return file, mem, fok && mok
}

// steady reports whether bleve's background threads have caught up with the
// current root, and whether it said at all.
func (i *Indexer) steady() (settled, reported bool) {
	stats, ok := i.index.StatsMap()["index"].(map[string]interface{})
	if !ok {
		return false, false
	}
	active, ok := stats["index_bgthreads_active"].(bool)
	return !active, ok
}

// Remove removes indexed data for files.
func (i *Indexer) Remove(ctx context.Context, repoID string, paths []string) error {
	if len(paths) == 0 {
		return i.deleteByRepoID(ctx, repoID)
	}
	ids, err := i.chunkIDs(repoID, paths)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	batch := i.index.NewBatch()
	for _, id := range ids {
		batch.Delete(id)
	}
	if err := i.index.Batch(batch); err != nil {
		return fmt.Errorf("batch delete: %w", err)
	}
	return nil
}

// deleteByRepoID deletes all documents for a repository.
func (i *Indexer) deleteByRepoID(ctx context.Context, repoID string) error {
	// Build a query to match all documents with this repo_id
	query := bleve.NewMatchQuery(repoID)
	query.SetField("repo_id")

	// Create a delete request for matching documents
	// Bleve doesn't support delete-by-query directly, so we need to:
	// 1. Search for all document IDs
	// 2. Batch delete them

	searchRequest := bleve.NewSearchRequestOptions(query, 1000, 0, false)
	searchRequest.Fields = []string{"_id"}

	for {
		searchResult, err := i.index.Search(searchRequest)
		if err != nil {
			return fmt.Errorf("search for documents: %w", err)
		}

		if len(searchResult.Hits) == 0 {
			return nil // Nothing to delete
		}

		// Batch delete all found documents
		batch := i.index.NewBatch()
		for _, hit := range searchResult.Hits {
			batch.Delete(hit.ID)
		}

		if err := i.index.Batch(batch); err != nil {
			return fmt.Errorf("batch delete: %w", err)
		}
	}
}

// ErrEmptyQuery is returned when a search carries no searchable text.
var ErrEmptyQuery = errors.New("bm25: query must not be empty")

// dirIsEmpty reports whether path is an existing directory with no entries.
// A missing directory is not empty in this sense: bleve.New creates it.
func dirIsEmpty(path string) bool {
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) == 0
}

// ErrIndexDamaged reports that a search could not be served because the
// on-disk index is unreadable. Rebuilding it (a forced reindex) is the only
// repair; the error exists so a caller can degrade instead of failing. It is
// the shared sentinel so callers outside this package can recognise it.
var ErrIndexDamaged = index.ErrIndexDamaged

// damagedSegmentMarker is how a segment whose bytes do not decode reports
// itself. zapx gives up in its varint reader and returns a plain fmt.Errorf,
// with no sentinel and no type to match on, so the text is the only hook there
// is; it is matched narrowly for that reason, and a decode failure that words
// itself differently stays an ordinary error rather than being guessed at.
const damagedSegmentMarker = "memUvarintReader overflow"

// damagedSegment reports whether err is a segment that will not decode. Such an
// error is not retryable and not the query's fault: every request touching the
// segment fails the same way until the index is rebuilt.
func damagedSegment(err error) bool {
	return err != nil && strings.Contains(err.Error(), damagedSegmentMarker)
}

// Search performs a BM25 search.
func (i *Indexer) Search(ctx context.Context, q *index.SearchQuery) (res *index.SearchResult, err error) {
	start := time.Now()

	if strings.TrimSpace(q.Query) == "" {
		return nil, ErrEmptyQuery
	}

	// A damaged segment makes bleve's postings reader index out of range rather
	// than report an overflow, and a panic here would take down the whole
	// request. Turning it into an error hands the query to the caller's existing
	// degradation path: hybrid search drops the keyword leg, says so in the
	// result metadata, and still answers from the remaining index.
	defer func() {
		if r := recover(); r != nil {
			res, err = nil, fmt.Errorf("%w: %v", ErrIndexDamaged, r)
			slog.Error("bm25 search panicked; the index segment is unreadable and needs a forced reindex",
				"query", q.Query, "panic", r)
			searchPanics.Inc()
			searchDamaged.Inc()
		}
	}()

	// A match query analyses the raw text. Bleve's query-string parser would
	// instead read reserved syntax out of ordinary code queries — "Content-Type:
	// application/json" parses as a field filter on a field named Content-Type
	// and matches nothing.
	qry := bleve.NewMatchQuery(q.Query)

	// The literal view of the query is the one above: identifiers as they are
	// written. When the index carries the code-aware field, the same words are
	// asked of it too, analysed the same way on both sides — so a question
	// spelled in words reaches an identifier that spells them without spaces.
	// The two clauses both score, so a candidate that matches an identifier
	// literally still outranks one that matches only its parts.
	views := []query.Query{qry}
	if i.splitIdentifiers {
		split := bleve.NewMatchQuery(q.Query)
		split.SetField(codeSplitField)
		views = append(views, split)
	}
	if i.indexPaths {
		// Where a document lives, at a fraction of the weight of what it says.
		path := bleve.NewMatchQuery(q.Query)
		path.SetField(pathTextField)
		path.SetBoost(i.pathBoost)
		views = append(views, path)
	}

	text := query.Query(qry)
	if len(views) > 1 {
		text = bleve.NewDisjunctionQuery(views...)
	}

	clauses := []query.Query{text}

	if len(q.Repos) > 0 {
		repoQueries := make([]query.Query, len(q.Repos))
		for idx, r := range q.Repos {
			tq := bleve.NewTermQuery(r)
			tq.SetField("repo_id")
			repoQueries[idx] = tq
		}
		clauses = append(clauses, bleve.NewDisjunctionQuery(repoQueries...)) // repo_id == any of q.Repos
	}

	filters := index.ParseFilters(q.Filter)
	if fq := fieldFilter("language", filters.Languages); fq != nil {
		clauses = append(clauses, fq)
	}
	if fq := fieldFilter("kind", filters.Kinds); fq != nil {
		clauses = append(clauses, fq)
	}
	if filters.PathPrefix != "" {
		pq := bleve.NewPrefixQuery(filters.PathPrefix)
		pq.SetField("file_path")
		clauses = append(clauses, pq)
	}

	searchQuery := text
	if len(clauses) > 1 {
		searchQuery = bleve.NewConjunctionQuery(clauses...)
	}

	// Build search request
	searchRequest := bleve.NewSearchRequestOptions(searchQuery, q.Limit, 0, false)
	searchRequest.Fields = []string{"repo_id", "file_path", "content", "language", "kind", "symbol", "start_line", "end_line"}

	// Execute search
	searchResult, err := i.index.Search(searchRequest)
	if err != nil {
		// A damaged segment reports itself two ways: the postings reader
		// panics, which the deferred recover above turns into ErrIndexDamaged,
		// or it returns the decode failure as an ordinary error, which lands
		// here. Both mean the same thing and both need the same handling, so
		// this path is classified rather than left to propagate as a generic
		// failure the caller cannot tell apart from a bad query.
		if damagedSegment(err) {
			slog.Error("bm25 search hit an unreadable index segment; it needs a forced reindex",
				"query", q.Query, "error", err)
			searchDamaged.Inc()
			return nil, fmt.Errorf("%w: %v", ErrIndexDamaged, err)
		}
		return nil, fmt.Errorf("search: %w", err)
	}

	// Convert to hits
	hits := make([]*index.Hit, 0, len(searchResult.Hits))
	for _, hit := range searchResult.Hits {
		doc := hit.Fields

		hits = append(hits, &index.Hit{
			RepoID:   getString(doc, "repo_id"),
			FilePath: getString(doc, "file_path"),
			Path:     getString(doc, "file_path"),
			Line:     getInt(doc, "start_line"),
			EndLine:  getInt(doc, "end_line"),
			Symbol:   getString(doc, "symbol"),
			Kind:     getString(doc, "kind"),
			Language: getString(doc, "language"),
			Score:    float32(hit.Score),
			Snippet:  getString(doc, "content"),
			Reason:   "keyword",
		})
	}

	return &index.SearchResult{
		Hits:     hits,
		Total:    int(searchResult.Total),
		Query:    q.Query,
		Duration: time.Since(start),
	}, nil
}

// fieldFilter builds "field matches any of values"; nil when nothing is set.
// Match (not term) queries are used because these fields are analysed.
func fieldFilter(field string, values []string) query.Query {
	if len(values) == 0 {
		return nil
	}
	qs := make([]query.Query, len(values))
	for i, v := range values {
		mq := bleve.NewMatchQuery(v)
		mq.SetField(field)
		qs[i] = mq
	}
	if len(qs) == 1 {
		return qs[0]
	}
	return bleve.NewDisjunctionQuery(qs...)
}

// Close closes the indexer. Calling it more than once is safe; every call
// returns what the first one returned.
//
// The guard is not politeness. Bleve closes its scorch index by closing an
// unguarded channel (index/scorch/scorch.go, v2.6.0), so a second delegated
// close panics with "close of closed channel" instead of returning an error —
// and a panic in a shutdown path takes the process down at the one moment it
// is trying to exit cleanly. Any shutdown that can run twice, such as an
// explicit close plus a deferred one on an error path, would hit it.
func (i *Indexer) Close() error {
	i.closeOnce.Do(func() { i.closeErr = i.index.Close() })
	return i.closeErr
}

// Stats returns indexer statistics.
func (i *Indexer) Stats(ctx context.Context) (*index.IndexerStats, error) {
	// Get document count from Bleve
	docCount, err := i.index.DocCount()
	if err != nil {
		return nil, fmt.Errorf("get doc count: %w", err)
	}

	return &index.IndexerStats{
		Documents: int64(docCount),
		Specific: map[string]interface{}{
			"path": i.path,
			"type": "bm25",
		},
	}, nil
}

// codeSplitField is a second, code-aware view of a chunk's text: the same
// content analysed by codeAnalyzer. It is indexed but not stored — a snippet
// still comes from "content" — and deliberately kept out of "_all", so that it
// enters a query as one clause with one weight rather than as a silent
// addition to every other field's.
const codeSplitField = "content_split"

// pathTextField makes a document's own path findable as words. "file_path" is
// analysed as a keyword — one indivisible term, which is what a per-file
// delete needs — and "content" holds no path at all, so "checkout service"
// could never reach src/checkoutservice/main.go through the keyword leg.
//
// The vector side already learned this: one line of path at the head of a
// symbol card bought recall@1 0.339 → 0.424 and span@10 0.695 → 0.763 with no
// reranker at all — more span@10 than the reranker itself buys. This is the
// same fact offered to the other channel.
//
// The weight is why it is a separate clause rather than more text. The path was
// measured on the vector side as one line among a card's forty; as a field of
// its own it is four tokens, and BM25's length normalisation lets a directory
// name outscore a body that answers the question. Observed at weight 1.0 on
// this repository: "rerank max doc bytes" lost the chunk that defines
// rerankMaxDocBytes to two files merely named *rerank_test.go. Hence
// defaultPathBoost below, and indexes.bm25.path_boost to move it — unlike the
// fields, a weight is query-time, so sweeping it needs no reindex.
const (
	pathTextField    = "path_text"
	defaultPathBoost = 0.3
)

// codeAnalyzer and codeDelimiters name the analysis chain that splits
// identifiers into the words they are made of.
//
// The default analyser does neither half of that. Bleve's unicode tokenizer
// follows UAX#29, where "_" *joins* words rather than separating them, and
// nothing splits camelCase at all, so
//
//	func getUserByID(loginAttempt *LoginAttempt)
//
// indexes as `func getuserbyid loginattempt loginattempt` — "get user by id"
// matches none of it, and neither does "login attempt". The keyword leg is an
// exact-identifier matcher wearing a full-text interface, which is why a
// question phrased in words has only ever reached the vector leg.
//
// Three details of the chain are load-bearing. The char filter replaces one
// delimiter with one space, so every token keeps the offsets of the source it
// came from. camelCase runs before to_lower because it needs the case it is
// named for. And there is no stop-word filter on purpose: "is", "by", "to"
// and "all" are load-bearing parts of identifiers (isValid, byID, toString,
// getAll), while idf already discounts what is common.
const (
	codeAnalyzer   = "code"
	codeDelimiters = "code_delimiters"
)

// buildMapping creates the Bleve index mapping. splitIdentifiers and
// indexPaths add the two code-aware fields; they are properties of the index
// rather than of a query, so they are fixed when the index is created.
func buildMapping(splitIdentifiers, indexPaths bool) (*mapping.IndexMappingImpl, error) {
	// Create document mapping
	docMapping := bleve.NewDocumentMapping()

	// Text field (full-text search)
	textFieldMapping := bleve.NewTextFieldMapping()
	textFieldMapping.Store = true
	textFieldMapping.Index = true
	textFieldMapping.IncludeTermVectors = true
	docMapping.AddFieldMappingsAt("content", textFieldMapping)

	// Exact-match fields (whole value as a single term) — needed for reliable per-file delete.
	keywordFieldMapping := bleve.NewTextFieldMapping()
	keywordFieldMapping.Store = true
	keywordFieldMapping.Index = true
	keywordFieldMapping.IncludeTermVectors = false
	keywordFieldMapping.Analyzer = keyword.Name
	docMapping.AddFieldMappingsAt("repo_id", keywordFieldMapping)
	docMapping.AddFieldMappingsAt("file_path", keywordFieldMapping)

	// Tokenized metadata fields.
	for _, field := range []string{"language", "kind", "symbol"} {
		fieldMapping := bleve.NewTextFieldMapping()
		fieldMapping.Store = true
		fieldMapping.Index = true
		fieldMapping.IncludeTermVectors = false
		docMapping.AddFieldMappingsAt(field, fieldMapping)
	}

	// Numeric fields (stored so we can read line numbers back on search)
	numMapping := bleve.NewNumericFieldMapping()
	numMapping.Store = true
	numMapping.Index = true
	for _, field := range []string{"start_line", "end_line"} {
		docMapping.AddFieldMappingsAt(field, numMapping)
	}

	// Create index mapping
	indexMapping := bleve.NewIndexMapping()

	if splitIdentifiers || indexPaths {
		if err := indexMapping.AddCustomCharFilter(codeDelimiters, map[string]interface{}{
			"type":    regexpchar.Name,
			"regexp":  `[_./\-]`,
			"replace": " ",
		}); err != nil {
			return nil, fmt.Errorf("define %s char filter: %w", codeDelimiters, err)
		}
		if err := indexMapping.AddCustomAnalyzer(codeAnalyzer, map[string]interface{}{
			"type":          custom.Name,
			"char_filters":  []string{codeDelimiters},
			"tokenizer":     unicode.Name,
			"token_filters": []string{camelcase.Name, lowercase.Name},
		}); err != nil {
			return nil, fmt.Errorf("define %s analyzer: %w", codeAnalyzer, err)
		}
	}

	if indexPaths {
		// The same analyser: a path is written the way identifiers are, and
		// src/checkoutservice/main.go has to reach a question that says
		// "checkout service".
		pathFieldMapping := bleve.NewTextFieldMapping()
		pathFieldMapping.Analyzer = codeAnalyzer
		pathFieldMapping.Index = true
		pathFieldMapping.Store = false
		pathFieldMapping.IncludeTermVectors = false
		pathFieldMapping.IncludeInAll = false
		docMapping.AddFieldMappingsAt(pathTextField, pathFieldMapping)
	}

	if splitIdentifiers {
		splitFieldMapping := bleve.NewTextFieldMapping()
		splitFieldMapping.Analyzer = codeAnalyzer
		splitFieldMapping.Index = true
		splitFieldMapping.Store = false
		splitFieldMapping.IncludeTermVectors = false
		splitFieldMapping.IncludeInAll = false
		docMapping.AddFieldMappingsAt(codeSplitField, splitFieldMapping)
	}

	indexMapping.AddDocumentMapping("doc", docMapping)
	indexMapping.DefaultMapping = docMapping
	// Bleve scores with tf-idf unless told otherwise; without this the
	// configured k1/b would have nothing to tune.
	indexMapping.ScoringModel = bleveIndexAPI.BM25Scoring

	return indexMapping, nil
}

// buildDocument creates a Bleve document from file content.
func buildDocument(repoID, path, content, language string, startLine, endLine int) map[string]interface{} {
	doc := map[string]interface{}{
		"repo_id":    repoID,
		"file_path":  path,
		"content":    content,
		"language":   language,
		"type":       "chunk",
		"start_line": startLine,
		"end_line":   endLine,
	}

	return doc
}

// symbolUnits parses a file's symbols so window chunks can carry the one they
// mostly cover. It is the fallback for a file whose symbols no other indexer
// published; returns nil when the language is unsupported or parsing fails —
// annotation is best-effort, indexing must not depend on it.
func symbolUnits(path, language string, content []byte) []index.Symbol {
	if !index.SymbolsAnnotated(language) {
		return nil
	}
	units, _, err := ast.GetParserForLanguage(language).Parse(path, string(content))
	if err != nil {
		return nil
	}
	return index.ProjectSymbols(units)
}

// dominantUnit returns the name and kind of the symbol covering most of the
// chunk. Ties are broken towards the smaller (more specific) symbol and then by
// name, so the annotation is stable across runs.
func dominantUnit(syms []index.Symbol, startLine, endLine int) (string, string) {
	if endLine < startLine {
		endLine = startLine
	}

	var best *index.Symbol
	bestOverlap, bestSize := 0, 0
	for idx := range syms {
		u := &syms[idx]
		if u.Name == "" {
			continue
		}
		overlap := min(endLine, u.EndLine) - max(startLine, u.StartLine) + 1
		if overlap <= 0 {
			continue
		}
		size := u.EndLine - u.StartLine + 1
		switch {
		case best == nil, overlap > bestOverlap,
			overlap == bestOverlap && size < bestSize,
			overlap == bestOverlap && size == bestSize && u.Name < best.Name:
			best, bestOverlap, bestSize = u, overlap, size
		}
	}
	if best == nil {
		return "", ""
	}
	name := best.Qualified
	if name == "" {
		name = best.Name
	}
	return name, best.Kind
}

// Helper functions

func readFileContent(repoPath, filePath string) ([]byte, error) {
	fullPath := filepath.Join(repoPath, filePath)
	return os.ReadFile(fullPath)
}

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func getInt(m map[string]interface{}, key string) int {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		}
	}
	return 0
}

// chunkScanPage is how many chunk ids one page of a scan carries. The scan
// sorts by document id, a total order, which is what makes paging by offset
// neither miss nor repeat an id.
var chunkScanPage = 4096

// chunkScanPaths bounds how many paths one scan query names, so a caller
// passing an unbounded list of files still produces bounded queries.
const chunkScanPaths = 512

// chunkIDs returns the ids of every chunk the given files currently have in
// the index.
//
// One query answers for the whole set. The per-file query it replaces cost a
// search for each file of a 512-file window, every one of them read against
// the index that same window was about to be written into; each also fetched
// the stored document of every hit — the chunk's whole text — only to read
// back an id the hit already carried.
func (i *Indexer) chunkIDs(repoID string, paths []string) ([]string, error) {
	var ids []string
	for start := 0; start < len(paths); start += chunkScanPaths {
		group := paths[start:min(start+chunkScanPaths, len(paths))]

		pathQueries := make([]query.Query, len(group))
		for n, path := range group {
			tq := bleve.NewTermQuery(path)
			tq.SetField("file_path")
			pathQueries[n] = tq
		}
		repoQuery := bleve.NewTermQuery(repoID)
		repoQuery.SetField("repo_id")
		scan := bleve.NewConjunctionQuery(repoQuery, bleve.NewDisjunctionQuery(pathQueries...))

		for from := 0; ; {
			req := bleve.NewSearchRequestOptions(scan, chunkScanPage, from, false)
			req.SortBy([]string{"_id"})
			res, err := i.index.Search(req)
			if err != nil {
				return nil, fmt.Errorf("search file chunks: %w", err)
			}
			for _, hit := range res.Hits {
				ids = append(ids, hit.ID)
			}
			if len(res.Hits) < chunkScanPage {
				break
			}
			from += len(res.Hits)
		}
	}
	return ids, nil
}

// dropStaleChunks adds to the batch a delete for every chunk the rewritten
// files have in the index and the batch will not write again.
//
// The deletes ride in the batch that carries the new documents rather than in
// batches of their own: the two id sets are disjoint by construction, so the
// batch's one-entry-per-id map cannot drop either operation, and the window's
// reads then all happen before its single write.
func (i *Indexer) dropStaleChunks(batch *bleve.Batch, repoID string, rewritten []string, chunked []chunkedFile) error {
	if len(rewritten) == 0 {
		return nil
	}
	indexed, err := i.chunkIDs(repoID, rewritten)
	if err != nil {
		return err
	}
	if len(indexed) == 0 {
		return nil
	}

	keep := make(map[string]struct{}, len(indexed))
	for _, cf := range chunked {
		for ordinal := range cf.chunks {
			keep[chunkDocID(repoID, cf.file.Path, ordinal)] = struct{}{}
		}
	}
	for _, id := range indexed {
		if _, ok := keep[id]; !ok {
			batch.Delete(id)
		}
	}
	return nil
}
