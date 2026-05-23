package config

// Unit-тесты пакета config. Используют только t.TempDir() и стандартную
// библиотеку — без сети и без внешних сервисов.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefault_HasSaneValues(t *testing.T) {
	c := Default()
	if c == nil {
		t.Fatal("Default returned nil")
	}
	if c.Qdrant.Host == "" || c.Qdrant.Port == 0 {
		t.Errorf("Qdrant defaults missing: %+v", c.Qdrant)
	}
	if c.Ollama.URL == "" || c.Ollama.EmbedModel == "" || c.Ollama.EmbedDim == 0 {
		t.Errorf("Ollama defaults missing: %+v", c.Ollama)
	}
	if len(c.Ignore) == 0 || len(c.Extensions) == 0 {
		t.Errorf("Ignore/Extensions must have defaults")
	}
	if len(c.LSP) == 0 {
		t.Errorf("LSP servers must have defaults")
	}
	// Изоляция дефолтных слайсов: модификация Default()-копии не должна
	// влиять на глобальные переменные.
	c.Ignore[0] = "MUTATED"
	if DefaultIgnore[0] == "MUTATED" {
		t.Errorf("Default() must clone DefaultIgnore, not share the backing array")
	}
}

func TestConfigPaths(t *testing.T) {
	c := Default()
	c.Root = "/tmp/proj"
	if got := c.DataDir(); got != "/tmp/proj/.ai-tools" {
		t.Errorf("DataDir = %q", got)
	}
	if got := c.SQLitePath(); !strings.HasSuffix(got, "/.ai-tools/treesitter.db") {
		t.Errorf("SQLitePath = %q", got)
	}
	if got := c.BM25Path(); !strings.HasSuffix(got, "/.ai-tools/bm25") {
		t.Errorf("BM25Path (default) = %q", got)
	}
	c.BM25.Path = "custom/bm25"
	if got := c.BM25Path(); got != "/tmp/proj/custom/bm25" {
		t.Errorf("BM25Path (relative) = %q", got)
	}
	c.BM25.Path = "/abs/bm25"
	if got := c.BM25Path(); got != "/abs/bm25" {
		t.Errorf("BM25Path (absolute) = %q", got)
	}
	if got := c.LogPath("server"); !strings.HasSuffix(got, "/logs/server.log") {
		t.Errorf("LogPath = %q", got)
	}
	if got := c.StatsPath(); !strings.HasSuffix(got, "/stats.json") {
		t.Errorf("StatsPath = %q", got)
	}
}

func TestCollectionDefaults(t *testing.T) {
	c := &Config{}
	code := c.CodeCollection()
	if code.Name == "" || code.EmbedModel == "" || code.EmbedDim == 0 {
		t.Errorf("CodeCollection defaults missing: %+v", code)
	}
	text := c.TextCollection()
	if text.Name == "" || text.EmbedModel == "" || text.EmbedDim == 0 {
		t.Errorf("TextCollection defaults missing: %+v", text)
	}
	// При заданной Ollama.EmbedModel/Dim — TextCollection должен их подхватить.
	c2 := &Config{Ollama: OllamaConfig{EmbedModel: "custom-embed", EmbedDim: 512}}
	t2 := c2.TextCollection()
	if t2.EmbedModel != "custom-embed" || t2.EmbedDim != 512 {
		t.Errorf("TextCollection from Ollama: %+v", t2)
	}
}

func TestRerankURL(t *testing.T) {
	c := &Config{Ollama: OllamaConfig{URL: "http://o"}}
	if got := c.RerankURL(); got != "http://o" {
		t.Errorf("RerankURL fallback = %q", got)
	}
	c.Rerank.URL = "http://r"
	if got := c.RerankURL(); got != "http://r" {
		t.Errorf("RerankURL explicit = %q", got)
	}
}

func TestLoad_DefaultsWhenFileMissing(t *testing.T) {
	root := t.TempDir()
	cfg, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Root == "" || !filepath.IsAbs(cfg.Root) {
		t.Errorf("Root must be absolute: %q", cfg.Root)
	}
	if cfg.Qdrant.Host == "" {
		t.Errorf("expected defaults to be applied")
	}
}

func TestLoad_ExplicitPathMissingReturnsError(t *testing.T) {
	_, err := Load(t.TempDir(), "/no/such/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing explicit config path")
	}
}

func TestLoad_ParsesYAML(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, ".ai-tools", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	yamlBody := `
qdrant:
  host: example.com
  port: 9999
ollama:
  url: http://ollama:11434
  embed_model: my-embed
  embed_dim: 256
`
	if err := os.WriteFile(cfgPath, []byte(yamlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Qdrant.Host != "example.com" || cfg.Qdrant.Port != 9999 {
		t.Errorf("qdrant overrides not applied: %+v", cfg.Qdrant)
	}
	if cfg.Ollama.EmbedModel != "my-embed" || cfg.Ollama.EmbedDim != 256 {
		t.Errorf("ollama overrides not applied: %+v", cfg.Ollama)
	}
	// Дефолты (не упомянутые в YAML) — должны остаться.
	if len(cfg.Ignore) == 0 || len(cfg.Extensions) == 0 {
		t.Errorf("defaults must remain for unspecified fields")
	}
}

func TestLoad_BadYAML(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, ".ai-tools", "config.yaml")
	_ = os.MkdirAll(filepath.Dir(cfgPath), 0o755)
	_ = os.WriteFile(cfgPath, []byte("qdrant: [not, a, map\n"), 0o644)
	if _, err := Load(root, ""); err == nil {
		t.Fatal("expected YAML parse error")
	}
}

func TestWriteDefault_AndOverwrite(t *testing.T) {
	target := filepath.Join(t.TempDir(), "nested", "cfg.yaml")
	path, err := WriteDefault(target, false)
	if err != nil {
		t.Fatalf("WriteDefault: %v", err)
	}
	if path != target {
		t.Errorf("path = %q; want %q", path, target)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "ai-tools default configuration") {
		t.Errorf("header missing")
	}
	// Без overwrite — повторный вызов должен ругнуться.
	if _, err := WriteDefault(target, false); err == nil {
		t.Errorf("expected error when overwrite=false and file exists")
	}
	// С overwrite — должен пройти.
	if _, err := WriteDefault(target, true); err != nil {
		t.Errorf("overwrite=true: %v", err)
	}
}

func TestEnsureDataDir(t *testing.T) {
	c := Default()
	c.Root = t.TempDir()
	if err := c.EnsureDataDir(); err != nil {
		t.Fatalf("EnsureDataDir: %v", err)
	}
	for _, sub := range []string{"", "logs"} {
		p := filepath.Join(c.DataDir(), sub)
		info, err := os.Stat(p)
		if err != nil || !info.IsDir() {
			t.Errorf("missing dir %s: err=%v", p, err)
		}
	}
}

func TestResolveConfigPath_PrefersLocal(t *testing.T) {
	root := t.TempDir()
	local := DefaultConfigPath(root)
	_ = os.MkdirAll(filepath.Dir(local), 0o755)
	_ = os.WriteFile(local, []byte("qdrant:\n  host: local\n"), 0o644)
	got, err := ResolveConfigPath(root, "")
	if err != nil {
		t.Fatalf("ResolveConfigPath: %v", err)
	}
	absLocal, _ := filepath.Abs(local)
	if got != absLocal {
		t.Errorf("ResolveConfigPath = %q; want %q", got, absLocal)
	}
}
