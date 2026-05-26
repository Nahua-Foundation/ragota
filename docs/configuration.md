# Конфигурация

Конфиг ищется в `.ragota/config.yaml` (проект) или `~/.ragota/config.yaml`.

Генерация дефолтного конфига:
```bash
./ragota gen-config
```

## Основные параметры

### Обход файлов

```yaml
ignore: [".git", "node_modules", "vendor"]   # фильтры обхода
extensions: [".go", ".ts", ".tsx", ".py", ".java"]  # индексируемые расширения
chunk_lines: 60                              # размер чанка в строках
chunk_overlap: 10                            # перекрытие чанков
```

### Коллекции (эмбеддинги)

```yaml
collections:
  code:
    name: code_embeddings
    model: qwen3-embedding:0.6b
    dim: 1024
  text:
    name: text_embeddings
    model: nomic-embed-text
    dim: 768
```

**При смене модели** индекс кода автоматически пересоздаётся (через `embed_meta`). Markdown-индекс не трогается.

### Ollama

```yaml
ollama:
  url: http://localhost:11434
  symbol_model: qwen2.5-coder:3b   # модель для sym.get_symbol_summary, get_file_intent
```

### Qdrant

```yaml
qdrant:
  host: localhost
  port: 6333                        # REST API
```

### BM25 (лексический индекс)

```yaml
bm25:
  enabled: true
  path: .ragota/bm25
  k1: 1.5
  b: 0.75
```

### Reranker

```yaml
rerank:
  enabled: true
  model: qllama/bge-reranker-v2-m3
  url: http://localhost:11434
  required: false                    # graceful fallback при недоступности
  top_n: 10
```

`required: false` → если модель не подгружена, реранкинг пропускается с warning в логе.

### Hybrid (слияние)

```yaml
hybrid:
  vector_weight: 0.5
  bm25_weight: 0.5
  rrf_k: 60
  candidates_per_source: 50
```

Если оба веса = 0 → используется чистый RRF.

### Порты MCP-серверов

```yaml
mcp:
  tree_sitter: 7771
  vector: 7772
  lsp: 7773
  symbol: 7774
```

### Docker

```yaml
docker:
  network: ragota-net
  qdrant:
    name: ragota-qdrant
    image: qdrant/qdrant:latest
    ports: ["127.0.0.1:6333:6333"]
    volumes: [".ragota/qdrant_storage:/qdrant/storage"]
  lsp:
    image: ragota-lsp:latest
    volumes: [".:/workspace"]
```

## Миграция со старого индекса

1. `./ragota install` — подтянет новые модели.
2. При первом запуске старый индекс кода автоматически пересоздаётся с новой моделью.
3. Для полной очистки: `./ragota clean`.
