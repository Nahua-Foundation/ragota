package config

// Файл содержит дефолтные значения конфига: DefaultIgnore/DefaultExtensions
// и фабрику Default(), формирующую полностью заполненный *Config.

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
		EmbedParallelism: 4,
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
