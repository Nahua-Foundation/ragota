# ragota

Единый бинарь: **4 MCP-сервера**, гибридный поиск (vector + BM25 + reranker), AST/graph-навигация и TUI-дашборд для индексации проектов.

## Быстрый старт

### 1. Установка

```bash
# Сборка
go build -o ragota ./cmd/ragota

# Автоустановка зависимостей (Docker, Ollama, модели, LSP-серверы)
./ragota install
```

**Требования:** Go 1.26+, Docker, Ollama, macOS/Linux с Xcode CLI (CGO для tree-sitter).

### 2. Запуск

```bash
# Всё сразу: LSP + Tree-Sitter + Vector + Symbol + индексация + TUI
./ragota run -ltvsw --env local /path/to/project

# Только индексация и TUI
./ragota watch /path/to/project

# Конфиг для Claude Desktop
./ragota mcp-config
```

### 3. Полезные команды

| Команда | Описание |
|---|---|
| `./ragota run -ltvsw .` | Запустить всё |
| `./ragota watch .` | Только индексация + TUI |
| `./ragota gen-config` | Сгенерировать `~/.ragota/config.yaml` |
| `./ragota mcp-config` | JSON для MCP-клиентов |
| `./ragota clean` | Очистить индексы |
| `./ragota install` | Установить зависимости |

## Что внутри

| Сервер | Назначение |
|---|---|
| **ts** (Tree-Sitter) | Структурный поиск символов: функции, методы, классы, интерфейсы |
| **vec** (Vector) | Гибридный поиск: семантический (Qdrant) + лексический (BM25) + реранкер |
| **sym** (Symbol) | Навигация по коду: callers/callees, references, implementors, call graph |
| **lsp** (LSP) | Прокси к `gopls`, `typescript-language-server`, `pyright`, `jdtls` |

Поддерживаемые языки: **Go, TypeScript/TSX, JavaScript/JSX, Python, Java**.

## Режимы запуска

| Режим | Qdrant | LSP | Ollama |
|---|---|---|---|
| `--env local` (default) | Docker | Хост (PATH) | Хост |
| `--env docker` | Docker | Docker-контейнер | Хост |

Флаги `run`: `-l` (LSP), `-t` (Tree-Sitter), `-v` (Vector), `-s` (Symbol), `-w` (Watch/TUI).

## Документация

| Документ | Содержание |
|---|---|
| [docs/mcp-servers.md](docs/mcp-servers.md) | Все MCP-серверы и их методы |
| [docs/multi-repo.md](docs/multi-repo.md) | Multi-repo workspace, скоупирование поиска |
| [docs/lsp-setup.md](docs/lsp-setup.md) | Настройка LSP-серверов и troubleshooting |
| [docs/configuration.md](docs/configuration.md) | Конфигурация config.yaml |
| [ARCHITECTURE.md](ARCHITECTURE.md) | Архитектура проекта для контрибьюторов |
| [docs/AGENTS.md](docs/AGENTS.md) | Гайд для AI-агентов (на английском) |

## Безопасность

- Все данные локальны: SQLite, Bleve, Qdrant (Docker-volume)
- Эмбеддинги и реранкер — Ollama на хосте, код не отправляется в облако
- MCP-серверы биндятся на `127.0.0.1`
- Никакой телеметрии

## Структура проекта

```
cmd/ragota/        — CLI (cobra)
pkg/
  config/           — YAML-конфиг, пути, дефолты
  docker/           — Docker container runner
  fileutil/         — Walk, hash, ignore-match, SecureJoin
  logger/           — zerolog wrapper
  lsp/              — LSP-клиент + менеджер (gopls, pyright, tsserver, jdtls)
  qdrant/           — HTTP-клиент Qdrant REST
  repos/            — Multi-repo discovery + resolver
  state/            — In-memory шина статусов
  watcher/          — fsnotify + debounce
internal/
  indexing/         — Индексаторы (ast, vector, treesitter, parser, chunker, embedder)
  search/           — Поиск (graph, symbols, hybrid, rerank, bm25)
  store/            — SQLite (ast_units, edges, embed_meta)
  transport/        — MCP-серверы + TUI
.ragota/            — Служебные данные (config, БД, индексы, логи)
```
