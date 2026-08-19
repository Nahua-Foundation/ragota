package config

// Config is the main ragota configuration.
type Config struct {
	Server    ServerConfig     `yaml:"server"`
	Log       LogConfig        `yaml:"log"`
	Storage   StorageConfig    `yaml:"storage"`
	Indexes   IndexesConfig    `yaml:"indexes"`
	Models    ModelsConfig     `yaml:"models"`
	Repos     ReposConfig      `yaml:"repos"`
	Summaries *SummariesConfig `yaml:"summaries,omitempty"`
	LSP       *LSPConfig       `yaml:"lsp,omitempty"`
	Search    *SearchConfig    `yaml:"search,omitempty"`

	// rawChunkingMethod remembers a deprecated chunking method as written, so
	// Warnings can report the substitution applyDefaults made.
	rawChunkingMethod string `yaml:"-"`
}

// LogConfig configures the process logger.
type LogConfig struct {
	// Level is debug | info | warn | error (default info). The LSP pass and
	// the linker log their per-file diagnostics at debug.
	Level string `yaml:"level"`
	// Format is text | json (default text).
	Format string `yaml:"format"`
}

// LSPConfig enables the language-server precision pass. Each supported
// language runs its own LSP server in a Docker container, exposed over TCP
// (stdio bridged via socat). Repositories must be volume-mounted into the
// containers; HostRoot/MountRoot describe the path mapping.
type LSPConfig struct {
	Enabled bool `yaml:"enabled"`
	// HostRoot is the host path prefix under which indexed repositories live.
	HostRoot string `yaml:"host_root"`
	// MountRoot is where HostRoot is mounted inside the LSP containers.
	MountRoot string `yaml:"mount_root"`
	// Servers maps a language ("go", "java", "csharp", "typescript") to its server.
	Servers map[string]LSPServerConfig `yaml:"servers"`
	// TimeoutSeconds bounds a single LSP request (default 30).
	TimeoutSeconds int `yaml:"timeout_seconds"`
	// Calls configures the call-edge correction pass.
	Calls *LSPCallsConfig `yaml:"calls,omitempty"`
}

// LSPCallsConfig bounds the call-edge correction pass: which callee
// definitions are worth a textDocument/references request.
//
// The pass cannot ask about every symbol — a references request per
// function over a large repository is tens of thousands of requests — so it
// asks only where name-based resolution is demonstrably weak or where the
// answer is worth most. See lsp.CallRefiner for what it does with the answer.
type LSPCallsConfig struct {
	// Enabled turns the pass on. It is off by default even when lsp.enabled
	// is set: the pass costs a language-server session per repository.
	Enabled bool `yaml:"enabled"`
	// Scope selects the candidate definitions:
	//   "boundary"  — endpoints of contract edges only (the cheapest useful set)
	//   "ambiguous" — definitions whose name is shared by another definition
	//                 in the same repository and that some call edge names
	//   "both"      — the union (default)
	Scope string `yaml:"scope,omitempty"`
	// MaxSymbols caps the references requests per repository (default 4000).
	// Reaching it is reported, so a truncated pass is never silent.
	MaxSymbols int `yaml:"max_symbols,omitempty"`
	// MaxRefsPerSymbol drops the answer for a symbol referenced more times
	// than this (default 200): a symbol with a thousand call sites is not an
	// answer to "what calls X", and rewriting a thousand edges for it costs
	// more than it is worth.
	MaxRefsPerSymbol int `yaml:"max_refs_per_symbol,omitempty"`
}

// LSPServerConfig describes one TCP-exposed language server.
type LSPServerConfig struct {
	Addr string `yaml:"addr"` // host:port of the TCP-wrapped LSP server
	// InitOptions is passed verbatim as LSP initializationOptions. Needed by
	// e.g. typescript-language-server, which must be pointed at the global
	// tsserver: {tsserver: {path: /usr/local/lib/node_modules/typescript/lib}}.
	InitOptions map[string]any `yaml:"init_options,omitempty"`
}

// SearchConfig tunes the retrieval pipeline.
type SearchConfig struct {
	// Intent controls query-intent detection ("what calls X" is answered from
	// the code graph rather than text retrieval alone): "auto" (the default)
	// detects the intent from the query phrasing, "off" disables detection.
	// An intent set explicitly on a request is always honoured.
	Intent string `yaml:"intent,omitempty"`
	// Fusion is how the keyword and vector legs are combined: "rrf" (the
	// default) adds reciprocal ranks and ignores the scores themselves;
	// "convex" adds the scores after normalising each leg inside its own
	// result list, which is what makes VectorWeight mean what it says.
	Fusion string `yaml:"fusion,omitempty"`
	// VectorWeight is the vector leg's share under "convex" fusion, between 0
	// and 1; the keyword leg gets the rest. Unset (0) leaves both legs equal.
	VectorWeight float64 `yaml:"vector_weight,omitempty"`
	// NoMergeSpans leaves a file's overlapping chunks in the answer as separate
	// hits. They are merged by default: line windows are cut with an overlap,
	// so the same evidence arrives two or three times and spends slots and
	// bytes that other files could have used.
	NoMergeSpans bool          `yaml:"no_merge_spans,omitempty"`
	Rerank       *RerankConfig `yaml:"rerank,omitempty"`
}

// RerankConfig enables LLM-based reranking of search results: POST
// {base_url}{path} with {query, documents, model}, in the shape TEI, Infinity,
// vLLM, Cohere and Jina all speak. Note that reranking is not part of the
// OpenAI API — an OpenAI-compatible server still exposes it as /v1/rerank, so
// a vLLM deployment needs base_url ending in "/v1".
type RerankConfig struct {
	Enabled        bool   `yaml:"enabled"`
	BaseURL        string `yaml:"base_url"`                  // reranker endpoint base URL
	APIKey         string `yaml:"api_key,omitempty"`         // sent as "Authorization: Bearer <key>"
	Path           string `yaml:"path,omitempty"`            // endpoint path (default "/rerank")
	Model          string `yaml:"model,omitempty"`           // required by vLLM and Cohere
	TopN           int    `yaml:"top_n"`                     // candidates fed to the reranker (default 50)
	TimeoutSeconds int    `yaml:"timeout_seconds,omitempty"` // default 30

	// Instruction is the task description for instruction-aware rerankers such
	// as Qwen3-Reranker. Neither the TEI nor the Cohere request has a field for
	// it, so it is rendered into the query text via QueryTemplate.
	Instruction string `yaml:"instruction,omitempty"`
	// QueryTemplate and DocumentTemplate override how the query and each
	// document are rendered before being sent. Placeholders: {instruction},
	// {query}, {doc}. QueryTemplate defaults to the Qwen3-Reranker format
	// "<Instruct>: {instruction}\n<Query>: {query}" when Instruction is set and
	// to a plain pass-through otherwise; DocumentTemplate defaults to a plain
	// pass-through. Set both explicitly to reproduce a model's full prompt,
	// including chat special tokens.
	QueryTemplate    string `yaml:"query_template,omitempty"`
	DocumentTemplate string `yaml:"document_template,omitempty"`
}

// SummariesConfig enables LLM-generated file and service summaries.
type SummariesConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Provider string `yaml:"provider"` // ollama | openai
	Model    string `yaml:"model"`
	MaxFiles int    `yaml:"max_files"` // per repo per index run (default 30)
	// Files enables the per-file and per-service summaries. Unset means true,
	// so that enabling summaries keeps its historical meaning; set it to false
	// to run the symbol pass alone.
	Files *bool `yaml:"files,omitempty"`
	// Symbols enables one-line summaries of the symbols that sit on a service
	// boundary — the endpoints of HTTP, RPC, messaging and table contracts.
	// They are indexed with the symbol, closing the gap between a question
	// asked in domain language and code named in implementation language.
	Symbols bool `yaml:"symbols"`
	// MaxSymbols caps summarized symbols per repo per index run (default 500).
	MaxSymbols int `yaml:"max_symbols"`
}

// ServerConfig represents server configuration.
type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`

	CORS CORSConfig `yaml:"cors"`

	Auth AuthConfig `yaml:"auth"`

	RateLimit *RateLimitConfig `yaml:"rate_limit"`

	// MaxBodyBytes caps a request body on the general JSON endpoints
	// (default 1 MiB). RAGOTA_MAX_BODY_BYTES overrides it.
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
	// MaxCommitBodyBytes caps POST /repos/{id}/commits, which legitimately
	// carries file contents (default 64 MiB). RAGOTA_MAX_COMMIT_BODY_BYTES
	// overrides it.
	MaxCommitBodyBytes int64 `yaml:"max_commit_body_bytes"`
	// TrustedProxies lists CIDRs or addresses whose X-Forwarded-For header the
	// rate limiter may believe. Empty means the peer address is always used.
	// RAGOTA_TRUSTED_PROXIES (comma-separated) overrides it.
	TrustedProxies []string `yaml:"trusted_proxies"`

	// HTTP server timeouts in seconds; 0 selects the default.
	// WriteTimeoutSeconds bounds handler time as well: /context (retrieval +
	// graph expansion + reranking) and a synchronous clone both outlive the
	// 15s that used to be hardcoded, so the default is 120.
	ReadTimeoutSeconds     int `yaml:"read_timeout_seconds"`
	WriteTimeoutSeconds    int `yaml:"write_timeout_seconds"`
	IdleTimeoutSeconds     int `yaml:"idle_timeout_seconds"`
	ShutdownTimeoutSeconds int `yaml:"shutdown_timeout_seconds"`
}

// CORSConfig represents CORS configuration.
type CORSConfig struct {
	Enabled bool `yaml:"enabled"`
	// Origins lists allowed origins. A single "*" allows every origin: the
	// middleware must echo the request's Origin back (a literal "*" never
	// matches a real Origin header) and set "Vary: Origin".
	Origins []string `yaml:"origins"`
}

// AuthConfig represents authentication configuration.
type AuthConfig struct {
	Type    string   `yaml:"type"` // none, api_key
	APIKeys []string `yaml:"api_keys,omitempty"`
}

// RateLimitConfig represents rate limiting configuration.
type RateLimitConfig struct {
	Enabled           bool `yaml:"enabled"`
	RequestsPerMinute int  `yaml:"requests_per_minute"`
	Burst             int  `yaml:"burst"`
}

// StorageConfig represents storage configuration.
type StorageConfig struct {
	SQLite   *SQLiteStorageConfig   `yaml:"sqlite,omitempty"`
	Postgres *PostgresStorageConfig `yaml:"postgres,omitempty"`
	Qdrant   *QdrantStorageConfig   `yaml:"qdrant,omitempty"`
}

// PostgresStorageConfig represents PostgreSQL storage configuration.
type PostgresStorageConfig struct {
	DSN      string `yaml:"dsn"` // e.g. postgres://user:pass@host:5432/ragota
	PoolSize int    `yaml:"pool_size"`
}

// SQLiteStorageConfig represents SQLite storage configuration.
type SQLiteStorageConfig struct {
	Path     string `yaml:"path"`
	PoolSize int    `yaml:"pool_size"`
}

// QdrantStorageConfig represents Qdrant storage configuration.
type QdrantStorageConfig struct {
	URL              string `yaml:"url"`
	APIKey           string `yaml:"api_key,omitempty"`
	CollectionPrefix string `yaml:"collection_prefix"`
	// Mode is docker_embedded (default, a local instance without auth) or
	// cloud (a managed instance, where api_key is required).
	Mode string `yaml:"mode,omitempty"`
}

// IndexesConfig represents indexes configuration.
type IndexesConfig struct {
	// Workers is the number of parallel workers used during indexing
	// (file read/hash stage and AST parse stage).
	// 0 means runtime.NumCPU(); the effective value is capped at 32.
	// Negative values are rejected by validation.
	Workers int `yaml:"workers"`

	// Distributed enables the shared indexing job queue: several ragota
	// instances over one database (PostgreSQL in production) split indexing
	// work by claiming jobs atomically. Off by default; in single-instance
	// mode indexing runs in-process exactly as before.
	Distributed bool `yaml:"distributed"`

	// JobPollSeconds is the poll interval of the distributed job worker
	// (also used as the heartbeat interval). Default 3.
	JobPollSeconds int `yaml:"job_poll_seconds"`

	// StaleJobSeconds is the heartbeat age after which a running job is
	// considered abandoned and requeued. Default 120.
	StaleJobSeconds int `yaml:"stale_job_seconds"`

	AST    *ASTIndexConfig    `yaml:"ast,omitempty"`
	Vector *VectorIndexConfig `yaml:"vector,omitempty"`
	BM25   *BM25IndexConfig   `yaml:"bm25,omitempty"`
}

// ASTIndexConfig represents AST index configuration.
type ASTIndexConfig struct {
	Enabled bool `yaml:"enabled"`
	// Languages restricts the parsers the AST indexer registers. Empty means
	// every supported language (see ASTLanguages); files of unregistered
	// languages are skipped.
	Languages []string `yaml:"languages"`
}

// VectorIndexConfig represents vector index configuration.
type VectorIndexConfig struct {
	Enabled  bool           `yaml:"enabled"`
	Embedder EmbedderConfig `yaml:"embedder"`
	Chunking ChunkingConfig `yaml:"chunking"`
	// Exclude drops files from the vector channel when their repo-relative
	// path contains one of these case-insensitive substrings (e.g. "/test/",
	// "_test.", "/vendor/"). Empty by default: everything indexed elsewhere
	// is embedded. The knob exists because embedding is the expensive channel
	// and test scaffolding or generated doc sites can be half a repository;
	// BM25 and AST indexing are unaffected, so excluded files remain
	// searchable by keyword.
	Exclude []string `yaml:"exclude,omitempty"`
}

// EmbedderConfig represents embedder model configuration.
type EmbedderConfig struct {
	Provider string `yaml:"provider"` // ollama, openai
	Model    string `yaml:"model"`
	// BaseURL overrides the provider endpoint for embeddings only; empty
	// falls back to models.providers.<provider>.base_url. Honoured for both
	// providers.
	BaseURL string `yaml:"base_url,omitempty"`
	// QueryInstruction wraps the search query — never a document — before it
	// is embedded, as "Instruct: {instruction}\nQuery: {query}": the form
	// instruction-aware embedders (Qwen3-Embedding) are trained on. Documents
	// stay bare, which is the other half of that training, so flipping this
	// key needs no reindex — it is a query-time setting.
	QueryInstruction string `yaml:"query_instruction,omitempty"`
	BatchSize        int    `yaml:"batch_size"`
	Dimensions       int    `yaml:"dimensions,omitempty"`
	// MaxChars caps the text sent to the embedder per chunk, in bytes
	// (0 = default 4096). Every embedding model has a context limit, and
	// some servers (llama.cpp) reject the whole batch when one input exceeds
	// it — one minified file then fails its repository's index. Servers
	// meter tokens, not bytes, and dense scripts (Arabic, CJK) run ~2
	// bytes/token where code runs ~4, so the default stays under a
	// 2048-token context for any script; raise it when the serving context
	// is larger. Chunks are truncated for embedding only; the stored chunk
	// text is untouched.
	MaxChars int `yaml:"max_chars,omitempty"`
	// Concurrency is how many embed requests may be in flight at once
	// (0 = default 2). The indexer also packs small files together into
	// full batches; without both, a GPU-backed endpoint idles between
	// per-file requests of a handful of chunks each.
	Concurrency int `yaml:"concurrency,omitempty"`
}

// ChunkingConfig represents chunking configuration.
type ChunkingConfig struct {
	// Method is window | semantic | hybrid | cards. "semantic" and "hybrid"
	// are implemented for Go only; every other language falls back to window
	// chunking.
	Method      string `yaml:"method"`
	WindowLines int    `yaml:"window_lines"`
	Overlap     int    `yaml:"overlap"`
}

// BM25IndexConfig represents BM25 index configuration.
type BM25IndexConfig struct {
	Enabled bool `yaml:"enabled"`
	// Path is the on-disk directory of the Bleve index
	// (default ~/.ragota/data/bm25). The RAGOTA_BM25_PATH environment
	// variable overrides it.
	Path string `yaml:"path"`
	// K1 and B are the BM25 scoring parameters (defaults 1.2 / 0.75). They are
	// applied when an index is created, so changing them on an existing index
	// takes effect only after it is rebuilt.
	K1 float64 `yaml:"k1,omitempty"`
	B  float64 `yaml:"b,omitempty"`
	// NoCompact skips the single-segment merge that ends a full index pass.
	// The merge is what makes a score depend on the indexed content alone
	// rather than on the segment layout the pass happened to leave behind; it
	// costs roughly 4% of the pass and transiently needs room for a second
	// copy of the index. Setting this trades reproducible scores for that
	// disk and time.
	NoCompact bool `yaml:"no_compact,omitempty"`
	// SplitIdentifiers indexes every chunk a second time through a code-aware
	// analyser, so that getUserByID and login_attempt are also findable as the
	// words they are made of. Without it the keyword leg matches whole
	// identifiers only. It shapes the index: turning it on takes effect after
	// a forced reindex, and the server says so at startup when the two
	// disagree.
	SplitIdentifiers bool `yaml:"split_identifiers,omitempty"`
	// SplitBoost weights the code-aware view against the literal one (default
	// 0.35). Query-time: it takes effect on restart and sweeps without a
	// reindex. At equal weight the split view measured as a regression — an
	// identifier taken apart contributes ordinary English, which matches many
	// documents weakly and outvotes the literal match it should supplement.
	SplitBoost float64 `yaml:"split_boost,omitempty"`
	// IndexPaths makes a document's path searchable as words, so that
	// "checkout service" can reach src/checkout_service/main.go. Without it the
	// path is one indivisible term and the indexed text holds no path at all.
	//
	// Unset means true. Measured on the full 103-question set: recall@10
	// 0.650 -> 0.709, recall@5 0.592 -> 0.680, never found 29 -> 24, against
	// recall@1 0.447 -> 0.408 — the answer lands in the list more often and at
	// the very top slightly less. It shapes the index the same way
	// SplitIdentifiers does, so an index built before this gains the field on
	// its next forced reindex, and the server says so at startup until then.
	IndexPaths *bool `yaml:"index_paths,omitempty"`
	// PathBoost weights the path against the text (default 0.3). Unlike the
	// two settings above it is a query-time weight, so it takes effect on
	// restart and can be swept without reindexing.
	PathBoost float64 `yaml:"path_boost,omitempty"`
}

// ModelsConfig represents AI models configuration.
type ModelsConfig struct {
	Providers map[string]ProviderConfig `yaml:"providers"`
	// Assistant is the auxiliary LLM used during indexing and retrieval:
	// repo reconnaissance before indexing, ambiguous-edge disambiguation,
	// and query rewriting. All of its outputs are marked source=llm.
	Assistant *AssistantConfig `yaml:"assistant,omitempty"`
}

// AssistantConfig configures the auxiliary LLM.
type AssistantConfig struct {
	Provider string `yaml:"provider"` // openai | ollama (default ollama)
	BaseURL  string `yaml:"base_url"` // base path of the LLM endpoint
	APIKey   string `yaml:"api_key,omitempty"`
	Model    string `yaml:"model"`
	// Recon and Disambiguate default true when the assistant is configured;
	// both are measured wins. QueryRewrite defaults false: it was measured as a
	// retrieval regression (see tools/eval/README.md), so it is opt-in.
	Recon        *bool `yaml:"recon,omitempty"`         // pre-index repo structure pass
	Disambiguate *bool `yaml:"disambiguate,omitempty"`  // low-confidence edge resolution
	QueryRewrite *bool `yaml:"query_rewrite,omitempty"` // rewrite queries before retrieval (default off: measured regression)
}

// ProviderConfig represents an AI provider configuration.
type ProviderConfig struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key,omitempty"`
}

// ReposConfig represents repositories configuration.
type ReposConfig struct {
	Sources ReposSourcesConfig `yaml:"sources"`
	Ignore  []string           `yaml:"ignore,omitempty"`
	// UseGitignore applies each checkout's own .gitignore and
	// .git/info/exclude on top of Ignore. It defaults to true — a developer
	// reads "gitignored" as "not my code" — and exists so that a repository
	// whose .gitignore hides something worth indexing can still be indexed.
	// nil means unset, which is the default; RAGOTA_USE_GITIGNORE overrides it.
	UseGitignore *bool `yaml:"use_gitignore,omitempty"`
}

// ReposSourcesConfig represents repository sources configuration.
type ReposSourcesConfig struct {
	Local *LocalSourceConfig `yaml:"local,omitempty"`
	Git   *GitSourceConfig   `yaml:"git,omitempty"`
}

// LocalSourceConfig represents local source configuration.
type LocalSourceConfig struct {
	Enabled bool     `yaml:"enabled"`
	Paths   []string `yaml:"paths,omitempty"`
}

// GitSourceConfig represents git source configuration.
type GitSourceConfig struct {
	Enabled bool           `yaml:"enabled"`
	WorkDir string         `yaml:"work_dir"`
	Auth    *GitAuthConfig `yaml:"auth,omitempty"`
}

// GitAuthConfig represents git authentication configuration. Empty fields fall
// back to the GITHUB_TOKEN / GITLAB_TOKEN environment variables.
type GitAuthConfig struct {
	GitHubToken string `yaml:"github_token,omitempty"`
	GitLabToken string `yaml:"gitlab_token,omitempty"`
}
