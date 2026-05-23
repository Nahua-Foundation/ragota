package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	yaml "gopkg.in/yaml.v3"
)

// Config — главная конфигурация ai-tools.
// Загружается из YAML-файла, путь определяется так:
//  1. явный --config / -c <path>
//  2. ai-tools/config.yaml (в корне проекта)
//
// Если файла нет — используются дефолтные значения.
type Config struct {
	// Корневая директория, которую обслуживает ai-tools (где лежит ai-tools/).
	// Заполняется CLI на основе аргумента, не из YAML.
	Root string `yaml:"-"`

	// Папки/маски, которые игнорируются при сканировании.
	Ignore []string `yaml:"ignore"`

	// Расширения файлов, которые индексируются (с точкой), напр. ".go".
	Extensions []string `yaml:"extensions"`

	// Адреса внешних сервисов.
	Qdrant QdrantConfig `yaml:"qdrant"`
	Ollama OllamaConfig `yaml:"ollama"`

	// Имя коллекции в Qdrant для векторного индекса (legacy, единая коллекция).
	// При включённом раздельном индексе кода/текста используются Collections.* .
	Collection  string            `yaml:"collection"`
	Collections CollectionsConfig `yaml:"collections"`

	// Размер чанка в строках при индексации.
	ChunkLines int `yaml:"chunk_lines"`
	// Перекрытие чанков в строках.
	ChunkOverlap int `yaml:"chunk_overlap"`

	// Порты MCP-серверов (SSE при run, stdio при serve-*).
	MCP MCPPorts `yaml:"mcp"`

	// Параметры контейнеров, которые ai-tools поднимает сам при --start-docker.
	Docker DockerConfig `yaml:"docker"`

	// Настройки производительности индексации.
	VectorWorkers    int `yaml:"vector_workers"`
	EmbedParallelism int `yaml:"embed_parallelism"`

	// Гибридный поиск (vector + BM25) и реранкинг.
	BM25   BM25Config   `yaml:"bm25"`
	Rerank RerankConfig `yaml:"rerank"`
	Hybrid HybridConfig `yaml:"hybrid"`

	// Настройки LSP-серверов.
	LSP []LSPServerConfig `yaml:"lsp"`
}

// CollectionsConfig — отдельные коллекции в Qdrant для кода и текста.
// Это позволяет использовать разные модели эмбеддингов (qwen3-embedding для
// кода, nomic-embed-text для markdown) с разными размерностями.
type CollectionsConfig struct {
	Code CollectionSpec `yaml:"code"`
	Text CollectionSpec `yaml:"text"`
}

// CollectionSpec — спецификация одной коллекции и используемой ею модели.
type CollectionSpec struct {
	Name       string `yaml:"name"`
	EmbedModel string `yaml:"embed_model"`
	EmbedDim   uint64 `yaml:"embed_dim"`
}

// BM25Config — параметры лексического индекса (Bleve, BM25).
type BM25Config struct {
	Enabled bool `yaml:"enabled"`
	// Путь к каталогу Bleve-индекса (по умолчанию .ai-tools/bm25/).
	Path string `yaml:"path"`
	// Параметры BM25 (если 0 — берутся значения по умолчанию Bleve).
	K1 float64 `yaml:"k1"`
	B  float64 `yaml:"b"`
}

// RerankConfig — реранкер на базе Ollama (BGE Reranker).
// Если модель недоступна — реранкинг пропускается с warning'ом (graceful fallback).
type RerankConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Model    string `yaml:"model"`
	URL      string `yaml:"url"`
	Required bool   `yaml:"required"`
	TopN     int    `yaml:"top_n"`
}

// HybridConfig — настройки слияния результатов vector + BM25.
type HybridConfig struct {
	// Веса при weighted sum нормализованных скор; если оба = 0 — используется RRF.
	VectorWeight float64 `yaml:"vector_weight"`
	BM25Weight   float64 `yaml:"bm25_weight"`
	// RRF k-параметр (Reciprocal Rank Fusion).
	RRFK int `yaml:"rrf_k"`
	// Сколько кандидатов брать из каждого источника до слияния.
	CandidatesPerSource int `yaml:"candidates_per_source"`
}

type QdrantConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type OllamaConfig struct {
	URL        string `yaml:"url"`
	EmbedModel string `yaml:"embed_model"`
	EmbedDim   uint64 `yaml:"embed_dim"`
}

type MCPPorts struct {
	TreeSitter int `yaml:"tree_sitter"`
	Vector     int `yaml:"vector"`
	LSP        int `yaml:"lsp"`
	Symbol     int `yaml:"symbol"`
}

// DockerConfig описывает контейнеры, поднимаемые встроенным docker-runner'ом
// (без docker-compose.yaml).
type DockerConfig struct {
	Network string             `yaml:"network"`
	Qdrant  DockerContainerCfg `yaml:"qdrant"`
}

type LSPServerConfig struct {
	Language string   `yaml:"language"`
	Command  string   `yaml:"command"`
	Args     []string `yaml:"args"`
}

// DockerContainerCfg — параметры одного контейнера.
type DockerContainerCfg struct {
	Name  string `yaml:"name"`
	Image string `yaml:"image"`
	// Проброс портов "host:container".
	Ports []string `yaml:"ports"`
	// Bind-mounts "host:container" (host может быть относительным — будет
	// разрешён относительно корня проекта).
	Volumes []string `yaml:"volumes"`
	// Доп. переменные окружения "KEY=VALUE".
	Env []string `yaml:"env"`
	// Переопределение сети (например, "none" для изоляции).
	Network string `yaml:"network"`
}

// DefaultIgnore — папки, которые по умолчанию исключаются.
var DefaultIgnore = []string{
	".git", ".hg", ".svn", ".idea", ".vscode", ".fleet",
	"vendor",
	"node_modules", "bower_components", ".next", ".nuxt", "dist", "build", ".turbo", ".svelte-kit",
	"__pycache__", ".venv", "venv", "env", ".tox", ".mypy_cache", ".pytest_cache", ".ruff_cache", "site-packages", "*.egg-info",
	"target", ".gradle", "out",
	".cache", "coverage", "tmp",
	"*.pb.go", "*_grpc.pb.go", "*.gen.go", "*.pb.js", "*.pb.ts", "*_pb2.py", "*_pb2_grpc.py",
	"ai-tools",
	".ai-tools",
}

// DefaultExtensions — расширения для индексирования по умолчанию.
var DefaultExtensions = []string{
	".go",
	".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs",
	".py",
	".java",
	".proto",
	".md", ".rst", ".txt",
	".json", ".yaml", ".yml", ".toml",
}

// Default возвращает дефолтный конфиг (без root — он задаётся CLI).
func Default() *Config {
	return &Config{
		Ignore:     append([]string{}, DefaultIgnore...),
		Extensions: append([]string{}, DefaultExtensions...),
		Qdrant: QdrantConfig{
			Host: "localhost",
			Port: 6333,
		},
		Ollama: OllamaConfig{
			URL:        "http://localhost:11434",
			EmbedModel: "nomic-embed-text",
			EmbedDim:   768,
		},
		Collection: "ai_tools_code",
		Collections: CollectionsConfig{
			Code: CollectionSpec{
				Name:       "ai_tools_code",
				EmbedModel: "qwen3-embedding:0.6b",
				EmbedDim:   1024,
			},
			Text: CollectionSpec{
				Name:       "ai_tools_text",
				EmbedModel: "nomic-embed-text",
				EmbedDim:   768,
			},
		},
		BM25: BM25Config{
			Enabled: true,
			Path:    "", // вычисляется через BM25Path() при пустом значении
			K1:      1.2,
			B:       0.75,
		},
		Rerank: RerankConfig{
			Enabled:  true,
			Model:    "qllama/bge-reranker-v2-m3",
			URL:      "", // если пусто — используется Ollama.URL
			Required: false,
			TopN:     20,
		},
		Hybrid: HybridConfig{
			VectorWeight:        0,
			BM25Weight:          0,
			RRFK:                60,
			CandidatesPerSource: 50,
		},
		ChunkLines:       60,
		ChunkOverlap:     10,
		VectorWorkers:    8,
		EmbedParallelism: 16,
		MCP: MCPPorts{
			TreeSitter: 7771,
			Vector:     7772,
			LSP:        7773,
			Symbol:     7774,
		},
		Docker: DockerConfig{
			Network: "ai-tools-net",
			Qdrant: DockerContainerCfg{
				Name:    "ai-tools-qdrant",
				Image:   "qdrant/qdrant:latest",
				Ports:   []string{"127.0.0.1:6333:6333", "127.0.0.1:6334:6334"},
				Volumes: []string{".ai-tools/qdrant_storage:/qdrant/storage"},
			},
		},
		LSP: []LSPServerConfig{
			{
				Language: "go",
				Command:  "gopls",
			},
			{
				Language: "typescript",
				Command:  "typescript-language-server",
				Args:     []string{"--stdio"},
			},
			{
				Language: "javascript",
				Command:  "typescript-language-server",
				Args:     []string{"--stdio"},
			},
			{
				Language: "python",
				Command:  "pyright-langserver",
				Args:     []string{"--stdio"},
			},
			{
				Language: "java",
				Command:  "jdtls",
				Args:     []string{"-data", ".ai-tools/jdtls-data"},
			},
		},
	}
}

// DataDir возвращает путь к служебной директории .ai-tools в корне.
func (c *Config) DataDir() string {
	return filepath.Join(c.Root, ".ai-tools")
}

// SQLitePath — путь к SQLite-базе tree-sitter индекса.
func (c *Config) SQLitePath() string {
	return filepath.Join(c.DataDir(), "treesitter.db")
}

// BM25Path — путь к каталогу Bleve-индекса. Если в конфиге задан явный
// BM25.Path — используется он, иначе .ai-tools/bm25/.
func (c *Config) BM25Path() string {
	if c.BM25.Path != "" {
		if filepath.IsAbs(c.BM25.Path) {
			return c.BM25.Path
		}
		return filepath.Join(c.Root, c.BM25.Path)
	}
	return filepath.Join(c.DataDir(), "bm25")
}

// CodeCollection возвращает спецификацию коллекции кода с подставленными
// дефолтами (qwen3-embedding:0.6b, dim=1024) если поля пустые.
func (c *Config) CodeCollection() CollectionSpec {
	sp := c.Collections.Code
	if sp.Name == "" {
		sp.Name = c.Collection
		if sp.Name == "" {
			sp.Name = "ai_tools_code"
		}
	}
	if sp.EmbedModel == "" {
		sp.EmbedModel = "qwen3-embedding:0.6b"
	}
	if sp.EmbedDim == 0 {
		sp.EmbedDim = 1024
	}
	return sp
}

// TextCollection — аналогично для текста/markdown.
func (c *Config) TextCollection() CollectionSpec {
	sp := c.Collections.Text
	if sp.Name == "" {
		sp.Name = "ai_tools_text"
	}
	if sp.EmbedModel == "" {
		sp.EmbedModel = c.Ollama.EmbedModel
		if sp.EmbedModel == "" {
			sp.EmbedModel = "nomic-embed-text"
		}
	}
	if sp.EmbedDim == 0 {
		sp.EmbedDim = c.Ollama.EmbedDim
		if sp.EmbedDim == 0 {
			sp.EmbedDim = 768
		}
	}
	return sp
}

// RerankURL возвращает URL Ollama-инстанса реранкера; если поле пустое —
// используется общий Ollama.URL.
func (c *Config) RerankURL() string {
	if c.Rerank.URL != "" {
		return c.Rerank.URL
	}
	return c.Ollama.URL
}

// StatsPath — путь к файлу со статистикой MCP вызовов сессии.
func (c *Config) StatsPath() string {
	return filepath.Join(c.DataDir(), "stats.json")
}

// LogPath — путь к лог-файлу.
func (c *Config) LogPath(name string) string {
	return filepath.Join(c.DataDir(), "logs", name+".log")
}

// DefaultConfigPath возвращает локальный путь конфига: .ai-tools/config.yaml в корне.
func DefaultConfigPath(root string) string {
	return filepath.Join(root, ".ai-tools", "config.yaml")
}

// HomeConfigPath возвращает глобальный путь конфига: ~/.ai-tools/config.yaml.
func HomeConfigPath() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".ai-tools", "config.yaml")
}

// ResolveConfigPath возвращает путь к конфигу, который будет загружен.
func ResolveConfigPath(root, configPath string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	path := configPath
	if path == "" {
		// Сначала ищем локальный (.ai-tools/config.yaml или старый ai-tools/config.yaml)
		local := DefaultConfigPath(absRoot)
		if _, err := os.Stat(local); err == nil {
			path = local
		} else {
			oldLocal := filepath.Join(absRoot, "ai-tools", "config.yaml")
			if info, err := os.Stat(oldLocal); err == nil && !info.IsDir() {
				path = oldLocal
			} else {
				global := HomeConfigPath()
				if global != "" {
					if _, err := os.Stat(global); err == nil {
						path = global
					} else {
						path = global
					}
				} else {
					path = local
				}
			}
		}
	}
	return filepath.Abs(path)
}

// Load загружает конфиг. Порядок поиска если configPath пустой:
// 1. .ai-tools/config.yaml (локальный в корне проекта)
// 2. ~/.ai-tools/config.yaml (глобальный в HOME)
// Если файла нет нигде — возвращается дефолт.
func Load(root, configPath string) (*Config, error) {
	path, err := ResolveConfigPath(root, configPath)
	if err != nil {
		return nil, err
	}
	absRoot, _ := filepath.Abs(root)
	cfg := Default()
	cfg.Root = absRoot

	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if configPath != "" {
			return nil, fmt.Errorf("config file not found: %s", path)
		}
		// Файла нет — возвращаем дефолты.
		return cfg, nil
	case err != nil:
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.Root = absRoot
	return cfg, nil
}

// WriteDefault записывает дефолтный конфиг в указанный путь.
// Если path пустой — пишет в HomeConfigPath().
// Если файл уже существует и overwrite=false — возвращается ошибка.
func WriteDefault(path string, overwrite bool) (string, error) {
	if path == "" {
		path = HomeConfigPath()
		if path == "" {
			return "", fmt.Errorf("could not determine home directory for config")
		}
	}
	if _, err := os.Stat(path); err == nil && !overwrite {
		return path, fmt.Errorf("config already exists: %s (use --force to overwrite)", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return path, err
	}
	cfg := Default()
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return path, err
	}
	header := []byte("# ai-tools default configuration.\n" +
		"# Place this file at .ai-tools/config.yaml or pass via --config <path>.\n" +
		"# If you are behind a corporate proxy, add HTTP_PROXY/HTTPS_PROXY to docker envs.\n\n")
	if err := os.WriteFile(path, append(header, data...), 0o644); err != nil {
		return path, err
	}
	return path, nil
}

// EnsureDataDir создаёт .ai-tools/ и .ai-tools/logs/ в корне проекта.
func (c *Config) EnsureDataDir() error {
	if err := os.MkdirAll(c.DataDir(), 0o755); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(c.DataDir(), "logs"), 0o755)
}
