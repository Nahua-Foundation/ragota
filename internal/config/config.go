package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
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

	// Имя коллекции в Qdrant для векторного индекса.
	Collection string `yaml:"collection"`

	// Размер чанка в строках при индексации.
	ChunkLines int `yaml:"chunk_lines"`
	// Перекрытие чанков в строках.
	ChunkOverlap int `yaml:"chunk_overlap"`

	// Порты MCP-серверов (SSE при run, stdio при serve-*).
	MCP MCPPorts `yaml:"mcp"`

	// Параметры контейнеров, которые ai-tools поднимает сам при --start-docker.
	Docker DockerConfig `yaml:"docker"`
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
}

// DockerConfig описывает контейнеры, поднимаемые встроенным docker-runner'ом
// (без docker-compose.yaml).
type DockerConfig struct {
	Network string             `yaml:"network"`
	Qdrant  DockerContainerCfg `yaml:"qdrant"`
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
}

// DefaultIgnore — папки, которые по умолчанию исключаются.
var DefaultIgnore = []string{
	".git", ".hg", ".svn", ".idea", ".vscode", ".fleet",
	"vendor",
	"node_modules", "bower_components", ".next", ".nuxt", "dist", "build", ".turbo", ".svelte-kit",
	"__pycache__", ".venv", "venv", "env", ".tox", ".mypy_cache", ".pytest_cache", ".ruff_cache", "site-packages", "*.egg-info",
	"target", ".gradle", "out",
	".cache", "coverage", "tmp",
	"ai-tools",
	".ai-tools",
}

// DefaultExtensions — расширения для индексирования по умолчанию.
var DefaultExtensions = []string{
	".go",
	".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs",
	".py",
	".java",
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
		Collection:   "ai_tools_code",
		ChunkLines:   60,
		ChunkOverlap: 10,
		MCP: MCPPorts{
			TreeSitter: 7771,
			Vector:     7772,
			LSP:        7773,
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
