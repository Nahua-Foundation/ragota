package config

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrMissingEnvVar is returned when an environment variable referenced in config is not set.
var ErrMissingEnvVar = errors.New("missing environment variable")

// ErrBadEnvRef is returned for a "$" that is neither an escaped "$$" nor a
// well-formed "${NAME}" reference.
var ErrBadEnvRef = errors.New("malformed environment reference")

// DefaultConfigPath is used when neither --config nor RAGOTA_CONFIG is set.
const DefaultConfigPath = "config.yaml"

// DefaultBM25Path is the fallback location of the BM25 index directory.
const DefaultBM25Path = "~/.ragota-core/data/bm25"

// DefaultSQLitePath is the fallback location of the SQLite metadata database.
const DefaultSQLitePath = "~/.ragota-core/data/ragota.db"

// Request body caps used when server.max_body_bytes / max_commit_body_bytes
// are unset. Commit ingestion carries file contents, hence its own limit.
const (
	DefaultMaxBodyBytes       int64 = 1 << 20  // 1 MiB
	DefaultMaxCommitBodyBytes int64 = 64 << 20 // 64 MiB
)

// Enum values accepted by Validate.
var (
	authTypes       = []string{"none", "api_key"}
	chunkingMethods = []string{"window", "cards"}
	// deprecatedChunking maps the retired Go-only methods onto symbol cards,
	// which supersede them for every language. Old configs keep working.
	deprecatedChunking = map[string]string{"semantic": "cards", "hybrid": "cards"}
	modelProviders     = []string{"ollama", "openai"}
	qdrantModes        = []string{"docker_embedded", "cloud"}
	logLevels          = []string{"debug", "info", "warn", "error"}
	astLanguagesAll    = []string{"go", "java", "kotlin", "csharp", "typescript", "javascript", "python", "proto", "sql", "yaml", "json", "properties"}
	lspLanguagesAll    = []string{"go", "java", "csharp", "typescript"}
)

// ASTLanguages returns the languages the AST indexer can register parsers for.
func ASTLanguages() []string { return append([]string(nil), astLanguagesAll...) }

// Load loads configuration from a YAML file.
// It expands environment references of the form ${VAR_NAME}; "$$" is a literal
// "$". Returns an error if any referenced environment variable is not set.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	expanded, err := expandEnv(string(data))
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.applyDefaults()

	return &cfg, nil
}

// CheckUnknownKeys re-parses the file strictly, rejecting any key that does not
// map to a config field. Load stays lenient — an unknown key there is silently
// ignored for backward compatibility — so this is the check --check-config runs
// to catch a typo like "rate_limt:" before it ships as a silently-ignored
// setting. Map-valued sections (e.g. models.providers) keep their free-form
// keys; only struct fields are checked.
func CheckUnknownKeys(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	expanded, err := expandEnv(string(data))
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(strings.NewReader(expanded))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return fmt.Errorf("unknown or misspelled config key: %w", err)
	}
	return nil
}

// expandEnv substitutes ${NAME} with the environment value, turns "$$" into a
// literal "$" and leaves every other "$" untouched. A bare "$NAME" is rejected
// instead of being silently expanded or mangled: os.Expand would eat the name,
// which corrupts DSN passwords and API keys that legitimately contain "$".
// YAML comments are copied verbatim, so a commented-out example may reference
// variables nobody has set (config.example.yaml is full of them).
func expandEnv(s string) (string, error) {
	var b strings.Builder
	b.Grow(len(s))

	missing := map[string]bool{}
	var badRefs []string

	for _, line := range splitLinesKeepEnds(s) {
		code, comment := splitComment(line)
		if err := expandInto(&b, code, missing, &badRefs); err != nil {
			return "", err
		}
		b.WriteString(comment)
	}

	if len(badRefs) > 0 {
		return "", fmt.Errorf("%w: %s (use ${NAME} to reference an environment variable and $$ for a literal $)",
			ErrBadEnvRef, strings.Join(dedup(badRefs), ", "))
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("%w: %s", ErrMissingEnvVar, strings.Join(slices.Sorted(maps.Keys(missing)), ", "))
	}
	return b.String(), nil
}

// splitLinesKeepEnds splits on "\n" keeping the separator, so expansion never
// rewrites the document's line endings.
func splitLinesKeepEnds(s string) []string {
	var out []string
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}

// splitComment splits a YAML line into its code part and its trailing comment
// (a "#" outside quotes, at line start or after whitespace).
func splitComment(line string) (code, comment string) {
	var inSingle, inDouble bool
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '#' && !inSingle && !inDouble:
			if i == 0 || line[i-1] == ' ' || line[i-1] == '\t' {
				return line[:i], line[i:]
			}
		}
	}
	return line, ""
}

// expandInto expands one comment-free fragment, collecting problems instead of
// failing at the first one.
func expandInto(b *strings.Builder, s string, missing map[string]bool, badRefs *[]string) error {
	for i := 0; i < len(s); {
		c := s[i]
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		switch {
		case i+1 < len(s) && s[i+1] == '$':
			b.WriteByte('$')
			i += 2
		case i+1 < len(s) && s[i+1] == '{':
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				*badRefs = append(*badRefs, s[i:])
				i = len(s)
				break
			}
			name := s[i+2 : i+2+end]
			i += 2 + end + 1
			if name == "" || !isEnvName(name) {
				*badRefs = append(*badRefs, "${"+name+"}")
				break
			}
			val, ok := os.LookupEnv(name)
			if !ok {
				missing[name] = true
				break
			}
			b.WriteString(val)
		case i+1 < len(s) && isEnvNameByte(s[i+1]):
			end := i + 1
			for end < len(s) && isEnvNameByte(s[end]) {
				end++
			}
			*badRefs = append(*badRefs, s[i:end])
			i = end
		default:
			// A literal "$" (end of value, "$ ", "$/", ...) is kept as is.
			b.WriteByte('$')
			i++
		}
	}
	return nil
}

func isEnvNameByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isEnvName(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isEnvNameByte(s[i]) {
			return false
		}
	}
	return true
}

// splitList splits a comma-separated environment list, dropping empty entries.
func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func dedup(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func boolPtr(v bool) *bool { return &v }

// envBool reads a boolean environment override, reporting whether the variable
// was set to something it understands. An unset or unparseable value leaves
// the configured setting alone rather than silently reading as false.
func envBool(name string) (bool, bool) {
	v, ok := os.LookupEnv(name)
	if !ok {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}

// applyDefaults applies default values to the configuration.
func (c *Config) applyDefaults() {
	// Server defaults
	if c.Server.Host == "" {
		c.Server.Host = "127.0.0.1"
	}
	if c.Server.Port == 0 {
		c.Server.Port = 8080
	}
	if c.Server.CORS.Origins == nil {
		c.Server.CORS.Origins = []string{"*"}
	}
	if c.Server.ReadTimeoutSeconds == 0 {
		c.Server.ReadTimeoutSeconds = 30
	}
	// /context runs retrieval + graph expansion + an optional reranker, which
	// legitimately outlives a 15s write deadline.
	if c.Server.WriteTimeoutSeconds == 0 {
		c.Server.WriteTimeoutSeconds = 120
	}
	if c.Server.IdleTimeoutSeconds == 0 {
		c.Server.IdleTimeoutSeconds = 60
	}
	if c.Server.ShutdownTimeoutSeconds == 0 {
		c.Server.ShutdownTimeoutSeconds = 10
	}
	if c.Server.MaxBodyBytes == 0 {
		c.Server.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if c.Server.MaxCommitBodyBytes == 0 {
		c.Server.MaxCommitBodyBytes = DefaultMaxCommitBodyBytes
	}
	// The environment wins over the file so an operator can raise a limit
	// without editing (or rebuilding around) a mounted config.
	if v, err := strconv.ParseInt(os.Getenv("RAGOTA_MAX_BODY_BYTES"), 10, 64); err == nil && v > 0 {
		c.Server.MaxBodyBytes = v
	}
	if v, err := strconv.ParseInt(os.Getenv("RAGOTA_MAX_COMMIT_BODY_BYTES"), 10, 64); err == nil && v > 0 {
		c.Server.MaxCommitBodyBytes = v
	}
	if v := os.Getenv("RAGOTA_TRUSTED_PROXIES"); v != "" {
		c.Server.TrustedProxies = splitList(v)
	}

	if c.Log.Level == "" {
		c.Log.Level = "info"
	}

	// Repos defaults. The environment wins over the file for the same reason
	// it does above, and for one more: the zero-configuration `--source DIR`
	// run has no file to edit, and it is exactly the run that meets a
	// .gitignore hiding something the user wanted indexed.
	if c.Repos.UseGitignore == nil {
		c.Repos.UseGitignore = boolPtr(true)
	}
	if v, ok := envBool("RAGOTA_USE_GITIGNORE"); ok {
		c.Repos.UseGitignore = &v
	}
	// Applying the config is what puts the setting in force: every matcher —
	// the indexing walk's, the watcher's, the one a pushed commit builds
	// inside the service package — reads it from there. See config.SetUseGitignore.
	SetUseGitignore(*c.Repos.UseGitignore)

	// Storage defaults
	if c.Storage.SQLite != nil && c.Storage.SQLite.Path == "" {
		c.Storage.SQLite.Path = DefaultSQLitePath
	}
	if c.Storage.SQLite != nil && c.Storage.SQLite.PoolSize == 0 {
		c.Storage.SQLite.PoolSize = 10
	}
	if c.Storage.Qdrant != nil && c.Storage.Qdrant.CollectionPrefix == "" {
		c.Storage.Qdrant.CollectionPrefix = "ragota_"
	}
	if c.Storage.Qdrant != nil && c.Storage.Qdrant.Mode == "" {
		c.Storage.Qdrant.Mode = "docker_embedded"
	}

	// Index defaults
	if c.Indexes.JobPollSeconds == 0 {
		c.Indexes.JobPollSeconds = 3
	}
	if c.Indexes.StaleJobSeconds == 0 {
		c.Indexes.StaleJobSeconds = 120
	}
	if c.Indexes.AST != nil && len(c.Indexes.AST.Languages) == 0 {
		c.Indexes.AST.Languages = ASTLanguages()
	}
	if c.Indexes.BM25 != nil && c.Indexes.BM25.Path == "" {
		c.Indexes.BM25.Path = DefaultBM25Path
	}
	if c.Indexes.Vector != nil && c.Indexes.Vector.Embedder.BatchSize == 0 {
		c.Indexes.Vector.Embedder.BatchSize = 64
	}
	if c.Indexes.Vector != nil && c.Indexes.Vector.Chunking.WindowLines == 0 {
		c.Indexes.Vector.Chunking.WindowLines = 60
	}
	if c.Indexes.Vector != nil && c.Indexes.Vector.Chunking.Overlap == 0 {
		c.Indexes.Vector.Chunking.Overlap = 10
	}
	// The Go-only "semantic"/"hybrid" methods were replaced by symbol cards,
	// which do the same thing for every language. Normalize instead of
	// rejecting so existing configs keep loading.
	if c.Indexes.Vector != nil {
		if to, ok := deprecatedChunking[c.Indexes.Vector.Chunking.Method]; ok {
			c.rawChunkingMethod = c.Indexes.Vector.Chunking.Method
			c.Indexes.Vector.Chunking.Method = to
		}
	}
}

// Validate validates the configuration and returns all validation errors.
func (c *Config) Validate() error {
	var errs []error

	errs = append(errs, c.validateServer()...)
	errs = append(errs, c.validateStorage()...)
	errs = append(errs, c.validateIndexes()...)
	errs = append(errs, c.validateModels()...)
	errs = append(errs, c.validateLSP()...)

	if c.Search != nil && c.Search.Rerank != nil && c.Search.Rerank.Enabled {
		if c.Search.Rerank.BaseURL == "" {
			errs = append(errs, fmt.Errorf("search.rerank.base_url is required when reranking is enabled"))
		}
		if c.Search.Rerank.TimeoutSeconds < 0 {
			errs = append(errs, fmt.Errorf("search.rerank.timeout_seconds must not be negative"))
		}
	}

	if c.Repos.Sources.Git != nil && c.Repos.Sources.Git.Enabled {
		if c.Repos.Sources.Git.WorkDir == "" {
			errs = append(errs, fmt.Errorf("repos.sources.git.work_dir is required when git source is enabled"))
		}
		if c.Repos.Sources.Git.Auth != nil {
			if c.Repos.Sources.Git.Auth.GitHubToken == "" && c.Repos.Sources.Git.Auth.GitLabToken == "" {
				errs = append(errs, fmt.Errorf("repos.sources.git.auth requires at least one of github_token or gitlab_token"))
			}
		}
	}

	errs = compact(errs)
	if len(errs) > 0 {
		return fmt.Errorf("validation failed: %w", errors.Join(errs...))
	}

	return nil
}

func (c *Config) validateServer() []error {
	var errs []error

	// An unrecognised auth type used to disable authentication silently:
	// internal/api compares against "api_key" and anything else means "open".
	if !oneOf(c.Server.Auth.Type, append(authTypes, "")) {
		errs = append(errs, fmt.Errorf("server.auth.type must be one of %s (got %q)", strings.Join(authTypes, ", "), c.Server.Auth.Type))
	}
	if c.Server.Auth.Type == "api_key" {
		if len(nonEmpty(c.Server.Auth.APIKeys)) == 0 {
			errs = append(errs, fmt.Errorf("server.auth.api_keys must contain at least one non-empty key when server.auth.type is api_key"))
		}
	}
	if c.Server.Auth.Type != "api_key" && len(c.Server.Auth.APIKeys) > 0 {
		errs = append(errs, fmt.Errorf("server.auth.api_keys is set but server.auth.type is %q: the keys are ignored", orDefault(c.Server.Auth.Type, "none")))
	}

	if c.Server.CORS.Enabled {
		if len(c.Server.CORS.Origins) == 0 {
			errs = append(errs, fmt.Errorf("server.cors.origins must not be empty when CORS is enabled"))
		}
		for _, o := range c.Server.CORS.Origins {
			if o == "*" {
				continue // wildcard: every origin is echoed back
			}
			if o == "" {
				errs = append(errs, fmt.Errorf("server.cors.origins must not contain an empty entry"))
				continue
			}
			if !strings.HasPrefix(o, "http://") && !strings.HasPrefix(o, "https://") {
				errs = append(errs, fmt.Errorf("server.cors.origins entry %q must be \"*\" or a scheme-qualified origin such as https://app.example.com", o))
			}
			if strings.HasSuffix(o, "/") {
				errs = append(errs, fmt.Errorf("server.cors.origins entry %q must not end with \"/\": browsers send the Origin header without a trailing slash", o))
			}
		}
	}

	if c.Server.Port < 0 || c.Server.Port > 65535 {
		errs = append(errs, fmt.Errorf("server.port must be between 0 and 65535"))
	}
	if c.Server.MaxBodyBytes < 0 {
		errs = append(errs, fmt.Errorf("server.max_body_bytes must not be negative"))
	}
	if c.Server.MaxCommitBodyBytes < 0 {
		errs = append(errs, fmt.Errorf("server.max_commit_body_bytes must not be negative"))
	}
	for _, p := range c.Server.TrustedProxies {
		if strings.TrimSpace(p) == "" {
			errs = append(errs, fmt.Errorf("server.trusted_proxies must not contain an empty entry"))
			continue
		}
		if _, err := netip.ParsePrefix(strings.TrimSpace(p)); err != nil {
			if _, err := netip.ParseAddr(strings.TrimSpace(p)); err != nil {
				errs = append(errs, fmt.Errorf("server.trusted_proxies entry %q is neither an IP address nor a CIDR", p))
			}
		}
	}
	for name, v := range map[string]int{
		"server.read_timeout_seconds":     c.Server.ReadTimeoutSeconds,
		"server.write_timeout_seconds":    c.Server.WriteTimeoutSeconds,
		"server.idle_timeout_seconds":     c.Server.IdleTimeoutSeconds,
		"server.shutdown_timeout_seconds": c.Server.ShutdownTimeoutSeconds,
	} {
		if v < 0 {
			errs = append(errs, fmt.Errorf("%s must not be negative", name))
		}
	}

	if !oneOf(c.Log.Level, append(logLevels, "")) {
		errs = append(errs, fmt.Errorf("log.level must be one of %s (got %q)", strings.Join(logLevels, ", "), c.Log.Level))
	}
	if !oneOf(c.Log.Format, []string{"", "text", "json"}) {
		errs = append(errs, fmt.Errorf("log.format must be text or json (got %q)", c.Log.Format))
	}

	return errs
}

func (c *Config) validateStorage() []error {
	var errs []error

	if c.Storage.SQLite == nil && c.Storage.Postgres == nil {
		errs = append(errs, fmt.Errorf("a relational storage backend (storage.sqlite or storage.postgres) must be configured"))
	}

	if c.Storage.Postgres != nil && c.Storage.Postgres.DSN == "" {
		errs = append(errs, fmt.Errorf("storage.postgres.dsn is required when postgres storage is configured"))
	}

	if c.Storage.SQLite != nil {
		if c.Storage.SQLite.Path == "" {
			errs = append(errs, fmt.Errorf("storage.sqlite.path is required when sqlite storage is configured"))
		}
		if c.Storage.SQLite.PoolSize < 1 {
			errs = append(errs, fmt.Errorf("storage.sqlite.pool_size must be at least 1"))
		}
	}

	if c.Storage.Qdrant != nil {
		if c.Storage.Qdrant.URL == "" {
			errs = append(errs, fmt.Errorf("storage.qdrant.url is required when qdrant storage is configured"))
		}
		if !oneOf(c.Storage.Qdrant.Mode, append(qdrantModes, "")) {
			errs = append(errs, fmt.Errorf("storage.qdrant.mode must be %q or %q", "docker_embedded", "cloud"))
		}
		// Qdrant Cloud rejects unauthenticated requests, so a missing key there
		// only shows up as 401s during the first upsert.
		if c.Storage.Qdrant.Mode == "cloud" && c.Storage.Qdrant.APIKey == "" {
			errs = append(errs, fmt.Errorf("storage.qdrant.api_key is required when storage.qdrant.mode is cloud"))
		}
	}

	return errs
}

// Warnings returns configuration that is valid but very likely not what the
// operator intended. They are logged at startup and printed by --check-config;
// they never prevent the process from running.
func (c *Config) Warnings() []string {
	var out []string

	// SQLite implements the job queue, so a distributed pair over one shared
	// file works — but only when that file really is shared.
	if c.Indexes.Distributed && c.Storage.Postgres == nil {
		out = append(out, "indexes.distributed is enabled without storage.postgres: the job queue is shared through the database, and a SQLite file is only shared between instances that mount the very same file")
	}
	if c.Server.CORS.Enabled && oneOf("*", c.Server.CORS.Origins) && c.Server.Auth.Type == "api_key" {
		out = append(out, "server.cors.origins is \"*\" while API key auth is on: any web page can drive the API with a key the browser holds")
	}
	if c.Indexes.Vector != nil && c.rawChunkingMethod != "" {
		out = append(out, fmt.Sprintf("indexes.vector.chunking.method %q is deprecated and was applied as \"cards\": symbol cards replace it and cover every language, not just Go", c.rawChunkingMethod))
	}
	if c.Server.Auth.Type == "" || c.Server.Auth.Type == "none" {
		if c.Server.Host != "" && c.Server.Host != "127.0.0.1" && c.Server.Host != "localhost" && c.Server.Host != "::1" {
			out = append(out, fmt.Sprintf("server.auth.type is none while server.host is %q: the API is reachable without authentication", c.Server.Host))
		}
	}
	if c.LSP != nil && c.LSP.Enabled && c.LSP.HostRoot == "" {
		out = append(out, "lsp.host_root/mount_root are empty: host paths are sent to the language servers unchanged, which only works when they see the very same filesystem layout")
	}

	return out
}

func (c *Config) validateIndexes() []error {
	var errs []error

	if c.Indexes.Workers < 0 {
		errs = append(errs, fmt.Errorf("indexes.workers must not be negative"))
	}

	if c.Indexes.Distributed {
		if c.Indexes.JobPollSeconds <= 0 {
			errs = append(errs, fmt.Errorf("indexes.job_poll_seconds must be positive when indexes.distributed is enabled"))
		}
		if c.Indexes.StaleJobSeconds <= 0 {
			errs = append(errs, fmt.Errorf("indexes.stale_job_seconds must be positive when indexes.distributed is enabled"))
		}
	}

	if c.Indexes.AST != nil && c.Indexes.AST.Enabled {
		for _, lang := range c.Indexes.AST.Languages {
			if !oneOf(lang, astLanguagesAll) {
				errs = append(errs, fmt.Errorf("indexes.ast.languages entry %q is not supported (supported: %s)", lang, strings.Join(astLanguagesAll, ", ")))
			}
		}
	}

	if c.Indexes.Vector != nil && c.Indexes.Vector.Enabled {
		if c.Indexes.Vector.Embedder.Provider == "" {
			errs = append(errs, fmt.Errorf("indexes.vector.embedder.provider is required when vector index is enabled"))
		} else if !oneOf(c.Indexes.Vector.Embedder.Provider, modelProviders) {
			errs = append(errs, fmt.Errorf("indexes.vector.embedder.provider must be one of %s (got %q)", strings.Join(modelProviders, ", "), c.Indexes.Vector.Embedder.Provider))
		}
		if c.Indexes.Vector.Embedder.Model == "" {
			errs = append(errs, fmt.Errorf("indexes.vector.embedder.model is required when vector index is enabled"))
		}
		if c.Indexes.Vector.Embedder.BatchSize < 0 {
			errs = append(errs, fmt.Errorf("indexes.vector.embedder.batch_size must not be negative"))
		}
		// Without a vector store the vector indexer builds and then dies at
		// startup with "vector store not available".
		if c.Storage.Qdrant == nil {
			errs = append(errs, fmt.Errorf("storage.qdrant must be configured when indexes.vector is enabled: the vector index has no other store"))
		}
		if m := c.Indexes.Vector.Chunking.Method; m != "" && !oneOf(m, chunkingMethods) {
			errs = append(errs, fmt.Errorf("indexes.vector.chunking.method must be one of %s (got %q)", strings.Join(chunkingMethods, ", "), m))
		}
		if c.Indexes.Vector.Chunking.WindowLines < 0 {
			errs = append(errs, fmt.Errorf("indexes.vector.chunking.window_lines must not be negative"))
		}
		if c.Indexes.Vector.Chunking.Overlap >= c.Indexes.Vector.Chunking.WindowLines && c.Indexes.Vector.Chunking.WindowLines > 0 {
			errs = append(errs, fmt.Errorf("indexes.vector.chunking.overlap must be smaller than window_lines"))
		}
	}

	return errs
}

func (c *Config) validateModels() []error {
	var errs []error

	if c.Summaries != nil && c.Summaries.Enabled {
		if p := c.Summaries.Provider; p != "" && !oneOf(p, modelProviders) {
			errs = append(errs, fmt.Errorf("summaries.provider must be one of %s (got %q)", strings.Join(modelProviders, ", "), p))
		}
		if c.Summaries.Model == "" {
			errs = append(errs, fmt.Errorf("summaries.model is required when summaries are enabled"))
		}
	}

	if a := c.Models.Assistant; a != nil {
		if a.Provider != "" && !oneOf(a.Provider, modelProviders) {
			errs = append(errs, fmt.Errorf("models.assistant.provider must be one of %s (got %q)", strings.Join(modelProviders, ", "), a.Provider))
		}
		if a.Model == "" {
			errs = append(errs, fmt.Errorf("models.assistant.model is required when the assistant is configured"))
		}
		if a.Provider == "openai" && a.APIKey == "" && isPublicOpenAI(a.BaseURL) {
			errs = append(errs, fmt.Errorf("models.assistant.api_key is required for the public OpenAI endpoint (set base_url for a self-hosted OpenAI-compatible gateway)"))
		}
	}

	// The public OpenAI endpoint always needs a key; a self-hosted
	// OpenAI-compatible gateway (vLLM, LiteLLM) usually does not.
	openai, hasOpenAI := c.Models.Providers["openai"]
	if hasOpenAI && openai.APIKey == "" && isPublicOpenAI(openai.BaseURL) {
		usesOpenAI := (c.Indexes.Vector != nil && c.Indexes.Vector.Enabled && c.Indexes.Vector.Embedder.Provider == "openai") ||
			(c.Summaries != nil && c.Summaries.Enabled && c.Summaries.Provider == "openai")
		if usesOpenAI {
			errs = append(errs, fmt.Errorf("models.providers.openai.api_key is required for the public OpenAI endpoint (set base_url for a self-hosted OpenAI-compatible gateway)"))
		}
	}

	return errs
}

func (c *Config) validateLSP() []error {
	if c.LSP == nil || !c.LSP.Enabled {
		return nil
	}
	var errs []error

	// An enabled LSP pass with no servers skips every file and still reports
	// success, which reads exactly like a working pass.
	if len(c.LSP.Servers) == 0 {
		errs = append(errs, fmt.Errorf("lsp.servers must not be empty when lsp.enabled is true"))
	}
	for _, lang := range sortedMapKeys(c.LSP.Servers) {
		srv := c.LSP.Servers[lang]
		if !oneOf(lang, lspLanguagesAll) {
			errs = append(errs, fmt.Errorf("lsp.servers has no support for language %q (supported: %s)", lang, strings.Join(lspLanguagesAll, ", ")))
		}
		if srv.Addr == "" {
			errs = append(errs, fmt.Errorf("lsp.servers.%s.addr is required", lang))
			continue
		}
		if !strings.Contains(srv.Addr, ":") {
			errs = append(errs, fmt.Errorf("lsp.servers.%s.addr must be host:port (got %q)", lang, srv.Addr))
		}
	}

	// A wrong host_root/mount_root mapping makes every server answer with
	// empty results while the pass still reports success.
	hostSet, mountSet := c.LSP.HostRoot != "", c.LSP.MountRoot != ""
	switch {
	case hostSet != mountSet:
		errs = append(errs, fmt.Errorf("lsp.host_root and lsp.mount_root must be set together (host_root=%q mount_root=%q)", c.LSP.HostRoot, c.LSP.MountRoot))
	case hostSet && mountSet:
		if !filepath.IsAbs(c.LSP.HostRoot) {
			errs = append(errs, fmt.Errorf("lsp.host_root must be an absolute path (got %q)", c.LSP.HostRoot))
		}
		if !strings.HasPrefix(c.LSP.MountRoot, "/") {
			errs = append(errs, fmt.Errorf("lsp.mount_root must be an absolute container path (got %q)", c.LSP.MountRoot))
		}
	}

	if c.LSP.TimeoutSeconds < 0 {
		errs = append(errs, fmt.Errorf("lsp.timeout_seconds must not be negative"))
	}
	if calls := c.LSP.Calls; calls != nil && calls.Enabled {
		if calls.Scope != "" && !oneOf(calls.Scope, lspCallScopes) {
			errs = append(errs, fmt.Errorf("lsp.calls.scope must be one of %s (got %q)",
				strings.Join(lspCallScopes, ", "), calls.Scope))
		}
		if calls.MaxSymbols < 0 {
			errs = append(errs, fmt.Errorf("lsp.calls.max_symbols must not be negative"))
		}
		if calls.MaxRefsPerSymbol < 0 {
			errs = append(errs, fmt.Errorf("lsp.calls.max_refs_per_symbol must not be negative"))
		}
	}

	return errs
}

// lspCallScopes are the accepted values of lsp.calls.scope.
var lspCallScopes = []string{"boundary", "ambiguous", "both"}

// isPublicOpenAI reports whether baseURL addresses api.openai.com (an empty
// base URL defaults to it).
func isPublicOpenAI(baseURL string) bool {
	if baseURL == "" {
		return true
	}
	return strings.Contains(baseURL, "api.openai.com")
}

func oneOf(v string, allowed []string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func compact(errs []error) []error {
	out := errs[:0]
	for _, e := range errs {
		if e != nil {
			out = append(out, e)
		}
	}
	return out
}

func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ExpandPath expands ~ in a path to the user's home directory. When the home
// directory cannot be resolved the path is returned unchanged (a "~/..." path
// that visibly fails to open, never a silently relative one) and the failure
// is logged; callers that must fail hard use ExpandPathErr.
func ExpandPath(path string) string {
	expanded, err := ExpandPathErr(path)
	if err != nil {
		slog.Warn("config: cannot expand ~ in path", "path", path, "err", err)
		return path
	}
	return expanded
}

// ExpandPathErr expands a leading ~ to the user's home directory, reporting
// the failure instead of degrading to a relative path.
func ExpandPathErr(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		if err == nil {
			err = errors.New("empty home directory")
		}
		return path, fmt.Errorf("resolve home directory for %q: %w", path, err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}
