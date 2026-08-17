package setup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/config"
)

func TestBuild(t *testing.T) {
	ctx := context.Background()

	cfg := &config.Config{
		Storage: config.StorageConfig{
			SQLite: &config.SQLiteStorageConfig{
				Path:     ":memory:",
				PoolSize: 1,
			},
		},
		Indexes: config.IndexesConfig{
			AST: &config.ASTIndexConfig{
				Enabled:   true,
				Languages: []string{"go"},
			},
		},
		Repos: config.ReposConfig{
			Sources: config.ReposSourcesConfig{
				Local: &config.LocalSourceConfig{
					Enabled: true,
					Paths:   []string{},
				},
			},
		},
	}

	svc, err := Build(ctx, cfg)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	if svc == nil {
		t.Error("Service is nil")
	}

	// Storage is accessed via interface - we cannot directly test it here
	// without exposing the internal field. Just verify the service is built.
}

func TestBuild_ValidateFails(t *testing.T) {
	ctx := context.Background()

	// Config with no storage backend - should fail validation
	cfg := &config.Config{
		Storage: config.StorageConfig{},
	}

	_, err := Build(ctx, cfg)
	if err == nil {
		t.Error("expected error for invalid config")
	}
}

func TestEmbedderBaseURL_EmbedderOverridesProvider(t *testing.T) {
	cfg := &config.Config{
		Models: config.ModelsConfig{Providers: map[string]config.ProviderConfig{
			"ollama": {BaseURL: "http://provider:11434"},
			"openai": {BaseURL: "http://provider:8000"},
		}},
		Indexes: config.IndexesConfig{Vector: &config.VectorIndexConfig{
			Enabled:  true,
			Embedder: config.EmbedderConfig{Provider: "ollama", Model: "m", BaseURL: "http://embedder:11434"},
		}},
	}

	if got := embedderBaseURL(cfg, "ollama"); got != "http://embedder:11434" {
		t.Errorf("ollama base url = %q, want the embedder override", got)
	}
	// The override belongs to the selected provider only.
	if got := embedderBaseURL(cfg, "openai"); got != "http://provider:8000" {
		t.Errorf("openai base url = %q, want the provider url", got)
	}

	cfg.Indexes.Vector.Embedder.BaseURL = ""
	if got := embedderBaseURL(cfg, "ollama"); got != "http://provider:11434" {
		t.Errorf("ollama base url = %q, want the provider url when no override is set", got)
	}
}

func TestOpenAIConfigured_WithoutAPIKey(t *testing.T) {
	// A self-hosted OpenAI-compatible gateway has an endpoint and no key.
	gateway := &config.Config{
		Models: config.ModelsConfig{Providers: map[string]config.ProviderConfig{
			"openai": {BaseURL: "http://vllm:8000/v1"},
		}},
	}
	if !openaiConfigured(gateway) {
		t.Error("an openai-compatible endpoint without a key must still register the provider")
	}

	selected := &config.Config{
		Indexes: config.IndexesConfig{Vector: &config.VectorIndexConfig{
			Enabled:  true,
			Embedder: config.EmbedderConfig{Provider: "openai", Model: "m", BaseURL: "http://litellm:4000"},
		}},
	}
	if !openaiConfigured(selected) {
		t.Error("an embedder that selects openai must register the provider")
	}

	if openaiConfigured(&config.Config{}) {
		t.Error("an unconfigured openai provider must not be registered")
	}
}

func TestGitAuth_ConfigWinsOverEnvironment(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "env-github")
	t.Setenv("GITLAB_TOKEN", "env-gitlab")

	auth := gitAuth(&config.GitAuthConfig{GitHubToken: "cfg-github"})
	if auth.GitHubToken != "cfg-github" {
		t.Errorf("github token = %q, want the configured one", auth.GitHubToken)
	}
	if auth.GitLabToken != "env-gitlab" {
		t.Errorf("gitlab token = %q, want the environment fallback", auth.GitLabToken)
	}

	auth = gitAuth(nil)
	if auth.GitHubToken != "env-github" || auth.GitLabToken != "env-gitlab" {
		t.Errorf("auth = %+v, want both tokens from the environment", auth)
	}
}

func TestBuild_ASTLanguagesFilterParsers(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{
		Storage: config.StorageConfig{SQLite: &config.SQLiteStorageConfig{Path: ":memory:", PoolSize: 1}},
		Indexes: config.IndexesConfig{AST: &config.ASTIndexConfig{Enabled: true, Languages: []string{"go"}}},
	}

	svc, err := Build(ctx, cfg)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	defer svc.Close(ctx)

	registered, skipped := parserLanguages([]string{"go"})
	if len(registered) != 1 || registered[0] != "go" {
		t.Errorf("registered = %v, want [go]", registered)
	}
	if len(skipped) != len(config.ASTLanguages())-1 {
		t.Errorf("skipped = %v, want every other supported language", skipped)
	}

	all, none := parserLanguages(nil)
	if len(all) != len(config.ASTLanguages()) || none != nil {
		t.Errorf("an empty list must register every language, got %v / %v", all, none)
	}
}

func TestBuild_BM25PathFromConfig(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := &config.Config{
		Storage: config.StorageConfig{SQLite: &config.SQLiteStorageConfig{Path: ":memory:", PoolSize: 1}},
		Indexes: config.IndexesConfig{BM25: &config.BM25IndexConfig{Enabled: true, Path: filepath.Join(dir, "bm25")}},
	}

	svc, err := Build(ctx, cfg)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	defer svc.Close(ctx)

	if _, err := os.Stat(filepath.Join(dir, "bm25")); err != nil {
		t.Errorf("bm25 index was not created at the configured path: %v", err)
	}
}

func TestBuild_BM25EnvOverridesConfig(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("RAGOTA_BM25_PATH", filepath.Join(dir, "from-env"))

	cfg := &config.Config{
		Storage: config.StorageConfig{SQLite: &config.SQLiteStorageConfig{Path: ":memory:", PoolSize: 1}},
		Indexes: config.IndexesConfig{BM25: &config.BM25IndexConfig{Enabled: true, Path: filepath.Join(dir, "from-config")}},
	}

	svc, err := Build(ctx, cfg)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	defer svc.Close(ctx)

	if _, err := os.Stat(filepath.Join(dir, "from-env")); err != nil {
		t.Errorf("RAGOTA_BM25_PATH must win over the config value: %v", err)
	}
}

func TestBuild_VectorWithoutQdrantFailsValidation(t *testing.T) {
	cfg := &config.Config{
		Storage: config.StorageConfig{SQLite: &config.SQLiteStorageConfig{Path: ":memory:", PoolSize: 1}},
		Indexes: config.IndexesConfig{Vector: &config.VectorIndexConfig{
			Enabled:  true,
			Embedder: config.EmbedderConfig{Provider: "ollama", Model: "nomic-embed-text"},
		}},
	}

	_, err := Build(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected Build to reject a vector index without a vector store")
	}
	if !strings.Contains(err.Error(), "storage.qdrant must be configured") {
		t.Errorf("error = %v, want the validation message about qdrant", err)
	}
}
