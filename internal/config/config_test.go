package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	// Create a temporary config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `
server:
  host: "127.0.0.1"
  port: 9090

storage:
  sqlite:
    path: "~/.ragota/data/db"
    pool_size: 5

indexes:
  ast:
    enabled: true
    languages:
      - go
      - python

models:
  providers:
    ollama:
      base_url: "http://localhost:11434"
`

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// Check server config
	if cfg.Server.Host != "127.0.0.1" {
		t.Errorf("Host = %s, want 127.0.0.1", cfg.Server.Host)
	}
	if cfg.Server.Port != 9090 {
		t.Errorf("Port = %d, want 9090", cfg.Server.Port)
	}

	// Check storage config
	if cfg.Storage.SQLite == nil {
		t.Error("SQLite config is nil")
	} else {
		if cfg.Storage.SQLite.Path != "~/.ragota/data/db" {
			t.Errorf("SQLite.Path = %s, want ~/.ragota/data/db", cfg.Storage.SQLite.Path)
		}
		if cfg.Storage.SQLite.PoolSize != 5 {
			t.Errorf("SQLite.PoolSize = %d, want 5", cfg.Storage.SQLite.PoolSize)
		}
	}

	// Check indexes config
	if cfg.Indexes.AST == nil {
		t.Error("AST config is nil")
	} else {
		if !cfg.Indexes.AST.Enabled {
			t.Error("AST.Enabled = false, want true")
		}
	}

	// Check models config
	if cfg.Models.Providers["ollama"].BaseURL != "http://localhost:11434" {
		t.Errorf("Ollama.BaseURL = %s, want http://localhost:11434", cfg.Models.Providers["ollama"].BaseURL)
	}
}

func TestApplyDefaults(t *testing.T) {
	cfg := &Config{}
	cfg.applyDefaults()

	if cfg.Server.Host == "" {
		t.Error("Host not set to default")
	}
	if cfg.Server.Port == 0 {
		t.Error("Port not set to default")
	}
	if len(cfg.Server.CORS.Origins) == 0 {
		t.Error("CORS origins not set to default")
	}
}

func TestExpandPath(t *testing.T) {
	home, _ := os.UserHomeDir()

	tests := []struct {
		input string
		want  string
	}{
		{"~/test", filepath.Join(home, "test")},
		{"~", home},
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := ExpandPath(tt.input)
			if got != tt.want {
				t.Errorf("ExpandPath(%s) = %s, want %s", tt.input, got, tt.want)
			}
		})
	}
}

func TestValidate_MissingSQLitePath(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{
			SQLite: &SQLiteStorageConfig{
				Path:     "",
				PoolSize: 10,
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing SQLite path")
	}

	if !containsStr(err.Error(), "storage.sqlite.path is required") {
		t.Errorf("expected error to mention sqlite path, got: %v", err)
	}
}

func TestValidate_VectorWithoutEmbedder(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{
			SQLite: &SQLiteStorageConfig{
				Path: "/tmp/test.db",
			},
		},
		Indexes: IndexesConfig{
			Vector: &VectorIndexConfig{
				Enabled: true,
				Embedder: EmbedderConfig{
					Provider: "",
					Model:    "",
				},
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing vector embedder config")
	}

	errStr := err.Error()
	if !containsStr(errStr, "indexes.vector.embedder.provider is required") {
		t.Errorf("expected error to mention embedder provider, got: %v", err)
	}
	if !containsStr(errStr, "indexes.vector.embedder.model is required") {
		t.Errorf("expected error to mention embedder model, got: %v", err)
	}
}

func TestLoad_UnsetEnvVar(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	// Config references an unset environment variable
	configContent := `
server:
  host: "127.0.0.1"
  port: 9090

storage:
  sqlite:
    path: "${UNSET_DATABASE_PATH}"
    pool_size: 5

indexes:
  ast:
    enabled: true
    languages:
      - go

models:
  providers:
    ollama:
      base_url: "http://localhost:11434"
`

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	_, err := Load(configPath)
	if err == nil {
		t.Fatal("expected error for unset environment variable")
	}

	if !containsStr(err.Error(), "missing environment variable") {
		t.Errorf("expected error to mention missing env var, got: %v", err)
	}

	if !containsStr(err.Error(), "UNSET_DATABASE_PATH") {
		t.Errorf("expected error to mention UNSET_DATABASE_PATH, got: %v", err)
	}
}

func TestValidate_NoStorage(t *testing.T) {
	cfg := &Config{}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for no storage backend")
	}

	if !containsStr(err.Error(), "storage.sqlite or storage.postgres") {
		t.Errorf("expected error to mention storage backend, got: %v", err)
	}
}

func TestValidate_QdrantAloneIsNotARelationalBackend(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{
			Qdrant: &QdrantStorageConfig{URL: "http://localhost:6333"},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error: qdrant cannot back files/units/edges")
	}
	if !containsStr(err.Error(), "storage.sqlite or storage.postgres") {
		t.Errorf("expected error to mention the relational backend, got: %v", err)
	}
}

func TestValidate_QdrantWithoutURL(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{
			Qdrant: &QdrantStorageConfig{},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing Qdrant URL")
	}

	if !containsStr(err.Error(), "storage.qdrant.url is required") {
		t.Errorf("expected error to mention qdrant url, got: %v", err)
	}
}

func TestValidate_GitWithoutWorkDir(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{
			SQLite: &SQLiteStorageConfig{Path: "/tmp/test.db", PoolSize: 10},
		},
		Repos: ReposConfig{
			Sources: ReposSourcesConfig{
				Git: &GitSourceConfig{
					Enabled: true,
				},
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing git work_dir")
	}

	if !containsStr(err.Error(), "repos.sources.git.work_dir is required") {
		t.Errorf("expected error to mention git work_dir, got: %v", err)
	}
}

func TestValidate_GitWithoutAuthTokens(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{
			SQLite: &SQLiteStorageConfig{Path: "/tmp/test.db", PoolSize: 10},
		},
		Repos: ReposConfig{
			Sources: ReposSourcesConfig{
				Git: &GitSourceConfig{
					Enabled: true,
					WorkDir: "/tmp/git-work",
					Auth: &GitAuthConfig{
						GitHubToken: "",
						GitLabToken: "",
					},
				},
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing git auth tokens")
	}

	if !containsStr(err.Error(), "repos.sources.git.auth requires at least one") {
		t.Errorf("expected error to mention git auth tokens, got: %v", err)
	}
}

func TestValidate_PoolSizeTooSmall(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{
			SQLite: &SQLiteStorageConfig{
				Path:     "/tmp/test.db",
				PoolSize: 0,
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for pool_size < 1")
	}

	if !containsStr(err.Error(), "pool_size must be at least 1") {
		t.Errorf("expected error to mention pool_size, got: %v", err)
	}
}

func TestValidate_RerankWithoutBaseURL(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{
			SQLite: &SQLiteStorageConfig{Path: "/tmp/test.db", PoolSize: 10},
		},
		Search: &SearchConfig{Rerank: &RerankConfig{Enabled: true}},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for rerank without base_url")
	}
	if !containsStr(err.Error(), "search.rerank.base_url is required") {
		t.Errorf("expected error to mention search.rerank.base_url, got: %v", err)
	}
}

func TestValidate_RerankDisabledNeedsNothing(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{
			SQLite: &SQLiteStorageConfig{Path: "/tmp/test.db", PoolSize: 10},
		},
		Search: &SearchConfig{Rerank: &RerankConfig{Enabled: false}},
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() error = %v, want nil for a disabled reranker", err)
	}
}

func TestValidate_NegativeIndexWorkers(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{
			SQLite: &SQLiteStorageConfig{
				Path:     "/tmp/test.db",
				PoolSize: 10,
			},
		},
		Indexes: IndexesConfig{
			Workers: -1,
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for negative indexes.workers")
	}

	if !containsStr(err.Error(), "indexes.workers must not be negative") {
		t.Errorf("expected error to mention indexes.workers, got: %v", err)
	}
}

func TestValidate_IndexWorkersZeroAndPositive(t *testing.T) {
	for _, workers := range []int{0, 4, 32} {
		cfg := &Config{
			Storage: StorageConfig{
				SQLite: &SQLiteStorageConfig{
					Path:     "/tmp/test.db",
					PoolSize: 10,
				},
			},
			Indexes: IndexesConfig{
				Workers: workers,
			},
		}

		if err := cfg.Validate(); err != nil {
			t.Errorf("unexpected validation error for workers=%d: %v", workers, err)
		}
	}
}

func TestValidate_ValidConfig(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{
			SQLite: &SQLiteStorageConfig{
				Path:     "/tmp/test.db",
				PoolSize: 10,
			},
		},
	}

	err := cfg.Validate()
	if err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestValidate_GitWithGitHubToken(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{
			SQLite: &SQLiteStorageConfig{Path: "/tmp/test.db", PoolSize: 10},
		},
		Repos: ReposConfig{
			Sources: ReposSourcesConfig{
				Git: &GitSourceConfig{
					Enabled: true,
					WorkDir: "/tmp/git-work",
					Auth: &GitAuthConfig{
						GitHubToken: "ghp_test123",
					},
				},
			},
		},
	}

	err := cfg.Validate()
	if err != nil {
		t.Fatalf("unexpected validation error for valid git auth: %v", err)
	}
}

func TestValidate_GitWithGitLabToken(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{
			SQLite: &SQLiteStorageConfig{Path: "/tmp/test.db", PoolSize: 10},
		},
		Repos: ReposConfig{
			Sources: ReposSourcesConfig{
				Git: &GitSourceConfig{
					Enabled: true,
					WorkDir: "/tmp/git-work",
					Auth: &GitAuthConfig{
						GitLabToken: "glpat_test123",
					},
				},
			},
		},
	}

	err := cfg.Validate()
	if err != nil {
		t.Fatalf("unexpected validation error for valid gitlab auth: %v", err)
	}
}

func TestValidate_QdrantInvalidMode(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{
			Qdrant: &QdrantStorageConfig{
				URL:  "http://localhost:6333",
				Mode: "invalid_mode",
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for invalid qdrant mode")
	}

	if !containsStr(err.Error(), "storage.qdrant.mode must be") {
		t.Errorf("expected error to mention qdrant mode, got: %v", err)
	}
}

func TestValidate_QdrantValidMode(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{
			SQLite: &SQLiteStorageConfig{Path: "/tmp/test.db", PoolSize: 10},
			Qdrant: &QdrantStorageConfig{
				URL:    "http://localhost:6333",
				Mode:   "cloud",
				APIKey: "qdrant-key",
			},
		},
	}

	err := cfg.Validate()
	if err != nil {
		t.Fatalf("unexpected validation error for valid qdrant mode: %v", err)
	}
}

func TestValidate_QdrantCloudRequiresAPIKey(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{
			SQLite: &SQLiteStorageConfig{Path: "/tmp/test.db", PoolSize: 10},
			Qdrant: &QdrantStorageConfig{URL: "https://x.cloud.qdrant.io", Mode: "cloud"},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for cloud qdrant without api_key")
	}
	if !containsStr(err.Error(), "storage.qdrant.api_key is required") {
		t.Errorf("expected error to mention qdrant api_key, got: %v", err)
	}
}

func TestValidate_MultipleErrors(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{
			SQLite: &SQLiteStorageConfig{
				Path:     "",
				PoolSize: 0,
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected multiple validation errors")
	}

	errStr := err.Error()
	if !containsStr(errStr, "storage.sqlite.path is required") {
		t.Errorf("expected error to mention sqlite path, got: %v", err)
	}
	if !containsStr(errStr, "storage.sqlite.pool_size must be at least 1") {
		t.Errorf("expected error to mention pool_size, got: %v", err)
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("RAGOTA_TEST_VAR", "value")

	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "reference", in: "a: ${RAGOTA_TEST_VAR}", want: "a: value"},
		{name: "literal dollar in password", in: "dsn: postgres://u:p$ss@h/db", wantErr: true},
		{name: "escaped dollar", in: "dsn: postgres://u:p$$ss@h/db", want: "dsn: postgres://u:p$ss@h/db"},
		{name: "trailing dollar", in: "key: abc$", want: "key: abc$"},
		{name: "dollar before punctuation", in: "key: a$-b$.c", want: "key: a$-b$.c"},
		{name: "bare var rejected", in: "key: $RAGOTA_TEST_VAR", wantErr: true},
		{name: "unterminated brace", in: "key: ${RAGOTA_TEST_VAR", wantErr: true},
		{name: "unset var", in: "key: ${RAGOTA_UNSET_VAR_XYZ}", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := expandEnv(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expandEnv(%q) = %q, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("expandEnv(%q) error = %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("expandEnv(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestLoad_LiteralDollarInPasswordIsReported(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "storage:\n  postgres:\n    dsn: \"postgres://u:pa$word@h:5432/db\"\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for a bare $VAR reference")
	}
	if !containsStr(err.Error(), "$word") || !containsStr(err.Error(), "$$") {
		t.Errorf("error should name the offending reference and the escape, got: %v", err)
	}
}

func TestLoad_EscapedDollarSurvives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "storage:\n  postgres:\n    dsn: \"postgres://u:pa$$word@h:5432/db\"\n    pool_size: 5\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if want := "postgres://u:pa$word@h:5432/db"; cfg.Storage.Postgres.DSN != want {
		t.Errorf("DSN = %q, want %q", cfg.Storage.Postgres.DSN, want)
	}
}

func TestValidate_AuthTypeTypoIsRejected(t *testing.T) {
	for _, typ := range []string{"apikey", "api-key", "API_KEY", "bearer"} {
		cfg := &Config{
			Storage: StorageConfig{SQLite: &SQLiteStorageConfig{Path: "/tmp/t.db", PoolSize: 1}},
			Server:  ServerConfig{Auth: AuthConfig{Type: typ, APIKeys: []string{"k"}}},
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("expected validation error for auth type %q", typ)
		}
		if !containsStr(err.Error(), "server.auth.type must be one of") {
			t.Errorf("auth type %q: unexpected error %v", typ, err)
		}
	}
}

func TestValidate_APIKeyRequiresKeys(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{SQLite: &SQLiteStorageConfig{Path: "/tmp/t.db", PoolSize: 1}},
		Server:  ServerConfig{Auth: AuthConfig{Type: "api_key", APIKeys: []string{"  "}}},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for api_key auth without keys")
	}
	if !containsStr(err.Error(), "server.auth.api_keys must contain at least one non-empty key") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_APIKeysWithoutAPIKeyAuth(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{SQLite: &SQLiteStorageConfig{Path: "/tmp/t.db", PoolSize: 1}},
		Server:  ServerConfig{Auth: AuthConfig{Type: "none", APIKeys: []string{"k"}}},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for keys configured with auth disabled")
	}
	if !containsStr(err.Error(), "the keys are ignored") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_CORSOrigins(t *testing.T) {
	tests := []struct {
		name    string
		origins []string
		wantErr string
	}{
		{name: "wildcard", origins: []string{"*"}},
		{name: "explicit", origins: []string{"https://app.example.com"}},
		{name: "no scheme", origins: []string{"app.example.com"}, wantErr: "scheme-qualified origin"},
		{name: "trailing slash", origins: []string{"https://app.example.com/"}, wantErr: "must not end with"},
		{name: "empty list", origins: []string{}, wantErr: "must not be empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Storage: StorageConfig{SQLite: &SQLiteStorageConfig{Path: "/tmp/t.db", PoolSize: 1}},
				Server:  ServerConfig{CORS: CORSConfig{Enabled: true, Origins: tt.origins}},
			}
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q", tt.wantErr)
			}
			if !containsStr(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_VectorRequiresQdrant(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{SQLite: &SQLiteStorageConfig{Path: "/tmp/t.db", PoolSize: 1}},
		Indexes: IndexesConfig{
			Vector: &VectorIndexConfig{
				Enabled:  true,
				Embedder: EmbedderConfig{Provider: "ollama", Model: "nomic-embed-text"},
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error: vector index without a vector store")
	}
	if !containsStr(err.Error(), "storage.qdrant must be configured") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_UnknownEnums(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
		want string
	}{
		{
			name: "chunking method",
			cfg: &Config{
				Storage: StorageConfig{
					SQLite: &SQLiteStorageConfig{Path: "/tmp/t.db", PoolSize: 1},
					Qdrant: &QdrantStorageConfig{URL: "http://localhost:6333"},
				},
				Indexes: IndexesConfig{Vector: &VectorIndexConfig{
					Enabled:  true,
					Embedder: EmbedderConfig{Provider: "ollama", Model: "m"},
					Chunking: ChunkingConfig{Method: "sliding", WindowLines: 60, Overlap: 10},
				}},
			},
			want: "indexes.vector.chunking.method must be one of",
		},
		{
			name: "embedder provider",
			cfg: &Config{
				Storage: StorageConfig{
					SQLite: &SQLiteStorageConfig{Path: "/tmp/t.db", PoolSize: 1},
					Qdrant: &QdrantStorageConfig{URL: "http://localhost:6333"},
				},
				Indexes: IndexesConfig{Vector: &VectorIndexConfig{
					Enabled:  true,
					Embedder: EmbedderConfig{Provider: "vllm", Model: "m"},
				}},
			},
			want: "indexes.vector.embedder.provider must be one of",
		},
		{
			name: "summaries provider",
			cfg: &Config{
				Storage:   StorageConfig{SQLite: &SQLiteStorageConfig{Path: "/tmp/t.db", PoolSize: 1}},
				Summaries: &SummariesConfig{Enabled: true, Provider: "anthropic", Model: "m"},
			},
			want: "summaries.provider must be one of",
		},
		{
			name: "assistant provider",
			cfg: &Config{
				Storage: StorageConfig{SQLite: &SQLiteStorageConfig{Path: "/tmp/t.db", PoolSize: 1}},
				Models:  ModelsConfig{Assistant: &AssistantConfig{Provider: "llamacpp", Model: "m"}},
			},
			want: "models.assistant.provider must be one of",
		},
		{
			name: "ast language",
			cfg: &Config{
				Storage: StorageConfig{SQLite: &SQLiteStorageConfig{Path: "/tmp/t.db", PoolSize: 1}},
				Indexes: IndexesConfig{AST: &ASTIndexConfig{Enabled: true, Languages: []string{"go", "cobol"}}},
			},
			want: "indexes.ast.languages entry \"cobol\"",
		},
		{
			name: "log level",
			cfg: &Config{
				Storage: StorageConfig{SQLite: &SQLiteStorageConfig{Path: "/tmp/t.db", PoolSize: 1}},
				Log:     LogConfig{Level: "verbose"},
			},
			want: "log.level must be one of",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q", tt.want)
			}
			if !containsStr(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestValidate_LSP(t *testing.T) {
	base := func(lsp *LSPConfig) *Config {
		return &Config{
			Storage: StorageConfig{SQLite: &SQLiteStorageConfig{Path: "/tmp/t.db", PoolSize: 1}},
			LSP:     lsp,
		}
	}

	tests := []struct {
		name string
		lsp  *LSPConfig
		want string
	}{
		{
			name: "no servers",
			lsp:  &LSPConfig{Enabled: true},
			want: "lsp.servers must not be empty",
		},
		{
			name: "missing addr",
			lsp:  &LSPConfig{Enabled: true, Servers: map[string]LSPServerConfig{"go": {}}},
			want: "lsp.servers.go.addr is required",
		},
		{
			name: "addr without port",
			lsp:  &LSPConfig{Enabled: true, Servers: map[string]LSPServerConfig{"go": {Addr: "localhost"}}},
			want: "must be host:port",
		},
		{
			name: "unsupported language",
			lsp:  &LSPConfig{Enabled: true, Servers: map[string]LSPServerConfig{"rust": {Addr: "localhost:7305"}}},
			want: "no support for language",
		},
		{
			name: "half mapping",
			lsp: &LSPConfig{
				Enabled:  true,
				HostRoot: "/srv/repos",
				Servers:  map[string]LSPServerConfig{"go": {Addr: "localhost:7301"}},
			},
			want: "must be set together",
		},
		{
			name: "relative host root",
			lsp: &LSPConfig{
				Enabled:   true,
				HostRoot:  "repos",
				MountRoot: "/workspace",
				Servers:   map[string]LSPServerConfig{"go": {Addr: "localhost:7301"}},
			},
			want: "lsp.host_root must be an absolute path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := base(tt.lsp).Validate()
			if err == nil {
				t.Fatalf("expected error containing %q", tt.want)
			}
			if !containsStr(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}

	valid := base(&LSPConfig{
		Enabled:   true,
		HostRoot:  "/srv/repos",
		MountRoot: "/workspace",
		Servers:   map[string]LSPServerConfig{"go": {Addr: "localhost:7301"}},
	})
	if err := valid.Validate(); err != nil {
		t.Errorf("unexpected error for a valid lsp config: %v", err)
	}
}

func TestValidate_DistributedOverSQLiteIsAWarningNotAnError(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{SQLite: &SQLiteStorageConfig{Path: "/tmp/t.db", PoolSize: 1}},
		Indexes: IndexesConfig{Distributed: true, JobPollSeconds: 3, StaleJobSeconds: 120},
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("distributed indexing over SQLite must stay valid: %v", err)
	}

	warnings := cfg.Warnings()
	found := false
	for _, w := range warnings {
		if containsStr(w, "indexes.distributed") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a warning about distributed indexing, got %v", warnings)
	}
}

func TestApplyDefaults_ServerAndIndexes(t *testing.T) {
	cfg := &Config{
		Indexes: IndexesConfig{
			AST:  &ASTIndexConfig{Enabled: true},
			BM25: &BM25IndexConfig{Enabled: true},
		},
		Storage: StorageConfig{SQLite: &SQLiteStorageConfig{}},
	}
	cfg.applyDefaults()

	if cfg.Server.WriteTimeoutSeconds != 120 {
		t.Errorf("write timeout = %d, want 120", cfg.Server.WriteTimeoutSeconds)
	}
	if cfg.Server.ReadTimeoutSeconds == 0 || cfg.Server.IdleTimeoutSeconds == 0 || cfg.Server.ShutdownTimeoutSeconds == 0 {
		t.Error("server timeouts must all have defaults")
	}
	if cfg.Server.MaxBodyBytes != DefaultMaxBodyBytes || cfg.Server.MaxCommitBodyBytes != DefaultMaxCommitBodyBytes {
		t.Errorf("body limits = %d/%d, want %d/%d",
			cfg.Server.MaxBodyBytes, cfg.Server.MaxCommitBodyBytes, DefaultMaxBodyBytes, DefaultMaxCommitBodyBytes)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("log level = %q, want info", cfg.Log.Level)
	}
	if cfg.Storage.SQLite.Path != DefaultSQLitePath {
		t.Errorf("sqlite path = %q, want %q", cfg.Storage.SQLite.Path, DefaultSQLitePath)
	}
	if cfg.Indexes.BM25.Path != DefaultBM25Path {
		t.Errorf("bm25 path = %q, want %q", cfg.Indexes.BM25.Path, DefaultBM25Path)
	}
	if len(cfg.Indexes.AST.Languages) != len(ASTLanguages()) {
		t.Errorf("ast languages = %v, want every supported language", cfg.Indexes.AST.Languages)
	}
}

func TestApplyDefaults_EnvOverridesBodyLimits(t *testing.T) {
	t.Setenv("RAGOTA_MAX_BODY_BYTES", "2048")
	t.Setenv("RAGOTA_MAX_COMMIT_BODY_BYTES", "4096")
	t.Setenv("RAGOTA_TRUSTED_PROXIES", "10.0.0.0/8, 192.168.1.1")

	cfg := &Config{Server: ServerConfig{MaxBodyBytes: 99, MaxCommitBodyBytes: 99}}
	cfg.applyDefaults()

	if cfg.Server.MaxBodyBytes != 2048 || cfg.Server.MaxCommitBodyBytes != 4096 {
		t.Errorf("body limits = %d/%d, want 2048/4096", cfg.Server.MaxBodyBytes, cfg.Server.MaxCommitBodyBytes)
	}
	if len(cfg.Server.TrustedProxies) != 2 || cfg.Server.TrustedProxies[1] != "192.168.1.1" {
		t.Errorf("trusted proxies = %v, want [10.0.0.0/8 192.168.1.1]", cfg.Server.TrustedProxies)
	}
	if err := cfg.Validate(); err == nil || !containsStr(err.Error(), "storage.sqlite or storage.postgres") {
		t.Errorf("trusted proxies must validate cleanly, got: %v", err)
	}
}

func TestValidate_TrustedProxiesMustParse(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{SQLite: &SQLiteStorageConfig{Path: "/tmp/t.db", PoolSize: 1}},
		Server:  ServerConfig{TrustedProxies: []string{"not-an-ip"}},
	}

	err := cfg.Validate()
	if err == nil || !containsStr(err.Error(), "neither an IP address nor a CIDR") {
		t.Errorf("expected a trusted_proxies parse error, got: %v", err)
	}
}

func TestExpandPathErr_ReportsUnresolvableHome(t *testing.T) {
	t.Setenv("HOME", "")
	if _, err := ExpandPathErr("~/data"); err == nil {
		t.Skip("home directory still resolvable on this platform")
	}
	if got := ExpandPath("~/data"); got != "~/data" {
		t.Errorf("ExpandPath(~/data) = %q, want the path unchanged rather than a relative one", got)
	}
}

func TestExpandEnv_CommentsAreNotExpanded(t *testing.T) {
	in := "server:\n" +
		"  # api_keys: [\"${RAGOTA_NEVER_SET}\"]\n" +
		"  host: \"127.0.0.1\"   # trailing ${RAGOTA_NEVER_SET} too\n"

	got, err := expandEnv(in)
	if err != nil {
		t.Fatalf("expandEnv() error = %v (commented-out examples must not require the variable)", err)
	}
	if got != in {
		t.Errorf("expandEnv() = %q, want the input unchanged", got)
	}
}

func TestExpandEnv_HashInsideAQuotedValueIsNotAComment(t *testing.T) {
	t.Setenv("RAGOTA_TEST_TOKEN", "secret")
	in := "auth:\n  key: \"#${RAGOTA_TEST_TOKEN}\"\n"

	got, err := expandEnv(in)
	if err != nil {
		t.Fatalf("expandEnv() error = %v", err)
	}
	if want := "auth:\n  key: \"#secret\"\n"; got != want {
		t.Errorf("expandEnv() = %q, want %q", got, want)
	}
}

func TestCheckUnknownKeys(t *testing.T) {
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "c.yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// A well-formed config with only known keys passes.
	if err := CheckUnknownKeys(write("storage:\n  sqlite:\n    path: /tmp/t.db\n    pool_size: 1\n")); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}

	// A typo'd top-level key is caught rather than silently ignored.
	if err := CheckUnknownKeys(write("storage:\n  sqlite:\n    path: /tmp/t.db\nrate_limt: true\n")); err == nil {
		t.Error("unknown top-level key accepted, want rejection")
	}

	// A typo in a nested struct field is caught too.
	if err := CheckUnknownKeys(write("server:\n  prt: 8080\n")); err == nil {
		t.Error("unknown nested key accepted, want rejection")
	}

	// Free-form map sections (models.providers) keep their arbitrary keys.
	if err := CheckUnknownKeys(write("models:\n  providers:\n    my-gateway:\n      base_url: http://x\n")); err != nil {
		t.Errorf("free-form provider key rejected: %v", err)
	}
}
