package config

// DefaultIgnorePatterns is the exclusion list the zero-configuration local
// profile starts from. repos.ignore has no built-in default on purpose — an
// operator's config file says exactly what to leave out — but a run with no
// config file has nobody to say it, and the first thing an unfiltered scan of
// a JavaScript project indexes is node_modules.
//
// It is the list config.example.yaml ships, in the "**/dir/**" form that
// matches whole path components at any depth.
func DefaultIgnorePatterns() []string {
	return []string{
		"**/node_modules/**",
		"**/vendor/**",
		"**/dist/**",
		"**/*.min.js",
		"**/*.bundle.js",
		"**/*.map",
		".git/**",
	}
}

// LocalDefault returns the configuration used by `ragota-core --source DIR`
// when there is no config file to read.
//
// It is the smallest profile that indexes something useful on a machine where
// nothing else is installed: SQLite for the relational data, the AST and
// keyword indexes, and no vector index — that one needs an embedding endpoint
// and a Qdrant instance, and a local tool that refuses to start because
// neither is running is not a local tool. Semantic search is what a config
// file adds.
//
// Nothing here is a special mode: these are ordinary config values, so a user
// who outgrows the profile writes the same keys into a file and everything
// downstream — the allowlist, the ignore patterns, .ragota.yaml, the indexers
// — behaves identically.
func LocalDefault() *Config {
	cfg := &Config{
		Storage: StorageConfig{
			SQLite: &SQLiteStorageConfig{Path: DefaultSQLitePath},
		},
		Indexes: IndexesConfig{
			AST:  &ASTIndexConfig{Enabled: true},
			BM25: &BM25IndexConfig{Enabled: true},
		},
		Repos: ReposConfig{
			Sources: ReposSourcesConfig{Local: &LocalSourceConfig{Enabled: true}},
			Ignore:  DefaultIgnorePatterns(),
		},
	}
	cfg.applyDefaults()
	return cfg
}
