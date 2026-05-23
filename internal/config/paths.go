package config

// Файл реализует методы Config для получения файловых путей внутри .ai-tools/
// (DataDir/SQLitePath/BM25Path/LogPath/StatsPath) и доступ к спецификациям
// коллекций с подстановкой дефолтов (CodeCollection/TextCollection/RerankURL).

import "path/filepath"

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
