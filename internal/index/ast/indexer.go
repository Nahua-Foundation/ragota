package ast

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/index"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// Compile-time interface assertions.
var _ index.Indexer = (*Indexer)(nil)

// Indexer implements index.Indexer for AST parsing.
type Indexer struct {
	store   store.Storage
	parsers map[string]Parser
	workers int
}

// Parser extracts AST units from source code.
type Parser interface {
	Parse(filePath, content string) ([]*domain.ASTUnit, []*domain.Edge, error)
	Language() string
}

// fileFacts is one file's parse output, including the facts that only become
// useful once the other files of a package are in view.
type fileFacts struct {
	Units []*domain.ASTUnit
	Edges []*domain.Edge
	// Wrappers are the local helpers that perform an outbound call on behalf
	// of their callers (see wrappers.go).
	Wrappers []wrapper
	// Coverage counts the call sites this file offered as outbound contracts,
	// per domain.ContractKind*. Nil from a parser that does not look for any.
	Coverage map[string]domain.CoverageCounts
	// Tables are the data-access sites whose table this file names by a
	// constant it does not declare, and Consts the table names it declares for
	// the rest of its package (see linkPackageTables).
	Tables []pendingTable
	Consts map[string]string
}

// pendingTable is one data-access site waiting for the package to say which
// table its constant stands for.
type pendingTable struct {
	Src   string   // source unit, as a positional marker (see srcMark)
	Ident string   // the identifier the table name arrives in
	Kind  string   // store.EdgeReadsFrom or store.EdgeWritesTo
	Line  int      // 1-based source line of the call
	Args  []string // call arguments, for the edge meta
}

// factsParser is a Parser that can also report per-file facts. Parsers that do
// not implement it are used through Parse and contribute no wrappers and no
// coverage — silence, which the coverage report distinguishes from a zero.
type factsParser interface {
	Parser
	ParseFacts(filePath, content string) (*fileFacts, error)
}

// Config is the AST indexer configuration.
type Config struct {
	Storage store.Storage
	// Workers is the parse-stage worker-pool size.
	// 0 means runtime.NumCPU(); the effective value is capped at 32.
	Workers int
}

// New creates a new AST indexer.
func New(cfg *Config) *Indexer {
	return &Indexer{
		store:   cfg.Storage,
		parsers: make(map[string]Parser),
		workers: cfg.Workers,
	}
}

// maxParseWorkers caps the parse-stage worker-pool size.
const maxParseWorkers = 32

// parseWorkers returns the effective worker-pool size for n configured
// workers: values <= 0 fall back to runtime.NumCPU(), capped at maxParseWorkers.
func parseWorkers(n int) int {
	if n <= 0 {
		n = runtime.NumCPU()
	}
	if n > maxParseWorkers {
		n = maxParseWorkers
	}
	if n < 1 {
		n = 1
	}
	return n
}

// Name returns the indexer name.
func (i *Indexer) Name() string {
	return "ast"
}

// Type returns the indexer type.
func (i *Indexer) Type() index.IndexType {
	return index.IndexTypeAST
}

// Init initializes the indexer.
func (i *Indexer) Init(ctx context.Context, config map[string]interface{}) error {
	// Initialize parsers for supported languages
	// For now, we'll initialize them when needed
	return nil
}

// RegisterParser registers a parser for a language.
func (i *Indexer) RegisterParser(parser Parser) {
	i.parsers[parser.Language()] = parser
}

// Index indexes a repository's files.
func (i *Indexer) Index(ctx context.Context, req *index.IndexRequest) (*index.IndexResult, error) {
	start := time.Now()

	result := &index.IndexResult{}

	// Group files by language
	byLanguage := groupByLanguage(req.Files)

	for language, files := range byLanguage {
		parser, ok := i.parsers[language]
		if !ok {
			// No parser for this language
			result.FilesSkipped += len(files)
			continue
		}

		// Parse stage: read (if needed) and parse files in a worker pool.
		// Results are collected by file index so the store stage below runs
		// in the original files order.
		parsed := i.parseFiles(req, language, files, parser)

		// Package stage: a helper that performs an outbound call, and the
		// constant a store's table is named by, are usually files of their own
		// next to the callers, so both are resolved once every file of the
		// batch has been parsed.
		linkPackageWrappers(files, parsed)
		linkPackageTables(files, parsed)

		for fi := range files {
			addCoverage(result, parsed[fi].coverage)
		}

		// Store stage: sequential, in the original files order, to keep the
		// transactional behaviour and write order of the sequential indexer.
		for s := 0; s < len(files); s += storeWindow {
			e := s + storeWindow
			if e > len(files) {
				e = len(files)
			}
			i.storeFiles(ctx, req, language, files[s:e], parsed[s:e], result)
		}
	}

	result.Duration = time.Since(start)
	return result, nil
}

// storeWindow is how many files' units and edges go out per storage call. A
// per-file store costs four transactions — two deletes, two inserts — which on
// a repository the size of Elasticsearch is 160k commits, and the commits
// rather than the inserts are what the edge store spends its time on.
const storeWindow = 256

// storeFiles writes one window of parsed files: the whole window's stale rows
// are deleted first, then its units and edges go out in one batch each — four
// transactions per window rather than four per file.
//
// Deleting the entire window before storing any of it is what keeps the
// per-file ordering intact: DeleteASTUnitsByFiles unresolves the edges that
// point into the files it clears, so a delete running after a store would hand
// back edges that had just been resolved. Units are deleted before edges for
// the same reason the single-file path does it in that order.
func (i *Indexer) storeFiles(ctx context.Context, req *index.IndexRequest, language string,
	files []*index.FileToIndex, parsed []parseResult, result *index.IndexResult) {
	stored := make([]bool, len(files))

	paths := make([]string, 0, len(files))
	for fi, file := range files {
		if err := parsed[fi].err; err != nil {
			result.FilesFailed++
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", file.Path, err))
			continue
		}
		for _, unit := range parsed[fi].units {
			unit.RepoID = req.RepoID
			unit.FilePath = file.Path
			unit.Language = language
		}
		for _, edge := range parsed[fi].edges {
			edge.RepoID = req.RepoID
			edge.FilePath = file.Path
		}
		stored[fi] = true
		paths = append(paths, file.Path)
	}

	if err := i.deleteWindow(ctx, req.RepoID, paths); err != nil {
		// A window-wide delete failure names no file, so the files are retried
		// one at a time: that clears the ones that are fine and attributes the
		// failure to the one that is not, which the batch error cannot.
		for fi, file := range files {
			if !stored[fi] {
				continue
			}
			if err := i.deleteWindow(ctx, req.RepoID, []string{file.Path}); err != nil {
				stored[fi] = false
				result.FilesFailed++
				result.Errors = append(result.Errors, fmt.Sprintf("%s: delete old data: %v", file.Path, err))
			}
		}
	}

	var units []*domain.ASTUnit
	for fi := range files {
		if stored[fi] {
			units = append(units, parsed[fi].units...)
		}
	}
	if err := i.store.BatchStoreASTUnits(ctx, units); err != nil {
		// One rejected row rolls the window's transaction back, so the files
		// are retried one at a time: that stores the ones that are fine and
		// names the one that is not, which a window-wide error cannot.
		for fi, file := range files {
			if !stored[fi] {
				continue
			}
			if err := i.store.BatchStoreASTUnits(ctx, parsed[fi].units); err != nil {
				stored[fi] = false
				result.FilesFailed++
				result.Errors = append(result.Errors, fmt.Sprintf("%s: store units: %v", file.Path, err))
			}
		}
	}

	var edges []*domain.Edge
	for fi := range files {
		if !stored[fi] {
			continue
		}
		parsed[fi].edges = resolveEdgeSources(parsed[fi].edges, parsed[fi].units)
		edges = append(edges, parsed[fi].edges...)
		result.FilesIndexed++
	}
	if err := i.store.BatchStoreEdges(ctx, edges); err != nil {
		for fi, file := range files {
			if !stored[fi] {
				continue
			}
			// Log but don't fail the file its edges belong to.
			if err := i.store.BatchStoreEdges(ctx, parsed[fi].edges); err != nil {
				slog.Warn("ast indexer: store edges failed", "file", file.Path, "error", err)
			}
		}
	}
}

// deleteWindow clears the stale units and edges of the given paths. Units go
// first: deleting them unresolves the edges that pointed into them, and an edge
// deleted before that would have nothing left to unresolve against.
func (i *Indexer) deleteWindow(ctx context.Context, repoID string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	if err := i.store.DeleteASTUnitsByFiles(ctx, repoID, paths); err != nil {
		return err
	}
	return i.store.DeleteEdgesByFiles(ctx, repoID, paths)
}

// resolveEdgeSources rewrites positional "#idx" source markers to the storage
// IDs the units were given, and drops the edges whose source cannot be
// resolved.
func resolveEdgeSources(edges []*domain.Edge, units []*domain.ASTUnit) []*domain.Edge {
	valid := edges[:0]
	for _, edge := range edges {
		if idx := resolveMark(edge.SrcID); idx >= 0 {
			if idx >= len(units) {
				continue
			}
			edge.SrcID = units[idx].ID
		}
		if edge.SrcID == "" {
			continue
		}
		valid = append(valid, edge)
	}
	return valid
}

// parseResult holds the parse output for a single file.
type parseResult struct {
	units    []*domain.ASTUnit
	edges    []*domain.Edge
	wrappers []wrapper
	coverage map[string]domain.CoverageCounts
	tables   []pendingTable
	consts   map[string]string
	err      error
}

// linkPackageWrappers attributes each helper's outbound call to the call sites
// in the other files of its directory, which is where the helper usually
// lives. It is the cross-file half of the one-level wrapper following the
// parser already did within each file; running it a second time over a file
// changes nothing, since a call site that already carries an http_call is
// skipped.
//
// Directory rather than repository: the same helper name ("apiRequest") is
// used by dozens of unrelated packages, and a name is all this pass has to
// join on.
func linkPackageWrappers(files []*index.FileToIndex, parsed []parseResult) {
	byDir := map[string]map[string]wrapper{}
	for fi := range parsed {
		for _, w := range parsed[fi].wrappers {
			table, ok := byDir[w.Dir]
			if !ok {
				table = map[string]wrapper{}
				byDir[w.Dir] = table
			}
			if _, dup := table[w.Name]; !dup {
				table[w.Name] = w
			}
		}
	}
	if len(byDir) == 0 {
		return
	}
	for fi := range parsed {
		table := byDir[path.Dir(files[fi].Path)]
		if len(table) == 0 || parsed[fi].err != nil {
			continue
		}
		added, edges := linkWrapperCalls(parsed[fi].edges, table)
		parsed[fi].edges = edges
		if added == 0 {
			continue
		}
		// A call site the wrapper explained is a recognized outbound contract
		// that produced an edge; it was not counted as a candidate before,
		// because until the wrapper was known it looked like any other call.
		if parsed[fi].coverage == nil {
			parsed[fi].coverage = newCoverage()
		}
		c := parsed[fi].coverage[domain.ContractKindHTTP]
		c.Candidates += added
		c.Edges += added
		parsed[fi].coverage[domain.ContractKindHTTP] = c
	}
}

// linkPackageTables gives the data-access sites whose table arrives in a
// constant the value that constant holds, when another file of the same
// directory declares it. Consul's state store is written that way — the table
// names live in a <topic>_schema.go and the 437 accesses in the query files
// beside it — and without this pass 368 of them name a table nothing can
// resolve.
//
// Directory rather than repository, as for the wrappers: an identifier is all
// this pass has to join on, and "tableIndex" means a different table in every
// package that declares one. It also sees only the files of this batch, so an
// incremental run over a query file whose schema file did not change leaves the
// table unresolved until the next full pass — which the coverage report shows
// as the gap it is.
//
// The sites were counted as candidates when they were parsed, so an edge added
// here is a candidate that turned out to be resolvable, and coverage moves by
// exactly the edges added.
func linkPackageTables(files []*index.FileToIndex, parsed []parseResult) {
	byDir := map[string]map[string]string{}
	for fi := range parsed {
		if len(parsed[fi].consts) == 0 {
			continue
		}
		dir := path.Dir(files[fi].Path)
		names, ok := byDir[dir]
		if !ok {
			names = map[string]string{}
			byDir[dir] = names
		}
		for ident, value := range parsed[fi].consts {
			if _, dup := names[ident]; !dup {
				names[ident] = value
			}
		}
	}
	if len(byDir) == 0 {
		return
	}
	for fi := range parsed {
		if parsed[fi].err != nil || len(parsed[fi].tables) == 0 {
			continue
		}
		names := byDir[path.Dir(files[fi].Path)]
		added := 0
		for _, t := range parsed[fi].tables {
			tbl := sqlTableName(names[t.Ident])
			if tbl == "" {
				continue
			}
			added++
			parsed[fi].edges = append(parsed[fi].edges, &domain.Edge{
				SrcID:      t.Src,
				Kind:       t.Kind,
				DstName:    contract.DB(tbl),
				Line:       t.Line,
				Confidence: contract.ConfCrossFile,
				Meta:       store.EncodeEdgeMeta(&store.EdgeMeta{Args: t.Args, BaseConf: contract.ConfCrossFile}),
			})
		}
		if added == 0 {
			continue
		}
		if parsed[fi].coverage == nil {
			parsed[fi].coverage = newCoverage()
		}
		c := parsed[fi].coverage[domain.ContractKindDB]
		c.Edges += added
		parsed[fi].coverage[domain.ContractKindDB] = c
	}
}

// addCoverage merges one file's counters into the run's result.
func addCoverage(result *index.IndexResult, counts map[string]domain.CoverageCounts) {
	if len(counts) == 0 {
		return
	}
	if result.Coverage == nil {
		result.Coverage = make(map[string]domain.CoverageCounts, len(counts))
	}
	for kind, c := range counts {
		acc := result.Coverage[kind]
		acc.Candidates += c.Candidates
		acc.Edges += c.Edges
		result.Coverage[kind] = acc
	}
}

// parseFiles reads (when Content is nil) and parses files in a worker pool.
// The returned slice is indexed by the position of the file in files, so the
// caller can process results in the original order. Parser implementations
// are safe for concurrent use: a fresh tree-sitter parser is created per
// Parse call (see TestTreeSitterParserConcurrent).
//
// Each file's symbols are published as soon as they exist, before the package
// and store stages: another indexer over the same window would otherwise parse
// the same bytes again only to find out which symbol covers a line. Publishing
// is a copy, so the store stage is still free to rewrite the units in place.
func (i *Indexer) parseFiles(req *index.IndexRequest, language string,
	files []*index.FileToIndex, parser Parser) []parseResult {
	results := make([]parseResult, len(files))

	facts, _ := parser.(factsParser)
	share := index.SymbolsAnnotated(language)
	publish := func(file *index.FileToIndex, units []*domain.ASTUnit) {
		if share {
			index.SharedSymbols.Put(req.RepoID, file.Path, file.Hash, index.ProjectSymbols(units))
		}
	}

	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < parseWorkers(i.workers); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for fi := range jobs {
				file := files[fi]
				if file.Content == nil {
					content, err := readFileContent(req.RepoPath, file.Path)
					if err != nil {
						results[fi] = parseResult{err: err}
						continue
					}
					file.Content = content
				}
				if facts != nil {
					f, err := facts.ParseFacts(file.Path, string(file.Content))
					if err != nil {
						results[fi] = parseResult{err: fmt.Errorf("parse: %w", err)}
						continue
					}
					publish(file, f.Units)
					results[fi] = parseResult{units: f.Units, edges: f.Edges, wrappers: f.Wrappers,
						coverage: f.Coverage, tables: f.Tables, consts: f.Consts}
					continue
				}
				units, edges, err := parser.Parse(file.Path, string(file.Content))
				if err != nil {
					results[fi] = parseResult{err: fmt.Errorf("parse: %w", err)}
					continue
				}
				publish(file, units)
				results[fi] = parseResult{units: units, edges: edges}
			}
		}()
	}
	for fi := range files {
		jobs <- fi
	}
	close(jobs)
	wg.Wait()

	return results
}

// Remove removes indexed data for files.
func (i *Indexer) Remove(ctx context.Context, repoID string, paths []string) error {
	if len(paths) == 0 {
		// Delete all for repo
		if err := i.store.DeleteASTUnitsByRepo(ctx, repoID); err != nil {
			return err
		}
		return i.store.DeleteEdgesByRepo(ctx, repoID)
	}

	return i.deleteWindow(ctx, repoID, paths)
}

// Close closes the indexer.
func (i *Indexer) Close() error {
	return nil
}

// Stats returns indexer statistics.
func (i *Indexer) Stats(ctx context.Context) (*index.IndexerStats, error) {
	count, err := i.store.CountASTUnits(ctx)
	if err != nil {
		return nil, fmt.Errorf("count ast units: %w", err)
	}

	return &index.IndexerStats{
		Documents: count,
		Specific: map[string]interface{}{
			"type": "ast",
		},
	}, nil
}

// Helper functions

func groupByLanguage(files []*index.FileToIndex) map[string][]*index.FileToIndex {
	result := make(map[string][]*index.FileToIndex)
	for _, f := range files {
		result[f.Language] = append(result[f.Language], f)
	}
	return result
}

func readFileContent(repoPath, filePath string) ([]byte, error) {
	fullPath := filepath.Join(repoPath, filePath)
	return os.ReadFile(fullPath)
}
