// Package config описывает конфигурацию ragota и её загрузку из YAML.
//
// Реализация декомпозирована по доменам:
//
//   - types.go    — все типы конфигурации (Config + вложенные структуры);
//   - defaults.go — функция Default() и константы DefaultExtensions/DefaultIgnorePatterns;
//   - paths.go    — методы доступа к файловым путям (DataDir/SQLitePath/
//     BM25Path/LogPath/StatsPath) и спецификации коллекций
//     (CodeCollection/TextCollection/RerankURL);
//   - io.go       — загрузка/сохранение конфига (Load/WriteDefault/
//     HomeConfigPath/EnsureDataDir).
package config

// Config — главная конфигурация ragota.
// Загружается из YAML-файла ~/.ragota/config.yaml.
// Если файла нет — используются дефолтные значения.
type Config struct {
	// Корневая директория, которую обслуживает ragota (где лежит ragota/).
	// Заполняется CLI на основе аргумента, не из YAML.
	Root string `yaml:"-"`

	// Паттерны игнорирования, загруженные из .ragotaignore + DefaultPatterns.
	// Не сериализуется в YAML — загружается из файловой системы.
	IgnorePatterns []string `yaml:"-"`

	// Расширения файлов, которые индексируются (с точкой), напр. ".go".
	Extensions []string `yaml:"extensions"`

	// Адреса внешних сервисов.
	Ollama OllamaConfig `yaml:"ollama"`

	// Коллекции в Qdrant для кода и текста.
	Collections CollectionsConfig `yaml:"collections"`

	// Размер чанка в строках при индексации.
	ChunkLines int `yaml:"chunk_lines"`
	// Перекрытие чанков в строках.
	ChunkOverlap int `yaml:"chunk_overlap"`

	// Порт MCP SSE сервера.
	MCPPort int `yaml:"mcp_port"`

	// Параметры производительности индексации.
	VectorWorkers    int `yaml:"vector_workers"`
	EmbedParallelism int `yaml:"embed_parallelism"`

	// Гибридный поиск (vector + BM25) и реранкинг.
	BM25   BM25Config   `yaml:"bm25"`
	Rerank RerankConfig `yaml:"rerank"`

	// Настройки LSP-серверов.
	LSP []LSPServerConfig `yaml:"lsp"`

	// --- Runtime поля (заполняются при старте, не из YAML) ---

	// QdrantURL — динамически определённый URL Qdrant (порт назначается Docker).
	QdrantURL string `yaml:"-"`
}

// CollectionsConfig — отдельные коллекции в Qdrant для кода и текста.
type CollectionsConfig struct {
	Code CollectionSpec `yaml:"code"`
	Text CollectionSpec `yaml:"text"`
}

// CollectionSpec — спецификация одной коллекции.
// EmbedDim выводится из модели автоматически.
type CollectionSpec struct {
	Name       string `yaml:"name"`
	EmbedModel string `yaml:"embed_model"`
}

// BM25Config — параметры лексического индекса (Bleve, BM25).
type BM25Config struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"` // путь к каталогу Bleve-индекса
	K1      float64 `yaml:"k1"`
	B       float64 `yaml:"b"`
}

// RerankConfig — реранкер на базе Ollama (BGE Reranker).
type RerankConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Model    string `yaml:"model"`
	URL      string `yaml:"url"`
	Required bool   `yaml:"required"`
	TopN     int    `yaml:"top_n"`
}

type OllamaConfig struct {
	URL         string `yaml:"url"`
	EmbedModel  string `yaml:"embed_model"`
	SymbolModel string `yaml:"symbol_model"`   // модель для LLM-анализа символов
	IgnoreModel string `yaml:"ignore_model"`    // модель для анализа .ragotaignore
}

type LSPServerConfig struct {
	Language string   `yaml:"language"`
	Command  string   `yaml:"command"`
	Args     []string `yaml:"args"`
}
