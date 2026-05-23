# ARCHITECTURE

Этот документ описывает архитектуру проекта `ai-tools` после серии декомпозиций
и реорганизации (см. `CHANGELOG.md`). Он адресован и людям, и AI-агентам,
которые правят код.

## TL;DR

`ai-tools` — единый Go-бинарь, который:

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
go build -o ai-tools ./cmd/ai-tools
```

```
cmd/ai-tools/                 # пакет main
  main.go                     # cobra-роутер подкоманд
  cli_run.go                  # run     — всё-в-одном (watch + MCP + TUI)
  cli_serve.go                # serve-* — отдельный MCP-сервер по stdio
  cli_watch.go                # watch   — индексация + TUI без MCP
  cli_clean.go                # clean   — снести индексы/БД
  cli_genconfig.go            # gen-config
  cli_install.go              # install — docker pull + ollama pull
  cli_mcpconfig.go            # mcp-config — печать JSON для MCP-клиента
```

`cmd/ai-tools` — единственный пакет, который знает про cobra, флаги, главный
жизненный цикл процесса. Никакой бизнес-логики в нём нет — он только
собирает зависимости из `internal/*` и запускает их.

## Слои `internal/`

```
                      ┌─────────────────────────────────┐
        TRANSPORT     │  mcp/   tui/   (cmd/ai-tools)   │
                      └───────────────▲─────────────────┘
                                      │
                      ┌───────────────┴─────────────────┐
        SERVICE       │ index/  graph/  astindex/       │
                      │ hybrid/ rerank/ symbols/        │
                      │ watcher/ docker/ state/         │
                      └───────────────▲─────────────────┘
                                      │
                      ┌───────────────┴─────────────────┐
        DOMAIN/IO     │ store/   parser/   chunker/     │
                      │ embedder/ qdrant/ bm25/ lsp/    │
                      │ config/  fileutil/              │
                      └─────────────────────────────────┘
```

Направление зависимостей строго сверху вниз. Нижние слои **не импортируют**
верхние.

### Domain / IO (нижний слой)

| Пакет | Ответственность |
|---|---|
| `internal/config` | YAML-конфиг, пути (`~/.ai-tools/...`), дефолты. Декомпозирован на `types.go` / `defaults.go` / `paths.go` / `io.go`. |
| `internal/store` | SQLite-хранилище (`ast_units`, `edges`, `embed_meta`). Без знания о LSP. Декомпозирован на `sqlite.go` / `ast_units.go` / `edges.go` / `neighbors.go` / `embed_meta.go` / `graph.go` (типы). |
| `internal/parser` | tree-sitter парсер для go/ts/js/py/java → `Symbol` + чанки. Декомпозирован на `parser.go` / `languages.go` / `symbols.go` / `chunks.go` / `imports.go` / `util.go`. |
| `internal/chunker` | Семантическое + оконное разбиение исходников. |
| `internal/embedder` | HTTP-клиент Ollama для embedding-ов (`/api/embed`, fallback на legacy `/api/embeddings`). |
| `internal/qdrant` | HTTP-клиент Qdrant REST. |
| `internal/bm25` | Bleve-индекс для BM25-ретривала. |
| `internal/lsp` | LSP-клиент (stdio + JSON-RPC) и менеджер серверов. Декомпозирован на 8 файлов (`client.go` / `jsonrpc.go` / `lifecycle.go` / `documents.go` / `navigation.go` / `types.go` / `uri.go` / `debug.go` + `server_spec.go` + `client_*.go` per-language). |
| `internal/fileutil` | Обход дерева репозитория с учётом ignore-списков. |

### Service (средний слой)

| Пакет | Ответственность |
|---|---|
| `internal/astindex` | Извлечение AST-units и edges из исходников, запись в `store`. Декомпозирован на `astindex.go` (оркестрация) / `parse_go.go` (Go-специфичный) / `parse_generic.go` (tree-sitter generic) / `treesitter_extractor.go` (java/ts/js специфичный) / `util.go`. |
| `internal/index` | Векторный индекс (Qdrant + Ollama + Bleve). Декомпозирован на `vector.go` (типы/Init) / `fullscan.go` (батч-пайплайн) / `indexfile.go` (инкрементальный путь) / `search.go` / `hybrid_adapter.go` / `treesitter.go`. |
| `internal/graph` | High-level API code-graph: `Callers`/`Callees`/`References`/`Implementations`. Объединяет данные из `store` (tree-sitter) и `lsp` (живые ссылки) с TTL-кэшем. |
| `internal/hybrid` | RRF и weighted-sum fusion для вектор + BM25. |
| `internal/rerank` | Cross-encoder/LLM-реранкер через Ollama. Декомпозирован на `rerank.go` / `noop.go` / `ollama.go` / `embed.go` / `prompt.go`. |
| `internal/symbols` | Поиск символов по AST-units (быстрый go-tools-style API). |
| `internal/watcher` | fsnotify-обёртка с дебаунсом. |
| `internal/docker` | Управление docker-compose стеком (Qdrant + Ollama). |
| `internal/state` | In-memory шина статусов для TUI/CLI (потокобезопасная). |

### Transport (верхний слой)

| Пакет | Ответственность |
|---|---|
| `internal/mcp` | MCP-инструменты (`mark3labs/mcp-go`): `vector.go` / `treesitter.go` / `symbol.go` / `lspsrv.go` + `util.go`. |
| `internal/tui` | bubbletea-TUI. Декомпозирован на `tui.go` / `model.go` / `view.go` / `render.go` / `util.go`. |
| `cmd/ai-tools` | CLI (cobra). |

## Поток данных при индексации (`run`/`watch`)

```
fs walk ──► parser ──► chunker ──┬──► embedder ──► qdrant   (вектор)
                                 ├──► bm25                  (текст)
                                 └──► astindex ──► store    (AST + рёбра)

LSP (gopls/pyright/tsserver/jdtls) ──► graph.Service ──► MCP-ответ
                                          ▲
                                       store (tree-sitter граф)
```

## Поток данных при MCP-запросе

```
MCP-клиент ──stdio──► mcp.Tool handler
                          │
                          ├──► index.Vector.Search    (Qdrant + BM25 + RRF)
                          │            └──► rerank
                          ├──► graph.Service.Callers/Callees/References
                          │            ├──► store (tree-sitter)
                          │            └──► lsp   (живые ссылки)
                          └──► symbols.Find
```

## Тесты

```
hybrid/         RRF + weighted-sum, моки ретриверов
chunker/        оконное и AST-разбиение
bm25/           full-flow (Open → Index → Search → persistence)
store/          in-memory SQLite через modernc.org/sqlite
parser/         (требует CGO — пока без unit-тестов)
astindex/       parseGo на Go-сэмпле (без БД)
rerank/         helpers + HTTP-моки Ollama через httptest
config/         load/save YAML, paths, defaults
embedder/       HTTP-моки Ollama
qdrant/         HTTP-моки Qdrant REST
state/          in-memory шина: Snapshot/Persist/счётчики
mcp/            util.go: упаковка ответов
lsp/            client_java_test.go: JDTLS-специфика
fileutil/       walk с ignore
```

Внешние сервисы (Qdrant, Ollama, LSP, docker) везде замоканы через
`httptest.Server` или стабовые интерфейсы — настоящих сервисов для тестов
не требуется.

## Правила для контрибьюторов / AI-агентов

1. **Не нарушай направление зависимостей.** `store` не знает про `lsp`,
   `parser` не знает про `index`, и так далее. Если возник цикл — значит,
   нужен новый промежуточный пакет, а не «временный» импорт.
2. **Не добавляй CLI-флаги в `internal/*`.** Конфигурация приходит через
   `internal/config.Config` или явные аргументы конструктора.
3. **Каждый новый внешний сервис — со своим HTTP-моком в тестах.**
   См. `internal/qdrant/client_test.go` и `internal/embedder/ollama_test.go`
   как образцы.
4. **Файлы >400 строк — повод декомпозировать.** Большие файлы режутся
   по доменам в одном пакете (см. `internal/lsp/`, `internal/store/`,
   `internal/index/`, `internal/config/`, `internal/parser/`,
   `internal/tui/`, `internal/rerank/`).
5. **Docstring пакета — обязателен** и должен содержать карту файлов-доменов
   (см. примеры в `internal/store/graph.go`, `internal/index/vector.go`).
