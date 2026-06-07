package config

// Файл содержит дефолтные значения конфига и фабрику Default().

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
		Extensions: append([]string{}, DefaultExtensions...),
		Ollama: OllamaConfig{
			URL:         "http://localhost:11434",
			EmbedModel:  "nomic-embed-text",
			SymbolModel: "qwen3:4b",
			IgnoreModel: "qwen3:4b",
		},
		Collections: CollectionsConfig{
			Code: CollectionSpec{
				Name:       "ai_tools_code",
				EmbedModel: "qwen3-embedding:0.6b",
			},
			Text: CollectionSpec{
				Name:       "ai_tools_text",
				EmbedModel: "nomic-embed-text",
			},
		},
		BM25: BM25Config{
			Enabled: true,
			K1:      1.2,
			B:       0.75,
		},
		Rerank: RerankConfig{
			Enabled:  true,
			Model:    "qllama/bge-reranker-v2-m3",
			Required: false,
			TopN:     20,
		},
		ChunkLines:       60,
		ChunkOverlap:     10,
		VectorWorkers:    16,
		EmbedParallelism: 16,
		MCPPort:          7772,
		LSP: []LSPServerConfig{
			{Language: "go", Command: "gopls"},
			{Language: "typescript", Command: "typescript-language-server", Args: []string{"--stdio"}},
			{Language: "javascript", Command: "typescript-language-server", Args: []string{"--stdio"}},
			{Language: "python", Command: "pyright-langserver", Args: []string{"--stdio"}},
			{Language: "java", Command: "jdtls", Args: []string{"-data", ".ragota/jdtls-data"}},
		},
	}
}
