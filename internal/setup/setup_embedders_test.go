package setup

import (
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/config"
)

// A config whose vector embedder names a provider is a config whose embedder
// must be constructed: the constructors own the default endpoints, and
// --check-config's dry-run resolves those same defaults, so "the dry-run said
// OK, Build said not found" has to be unrepresentable. It was a live failure:
// vector.enabled with provider ollama and no models block passed
// --check-config and died on the first real run with "embedder ollama not
// found".
func TestEmbedderSelectionNeedsNoModelsBlock(t *testing.T) {
	vector := func(provider string) *config.VectorIndexConfig {
		return &config.VectorIndexConfig{
			Enabled:  true,
			Embedder: config.EmbedderConfig{Provider: provider},
		}
	}
	cases := []struct {
		name   string
		cfg    *config.Config
		ollama bool
		openai bool
	}{
		{
			name:   "bare ollama selection",
			cfg:    &config.Config{Indexes: config.IndexesConfig{Vector: vector("ollama")}},
			ollama: true,
		},
		{
			name:   "bare openai selection",
			cfg:    &config.Config{Indexes: config.IndexesConfig{Vector: vector("openai")}},
			openai: true,
		},
		{
			name: "no vector index",
			cfg:  &config.Config{},
		},
		{
			name: "vector disabled selects nothing",
			cfg: &config.Config{Indexes: config.IndexesConfig{
				Vector: &config.VectorIndexConfig{Embedder: config.EmbedderConfig{Provider: "ollama"}},
			}},
		},
		{
			name: "provider endpoint alone is enough",
			cfg: &config.Config{Models: config.ModelsConfig{
				Providers: map[string]config.ProviderConfig{"ollama": {BaseURL: "http://models:11434"}},
			}},
			ollama: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ollamaConfigured(tc.cfg); got != tc.ollama {
				t.Errorf("ollamaConfigured = %v, want %v", got, tc.ollama)
			}
			if got := openaiConfigured(tc.cfg); got != tc.openai {
				t.Errorf("openaiConfigured = %v, want %v", got, tc.openai)
			}
		})
	}
}
