# ai-tools

Единый бинарь, объединяющий **четыре MCP-сервера**, гибридный (vector + BM25) поиск с реранкером, AST/graph-индекс для symbol-aware навигации, докер-инфраструктуру и live-дашборд для индексации проектов.

### Возможности

- **tree-sitter MCP (`ts`)** — разбирает код на символы (function/method/class/interface/type/enum/var/const) для Go, TypeScript/TSX, JavaScript/JSX, Python, Java; хранит их в SQLite; следит за изменениями в директории.
- **vector MCP (`vec`)** — гибридный (vector + BM25) поиск с опциональным реранкингом:
  - Чанкинг через tree-sitter (Go/TS/JS/Python/Java) либо построчное окно для остальных.
  - **Две раздельные коллекции в Qdrant**: код (`qwen3-embedding`, 1024-dim) и текст/markdown (`nomic-embed-text`, 768-dim).
  - **BM25** — локальный лексический индекс на [Bleve](https://github.com/blevesearch/bleve), pure-Go.
  - **Reranker** — Ollama `qllama/bge-reranker-v2-m3` с graceful fallback (если модель не подгружена — реранкинг пропускается, в логе warning).
  - **Авто-reindex** при смене embed-модели: метаданные хранятся в SQLite (`embed_meta`), коллекция кода автоматически пересоздаётся, markdown-индекс не трогается.
- **symbol MCP (`sym`)** — symbol-aware навигация поверх AST units + code graph (`ast_units`, `edges` в SQLite). Извлечение:
  - **Go** — `go/parser` + `go/ast` (точно: functions, methods, types, imports, calls, embedded types).
  - **Java / TypeScript / JavaScript** — tree-sitter (classes, interfaces, methods, functions, calls, imports, `extends`, `implements`).
  - **Python** — AST units (без edges).
- **LSP MCP (`lsp`)** — проксирует официальные LSP-сервера (`gopls`, `typescript-language-server`, `pyright-langserver`, `jdtls`). Работает с локально установленными серверами.
- **`ai-tools watch .`** — поднимает Qdrant в Docker (если указано `--start-docker`), использует Ollama на хосте, запускает индексаторы (vector + BM25 + AST/graph), открывает TUI-дашборд (bubbletea).

### Требования

- Go 1.26+
- macOS/Linux с Xcode CLI tools (CGO для tree-sitter)
- Docker (для Qdrant)
- Ollama на хосте с моделями:
  - `qwen3-embedding` — эмбеддинги кода (обязательно)
  - `nomic-embed-text` — эмбеддинги markdown/текста (обязательно)
  - `qllama/bge-reranker-v2-m3` — реранкер (опционально; при отсутствии — fallback)
- В PATH для LSP MCP: `gopls`, `typescript-language-server`, `pyright-langserver`, `jdtls` (опционально, любой набор). Либо настроенные Docker-контейнеры в конфиге.

### Установка зависимостей

#### Автоматическая установка
```bash
./ai-tools install
```
Команда проверит наличие Docker, Ollama, всех нужных моделей (`qwen3-embedding`, `nomic-embed-text`, реранкер) и LSP-серверов и предложит установить недостающие. Обязательные модели помечены как required, реранкер — optional.

#### Ручная установка
- **Docker**: [инструкция](https://docs.docker.com/get-docker/).
- **Ollama**: см. раздел [Настройка Ollama](#настройка-ollama).
- **LSP-серверы**:
    - **Go**: `go install golang.org/x/tools/gopls@latest`
    - **TypeScript/JavaScript**: `npm install -g typescript-language-server typescript`
    - **Python**: `npm install -g pyright`
    - **Java**: установите [jdtls](https://github.com/eclipse-jdtls/eclipse.jdt.ls) и убедитесь, что бинарник `jdtls` доступен в PATH.

### Сборка
```
go build -o ai-tools ./cmd/ai-tools
```

### Запуск

Основная команда — `run`. При первом запуске ищется `.ai-tools/config.yaml` в проекте или `~/.ai-tools/config.yaml`. Если их нет — используются дефолты.

#### Примеры запуска

1. **Запустить всё сразу** (LSP + Tree-Sitter + Vector + Symbol MCP + индексация + TUI + Docker для Qdrant):
   ```bash
   ./ai-tools run -ltvsw --start-docker .
   ```
   *Флаги: `-l` (LSP), `-t` (Tree-Sitter), `-v` (Vector), `-s` (Symbol), `-w` (Watch/Индексация).*

2. **Только индексация и TUI**:
   ```bash
   ./ai-tools watch .
   ```
   *(Эквивалентно `./ai-tools run -w .`)*

3. **Все MCP-серверы для Claude Desktop** (без TUI):
   ```bash
   ./ai-tools run -ltvs --no-tui .
   ```

4. **Только symbol-aware навигация**:
   ```bash
   ./ai-tools run -s .
   ```

- `./ai-tools gen-config` — сгенерировать конфиг (по умолчанию `~/.ai-tools/config.yaml`).
- `./ai-tools mcp-config` — сгенерировать JSON для Claude Desktop / других MCP-клиентов (включает все 4 сервера).
- `./ai-tools clean` — очистить локальный индекс (SQLite, Qdrant-коллекции, Bleve).
- `q`/`Esc`/`Ctrl+C` — выход из TUI.

#### Флаги `run`
- `-l`, `--lsp` — MCP-сервер LSP
- `-t`, `--ts`  — MCP-сервер Tree-Sitter
- `-v`, `--vec` — MCP-сервер Vector (hybrid + rerank)
- `-s`, `--sym` — MCP-сервер Symbol (AST/graph)
- `-w`, `--watch` — индексация + TUI
- `--start-docker` — поднять Qdrant из секции `docker:` конфига
- `--no-tui` — не открывать дашборд

### MCP-серверы по отдельности (stdio)

```
ai-tools serve-treesitter --root /path/to/project
ai-tools serve-vector     --root /path/to/project
ai-tools serve-symbol     --root /path/to/project
ai-tools serve-lsp        --root /path/to/project
```

#### Методы MCP

**ts (Tree-Sitter)** — структурный поиск символов
- `ts.search_symbols(query, kind?, language?, limit?)` — поиск по имени. **Go-specific**: `kind="function"` finds only functions (e.g., `func foo()`), `kind="method"` finds only methods (e.g., `func (r Receiver) foo()`). Use the correct kind for your search target.
- `ts.list_symbols(file)` — дерево символов файла.
- `ts.reindex(path?)` — переиндексация.
- `ts.stats()` — статистика индекса.

**vec (Vector)** — гибридный семантический + лексический поиск
- `vec.search_semantic(query, top_k?, language?)` — только vector (Qdrant).
- `vec.search_keyword(query, top_k?, language?)` — только BM25 (Bleve).
- `vec.search_hybrid(query, top_k?, language?)` — слияние vector + BM25 (RRF или weighted-sum, см. `hybrid.*` в конфиге).
- `vec.rerank(query, candidates, top_n?)` — реранк списка кандидатов через BGE-Reranker.
- `vec.search(query, limit?, language?)` — alias к `search_hybrid` (backward-compatible).
- `vec.reindex(path?)` — обновление vector + BM25 индексов.
- `vec.count()` — число чанков.

**sym (Symbol)** — symbol-aware навигация (AST units + code graph)
- Symbol lookup:
  - `sym.find_definition(symbol)`
  - `sym.find_references(symbol)`
  - `sym.find_implementations(interface)`
  - `sym.find_callers(function)`
  - `sym.find_callees(function)`
- AST / structure:
  - `sym.get_file_symbols(path)`
  - `sym.get_symbol(symbol_id)`
  - `sym.get_parent(symbol_id)`
  - `sym.get_children(symbol_id)`
- Graph:
  - `sym.expand_neighbors(node_id, depth)` — BFS по edges.
  - `sym.get_dependency_graph(module)` — граф import-связей.
  - `sym.get_call_graph(function)` — граф вызовов.
- Context:
  - `sym.get_surrounding_context(symbol_id)`
  - `sym.get_related_files(symbol_id)`
  - `sym.get_similar_code(symbol_id)` — семантически близкие фрагменты через vector.

**lsp (Language Server Protocol)** — навигация через родные LSP
- `lsp.definition(file, line, character)`
- `lsp.references(file, line, character, include_declaration?)`
- `lsp.hover(file, line, character)`
- `lsp.languages()`

### Настройка LSP

`ai-tools` запускает LSP-серверы **локально** (как дочерние процессы через stdio). Docker для LSP больше не используется — это упрощает маппинг путей и убирает оверхед на контейнер. Убедитесь, что нужные серверы установлены и доступны в `PATH`.

#### Инструкции по установке:

- **Go (gopls)**:
  ```bash
  go install golang.org/x/tools/gopls@latest
  ```
- **TypeScript/JavaScript (typescript-language-server)**:
  ```bash
  npm install -g typescript typescript-language-server
  ```
- **Python (pyright)**:
  ```bash
  npm install -g pyright
  ```
- **Java (jdtls)**:
  - macOS: `brew install jdtls`
  - Linux / вручную: скачайте [Eclipse JDT.LS](https://github.com/eclipse-jdtls/eclipse.jdt.ls) и положите `jdtls` в `PATH`.
  - Требуется **JDK 21+** (`java -version`). На JDK <17 jdtls не стартует, на JDK 24+ возможны WARNING о deprecated reflection — они безопасны.

Пример секции `lsp` в `config.yaml`:
```yaml
lsp:
  - language: go
    command: gopls
  - language: java
    command: jdtls
    args: ["-data", ".ai-tools/jdtls-data"]
  - language: typescript
    command: typescript-language-server
    args: ["--stdio"]
  - language: python
    command: pyright-langserver
    args: ["--stdio"]
```

#### Возможные ошибки и их исправление

- **`jdtls` запускается, но `lsp.definition/hover/references` возвращают `null` или пустой результат.**
  - Причина: jdtls долго импортирует Maven/Gradle проект (30–120с на первый запуск) — клиент дожидается уведомления `language/status: ServiceReady`.
  - Что делать: подождать; при больших проектах увеличить таймаут через env `JDTLS_READY_TIMEOUT=240` (секунды).
  - Лог: `.ai-tools/logs/lsp-debug.log` (в корне проекта; путь можно переопределить env `AI_TOOLS_LSP_LOG`) — ищите `LSP java: ready signal received`. Если вместо него `ready timeout` — сервер не успел проиндексировать.

- **`References RESULT: locations=0` (пусто), хотя символ заведомо используется.**
  - Причина: файл лежит вне source roots Maven/Gradle (в корне проекта рядом с `pom.xml`, а не в `src/main/java/`). jdtls в режиме Maven индексирует только `src/main/java/**` и `src/test/java/**`.
  - Что делать: переместить файл в `src/main/java/`, либо удалить `pom.xml`/`build.gradle` (jdtls перейдёт в invisible-project режим и проиндексирует всё).

- **`process exited: signal: killed` сразу после успешного ответа.**
  - Причина: была починена — ранее процесс получал SIGKILL по отмене RPC-контекста. Если симптом вернулся: проверьте, что JDK даёт jdtls достаточно памяти (по умолчанию `-Xmx4G`); на маленькой машине поднимите/опустите явно.

- **`WARNING: sun.misc.Unsafe...`, `Using incubator modules`, `Final field mutation` в stderr.**
  - Это шум JVM (JDK 24+) от внутренних библиотек jdtls (Guice/Plexus/Sisu). Не влияет на работу, отфильтровывается из логов.

- **`jdtls --version` пишет `Could not load Gradle version information`.**
  - Безвреден: значит Gradle wrapper отсутствует или не в PATH. Для Maven-проектов не нужен.

- **`pyright` / `typescript-language-server` не находит определения.**
  - Убедитесь, что в корне проекта есть `pyrightconfig.json`/`pyproject.toml` (для Python) или `tsconfig.json`/`package.json` (для TS). Без них серверы работают в режиме single-file.
  - Установлены ли пакеты проекта (`npm install`, `pip install -e .`) — без них типы из зависимостей не разрешаются.

- **`LSP <lang>: client dead, recreating` повторяется на каждом запросе.**
  - Откройте `.ai-tools/logs/lsp-debug.log` и посмотрите строку `process exited: ...` — там будет `exit_code` и `signal`. `signal=killed` обычно означает OOM; увеличьте `-Xmx` (для Java) или память контейнера. Реальные Exception'ы jdtls видны в `stderr tail`.

### Настройка Ollama

1.  **Установка**:
    - macOS/Windows: [ollama.com](https://ollama.com).
    - Linux: `curl -fsSL https://ollama.com/install.sh | sh`
2.  **Модели**:
    ```bash
    ollama pull qwen3-embedding:0.6b              # эмбеддинги кода (1024-dim)
    ollama pull nomic-embed-text             # эмбеддинги markdown/текста (768-dim)
    ollama pull qllama/bge-reranker-v2-m3   # опционально — реранкер
    ```
3.  **Запуск**: по умолчанию `http://localhost:11434`. Адрес можно поменять в `.ai-tools/config.yaml` (`ollama.url`).

### Безопасность и приватность

- **Все данные локальны**: SQLite (`.ai-tools/treesitter.db`), Bleve (`.ai-tools/bm25/`), Qdrant (Docker-volume).
- **Локальный LLM**: эмбеддинги и реранкер — Ollama на вашем хосте. Код не отправляется в облачные API.
- **Безопасные порты**: MCP-серверы при `run` биндятся на `127.0.0.1`.
- **Никакой телеметрии**.

### Структура проекта
```
main.go, cli_*.go          — cobra CLI
internal/
  config/    — конфиг (collections, bm25, rerank, hybrid)
  fileutil/  — обход + ignore-фильтр
  watcher/   — рекурсивный fsnotify + debounce
  state/     — потокобезопасный bus статистики
  embedder/  — клиент Ollama /api/embeddings
  qdrant/    — REST-клиент Qdrant
  store/     — SQLite (modernc.org/sqlite, pure-Go): symbols, ast_units, edges, embed_meta
  parser/    — tree-sitter биндинги и AST-чанкинг
  chunker/   — окно + AST-чанки
  astindex/  — извлечение AST units + edges (Go: go/ast; Java/TS/JS: tree-sitter)
  bm25/      — Bleve-индекс (pure-Go BM25)
  rerank/    — HTTP-клиент к Ollama-реранкеру (BGE) + graceful fallback
  hybrid/    — RRF / weighted-sum слияние vector + BM25
  graph/     — сервис над edges: callers/callees/imports/implementations + BFS
  symbols/   — symbol-aware retrieval (find_*, get_*, context, similar)
  index/     — TreeSitter + Vector индексаторы (full scan + watch, auto-reindex)
  lsp/       — JSON-RPC LSP-клиент и менеджер серверов
  docker/    — нативный запуск Qdrant
  mcp/       — 4 MCP-сервера (ts/vec/sym/lsp) на github.com/mark3labs/mcp-go
  tui/       — дашборд на bubbletea + lipgloss
.ai-tools/   — служебные данные: config.yaml, treesitter.db, bm25/, logs/, qdrant_storage
```

### Кастомизация

Правьте `.ai-tools/config.yaml`:

- `ignore`, `extensions` — фильтры обхода.
- `chunk_lines`, `chunk_overlap` — параметры чанкинга.
- `collections.code` / `collections.text` — имя коллекции, embed-модель и размерность для кода и текста раздельно. **При смене модели индекс кода будет автоматически пересоздан** (через `embed_meta`).
- `ollama.url` — адрес Ollama (legacy `ollama.embed_model` / `ollama.embed_dim` используются для текста, если `collections` не задан).
- `qdrant.host`, `qdrant.port` (REST по умолчанию 6333).
- `bm25.enabled`, `bm25.path`, `bm25.k1`, `bm25.b` — лексический индекс.
- `rerank.enabled`, `rerank.model`, `rerank.url`, `rerank.required`, `rerank.top_n` — реранкер. `required: false` → graceful fallback при недоступности модели.
- `hybrid.vector_weight`, `hybrid.bm25_weight`, `hybrid.rrf_k`, `hybrid.candidates_per_source` — параметры слияния. Если оба веса = 0 → используется RRF.
- `mcp.tree_sitter` (7771), `mcp.vector` (7772), `mcp.lsp` (7773), `mcp.symbol` (7774) — порты MCP-серверов.

### Миграция со старого индекса

Если вы обновляетесь с версии без `qwen3-embedding`:

1. `./ai-tools install` — подтянет новые модели.
2. При первом запуске `./ai-tools run -vw .` старый индекс кода будет автоматически удалён и пересоздан с новой моделью (markdown-индекс при этом сохраняется).
3. Если хочется начисто — `./ai-tools clean`.

### Работа с AI-агентами

Для AI-агентов (Claude, Junie и др.) подготовлен технический гайд на английском: [AGENTS.md](docs/AGENTS.md). Он описывает hybrid-first policy, reranker fallback, symbol/graph сценарии и семантику авто-reindex при смене embed-модели.
