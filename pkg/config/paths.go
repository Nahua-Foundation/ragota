package config

// Файл реализует методы Config для получения файловых путей и спецификаций
// коллекций с подстановкой дефолтов.

import "path/filepath"

// DataDir возвращает путь к служебной директории .ragota в корне.
func (c *Config) DataDir() string {
	return filepath.Join(c.Root, ".ragota")
}

// SQLitePath — путь к SQLite-базе AST индекса.
func (c *Config) SQLitePath() string {
	return filepath.Join(c.DataDir(), "treesitter.db")
}

// BM25Path — путь к каталогу Bleve-индекса.
func (c *Config) BM25Path() string {
	if c.BM25.Path != "" {
		if filepath.IsAbs(c.BM25.Path) {
			return c.BM25.Path
		}
		return filepath.Join(c.Root, c.BM25.Path)
	}
	return filepath.Join(c.DataDir(), "bm25")
}

// modelDim возвращает размерность эмбеддинга для известной модели.
// Если модель неизвестна — возвращает 0 (caller должен определить сам).
func modelDim(model string) uint64 {
	dims := map[string]uint64{
		"nomic-embed-text":        768,
		"nomic-embed-text:latest": 768,
		"qwen3-embedding:0.6b":    1024,
		"qwen3-embedding:4b":      1024,
		"qwen3-embedding:8b":      1024,
		"qwen3-embedding":         1024,
		"all-minilm":              384,
		"all-minilm:latest":       384,
		"all-minilm:22m":          384,
		"all-minilm:33m":          384,
		"mxbai-embed-large":       1024,
		"snowflake-arctic-embed":  1024,
	}
	if d, ok := dims[model]; ok {
		return d
	}
	return 0
}

// CodeCollection возвращает спецификацию коллекции кода с подставленными
// дефолтами. EmbedDim выводится из модели автоматически.
func (c *Config) CodeCollection() CollectionSpec {
	sp := c.Collections.Code
	if sp.Name == "" {
		sp.Name = "ragota_code"
	}
	if sp.EmbedModel == "" {
		sp.EmbedModel = "qwen3-embedding:0.6b"
	}
	return sp
}

// CodeEmbedDim возвращает размерность эмбеддингов для коллекции кода.
func (c *Config) CodeEmbedDim() uint64 {
	m := c.CodeCollection().EmbedModel
	if d := modelDim(m); d > 0 {
		return d
	}
	return 1024 // fallback
}

// TextCollection — аналогично для текста/markdown.
func (c *Config) TextCollection() CollectionSpec {
	sp := c.Collections.Text
	if sp.Name == "" {
		sp.Name = "ragota_text"
	}
	if sp.EmbedModel == "" {
		sp.EmbedModel = c.Ollama.EmbedModel
		if sp.EmbedModel == "" {
			sp.EmbedModel = "nomic-embed-text"
		}
	}
	return sp
}

// TextEmbedDim возвращает размерность эмбеддингов для коллекции текста.
func (c *Config) TextEmbedDim() uint64 {
	m := c.TextCollection().EmbedModel
	if d := modelDim(m); d > 0 {
		return d
	}
	return 768 // fallback
}

// EmbedDimForModel возвращает размерность эмбеддингов для модели.
func (c *Config) EmbedDimForModel(model string) uint64 {
	if d := modelDim(model); d > 0 {
		return d
	}
	return 1024 // fallback
}

// RerankURL возвращает URL Ollama-инстанса реранкера.
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
