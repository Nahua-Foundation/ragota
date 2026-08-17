// Package setup provides initialization functions for all components.
package setup

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/indexing"
	"github.com/Nahua-Foundation/ragota/internal/indexing/ast"
	"github.com/Nahua-Foundation/ragota/internal/indexing/bm25"
	"github.com/Nahua-Foundation/ragota/internal/indexing/vector"
	"github.com/Nahua-Foundation/ragota/internal/llm"
	"github.com/Nahua-Foundation/ragota/internal/lsp"
	"github.com/Nahua-Foundation/ragota/internal/obs"
	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/repos/git"
	"github.com/Nahua-Foundation/ragota/internal/repos/local"
	"github.com/Nahua-Foundation/ragota/internal/search"
	"github.com/Nahua-Foundation/ragota/internal/service"
	"github.com/Nahua-Foundation/ragota/internal/storage"
	"github.com/Nahua-Foundation/ragota/internal/storage/postgres"
	"github.com/Nahua-Foundation/ragota/internal/storage/qdrant"
	"github.com/Nahua-Foundation/ragota/internal/storage/sqlite"
)

// Build initializes all components based on configuration and returns a Service.
func Build(ctx context.Context, cfg *config.Config) (_ *service.Service, retErr error) {
	// Validate config first
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	for _, w := range cfg.Warnings() {
		obs.Inc(obs.MetricConfigWarnings, 1)
		slog.Warn("config", "warning", w)
	}

	var stor storage.Storage
	// Any wiring step after the store opens can fail (qdrant down, embedder
	// unreachable); closing the store on that path keeps a failed Build from
	// leaking the connection pool.
	defer func() {
		if retErr != nil && stor != nil {
			_ = stor.Close()
		}
	}()
	var indexers = make(map[indexing.IndexType]indexing.Indexer)

	// Initialize storage based on config (postgres takes precedence over sqlite)
	switch {
	case cfg.Storage.Postgres != nil:
		pgStor, err := postgres.Open(&postgres.Config{
			DSN:      cfg.Storage.Postgres.DSN,
			PoolSize: cfg.Storage.Postgres.PoolSize,
		})
		if err != nil {
			return nil, fmt.Errorf("init postgres: %w", err)
		}
		stor = pgStor // assigned before qdrant so the defer closes it on failure
		if cfg.Storage.Qdrant != nil {
			vecStore, err := initQdrant(cfg, ctx)
			if err != nil {
				return nil, fmt.Errorf("init qdrant: %w", err)
			}
			pgStor.SetVectorStore(vecStore)
		}
	case cfg.Storage.SQLite != nil:
		sqlStor, err := initSQLite(cfg)
		if err != nil {
			return nil, fmt.Errorf("init sqlite: %w", err)
		}
		stor = sqlStor // assigned before qdrant so the defer closes it on failure
		if cfg.Storage.Qdrant != nil {
			vecStore, err := initQdrant(cfg, ctx)
			if err != nil {
				return nil, fmt.Errorf("init qdrant: %w", err)
			}
			sqlStor.SetVectorStore(vecStore)
		}
	default:
		// Same wording as config.Validate: qdrant alone cannot hold files,
		// units and edges.
		return nil, fmt.Errorf("no relational storage backend configured (storage.sqlite or storage.postgres required)")
	}

	// Initialize embedders
	embedders := make(map[string]llm.Embedder)

	if cfg.Indexes.Vector != nil && cfg.Indexes.Vector.Enabled {
		if embedderBaseURL(cfg, "ollama") != "" {
			emb, err := initOllamaEmbedder(cfg)
			if err != nil {
				obs.Inc(obs.MetricEmbedderInitFailure, 1)
				return nil, fmt.Errorf("init ollama embedder: %w", err)
			}
			embedders["ollama"] = emb
		}

		// An OpenAI-compatible gateway (vLLM, LiteLLM, a corporate proxy) is
		// reached through the openai provider and usually needs no key, so
		// registration keys off the endpoint rather than off the API key.
		if openaiConfigured(cfg) {
			emb, err := initOpenAIEmbedder(cfg)
			if err != nil {
				obs.Inc(obs.MetricEmbedderInitFailure, 1)
				return nil, fmt.Errorf("init openai embedder: %w", err)
			}
			embedders["openai"] = emb
		}
	}

	// Initialize indexers
	if cfg.Indexes.AST != nil && cfg.Indexes.AST.Enabled {
		idx := initASTIndexer(cfg, stor)
		indexers[indexing.IndexTypeAST] = idx
	}

	if cfg.Indexes.Vector != nil && cfg.Indexes.Vector.Enabled {
		idx, err := initVectorIndexer(cfg, stor, embedders)
		if err != nil {
			return nil, fmt.Errorf("init vector indexer: %w", err)
		}
		indexers[indexing.IndexTypeVector] = idx
	}

	if cfg.Indexes.BM25 != nil && cfg.Indexes.BM25.Enabled {
		idx, err := initBM25Indexer(cfg)
		if err != nil {
			return nil, fmt.Errorf("init bm25 indexer: %w", err)
		}
		indexers[indexing.IndexTypeBM25] = idx
	}

	// Optional LSP refinement pass. It must run after the AST indexer (whose
	// per-file cleanup would otherwise discard its output), so when the AST
	// indexer is enabled the refiner is chained behind it instead of being
	// registered as an independent map entry (map iteration order is random).
	if cfg.LSP != nil && cfg.LSP.Enabled {
		refiner := lsp.NewRefiner(stor, cfg.LSP)
		if astIdx, ok := indexers[indexing.IndexTypeAST]; ok {
			indexers[indexing.IndexTypeAST] = lsp.Chain(astIdx, refiner)
		} else {
			indexers[indexing.IndexTypeCustom] = refiner
		}
	}
	// The call-edge correction pass is repository-scoped, not file-scoped, so
	// it is not an indexer: the service runs it once per repository, after
	// linking, over the edges the name matcher has already resolved.
	callRefiner := lsp.NewCallRefiner(stor, cfg.LSP)

	// Initialize sources
	sources := make(map[repos.SourceType]repos.RepoSource)

	if cfg.Repos.Sources.Local != nil && cfg.Repos.Sources.Local.Enabled {
		src := initLocalSource(cfg)
		sources[repos.SourceTypeLocal] = src
	}

	if cfg.Repos.Sources.Git != nil && cfg.Repos.Sources.Git.Enabled {
		src, err := initGitSource(cfg)
		if err != nil {
			return nil, fmt.Errorf("init git source: %w", err)
		}
		sources[repos.SourceTypeGit] = src
	}

	// Build search service from indexers that implement Searcher
	searchers := make(map[indexing.IndexType]indexing.Searcher)
	for _, idx := range indexers {
		if srch, ok := idx.(indexing.Searcher); ok {
			searchers[idx.Type()] = srch
		}
	}
	searchSvc := search.New(searchers, search.DefaultConfig())

	// Optional rerank stage over search results.
	if cfg.Search != nil && cfg.Search.Rerank != nil && cfg.Search.Rerank.Enabled {
		rr, err := llm.NewHTTPReranker(cfg.Search.Rerank)
		if err != nil {
			return nil, fmt.Errorf("init reranker: %w", err)
		}
		searchSvc.SetReranker(rr, cfg.Search.Rerank.TopN)
	}

	// Initialize storage
	if err := stor.Init(ctx); err != nil {
		return nil, fmt.Errorf("init storage: %w", err)
	}

	// Initialize indexers
	for _, idx := range indexers {
		if err := idx.Init(ctx, nil); err != nil {
			return nil, fmt.Errorf("init indexer %s: %w", idx.Name(), err)
		}
	}

	// Initialize sources
	for _, src := range sources {
		if err := src.Init(ctx, nil); err != nil {
			return nil, fmt.Errorf("init source %s: %w", src.Name(), err)
		}
	}

	svc := service.New(cfg, stor, indexers, sources, searchSvc)
	if callRefiner != nil {
		svc.SetCallRefiner(callRefiner)
	}

	// Optional LLM summaries
	if cfg.Summaries != nil && cfg.Summaries.Enabled {
		gen, err := initGenerator(cfg)
		if err != nil {
			return nil, fmt.Errorf("init summary generator: %w", err)
		}
		files := cfg.Summaries.Files == nil || *cfg.Summaries.Files
		svc.SetGenerator(gen, cfg.Summaries.MaxFiles, files)
		svc.SetSymbolSummaries(cfg.Summaries.Symbols, cfg.Summaries.MaxSymbols)
	}

	// Optional assistant LLM (query rewrite before retrieval).
	if cfg.Models.Assistant != nil {
		gen, err := initAssistant(cfg.Models.Assistant)
		if err != nil {
			return nil, fmt.Errorf("init assistant: %w", err)
		}
		// Query rewriting defaults OFF: it was measured as a retrieval
		// regression (tools/eval/README.md). Only an explicit true enables it.
		rewrite := cfg.Models.Assistant.QueryRewrite != nil && *cfg.Models.Assistant.QueryRewrite
		svc.SetAssistant(gen, rewrite)

		// Pre-index repo recon and ambiguous-edge disambiguation reuse the
		// same assistant generator (see internal/service/recon.go).
		recon := cfg.Models.Assistant.Recon == nil || *cfg.Models.Assistant.Recon
		disambiguate := cfg.Models.Assistant.Disambiguate == nil || *cfg.Models.Assistant.Disambiguate
		svc.SetReconAssistant(gen, recon, disambiguate)
	}

	return svc, nil
}

// initAssistant builds the auxiliary assistant generator from its own
// endpoint configuration.
func initAssistant(cfg *config.AssistantConfig) (llm.Generator, error) {
	switch cfg.Provider {
	case "ollama", "":
		return llm.NewOllamaGenerator(cfg.BaseURL, cfg.Model)
	case "openai":
		return llm.NewOpenAIGenerator(cfg.BaseURL, cfg.APIKey, cfg.Model)
	default:
		return nil, fmt.Errorf("unknown assistant provider: %s", cfg.Provider)
	}
}

// initGenerator builds the text generator for LLM summaries.
func initGenerator(cfg *config.Config) (llm.Generator, error) {
	switch cfg.Summaries.Provider {
	case "ollama", "":
		return llm.NewOllamaGenerator(cfg.Models.Providers["ollama"].BaseURL, cfg.Summaries.Model)
	case "openai":
		p := cfg.Models.Providers["openai"]
		return llm.NewOpenAIGenerator(p.BaseURL, p.APIKey, cfg.Summaries.Model)
	default:
		return nil, fmt.Errorf("unknown summaries provider: %s", cfg.Summaries.Provider)
	}
}

// initSQLite initializes SQLite storage.
func initSQLite(cfg *config.Config) (*sqlite.SQLite, error) {
	path, err := config.ExpandPathErr(cfg.Storage.SQLite.Path)
	if err != nil {
		return nil, fmt.Errorf("storage.sqlite.path: %w", err)
	}
	sqlCfg := &sqlite.Config{
		Path:     path,
		PoolSize: cfg.Storage.SQLite.PoolSize,
	}
	return sqlite.Open(sqlCfg)
}

// initQdrant initializes Qdrant vector storage. storage.qdrant.mode selects
// the deployment: "cloud" is authenticated (a missing key is a hard error
// rather than a stream of 401s at the first upsert), "docker_embedded" is a
// local instance.
func initQdrant(cfg *config.Config, ctx context.Context) (*qdrant.Qdrant, error) {
	mode := cfg.Storage.Qdrant.Mode
	if mode == "" {
		mode = "docker_embedded"
	}
	if mode == "cloud" && cfg.Storage.Qdrant.APIKey == "" {
		return nil, fmt.Errorf("storage.qdrant.api_key is required when storage.qdrant.mode is cloud")
	}
	if mode == "docker_embedded" && cfg.Storage.Qdrant.APIKey != "" {
		slog.Info("qdrant: api_key set for a docker_embedded instance", "url", cfg.Storage.Qdrant.URL)
	}
	slog.Info("qdrant: vector store configured", "mode", mode, "url", cfg.Storage.Qdrant.URL,
		"collection_prefix", cfg.Storage.Qdrant.CollectionPrefix)
	qdrantCfg := &qdrant.Config{
		URL:              cfg.Storage.Qdrant.URL,
		APIKey:           cfg.Storage.Qdrant.APIKey,
		CollectionPrefix: cfg.Storage.Qdrant.CollectionPrefix,
	}
	vecStore := qdrant.Open(qdrantCfg)
	if err := vecStore.Init(ctx); err != nil {
		return nil, fmt.Errorf("init qdrant: %w", err)
	}
	return vecStore, nil
}

// embedderBaseURL resolves the endpoint of one provider: the embedder's own
// base_url wins over the provider-level one, for every provider (it used to be
// honoured for openai only, which silently ignored the documented option for
// ollama).
func embedderBaseURL(cfg *config.Config, provider string) string {
	if cfg.Indexes.Vector != nil && cfg.Indexes.Vector.Embedder.BaseURL != "" &&
		cfg.Indexes.Vector.Embedder.Provider == provider {
		return cfg.Indexes.Vector.Embedder.BaseURL
	}
	return cfg.Models.Providers[provider].BaseURL
}

// openaiConfigured reports whether the openai provider is usable: an endpoint,
// an API key or an embedder that explicitly selects it is enough. The public
// endpoint still needs a key, which config.Validate checks.
func openaiConfigured(cfg *config.Config) bool {
	p := cfg.Models.Providers["openai"]
	if p.BaseURL != "" || p.APIKey != "" {
		return true
	}
	return cfg.Indexes.Vector != nil && cfg.Indexes.Vector.Enabled &&
		cfg.Indexes.Vector.Embedder.Provider == "openai"
}

// initOllamaEmbedder initializes Ollama embedder.
func initOllamaEmbedder(cfg *config.Config) (llm.Embedder, error) {
	if cfg.Indexes.Vector == nil {
		return nil, fmt.Errorf("vector index is not enabled")
	}
	if cfg.Indexes.Vector.Embedder.Model == "" {
		return nil, fmt.Errorf("vector embedder model is not configured")
	}
	ollamaCfg := &config.EmbedderConfig{
		Model:      cfg.Indexes.Vector.Embedder.Model,
		BaseURL:    embedderBaseURL(cfg, "ollama"),
		BatchSize:  cfg.Indexes.Vector.Embedder.BatchSize,
		Dimensions: cfg.Indexes.Vector.Embedder.Dimensions,
	}
	return llm.NewOllama(ollamaCfg)
}

// initOpenAIEmbedder initializes OpenAI embedder.
func initOpenAIEmbedder(cfg *config.Config) (llm.Embedder, error) {
	if cfg.Indexes.Vector == nil {
		return nil, fmt.Errorf("vector index is not enabled")
	}
	if cfg.Indexes.Vector.Embedder.Model == "" {
		return nil, fmt.Errorf("vector embedder model is not configured")
	}
	// The embedder may point at its own OpenAI-compatible endpoint; otherwise
	// it shares the one declared for the openai provider.
	baseURL := embedderBaseURL(cfg, "openai")
	openaiCfg := &config.EmbedderConfig{
		Model:      cfg.Indexes.Vector.Embedder.Model,
		BaseURL:    baseURL,
		BatchSize:  cfg.Indexes.Vector.Embedder.BatchSize,
		Dimensions: cfg.Indexes.Vector.Embedder.Dimensions,
	}
	return llm.NewOpenAI(openaiCfg, cfg.Models.Providers["openai"].APIKey)
}

// initASTIndexer initializes the AST indexer, registering only the parsers
// indexes.ast.languages asks for (empty means every supported language).
func initASTIndexer(cfg *config.Config, stor storage.Storage) indexing.Indexer {
	astIndexer := ast.New(&ast.Config{Storage: stor, Workers: cfg.Indexes.Workers})

	registered, skipped := parserLanguages(cfg.Indexes.AST.Languages)
	if len(skipped) == 0 {
		ast.RegisterDefaultParsers(astIndexer)
		return astIndexer
	}
	for _, lang := range registered {
		astIndexer.RegisterParser(ast.GetParserForLanguage(lang))
	}
	obs.IncBy(obs.MetricParserSkippedLangs, len(skipped))
	slog.Info("ast: parsers restricted by indexes.ast.languages",
		"registered", registered, "skipped", skipped)
	return astIndexer
}

// parserLanguages splits the supported languages into the ones the config
// asks for and the ones it leaves out. An empty or exhaustive list registers
// everything (no filtering, no log line).
func parserLanguages(wanted []string) (registered, skipped []string) {
	if len(wanted) == 0 {
		return config.ASTLanguages(), nil
	}
	enabled := make(map[string]bool, len(wanted))
	for _, lang := range wanted {
		enabled[lang] = true
	}
	for _, lang := range config.ASTLanguages() {
		if enabled[lang] {
			registered = append(registered, lang)
			continue
		}
		skipped = append(skipped, lang)
	}
	return registered, skipped
}

// initVectorIndexer initializes vector indexer.
func initVectorIndexer(cfg *config.Config, stor storage.Storage, embedders map[string]llm.Embedder) (indexing.Indexer, error) {
	provider := cfg.Indexes.Vector.Embedder.Provider
	emb, ok := embedders[provider]
	if !ok {
		return nil, fmt.Errorf("embedder %s not found", provider)
	}

	vecStore := stor.VectorStore()
	if vecStore == nil {
		return nil, fmt.Errorf("vector store not available")
	}

	vecIndexer := vector.New(&vector.Config{
		Embedder:    emb,
		Storage:     vecStore,
		MaxChars:    cfg.Indexes.Vector.Embedder.MaxChars,
		Concurrency: cfg.Indexes.Vector.Embedder.Concurrency,
		Exclude:     cfg.Indexes.Vector.Exclude,
		Chunking: indexing.ChunkConfig{
			Method:      cfg.Indexes.Vector.Chunking.Method,
			WindowLines: cfg.Indexes.Vector.Chunking.WindowLines,
			Overlap:     cfg.Indexes.Vector.Chunking.Overlap,
		},
		// "cards" builds one document per symbol (semantic v2); files
		// without units fall back to window chunking inside the indexer.
		Cards: cfg.Indexes.Vector.Chunking.Method == "cards",
	})
	return vecIndexer, nil
}

// initBM25Indexer initializes BM25 indexer. The path comes from
// indexes.bm25.path (default ~/.ragota-core/data/bm25); RAGOTA_BM25_PATH
// overrides it for one process.
func initBM25Indexer(cfg *config.Config) (indexing.Indexer, error) {
	configured := config.DefaultBM25Path
	if cfg.Indexes.BM25 != nil && cfg.Indexes.BM25.Path != "" {
		configured = cfg.Indexes.BM25.Path
	}
	if env := os.Getenv("RAGOTA_BM25_PATH"); env != "" {
		configured = env
	}
	bm25Path, err := config.ExpandPathErr(configured)
	if err != nil {
		return nil, fmt.Errorf("indexes.bm25.path: %w", err)
	}
	k1, b := cfg.Indexes.BM25.K1, cfg.Indexes.BM25.B
	if k1 <= 0 {
		k1 = 1.2
	}
	if b <= 0 {
		b = 0.75
	}
	bm25Indexer, err := bm25.New(&bm25.Config{
		Path:      bm25Path,
		K1:        k1,
		B:         b,
		NoCompact: cfg.Indexes.BM25.NoCompact,
	})
	if err != nil {
		return nil, err
	}
	return bm25Indexer, nil
}

// initLocalSource initializes local repository source.
func initLocalSource(cfg *config.Config) repos.RepoSource {
	localSource := local.New()
	if err := localSource.Init(context.Background(), map[string]interface{}{
		"paths": cfg.Repos.Sources.Local.Paths,
	}); err != nil {
		slog.Warn("init local source", "err", err)
	}
	return localSource
}

// initGitSource initializes Git repository source. Tokens come from
// repos.sources.git.auth, with GITHUB_TOKEN / GITLAB_TOKEN as the fallback for
// each one separately.
func initGitSource(cfg *config.Config) (repos.RepoSource, error) {
	workDir, err := config.ExpandPathErr(cfg.Repos.Sources.Git.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("repos.sources.git.work_dir: %w", err)
	}
	auth := gitAuth(cfg.Repos.Sources.Git.Auth)
	gitSource := git.New(&git.Config{
		WorkDir: workDir,
		Auth:    auth,
	})
	if err := gitSource.Init(context.Background(), nil); err != nil {
		return nil, fmt.Errorf("init git source: %w", err)
	}
	return gitSource, nil
}

// gitAuth resolves configured tokens, falling back per token to the
// environment.
func gitAuth(cfg *config.GitAuthConfig) *git.Auth {
	auth := &git.Auth{}
	if cfg != nil {
		auth.GitHubToken = cfg.GitHubToken
		auth.GitLabToken = cfg.GitLabToken
	}
	if auth.GitHubToken == "" {
		if v := os.Getenv("GITHUB_TOKEN"); v != "" {
			auth.GitHubToken = v
			obs.Inc(obs.MetricGitAuthFromEnv, 1)
		}
	}
	if auth.GitLabToken == "" {
		if v := os.Getenv("GITLAB_TOKEN"); v != "" {
			auth.GitLabToken = v
			obs.Inc(obs.MetricGitAuthFromEnv, 1)
		}
	}
	return auth
}
