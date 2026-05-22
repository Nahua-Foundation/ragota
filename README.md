# ai-tools

Единый бинарь, объединяющий три MCP-сервера, докер-инфраструктуру и live-дашборд для индексации проектов.

### Возможности
- **tree-sitter MCP** — разбирает код на символы (function/method/class/interface/type/enum/var/const) для Go, TypeScript/TSX, JavaScript/JSX, Python, Java; хранит их в SQLite; следит за изменениями в директории.
- **vector MCP** — ререзать файлы на чанки (использует **tree-sitter чанкинг** для Go/TS/Python/Java, либо построчное окно для остальных), эмбеддит через Ollama (`nomic-embed-text` по умолчанию), кладёт в Qdrant; перестраивает индекс при изменении файлов; поддерживает `ignore`-папки (vendor, node_modules, __pycache__, target и т. п.).
- **LSP MCP** — проксирует официальные LSP-сервера (`gopls`, `typescript-language-server`, `pyright-langserver`, `jdtls`) через MCP-tools `lsp.definition`, `lsp.references`, `lsp.hover`, `lsp.languages`.
- **`ai-tools watch .`** — поднимает qdrant в докере (если указано `--start-docker`), использует Ollama на хосте, запускает оба индексатора, открывает TUI-дашборд (bubbletea) со статусом индексации, последними изменёнными/индексированными файлами, числом чанков/символов, статистикой вызовов MCP-серверов текущей сессии.

### Требования
- Go 1.26+
- macOS/Linux с Xcode CLI tools (CGO для tree-sitter)
- Docker (для Qdrant)
- Ollama на хосте (см. раздел [Настройка Ollama](#настройка-ollama))
- В PATH для LSP MCP: `gopls`, `typescript-language-server`, `pyright-langserver`, `jdtls` (любой набор — если бинаря нет, соответствующий язык просто не запустится)

### Установка зависимостей

Для работы всех функций требуются внешние инструменты. Вы можете установить их автоматически или вручную.

#### Автоматическая установка
```bash
./ai-tools install
```
Команда проверит наличие Docker, Ollama, нужных моделей и LSP-серверов, и предложит установить недостающие (с подтверждением для каждого).

#### Ручная установка
- **Docker**: необходим для Qdrant. [Инструкция](https://docs.docker.com/get-docker/).
- **Ollama**: необходима для векторного поиска. См. раздел [Настройка Ollama](#настройка-ollama).
- **LSP-серверы**:
    - **Go**: `go install golang.org/x/tools/gopls@latest`
    - **TypeScript**: `npm install -g typescript-language-server typescript`
    - **Python**: `npm install -g pyright`
    - **Java**: [jdtls](https://github.com/eclipse-jdtls/eclipse.jdt.ls)

### Сборка
```
go build -o ai-tools .
```

### Запуск
```
./ai-tools watch .
```
- При первом запуске ищется `.ai-tools/config.yaml` в проекте или `~/.ai-tools/config.yaml`. Если их нет — используются дефолты.
- Сгенерировать конфиг: `./ai-tools gen-config` (по умолчанию в `~/.ai-tools/config.yaml`).
- `ai-tools mcp-config` — сгенерировать JSON для вставки в конфиг Claude Desktop или других MCP-клиентов.
- `q`/`Esc`/`Ctrl+C` — выход.

#### Флаги `watch`
- `--start-docker` — запустить контейнер qdrant из конфига
- `--skip-vector` — отключить векторный индекс
- `--skip-treesitter` — отключить tree-sitter индекс
- `--no-tui` — не открывать TUI (только фоновый процесс)

### MCP-серверы по отдельности (stdio)
Для подключения из MCP-клиента (Claude Desktop, etc):
```
ai-tools serve-treesitter --root /path/to/project
ai-tools serve-vector     --root /path/to/project
ai-tools serve-lsp        --root /path/to/project
```

#### Tools

**ts.\***
- `ts.search_symbols(query, kind?, language?, limit?)`
- `ts.list_symbols(file)`
- `ts.reindex(path?)` — переиндексация одного файла либо полный скан
- `ts.stats()`

**vec.\***
- `vec.search(query, limit?, language?)`
- `vec.reindex(path?)`
- `vec.count()`

**lsp.\***
- `lsp.definition(file, line, character)`
- `lsp.references(file, line, character, include_declaration?)`
- `lsp.hover(file, line, character)`
- `lsp.languages()`

### Настройка Ollama

Для работы векторного поиска необходимо установить Ollama на хост-систему:

1.  **Установите Ollama**:
    - macOS/Windows: Скачайте с [ollama.com](https://ollama.com).
    - Linux: `curl -fsSL https://ollama.com/install.sh | sh`
2.  **Скачайте модель эмбеддингов**:
    ```bash
    ollama pull nomic-embed-text
    ```
3.  **Убедитесь, что Ollama запущена**:
    По умолчанию она слушает `http://localhost:11434`. Если вы используете другой адрес, поправьте его в `.ai-tools/config.yaml`.

### Безопасность и Приватность

- **Все данные локальны**: Индексированный код, символы и векторы хранятся исключительно на вашем устройстве (SQLite в `.ai-tools/treesitter.db` и Qdrant в Docker).
- **Локальный LLM**: Эмбеддинги генерируются через Ollama на вашем хосте. Код не отправляется в облачные API (OpenAI, Anthropic и др.).
- **Безопасные порты**: MCP-серверы при запуске через `run` по умолчанию биндятся на `127.0.0.1`, что предотвращает доступ к ним из внешней сети.
- **Никакой телеметрии**: Приложение не собирает и не отправляет аналитику или отчеты об ошибках на внешние серверы.

### Структура проекта
```
main.go, cli_*.go          — cobra CLI
internal/
  config/   — конфиг и дефолтные ignore-листы (vendor, node_modules, …)
  fileutil/ — обход + ignore-фильтр
  watcher/  — рекурсивный fsnotify + debounce
  state/    — потокобезопасный bus статистики
  embedder/ — клиент Ollama /api/embeddings
  qdrant/   — REST-клиент Qdrant (collections / upsert / search / delete)
  store/    — SQLite (modernc.org/sqlite, pure-Go) для tree-sitter
  parser/   — tree-sitter биндинги и рекурсивный AST-чанкинг
  chunker/  — окно + AST-чанки (tree-sitter chunking)
  index/    — TreeSitter + Vector индексаторы (full scan + watch)
  lsp/      — JSON-RPC LSP-клиент и менеджер серверов на язык
  docker/   — нативный запуск Qdrant
  mcp/      — 3 MCP-сервера на github.com/mark3labs/mcp-go
  tui/      — дашборд на bubbletea + lipgloss
.ai-tools/  — служебные данные: config.yaml, treesitter.db, logs/, qdrant_storage
```

### Кастомизация
Правьте `.ai-tools/config.yaml`:
- `ignore` — список паттернов
- `extensions` — расширения для индексации
- `chunk_lines`, `chunk_overlap`
- `ollama.embed_model` и `ollama.embed_dim` (должны совпадать!)
- `qdrant.host`, `qdrant.port` (REST порт по умолчанию 6333)

### Работа с AI-агентами
Для AI-агентов (Claude, Junie и др.) подготовлен технический гайд на английском языке: [AGENTS.md](docs/AGENTS.md). Он помогает им эффективнее использовать предоставляемые MCP-серверы и "встраивать" их в свой процесс рассуждения.
