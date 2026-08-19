package lsp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/index"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// unitHashLSP marks units discovered by the LSP pass (absent from the
// tree-sitter output).
const unitHashLSP = "lsp"

// languageIDs maps ragota language names to LSP languageId values and doubles
// as the supported-language set.
var languageIDs = map[string]string{
	"go":         "go",
	"java":       "java",
	"csharp":     "csharp",
	"typescript": "typescript",
}

// LanguageID returns the LSP languageId for a ragota language name ("" when
// the language has no LSP support).
func LanguageID(lang string) string {
	return languageIDs[lang]
}

// symbolKinds maps LSP SymbolKind values to ragota unit kinds.
var symbolKinds = map[int]string{
	5:  "class",     // Class
	6:  "method",    // Method
	7:  "field",     // Property
	8:  "field",     // Field
	9:  "method",    // Constructor
	10: "type",      // Enum
	11: "interface", // Interface
	12: "function",  // Function
	13: "var",       // Variable
	14: "const",     // Constant
	23: "type",      // Struct
}

// Compile-time interface assertion.
var _ index.Indexer = (*Refiner)(nil)

// Refiner is an index.Indexer (type "custom", name "lsp") that refines the
// tree-sitter graph with language-server data: it adds the function/method
// units the heuristic parsers missed (documentSymbol). It is registered by
// bootstrap.Build chained after the AST indexer, so tree-sitter units are already
// stored when it runs.
//
// Edges are not its job. This pass sees one batch of files at a time, and a
// language-server session costs a whole workspace load — 60-90 s for a large
// Go module, minutes for a Maven or MSBuild project — so a per-batch pass pays
// that repeatedly, and a references request per definition in every file is
// tens of thousands of requests per repository. Call-edge correction is
// therefore a separate, repository-scoped, bounded pass: see CallRefiner.
type Refiner struct {
	store   store.Storage
	cfg     *config.LSPConfig
	mapper  Mapper
	timeout time.Duration
}

// NewRefiner creates a Refiner over the given storage and LSP configuration.
func NewRefiner(st store.Storage, cfg *config.LSPConfig) *Refiner {
	timeout := DefaultTimeout
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	return &Refiner{
		store:   st,
		cfg:     cfg,
		mapper:  NewMapper(cfg.HostRoot, cfg.MountRoot),
		timeout: timeout,
	}
}

// Name returns the indexer name.
func (r *Refiner) Name() string { return "lsp" }

// Type returns the indexer type.
func (r *Refiner) Type() index.IndexType { return index.IndexTypeCustom }

// Init validates the configuration and probes every configured server once so
// an unreachable or mis-mapped LSP deployment is visible at startup instead of
// showing up as a silently empty refinement pass.
func (r *Refiner) Init(ctx context.Context, _ map[string]interface{}) error {
	if len(r.cfg.Servers) == 0 {
		return fmt.Errorf("lsp: enabled with an empty servers map: every file would be skipped")
	}

	langs := make([]string, 0, len(r.cfg.Servers))
	for lang := range r.cfg.Servers {
		langs = append(langs, lang)
	}
	sort.Strings(langs)

	reachable := 0
	for _, lang := range langs {
		srv := r.cfg.Servers[lang]
		lspDialChecks.Inc()
		client, err := Dial(srv.Addr, r.dialTimeout())
		if err != nil {
			lspDialFailures.Inc()
			slog.Warn("lsp: server unreachable at startup", "language", lang, "addr", srv.Addr, "error", err)
			continue
		}
		_ = client.Close()
		reachable++
		slog.Info("lsp: server reachable", "language", lang, "addr", srv.Addr)
	}

	// Startup never fails on this: the servers are containers that may still
	// be booting, and the pass is best-effort by design.
	if reachable == 0 {
		slog.Warn("lsp: no configured language server is reachable; the refinement pass will add nothing",
			"servers", len(r.cfg.Servers))
	}
	slog.Info("lsp: refinement pass configured",
		"languages", langs, "reachable", reachable,
		"host_root", r.cfg.HostRoot, "mount_root", r.cfg.MountRoot)
	return nil
}

// dialTimeout bounds the startup probe; it is deliberately shorter than the
// per-request timeout so a dead server does not stall startup.
func (r *Refiner) dialTimeout() time.Duration {
	if r.timeout < 5*time.Second {
		return r.timeout
	}
	return 5 * time.Second
}

// Remove is a no-op: the refiner's units and edges live in the shared
// metadata storage and are removed by the service's per-file/per-repo cleanup.
func (r *Refiner) Remove(ctx context.Context, repoID string, paths []string) error { return nil }

// Stats returns minimal statistics (the refiner owns no separate store).
func (r *Refiner) Stats(ctx context.Context) (*index.IndexerStats, error) {
	return &index.IndexerStats{Specific: map[string]interface{}{"type": "lsp"}}, nil
}

// Close releases resources (no-op; connections are per Index run).
func (r *Refiner) Close() error { return nil }

// Index runs the refinement pass over the request's files. A missing or
// unreachable language server is logged once per language and skipped;
// per-file failures are recorded in the result without failing the run.
func (r *Refiner) Index(ctx context.Context, req *index.IndexRequest) (*index.IndexResult, error) {
	start := time.Now()
	res := &index.IndexResult{}
	lspPassTotal.Inc()

	if len(r.cfg.Servers) == 0 {
		// Reporting success here is what makes a mis-configured pass look like
		// a working one; the caller logs the error and carries on.
		return res, fmt.Errorf("lsp: no servers configured, nothing was refined")
	}

	byLang := make(map[string][]*index.FileToIndex)
	for _, f := range req.Files {
		_, supported := languageIDs[f.Language]
		_, hasServer := r.cfg.Servers[f.Language]
		if !supported || !hasServer {
			res.FilesSkipped++
			continue
		}
		byLang[f.Language] = append(byLang[f.Language], f)
	}
	lspFilesSkipped.Add(float64(res.FilesSkipped))
	if len(byLang) == 0 {
		res.Duration = time.Since(start)
		lspPassSeconds.Observe(res.Duration.Seconds())
		return res, nil
	}

	langs := make([]string, 0, len(byLang))
	for lang := range byLang {
		langs = append(langs, lang)
	}
	sort.Strings(langs)

	var failed []string
	for _, lang := range langs {
		files := byLang[lang]
		srv := r.cfg.Servers[lang]
		symbols, err := r.refineLanguage(ctx, req, lang, srv, files, res)
		switch {
		case err != nil:
			lspServerFailures.Inc()
			slog.Warn("lsp: language server unavailable, skipping language",
				"language", lang, "addr", srv.Addr, "error", err)
			res.FilesSkipped += len(files)
			lspFilesSkipped.Add(float64(len(files)))
			failed = append(failed, lang)
		case symbols == 0:
			// A reachable server that returns nothing for every file means a
			// broken workspace, most often a wrong host_root/mount_root
			// mapping — indistinguishable from success without this.
			lspEmptyLanguages.Inc()
			slog.Warn("lsp: server returned no symbols for any file; check lsp.host_root/mount_root and the container mount",
				"language", lang, "addr", srv.Addr, "files", len(files),
				"host_root", r.cfg.HostRoot, "mount_root", r.cfg.MountRoot)
			res.FilesFailed += len(files)
			lspFilesFailed.Add(float64(len(files)))
			res.Errors = append(res.Errors, fmt.Sprintf("%s: language server %s returned no symbols for %d files", lang, srv.Addr, len(files)))
			failed = append(failed, lang)
		}
	}

	res.Duration = time.Since(start)
	lspPassSeconds.Observe(res.Duration.Seconds())
	if len(failed) == len(langs) {
		return res, fmt.Errorf("lsp: every language failed (%s)", strings.Join(failed, ", "))
	}
	return res, nil
}

// refineLanguage connects to one language server and refines its files. It
// returns the total number of symbols the server reported: zero across every
// file means the server cannot see the workspace, which the caller treats as a
// failure rather than as an empty success. An error is returned only when the
// server itself is unusable (dial or initialize failed); per-file errors are
// recorded in res.
func (r *Refiner) refineLanguage(ctx context.Context, req *index.IndexRequest, lang string, srv config.LSPServerConfig, files []*index.FileToIndex, res *index.IndexResult) (int, error) {
	client, err := Dial(srv.Addr, r.timeout)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	repoRoot := absPath(req.RepoPath)
	if err := client.Initialize(ctx, r.mapper.ToURI(repoRoot), srv.InitOptions); err != nil {
		return 0, err
	}

	// unitCache caches storage lookups of a file's units within this run.
	unitCache := make(map[string][]*domain.ASTUnit)
	symbols := 0
	for _, f := range files {
		n, err := r.refineFile(ctx, client, req, repoRoot, lang, f, unitCache)
		if err != nil {
			res.FilesFailed++
			lspFilesFailed.Inc()
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", f.Path, err))
			continue
		}
		symbols += n
		res.FilesIndexed++
		lspFilesRefined.Inc()
	}
	return symbols, nil
}

// refineFile opens one file on the server, merges documentSymbol output into
// the stored units and creates precise reference edges for its definitions.
// It returns how many symbols the server reported for the file.
func (r *Refiner) refineFile(ctx context.Context, client *Client, req *index.IndexRequest, repoRoot, lang string, f *index.FileToIndex, unitCache map[string][]*domain.ASTUnit) (int, error) {
	content := f.Content
	if content == nil {
		data, err := os.ReadFile(filepath.Join(repoRoot, f.Path))
		if err != nil {
			return 0, fmt.Errorf("read: %w", err)
		}
		content = data
	}

	uri := r.mapper.ToURI(filepath.Join(repoRoot, f.Path))
	if err := client.DidOpen(uri, languageIDs[lang], string(content)); err != nil {
		return 0, fmt.Errorf("didOpen: %w", err)
	}
	symbols, err := client.DocumentSymbols(ctx, uri)
	if err != nil {
		return 0, fmt.Errorf("documentSymbol: %w", err)
	}

	units, err := r.fileUnits(ctx, req.RepoID, f.Path, unitCache)
	if err != nil {
		return 0, err
	}

	// Merge: add function/method symbols the tree-sitter pass missed.
	// Units matching an existing one by name are left untouched.
	known := make(map[string]bool, len(units))
	for _, u := range units {
		known[u.Name] = true
	}
	for _, s := range symbols {
		kind := symbolKinds[s.Kind]
		if kind != "function" && kind != "method" {
			continue
		}
		if s.Name == "" || known[s.Name] {
			continue
		}
		u := &domain.ASTUnit{
			RepoID:    req.RepoID,
			FilePath:  f.Path,
			Language:  lang,
			Kind:      kind,
			Name:      s.Name,
			Qualified: qualifiedName(s),
			StartLine: s.StartLine + 1,
			EndLine:   s.EndLine + 1,
			Hash:      unitHashLSP,
		}
		if err := r.store.StoreASTUnit(ctx, u); err != nil {
			return 0, fmt.Errorf("store unit %s: %w", s.Name, err)
		}
		known[s.Name] = true
		units = append(units, u)
	}
	unitCache[f.Path] = units
	return len(symbols), nil
}

// fileUnits returns (and caches) the stored units of a file.
func (r *Refiner) fileUnits(ctx context.Context, repoID, path string, cache map[string][]*domain.ASTUnit) ([]*domain.ASTUnit, error) {
	if units, ok := cache[path]; ok {
		return units, nil
	}
	units, err := r.store.GetASTUnits(ctx, domain.QueryOpts{RepoID: repoID, FilePath: path})
	if err != nil {
		return nil, fmt.Errorf("get units of %s: %w", path, err)
	}
	cache[path] = units
	return units, nil
}

// namePosition finds the 0-based (line, character) of a unit's name near its
// declaration start: the name is searched in the unit's first lines. Returns
// col -1 when the name cannot be located.
func namePosition(lines []string, u *domain.ASTUnit) (int, int) {
	start := u.StartLine - 1 // storage lines are 1-based
	end := start + 4
	if u.EndLine-1 < end {
		end = u.EndLine - 1
	}
	for i := start; i <= end && i < len(lines); i++ {
		if i < 0 {
			continue
		}
		if col := indexIdent(lines[i], u.Name); col >= 0 {
			return i, col
		}
	}
	return 0, -1
}

// indexIdent finds name in line at an identifier boundary.
func indexIdent(line, name string) int {
	for from := 0; from < len(line); {
		i := strings.Index(line[from:], name)
		if i < 0 {
			return -1
		}
		i += from
		before := i == 0 || !isIdentByte(line[i-1])
		afterIdx := i + len(name)
		after := afterIdx >= len(line) || !isIdentByte(line[afterIdx])
		if before && after {
			return i
		}
		from = i + 1
	}
	return -1
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// qualifiedName builds a qualified name from a symbol and its container.
func qualifiedName(s Symbol) string {
	if s.Container != "" {
		return s.Container + "." + s.Name
	}
	return s.Name
}

// absPath resolves p to an absolute path (best effort).
func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
