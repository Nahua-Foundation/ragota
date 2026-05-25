// Package config описывает конфигурацию ragota и её загрузку из YAML.
//
// Реализация декомпозирована по доменам:
//
//   - types.go    — все типы конфигурации (Config + вложенные структуры);
//   - defaults.go — функция Default() и константы DefaultIgnore/DefaultExtensions;
//   - paths.go    — методы доступа к файловым путям (DataDir/SQLitePath/
//     BM25Path/LogPath/StatsPath) и спецификации коллекций
//     (CodeCollection/TextCollection/RerankURL);
//   - io.go       — загрузка/сохранение конфига (Load/WriteDefault/
//     ResolveConfigPath/DefaultConfigPath/HomeConfigPath/EnsureDataDir).
package config

// Config — главная конфигурация ragota.
// Загружается из YAML-файла, путь определяется так:
//  1. явный --config / -c <path>
//  2. ragota/config.yaml (в корне проекта)
//
// Если файла нет — используются дефолтные значения.
type Config struct {
	// Корневая директория, которую обслуживает ragota (где лежит ragota/).
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

	// Параметры контейнеров, которые ragota поднимает сам при --start-docker.
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
	// Путь к каталогу Bleve-индекса (по умолчанию .ragota/bm25/).
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
