# ARCHITECTURE

Этот документ описывает архитектуру проекта `ragota` после реструктуризации
файловой структуры. Он адресован и людям, и AI-агентам, которые правят код.

## TL;DR

`ragota` — единый Go-бинарь, который:

1. **Индексирует** репозиторий: парсит AST (tree-sitter + go/ast), бьёт код на
   чанки, считает embedding-ы и складывает их в Qdrant + Bleve (BM25) + SQLite
   (граф вызовов/ссылок).
2. **Отдаёт** результаты как **MCP-серверы** (stdio): `vector`, `tree-sitter`,
   `lsp` — для интеграции с AI-агентами.
3. **Дополняет** статический граф ссылками от LSP (gopls / pyright /
   tsserver / jdtls).
4. Имеет **TUI** для наблюдения за индексаторами и docker-сервисами.

## Сборка и точка входа

```bash
go build -o ragota ./cmd/ragota
```

```
cmd/ragota/main.go         # тонкая точка входа → вызывает cli.Execute()

internal/app/cli/           # пакет cli — cobra-роутер подкоманд
  main.go                     # Execute() — сборка rootCmd, expandRunShortFlags
  cli_run.go                  # run     — всё-в-одном (watch + MCP + TUI)
  cli_serve.go                # serve-* — отдельный MCP-сервер по stdio
  cli_watch.go                # watch   — индексация + TUI без MCP
  cli_clean.go                # clean   — снести индексы/БД
  cli_genconfig.go            # gen-config
  cli_install.go              # install — docker pull + ollama pull
  cli_mcpconfig.go            # mcp-config — печать JSON для MCP-клиента
  cli_current.go              # current — статистика файлов
```

`internal/app/cli` — единственный пакет, который знает про cobra, флаги,
главный жизненный цикл процесса. Никакой бизнес-логики в нём нет — он только
собирает зависимости и запускает их.

## Структура проекта

```
ragota/
├── cmd/ragota/               # Тонкая точка входа (package main)
├── pkg/                      # Переиспользуемые пакеты без бизнес-логики ragota
│   ├── config/                 # YAML-конфиг, пути, дефолты
│   ├── docker/                 # Docker container runner
│   ├── fileutil/               # Walk, hash, ignore-match, SecureJoin
│   ├── logger/                 # zerolog wrapper
│   ├── lsp/                    # LSP-клиент + менеджер (gopls, pyright, tsserver, jdtls)
│   │   ├── jsonrpc/              # Generic JSON-RPC 2.0 transport
│   │   ├── process/              # OS-процесс: запуск, мониторинг, остановка
│   │   ├── lang/                 # Language capabilities (go, python, typescript, java)
│   │   ├── session/              # Активная LSP-сессия (documents, navigation, handlers)
│   │   ├── lifecycle/            # Запуск + LSP handshake + debug log
│   │   └── manager/              # Кэш клиентов per (repo, language)
│   ├── qdrant/                 # HTTP-клиент Qdrant REST
│   ├── repos/                  # Multi-repo discovery + resolver
│   ├── state/                  # In-memory шина статусов (event bus)
│   └── watcher/                # fsnotify + debounce
├── internal/                   # Приватная бизнес-логика ragota
│   ├── indexing/               # Индексаторы
│   │   ├── ast/                  # AST units + edges extraction
│   │   ├── vector/               # Vector pipeline (fullscan, batcher, search)
│   │   ├── treesitter/           # Symbol indexer → SQLite
│   │   ├── parser/               # tree-sitter: symbols, chunks, imports
│   │   ├── chunker/              # Semantic + window chunking
│   │   └── embedder/             # Ollama embeddings client
│   ├── search/                 # Поиск и ранжирование
│   │   ├── graph/                # Code graph API + LLM enrichment
│   │   ├── symbols/              # Symbol-aware retrieval
│   │   ├── hybrid/               # RRF / weighted-sum fusion
│   │   ├── rerank/               # Ollama cross-encoder reranker
│   │   └── bm25/                 # Bleve BM25 index
│   ├── store/                  # SQLite: миграции, CRUD, доменные типы
│   ├── transport/              # Точки входа (транспортный слой)
│   │   ├── mcp/                  # MCP-серверы (vector, treesitter, lsp, symbol)
│   │   └── tui/                  # bubbletea дашборд
│   └── app/
│       └── cli/                  # cobra + DI
└── tests/                      # Интеграционные тестовые проекты
```

## Слои зависимостей

```
                      ┌─────────────────────────────────────┐
        TRANSPORT     │  transport/mcp/  transport/tui/     │
                      │  app/cli/                            │
                      └──────────────────▲──────────────────┘
                                         │
                      ┌──────────────────┴──────────────────┐
        SEARCH        │  search/graph/  search/symbols/     │
                      │  search/hybrid/ search/rerank/      │
                      │  search/bm25/                        │
                      └──────────────────▲──────────────────┘
                                         │
                      ┌──────────────────┴──────────────────┐
        INDEXING      │  indexing/ast/   indexing/vector/   │
                      │  indexing/treesitter/                │
                      │  indexing/parser/ indexing/chunker/ │
                      │  indexing/embedder/                  │
                      └──────────────────▲──────────────────┘
                                         │
                      ┌──────────────────┴──────────────────┐
        STORE         │  store/                              │
                      └──────────────────▲──────────────────┘
                                         │
                      ┌──────────────────┴──────────────────┐
        PKG           │  pkg/config/  pkg/docker/ pkg/lsp/  │
                      │  pkg/qdrant/  pkg/state/ pkg/repos/ │
                      │  pkg/logger/  pkg/fileutil/          │
                      │  pkg/watcher/                        │
                      └─────────────────────────────────────┘
```

Направление зависимостей строго сверху вниз. Нижние слои **не импортируют**
верхние.

### pkg/ — переиспользуемые пакеты

| Пакет | Ответственность |
|---|---|
| `pkg/config` | YAML-конфиг, пути (`~/.ragota/...`), дефолты. Файлы: `types.go` / `defaults.go` / `paths.go` / `io.go`. |
| `pkg/docker` | Нативный запуск контейнеров через docker CLI (без docker-compose): Qdrant + LSP-контейнер. Файлы: `docker.go` / `container.go` / `embeds.go`. |
| `pkg/fileutil` | Обход дерева репозитория с учётом ignore-списков, хэширование, `SecureJoin`. |
| `pkg/logger` | Глобальный структурированный логгер на базе zerolog. |
| `pkg/lsp` | LSP-клиент и менеджер. Корень пакета: `Client` interface, `Location`, `ServerSpec`, URI-хелперы. Подпакеты: `jsonrpc/` — generic JSON-RPC 2.0 transport; `process/` — OS-процесс (запуск, мониторинг); `lang/` — language capabilities (go, python, typescript, java); `session/` — активная сессия (documents, navigation, handlers); `lifecycle/` — запуск + handshake + debug log; `manager/` — кэш клиентов per (repo, language). |
| `pkg/qdrant` | Минимальный HTTP-клиент Qdrant REST: CRUD коллекций, upsert, search, count. |
| `pkg/repos` | Автоматическое обнаружение single-repo / multi-repo workspace по `.git`. |
| `pkg/state` | In-memory шина статусов для TUI/CLI (потокобезопасная). |
| `pkg/watcher` | fsnotify-обёртка с дебаунсом, фильтрацией по ignore/extensions. |

### internal/indexing/ — индексаторы

| Пакет | Ответственность |
|---|---|
| `indexing/ast` | Извлечение AST-units и edges из исходников, запись в `store`. Go-специфичный + tree-sitter generic экстракторы. |
| `indexing/vector` | Векторный индекс (Qdrant + Ollama + BM25): полный и инкрементальный пайплайн, поиск, адаптеры для hybrid. |
| `indexing/treesitter` | Индексатор символов tree-sitter → SQLite. |
| `indexing/parser` | tree-sitter парсер для go/ts/js/py/java → `Symbol` + чанки. |
| `indexing/chunker` | Семантическое + оконное разбиение исходников. |
| `indexing/embedder` | HTTP-клиент Ollama для embedding-ов (`/api/embed`, fallback на legacy). |

### internal/search/ — поиск и ранжирование

| Пакет | Ответственность |
|---|---|
| `search/graph` | High-level API code-graph: `Callers`/`Callees`/`References`/`Implementations`. Объединяет `store` (tree-sitter) и `lsp` с TTL-кэшем. LLM-обогащение через Ollama. |
| `search/symbols` | Поиск символов по AST-units: find_definition, find_references, find_implementations, find_callers, find_callees, навигация parent/children. |
| `search/hybrid` | RRF и weighted-sum fusion для вектор + BM25. |
| `search/rerank` | Cross-encoder/LLM-реранкер через Ollama с graceful fallback. |
| `search/bm25` | Bleve-индекс для BM25-ретривала. |

### internal/store/ — хранилище

| Пакет | Ответственность |
|---|---|
| `store` | SQLite-хранилище: файлы, символы, AST units, рёбра графа, embed metadata. Миграции при открытии. Доменные типы: `ASTUnit`, `Edge`. |

### internal/transport/ — транспортный слой

| Пакет | Ответственность |
|---|---|
| `transport/mcp` | MCP-инструменты (`mark3labs/mcp-go`): `vector.go` / `treesitter.go` / `symbol.go` / `lspsrv.go` + `util.go`. |
| `transport/tui` | bubbletea-TUI: статус docker, индексаторов, MCP-статистика, графики. |

## Поток данных при индексации (`run`/`watch`)

```
fs walk ──► parser ──► chunker ──┬──► embedder ──► qdrant   (вектор)
                                 ├──► bm25                  (текст)
                                 └──► ast ──► store         (AST + рёбра)

LSP (gopls/pyright/tsserver/jdtls) ──► graph.Service ──► MCP-ответ
                                          ▲
                                       store (tree-sitter граф)
```

## Поток данных при MCP-запросе

```
MCP-клиент ──stdio──► mcp.Tool handler
                          │
                          ├──► vector.Search    (Qdrant + BM25 + RRF)
                          │            └──► rerank
                          ├──► graph.Callers/Callees/References
                          │            ├──► store (tree-sitter)
                          │            └──► lsp   (живые ссылки)
                          └──► symbols.Find
```

## Тесты

```
search/hybrid/        RRF + weighted-sum, моки ретриверов
indexing/chunker/     оконное и AST-разбиение
search/bm25/          full-flow (Open → Index → Search → persistence)
store/                in-memory SQLite через modernc.org/sqlite
indexing/ast/         parseGo на Go-сэмпле (без БД)
search/rerank/        helpers + HTTP-моки Ollama через httptest
pkg/config/           load/save YAML, paths, defaults
indexing/embedder/    HTTP-моки Ollama
pkg/qdrant/           HTTP-моки Qdrant REST
pkg/state/            in-memory шина: Snapshot/Persist/счётчики
transport/mcp/        util.go: упаковка ответов
pkg/lsp/              client_java_test.go: JDTLS-специфика
pkg/fileutil/         walk с ignore
pkg/watcher/          fsnotify events
pkg/docker/           container runner
pkg/repos/            repo discovery
```

Внешние сервисы (Qdrant, Ollama, LSP, docker) везде замоканы через
`httptest.Server` или стабовые интерфейсы — настоящих сервисов для тестов
не требуется.

## Правила для контрибьюторов / AI-агентов

1. **Не нарушай направление зависимостей.** `store` не знает про `lsp`,
   `parser` не знает про `indexing/vector`, и так далее. Если возник цикл —
   значит, нужен новый промежуточный пакет, а не «временный» импорт.
2. **Не добавляй CLI-флаги в `internal/*` или `pkg/*`.** Конфигурация приходит
   через `pkg/config.Config` или явные аргументы конструктора.
3. **Каждый новый внешний сервис — со своим HTTP-моком в тестах.**
   См. `pkg/qdrant/client_test.go` и `indexing/embedder/ollama_test.go`
   как образцы.
4. **Файлы >400 строк — повод декомпозировать.** Большие файлы режутся
   по доменам в одном пакете.
5. **Docstring пакета — обязателен** и должен содержать карту файлов-доменов.
6. **`pkg/` — только переиспользуемое.** Если пакет специфичен для ragota
   и не имеет смысла вне проекта — ему место в `internal/`.
7. **`cmd/ragota/` — только тонкий `main.go`.** Вся логика CLI —
   в `internal/app/cli/`.
