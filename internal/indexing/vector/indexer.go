package vector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Nahua-Foundation/ragota/internal/indexing"
	"github.com/Nahua-Foundation/ragota/internal/indexing/ast"
	"github.com/Nahua-Foundation/ragota/internal/llm"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// Compile-time interface assertions.
var _ indexing.Indexer = (*Indexer)(nil)
var _ indexing.Searcher = (*Indexer)(nil)

// Indexer implements indexing.Indexer and indexing.Searcher for vector search.
type Indexer struct {
	embedder llm.Embedder
	vecStore storage.VectorStorage
	chunkCfg indexing.ChunkConfig
	cards    bool
	maxChars int
	workers  int
	excludes []string
}

// defaultEmbedWorkers is how many embed requests are in flight at once, and
// packTargetTexts is the batch the workers pack small files up to. The
// average source file chunks to a handful of texts; sent alone, per file,
// those starve a GPU endpoint — the request round-trip and the tiny batch
// both waste it. Packing neighbours into one request and keeping a second
// request in flight while the first is served keeps the endpoint busy.
const (
	defaultEmbedWorkers = 2
	packTargetTexts     = 64
)

// storeWorkers is how many files are written to the vector store at once, and
// storeQueue how many wait for a writer.
//
// The writers are a stage of their own rather than the tail of each embed
// worker. An embed worker that writes is an embed worker that is not
// embedding, and with two of them the endpoint went idle whenever both were
// talking to the store — measured on an Elasticsearch pass, the workers were
// in the embed call only 1.27 of 2 workers' time. The endpoint serializes
// requests behind one accelerator, so the only thing the client controls is
// whether its queue is ever empty; nothing else in this file matters as much.
//
// The queue is short on purpose. It exists to absorb the jitter of a store
// round trip, not to buffer work: a waiting job holds its file's chunk texts
// and its vectors, and a file of a hundred thousand lines is megabytes of
// both. Four writers clear a file in a couple of round trips while embedding
// one takes an order of magnitude longer, so the queue is never the thing that
// fills.
const (
	storeWorkers = 4
	storeQueue   = 16
)

// defaultEmbedMaxChars bounds the text sent to the embedder per chunk.
// Line-window chunking bounds lines, not bytes: sixty minified lines can be
// megabytes, every embedding model has a context limit, and a strict server
// (llama.cpp) rejects the entire batch over one oversized input, failing the
// file. The stored chunk keeps its full text — only the embedder input is
// truncated.
//
// The budget is in bytes but servers meter tokens, and density is script-
// dependent: code and English run ~4 bytes/token, but Arabic or CJK text
// (localization files) reaches ~2 — an 8 KB Arabic chunk is ~2.5k tokens.
// 4096 bytes stays under a 2048-token context for every script; raise
// embedder.max_chars when the serving context is known to be larger.
const defaultEmbedMaxChars = 4096

// Config is the vector indexer configuration.
type Config struct {
	Embedder llm.Embedder
	Storage  storage.VectorStorage
	Chunking indexing.ChunkConfig
	// MaxChars caps the embedder input per chunk (0 = default 4096); see
	// defaultEmbedMaxChars.
	MaxChars int
	// Concurrency is the number of in-flight embed requests (0 = default 2).
	Concurrency int
	// Exclude keeps files whose repo-relative path contains one of these
	// case-insensitive substrings out of the vector channel.
	Exclude []string
	// Cards enables symbol-card documents ("semantic v2"): for code files,
	// one document per AST unit (kind + qualified name + signature + doc +
	// body head) replaces window chunks. Files without units fall back to
	// window chunking.
	Cards bool
}

// New creates a new vector indexer.
func New(cfg *Config) *Indexer {
	excludes := make([]string, 0, len(cfg.Exclude))
	for _, e := range cfg.Exclude {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			excludes = append(excludes, e)
		}
	}
	return &Indexer{
		embedder: cfg.Embedder,
		vecStore: cfg.Storage,
		chunkCfg: cfg.Chunking,
		cards:    cfg.Cards,
		maxChars: cfg.MaxChars,
		workers:  cfg.Concurrency,
		excludes: excludes,
	}
}

// embedWorkers returns the configured embed-request concurrency.
func (i *Indexer) embedWorkers() int {
	if i.workers > 0 {
		return i.workers
	}
	return defaultEmbedWorkers
}

// excluded reports whether the path is kept out of the vector channel by
// indexes.vector.exclude. Matching is a case-insensitive substring over the
// slash-normalized repo-relative path.
func (i *Indexer) excluded(path string) bool {
	if len(i.excludes) == 0 {
		return false
	}
	p := "/" + strings.ToLower(filepath.ToSlash(path))
	for _, e := range i.excludes {
		if strings.Contains(p, e) {
			return true
		}
	}
	return false
}

// embedMaxChars returns the per-chunk embedder input budget.
func (i *Indexer) embedMaxChars() int {
	if i.maxChars > 0 {
		return i.maxChars
	}
	return defaultEmbedMaxChars
}

// truncateForEmbed cuts text to at most max bytes on a rune boundary.
func truncateForEmbed(text string, max int) string {
	if len(text) <= max {
		return text
	}
	for max > 0 && !utf8.RuneStart(text[max]) {
		max--
	}
	return text[:max]
}

// embedFileChunks embeds one file's chunks, halving the byte budget and
// retrying when the embedder rejects the input. Bytes cannot guarantee
// tokens: servers meter tokens and density is content-dependent — code runs
// ~4 bytes/token, Arabic ~2, and a JSON of floats under 2 — so any fixed
// byte cap has a file that still exceeds the serving context. Two halvings
// bring the worst real content under a 2048-token context; a failure after
// that fails this file only, never the batch of files around it.
func (i *Indexer) embedFileChunks(ctx context.Context, chunks []*indexing.Chunk) ([][]float32, error) {
	max := i.embedMaxChars()
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		texts := make([]string, len(chunks))
		for j, chunk := range chunks {
			texts[j] = truncateForEmbed(chunk.Text, max)
		}
		embeddings, err := i.embedder.Embed(ctx, texts)
		if err == nil {
			return embeddings, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, err
		}
		max /= 2
	}
	return nil, lastErr
}

// Name returns the indexer name.
func (i *Indexer) Name() string {
	return "vector"
}

// Type returns the indexer type.
func (i *Indexer) Type() indexing.IndexType {
	return indexing.IndexTypeVector
}

// Init initializes the indexer.
func (i *Indexer) Init(ctx context.Context, config map[string]interface{}) error {
	return nil
}

// fileJob is one chunked file waiting to be embedded and stored.
type fileJob struct {
	file     *indexing.FileToIndex
	language string
	chunks   []*indexing.Chunk
}

// storeJob is one embedded file waiting to be written to the vector store.
type storeJob struct {
	job        *fileJob
	embeddings [][]float32
}

// Index indexes a repository's files.
//
// The work is a three-stage pipeline: this goroutine reads and chunks files in
// order, embedWorkers() workers pack the chunked files into full embed
// requests, and storeWorkers writers put the results into the vector store.
// Embedding dominates the cost and runs on a remote accelerator, so the client
// must neither send tiny per-file requests (see packTargetTexts) nor sit idle
// between them — which is why writing is a stage and not the tail of the embed
// worker that produced the vectors (see storeWorkers).
func (i *Indexer) Index(ctx context.Context, req *indexing.IndexRequest) (*indexing.IndexResult, error) {
	start := time.Now()

	result := &indexing.IndexResult{}
	var mu sync.Mutex // guards result across workers

	fail := func(path, stage string, err error) {
		mu.Lock()
		result.FilesFailed++
		result.Errors = append(result.Errors, fmt.Sprintf("%s: %s: %v", path, stage, err))
		mu.Unlock()
	}

	stores := make(chan *storeJob, storeQueue)
	var storeWg sync.WaitGroup
	for w := 0; w < storeWorkers; w++ {
		storeWg.Add(1)
		go func() {
			defer storeWg.Done()
			for s := range stores {
				i.storeFile(ctx, req, s.job, s.embeddings, result, &mu, fail)
			}
		}()
	}

	jobs := make(chan *fileJob, packTargetTexts)
	var wg sync.WaitGroup
	for w := 0; w < i.embedWorkers(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				group := []*fileJob{job}
				total := len(job.chunks)
				// Pack whatever is already waiting, without blocking on the
				// producer: a full batch is better, an idle worker is worse.
				for total < packTargetTexts {
					select {
					case next, ok := <-jobs:
						if !ok {
							total = packTargetTexts
							continue
						}
						group = append(group, next)
						total += len(next.chunks)
					default:
						total = packTargetTexts
					}
				}
				i.embedGroup(ctx, group, stores, fail)
			}
		}()
	}

	byLanguage := groupByLanguage(req.Files)
	for language, files := range byLanguage {
		if !shouldVectorize(language) {
			mu.Lock()
			result.FilesSkipped += len(files)
			mu.Unlock()
			continue
		}

		for _, file := range files {
			if ctx.Err() != nil {
				break
			}
			if i.excluded(file.Path) {
				mu.Lock()
				result.FilesSkipped++
				mu.Unlock()
				continue
			}
			if file.Content == nil {
				content, err := readFileContent(req.RepoPath, file.Path)
				if err != nil {
					fail(file.Path, "read", err)
					continue
				}
				file.Content = content
			}

			var chunks []*indexing.Chunk
			if i.cards {
				chunks = buildCards(file.Path, language, file.Content, req.Annotations)
			}
			if len(chunks) == 0 {
				// Window/semantic chunking; also the fallback for files
				// without units (or non-code files) in cards mode.
				var err error
				chunks, err = indexing.ForFile(i.chunkCfg, language).Chunk(ctx, &indexing.ChunkInput{
					Path:     file.Path,
					Language: language,
					Content:  file.Content,
				})
				if err != nil {
					fail(file.Path, "chunk", err)
					continue
				}
			}

			if len(chunks) == 0 {
				mu.Lock()
				result.FilesSkipped++
				mu.Unlock()
				continue
			}

			jobs <- &fileJob{file: file, language: language, chunks: chunks}
		}
	}
	close(jobs)
	wg.Wait()
	close(stores)
	storeWg.Wait()

	result.Duration = time.Since(start)
	// A cancelled run breaks out of the file loop above with files neither
	// indexed nor failed; report the cancellation so the caller does not read a
	// partial pass as a complete one.
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return result, nil
}

// embedGroup embeds a packed group of files in one request and hands each
// file's vectors to the store stage. When the grouped request fails, every
// file retries alone through the halving path, so an oversized or rejected
// input still fails only its own file and attribution stays per file.
func (i *Indexer) embedGroup(ctx context.Context, group []*fileJob,
	stores chan<- *storeJob, fail func(string, string, error)) {

	max := i.embedMaxChars()
	var texts []string
	for _, job := range group {
		for _, chunk := range job.chunks {
			texts = append(texts, truncateForEmbed(chunk.Text, max))
		}
	}

	embeddings, err := i.embedder.Embed(ctx, texts)
	if err == nil && len(embeddings) != len(texts) {
		err = fmt.Errorf("got %d embeddings for %d texts", len(embeddings), len(texts))
	}
	if err != nil {
		if ctx.Err() != nil {
			for _, job := range group {
				fail(job.file.Path, "embed", ctx.Err())
			}
			return
		}
		for _, job := range group {
			solo, serr := i.embedFileChunks(ctx, job.chunks)
			if serr != nil {
				fail(job.file.Path, "embed", serr)
				continue
			}
			stores <- &storeJob{job: job, embeddings: solo}
		}
		return
	}

	off := 0
	for _, job := range group {
		stores <- &storeJob{job: job, embeddings: embeddings[off : off+len(job.chunks)]}
		off += len(job.chunks)
	}
}

// storeFile turns one file's chunks and embeddings into points and replaces
// the file's points in the vector store.
func (i *Indexer) storeFile(ctx context.Context, req *indexing.IndexRequest, job *fileJob,
	embeddings [][]float32, result *indexing.IndexResult, mu *sync.Mutex, fail func(string, string, error)) {

	file, language, chunks := job.file, job.language, job.chunks
	if len(embeddings) != len(chunks) {
		fail(file.Path, "embed", fmt.Errorf("embedding count mismatch"))
		return
	}

	points := make([]*storage.VectorPoint, len(chunks))
	for j, chunk := range chunks {
		points[j] = &storage.VectorPoint{
			ID:        generateChunkID(req.RepoID, file.Path, chunk.StartLine, chunk.EndLine),
			Vector:    embeddings[j],
			RepoID:    req.RepoID,
			FilePath:  file.Path,
			Language:  language,
			StartLine: chunk.StartLine,
			EndLine:   chunk.EndLine,
			Kind:      chunk.SymbolKind,
			Symbol:    chunk.SymbolName,
			Text:      chunk.Text,
			Metadata: map[string]string{
				"file":       file.Path,
				"language":   language,
				"chunk_type": chunk.SymbolKind,
			},
		}
		if chunk.SymbolKind != "" {
			points[j].Metadata["kind"] = chunk.SymbolKind
		}
		if chunk.SymbolName != "" {
			points[j].Metadata["symbol"] = chunk.SymbolName
		}
	}

	// Point IDs embed the chunk's line range, so an edit that shifts lines
	// produces new IDs and the old points would survive forever. Drop the
	// file's points first (mirrors the BM25 indexer).
	if err := i.vecStore.Delete(ctx, req.RepoID, file.Path); err != nil {
		fail(file.Path, "delete old points", err)
		return
	}
	if err := i.vecStore.Upsert(ctx, points); err != nil {
		fail(file.Path, "upsert", err)
		return
	}
	mu.Lock()
	result.FilesIndexed++
	mu.Unlock()
}

// Remove removes indexed data for files.
func (i *Indexer) Remove(ctx context.Context, repoID string, paths []string) error {
	if len(paths) == 0 {
		return i.vecStore.Delete(ctx, repoID, "")
	}
	var errs []error
	for _, p := range paths {
		if err := i.vecStore.Delete(ctx, repoID, p); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", p, err))
		}
	}
	return errors.Join(errs...)
}

// Search performs a vector search.
func (i *Indexer) Search(ctx context.Context, q *indexing.SearchQuery) (*indexing.SearchResult, error) {
	if len(q.Vector) > 0 {
		return i.searchWithVector(ctx, q, q.Vector)
	}

	embeddings, err := i.embedder.Embed(ctx, []string{truncateForEmbed(q.Query, i.embedMaxChars())})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}

	if len(embeddings) == 0 {
		return nil, fmt.Errorf("no embeddings returned")
	}

	return i.searchWithVector(ctx, q, embeddings[0])
}

// overFetchFactor is how many extra candidates are requested from the store
// when a filter has to be applied to the returned points: the store may not
// enforce every filter itself, and post-filtering a page of exactly Limit
// results would silently shrink the result set.
const overFetchFactor = 5

// maxOverFetch caps the over-fetch so a large limit cannot turn into a huge
// store query.
const maxOverFetch = 500

// searchWithVector performs search with pre-computed vector.
func (i *Indexer) searchWithVector(ctx context.Context, q *indexing.SearchQuery, vector []float32) (*indexing.SearchResult, error) {
	start := time.Now()

	filters := indexing.ParseFilters(q.Filter)

	opts := storage.VectorSearchOpts{
		Query:     vector,
		Repos:     q.Repos, // empty → search all repos
		Languages: filters.Languages,
		Limit:     q.Limit,
		Filter:    make(map[string]string),
	}
	// Stores match Filter entries exactly, so only single-valued constraints
	// can be pushed down; the rest is enforced below.
	if len(filters.Languages) == 1 {
		opts.Filter["language"] = filters.Languages[0]
	}
	if len(filters.Kinds) == 1 {
		opts.Filter["kind"] = filters.Kinds[0]
	}
	if !filters.Empty() && q.Limit > 0 {
		opts.Limit = min(q.Limit*overFetchFactor, maxOverFetch)
	}

	results, err := i.vecStore.Search(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("vector search: %w", err)
	}

	hits := make([]*indexing.Hit, 0, len(results))
	for _, r := range results {
		hit := &indexing.Hit{
			RepoID:   r.RepoID,
			FilePath: r.FilePath,
			Path:     r.FilePath,
			Line:     r.Line,
			EndLine:  r.EndLine,
			Score:    r.Score,
			Snippet:  r.Text,
			Reason:   "semantic",
		}

		if kind, ok := r.Metadata["kind"]; ok {
			hit.Kind = kind
		}
		if symbol, ok := r.Metadata["symbol"]; ok {
			hit.Symbol = symbol
		}
		if language, ok := r.Metadata["language"]; ok {
			hit.Language = language
		}

		if !filters.Match(hit.Language, hit.Kind, hit.FilePath) {
			continue
		}
		hits = append(hits, hit)
		if q.Limit > 0 && len(hits) >= q.Limit {
			break
		}
	}

	return &indexing.SearchResult{
		Hits:     hits,
		Total:    len(hits),
		Query:    q.Query,
		Duration: time.Since(start),
	}, nil
}

// Close closes the indexer.
func (i *Indexer) Close() error {
	return nil
}

// Stats returns indexer statistics.
func (i *Indexer) Stats(ctx context.Context) (*indexing.IndexerStats, error) {
	stats, err := i.vecStore.Stats(ctx)
	if err != nil {
		return nil, fmt.Errorf("get storage stats: %w", err)
	}

	return &indexing.IndexerStats{
		Documents: stats.Documents,
		SizeBytes: stats.SizeBytes,
		Repos:     stats.Repos,
		Specific: map[string]interface{}{
			"type":       "vector",
			"collection": stats.Collection,
		},
	}, nil
}

// Helper functions

func groupByLanguage(files []*indexing.FileToIndex) map[string][]*indexing.FileToIndex {
	result := make(map[string][]*indexing.FileToIndex)
	for _, f := range files {
		result[f.Language] = append(result[f.Language], f)
	}
	return result
}

func shouldVectorize(language string) bool {
	vectorizable := map[string]bool{
		"go":         true,
		"typescript": true,
		"javascript": true,
		"python":     true,
		"java":       true,
		"markdown":   true,
		"rst":        true,
		"text":       true,
		"proto":      true,
		"json":       true,
		"yaml":       true,
		"toml":       true,
	}
	return vectorizable[language]
}

func readFileContent(repoPath, filePath string) ([]byte, error) {
	fullPath := filepath.Join(repoPath, filePath)
	return os.ReadFile(fullPath)
}

// cardLanguages are the code languages symbol cards are built for; everything
// else keeps window chunking even in cards mode.
var cardLanguages = map[string]bool{
	"go":         true,
	"java":       true,
	"kotlin":     true,
	"csharp":     true,
	"typescript": true,
	"javascript": true,
	"python":     true,
	"proto":      true,
}

// cardBodyLines caps how many body lines of a unit go into its card.
const cardBodyLines = 40

// buildCards builds one embedding document per AST unit of a code file:
// "<path>\n<kind> <qualified>\n<signature>\n<doc>\n" plus the first lines of
// the unit body. Returns nil when the language is not a code language, parsing
// fails or the file has no units — callers then fall back to window chunking.
func buildCards(path, language string, content []byte, annotations map[string]string) []*indexing.Chunk {
	if !cardLanguages[language] {
		return nil
	}
	units, _, err := ast.GetParserForLanguage(language).Parse(path, string(content))
	if err != nil || len(units) == 0 {
		return nil
	}

	lines := strings.Split(string(content), "\n")
	chunks := make([]*indexing.Chunk, 0, len(units))
	for _, u := range units {
		var b strings.Builder
		// Where the symbol lives, before what it is called. A qualified name is
		// only as informative as the language makes it: Go names every service's
		// entry point "main.main", and for src/checkoutservice/main.go this line
		// is the only place the word "checkout" appears in the card at all.
		// Measured on the 59-question subset, adding it moves MRR 0.506 -> 0.561
		// and span@10 0.695 -> 0.763 with no reranker (see tools/eval/README.md).
		b.WriteString(path)
		b.WriteString("\n")
		b.WriteString(u.Kind)
		b.WriteString(" ")
		if u.Qualified != "" {
			b.WriteString(u.Qualified)
		} else {
			b.WriteString(u.Name)
		}
		b.WriteString("\n")
		if u.Signature != "" {
			b.WriteString(u.Signature)
			b.WriteString("\n")
		}
		if u.Doc != "" {
			b.WriteString(u.Doc)
			b.WriteString("\n")
		}
		// An annotation is a sentence about this symbol that the file does not
		// contain — what it does, in the words someone would ask the question
		// in. It goes where a doc comment would, because that is what it is:
		// the documentation the code never wrote down.
		if note := annotations[indexing.AnnotationKey(path, u.StartLine)]; note != "" {
			b.WriteString(note)
			b.WriteString("\n")
		}

		// First lines of the unit body (1-based line numbers).
		start := u.StartLine
		if start < 1 {
			start = 1
		}
		end := u.EndLine
		if end > len(lines) {
			end = len(lines)
		}
		if end >= start+cardBodyLines {
			end = start + cardBodyLines - 1
		}
		if start <= len(lines) && start <= end {
			b.WriteString(strings.Join(lines[start-1:end], "\n"))
		}

		chunks = append(chunks, &indexing.Chunk{
			FilePath:   path,
			Language:   language,
			StartLine:  u.StartLine,
			EndLine:    u.EndLine,
			Text:       b.String(),
			SymbolName: u.Name,
			SymbolKind: u.Kind,
		})
	}
	return chunks
}

// generateChunkID derives a stable point ID from the chunk's identity. It is
// formatted as a UUID because vector stores accept only an unsigned integer or
// a UUID as a point ID — a bare hex digest is rejected.
func generateChunkID(repoID, filePath string, startLine, endLine int) string {
	h := sha256.New()
	h.Write([]byte(repoID))
	h.Write([]byte(filePath))
	_, _ = fmt.Fprintf(h, "%d-%d", startLine, endLine)
	sum := h.Sum(nil)

	var uuid [16]byte
	copy(uuid[:], sum[:16])
	uuid[6] = (uuid[6] & 0x0f) | 0x40 // version 4
	uuid[8] = (uuid[8] & 0x3f) | 0x80 // RFC 4122 variant
	hexed := hex.EncodeToString(uuid[:])
	return hexed[0:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" + hexed[16:20] + "-" + hexed[20:32]
}
