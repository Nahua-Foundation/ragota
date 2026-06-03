package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Default()
// ---------------------------------------------------------------------------

func TestDefault_QdrantValues(t *testing.T) {
	c := Default()
	assert.Equal(t, "localhost", c.Qdrant.Host)
	assert.Equal(t, 6333, c.Qdrant.Port)
}

func TestDefault_OllamaValues(t *testing.T) {
	c := Default()
	assert.Equal(t, "http://localhost:11434", c.Ollama.URL)
	assert.Equal(t, "nomic-embed-text", c.Ollama.EmbedModel)
	assert.Equal(t, uint64(768), c.Ollama.EmbedDim)
	assert.NotEmpty(t, c.Ollama.SymbolModel)
}

func TestDefault_CollectionsValues(t *testing.T) {
	c := Default()
	assert.Equal(t, "ai_tools_code", c.Collection)
	assert.Equal(t, "ai_tools_code", c.Collections.Code.Name)
	assert.Equal(t, uint64(1024), c.Collections.Code.EmbedDim)
	assert.NotEmpty(t, c.Collections.Code.EmbedModel)
	assert.Equal(t, "ai_tools_text", c.Collections.Text.Name)
	assert.Equal(t, uint64(768), c.Collections.Text.EmbedDim)
}

func TestDefault_HybridValues(t *testing.T) {
	c := Default()
	assert.Equal(t, 60, c.Hybrid.RRFK)
	assert.Equal(t, 50, c.Hybrid.CandidatesPerSource)
	assert.Equal(t, float64(0), c.Hybrid.VectorWeight)
	assert.Equal(t, float64(0), c.Hybrid.BM25Weight)
}

func TestDefault_BM25Values(t *testing.T) {
	c := Default()
	assert.True(t, c.BM25.Enabled)
	assert.InDelta(t, 1.2, c.BM25.K1, 0.001)
	assert.InDelta(t, 0.75, c.BM25.B, 0.001)
}

func TestDefault_RerankValues(t *testing.T) {
	c := Default()
	assert.True(t, c.Rerank.Enabled)
	assert.NotEmpty(t, c.Rerank.Model)
	assert.Equal(t, 20, c.Rerank.TopN)
}

func TestDefault_MCPPorts(t *testing.T) {
	c := Default()
	assert.Equal(t, 7771, c.MCP.TreeSitter)
	assert.Equal(t, 7772, c.MCP.Vector)
	assert.Equal(t, 7773, c.MCP.LSP)
	assert.Equal(t, 7774, c.MCP.Symbol)
}

func TestDefault_ChunkSettings(t *testing.T) {
	c := Default()
	assert.Equal(t, 60, c.ChunkLines)
	assert.Equal(t, 10, c.ChunkOverlap)
}

func TestDefault_LSPServers(t *testing.T) {
	c := Default()
	require.NotEmpty(t, c.LSP)
	langs := map[string]bool{}
	for _, l := range c.LSP {
		langs[l.Language] = true
		assert.NotEmpty(t, l.Command)
	}
	assert.True(t, langs["go"])
	assert.True(t, langs["python"])
	assert.True(t, langs["typescript"])
	assert.True(t, langs["java"])
}

func TestDefault_DockerConfig(t *testing.T) {
	c := Default()
	assert.Equal(t, "ragota-net", c.Docker.Network)
	assert.Equal(t, "ragota-qdrant", c.Docker.Qdrant.Name)
	assert.NotEmpty(t, c.Docker.Qdrant.Image)
	assert.NotEmpty(t, c.Docker.Qdrant.Ports)
	assert.NotEmpty(t, c.Docker.LSP.Image)
}

func TestDefault_ExtensionsCloned(t *testing.T) {
	c := Default()
	origLen := len(c.Extensions)
	c.Extensions = append(c.Extensions, ".xyz")
	c2 := Default()
	assert.Len(t, c2.Extensions, origLen, "modifying one Default() must not affect another")
}

// ---------------------------------------------------------------------------
// Path methods
// ---------------------------------------------------------------------------

func TestDataDir(t *testing.T) {
	c := &Config{Root: "/home/user/project"}
	assert.Equal(t, "/home/user/project/.ragota", c.DataDir())
}

func TestSQLitePath(t *testing.T) {
	c := &Config{Root: "/home/user/project"}
	assert.Equal(t, "/home/user/project/.ragota/treesitter.db", c.SQLitePath())
}

func TestBM25Path_Default(t *testing.T) {
	c := &Config{Root: "/home/user/project"}
	assert.Equal(t, "/home/user/project/.ragota/bm25", c.BM25Path())
}

func TestBM25Path_Relative(t *testing.T) {
	c := &Config{Root: "/home/user/project"}
	c.BM25.Path = "custom/bm25"
	assert.Equal(t, "/home/user/project/custom/bm25", c.BM25Path())
}

func TestBM25Path_Absolute(t *testing.T) {
	c := &Config{Root: "/home/user/project"}
	c.BM25.Path = "/abs/bm25"
	assert.Equal(t, "/abs/bm25", c.BM25Path())
}

func TestStatsPath(t *testing.T) {
	c := &Config{Root: "/p"}
	assert.Equal(t, "/p/.ragota/stats.json", c.StatsPath())
}

func TestLogPath(t *testing.T) {
	c := &Config{Root: "/p"}
	assert.Equal(t, "/p/.ragota/logs/server.log", c.LogPath("server"))
	assert.Equal(t, "/p/.ragota/logs/mcp.log", c.LogPath("mcp"))
}

// ---------------------------------------------------------------------------
// CodeCollection / TextCollection fallback chains
// ---------------------------------------------------------------------------

func TestCodeCollection_FullDefaults(t *testing.T) {
	c := &Config{}
	spec := c.CodeCollection()
	assert.NotEmpty(t, spec.Name)
	assert.NotEmpty(t, spec.EmbedModel)
	assert.NotZero(t, spec.EmbedDim)
}

func TestCodeCollection_FallbackToCollection(t *testing.T) {
	c := &Config{Collection: "my_coll"}
	spec := c.CodeCollection()
	assert.Equal(t, "my_coll", spec.Name)
}

func TestCodeCollection_FallbackToHardcoded(t *testing.T) {
	c := &Config{}
	c.Collection = ""
	spec := c.CodeCollection()
	assert.Equal(t, "ragota_code", spec.Name)
}

func TestCodeCollection_ExplicitOverrides(t *testing.T) {
	c := &Config{}
	c.Collections.Code = CollectionSpec{Name: "custom", EmbedModel: "m", EmbedDim: 128}
	spec := c.CodeCollection()
	assert.Equal(t, "custom", spec.Name)
	assert.Equal(t, "m", spec.EmbedModel)
	assert.Equal(t, uint64(128), spec.EmbedDim)
}

func TestTextCollection_FullDefaults(t *testing.T) {
	c := &Config{}
	spec := c.TextCollection()
	assert.NotEmpty(t, spec.Name)
	assert.NotEmpty(t, spec.EmbedModel)
	assert.NotZero(t, spec.EmbedDim)
}

func TestTextCollection_FallbackToHardcoded(t *testing.T) {
	c := &Config{}
	spec := c.TextCollection()
	assert.Equal(t, "ragota_text", spec.Name)
	assert.Equal(t, "nomic-embed-text", spec.EmbedModel)
	assert.Equal(t, uint64(768), spec.EmbedDim)
}

func TestTextCollection_InheritsOllamaEmbedModel(t *testing.T) {
	c := &Config{Ollama: OllamaConfig{EmbedModel: "my-embed", EmbedDim: 512}}
	spec := c.TextCollection()
	assert.Equal(t, "my-embed", spec.EmbedModel)
	assert.Equal(t, uint64(512), spec.EmbedDim)
}

func TestTextCollection_ExplicitOverrides(t *testing.T) {
	c := &Config{}
	c.Collections.Text = CollectionSpec{Name: "txt", EmbedModel: "txt-m", EmbedDim: 64}
	spec := c.TextCollection()
	assert.Equal(t, "txt", spec.Name)
	assert.Equal(t, "txt-m", spec.EmbedModel)
	assert.Equal(t, uint64(64), spec.EmbedDim)
}

// ---------------------------------------------------------------------------
// RerankURL
// ---------------------------------------------------------------------------

func TestRerankURL_FallbackToOllama(t *testing.T) {
	c := &Config{Ollama: OllamaConfig{URL: "http://ollama:11434"}}
	assert.Equal(t, "http://ollama:11434", c.RerankURL())
}

func TestRerankURL_Explicit(t *testing.T) {
	c := &Config{
		Ollama: OllamaConfig{URL: "http://ollama:11434"},
		Rerank: RerankConfig{URL: "http://rerank:8080"},
	}
	assert.Equal(t, "http://rerank:8080", c.RerankURL())
}

// ---------------------------------------------------------------------------
// ResolveConfigPath
// ---------------------------------------------------------------------------

func TestResolveConfigPath_ExplicitPath(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "my.yaml")
	require.NoError(t, os.WriteFile(cfgFile, []byte(""), 0o644))
	got, err := ResolveConfigPath(dir, cfgFile)
	require.NoError(t, err)
	abs, _ := filepath.Abs(cfgFile)
	assert.Equal(t, abs, got)
}

func TestResolveConfigPath_PrefersLocalConfig(t *testing.T) {
	root := t.TempDir()
	local := DefaultConfigPath(root)
	require.NoError(t, os.MkdirAll(filepath.Dir(local), 0o755))
	require.NoError(t, os.WriteFile(local, []byte("qdrant:\n  host: local\n"), 0o644))
	got, err := ResolveConfigPath(root, "")
	require.NoError(t, err)
	absLocal, _ := filepath.Abs(local)
	assert.Equal(t, absLocal, got)
}

func TestResolveConfigPath_FallsBackToOldLocal(t *testing.T) {
	root := t.TempDir()
	oldLocal := filepath.Join(root, "ragota", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(oldLocal), 0o755))
	require.NoError(t, os.WriteFile(oldLocal, []byte(""), 0o644))
	got, err := ResolveConfigPath(root, "")
	require.NoError(t, err)
	absOld, _ := filepath.Abs(oldLocal)
	assert.Equal(t, absOld, got)
}

func TestResolveConfigPath_FallsBackToHome(t *testing.T) {
	root := t.TempDir()
	// No local config, no old local — should fallback to home.
	got, err := ResolveConfigPath(root, "")
	require.NoError(t, err)
	home := HomeConfigPath()
	if home != "" {
		absHome, _ := filepath.Abs(home)
		assert.Equal(t, absHome, got)
	}
}

// ---------------------------------------------------------------------------
// DefaultConfigPath / HomeConfigPath
// ---------------------------------------------------------------------------

func TestDefaultConfigPath(t *testing.T) {
	assert.Equal(t, "/foo/.ragota/config.yaml", DefaultConfigPath("/foo"))
}

func TestHomeConfigPath(t *testing.T) {
	home := HomeConfigPath()
	if home == "" {
		t.Skip("no HOME")
	}
	assert.Contains(t, home, ".ragota/config.yaml")
	assert.True(t, filepath.IsAbs(home))
}

// ---------------------------------------------------------------------------
// Load
// ---------------------------------------------------------------------------

func TestLoad_DefaultsWhenNoFile(t *testing.T) {
	root := t.TempDir()
	cfg, err := Load(root, "")
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(cfg.Root))
	assert.Equal(t, "localhost", cfg.Qdrant.Host)
	assert.Equal(t, 6333, cfg.Qdrant.Port)
}

func TestLoad_ExplicitMissingErrors(t *testing.T) {
	_, err := Load(t.TempDir(), "/no/such/file.yaml")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestLoad_YAMLOverrides(t *testing.T) {
	root := t.TempDir()
	cfgDir := filepath.Join(root, ".ragota")
	require.NoError(t, os.MkdirAll(cfgDir, 0o755))
	body := `
qdrant:
  host: remote
  port: 9999
chunk_lines: 100
chunk_overlap: 20
`
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(body), 0o644))
	cfg, err := Load(root, "")
	require.NoError(t, err)
	assert.Equal(t, "remote", cfg.Qdrant.Host)
	assert.Equal(t, 9999, cfg.Qdrant.Port)
	assert.Equal(t, 100, cfg.ChunkLines)
	assert.Equal(t, 20, cfg.ChunkOverlap)
	// Defaults preserved
	assert.NotEmpty(t, cfg.Ignore)
	assert.NotEmpty(t, cfg.Extensions)
}

func TestLoad_InvalidYAMLContent(t *testing.T) {
	root := t.TempDir()
	cfgDir := filepath.Join(root, ".ragota")
	require.NoError(t, os.MkdirAll(cfgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte("qdrant: [not\n"), 0o644))
	_, err := Load(root, "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "parse config")
}

func TestLoad_SetsAbsRoot(t *testing.T) {
	root := t.TempDir()
	cfg, err := Load(root, "")
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(cfg.Root))
}

func TestLoad_PartialYAML(t *testing.T) {
	root := t.TempDir()
	cfgDir := filepath.Join(root, ".ragota")
	require.NoError(t, os.MkdirAll(cfgDir, 0o755))
	body := `
ollama:
  embed_model: custom-model
`
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(body), 0o644))
	cfg, err := Load(root, "")
	require.NoError(t, err)
	assert.Equal(t, "custom-model", cfg.Ollama.EmbedModel)
	// Other defaults still present
	assert.Equal(t, "http://localhost:11434", cfg.Ollama.URL)
}

// ---------------------------------------------------------------------------
// WriteDefault
// ---------------------------------------------------------------------------

func TestWriteDefault_CreatesNestedDirs(t *testing.T) {
	target := filepath.Join(t.TempDir(), "a", "b", "c", "cfg.yaml")
	path, err := WriteDefault(target, false)
	require.NoError(t, err)
	assert.Equal(t, target, path)
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Contains(t, string(data), "ragota default configuration")
}

func TestWriteDefault_RefusesOverwrite(t *testing.T) {
	target := filepath.Join(t.TempDir(), "cfg.yaml")
	_, err := WriteDefault(target, false)
	require.NoError(t, err)
	_, err = WriteDefault(target, false)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestWriteDefault_AllowsOverwrite(t *testing.T) {
	target := filepath.Join(t.TempDir(), "cfg.yaml")
	_, err := WriteDefault(target, false)
	require.NoError(t, err)
	_, err = WriteDefault(target, true)
	assert.NoError(t, err)
}

// ---------------------------------------------------------------------------
// EnsureDataDir
// ---------------------------------------------------------------------------

func TestEnsureDataDir_CreatesDirsAndLogs(t *testing.T) {
	c := &Config{Root: t.TempDir()}
	require.NoError(t, c.EnsureDataDir())
	info, err := os.Stat(c.DataDir())
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	info, err = os.Stat(filepath.Join(c.DataDir(), "logs"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestEnsureDataDir_Idempotent(t *testing.T) {
	c := &Config{Root: t.TempDir()}
	require.NoError(t, c.EnsureDataDir())
	require.NoError(t, c.EnsureDataDir())
}

// ---------------------------------------------------------------------------
// DefaultIgnore / DefaultExtensions
// ---------------------------------------------------------------------------

func TestDefaultIgnore_ContainsCommonDirs(t *testing.T) {
	assert.Contains(t, DefaultIgnore, ".git")
	assert.Contains(t, DefaultIgnore, "node_modules")
	assert.Contains(t, DefaultIgnore, "__pycache__")
	assert.Contains(t, DefaultIgnore, "vendor")
	assert.Contains(t, DefaultIgnore, "target")
	assert.Contains(t, DefaultIgnore, ".ragota")
}

func TestDefaultExtensions_ContainsCommonExts(t *testing.T) {
	assert.Contains(t, DefaultExtensions, ".go")
	assert.Contains(t, DefaultExtensions, ".py")
	assert.Contains(t, DefaultExtensions, ".ts")
	assert.Contains(t, DefaultExtensions, ".java")
	assert.Contains(t, DefaultExtensions, ".md")
}

func TestDefaultIgnore_NotSharedBetweenCalls(t *testing.T) {
	a := Default()
	b := Default()
	a.Ignore[0] = "MUTATED"
	assert.NotEqual(t, "MUTATED", b.Ignore[0])
	assert.NotEqual(t, "MUTATED", DefaultIgnore[0])
}

func TestDefaultExtensions_NotSharedBetweenCalls(t *testing.T) {
	a := Default()
	b := Default()
	a.Extensions[0] = ".MUTATED"
	assert.NotEqual(t, ".MUTATED", b.Extensions[0])
	assert.NotEqual(t, ".MUTATED", DefaultExtensions[0])
}

// ---------------------------------------------------------------------------
// Edge cases
// ---------------------------------------------------------------------------

func TestConfig_EmptyRoot(t *testing.T) {
	c := &Config{}
	assert.Equal(t, ".ragota", c.DataDir())
	assert.Equal(t, ".ragota/treesitter.db", c.SQLitePath())
}

func TestConfig_RootWithTrailingSlash(t *testing.T) {
	c := &Config{Root: "/tmp/proj/"}
	dir := c.DataDir()
	assert.True(t, strings.HasSuffix(dir, ".ragota"))
}

func TestLoad_EmptyYAMLFile(t *testing.T) {
	root := t.TempDir()
	cfgDir := filepath.Join(root, ".ragota")
	require.NoError(t, os.MkdirAll(cfgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(""), 0o644))
	cfg, err := Load(root, "")
	require.NoError(t, err)
	assert.Equal(t, "localhost", cfg.Qdrant.Host)
}
