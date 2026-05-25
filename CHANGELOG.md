# Changelog

All notable changes to this project will be documented in this file.

## [v0.0.5] - 2026-05-25

### Added
- **Tree-sitter: извлечение JSDoc-ссылок**: `extractJSDocRefs` парсит `@param`, `@returns`, `@type` и создаёт `reference` edges на типы из `{Type}`.
- **Tree-sitter: поддержка namespace/module**: новые виды контейнеров `module_declaration`, `internal_module`, `namespace_declaration` → kind `namespace`.
- **Tree-sitter: метод `method_signature`**: распознаётся как `method` (TS/JS интерфейсы).
- **Tree-sitter: `searchHeritage`**: универсальный поиск `extends`/`implements` для всех языков.
- **Go: методы интерфейсов**: извлечение методов интерфейса как отдельных ASTUnit с `ParentID` на интерфейс.
- **Go: `NameStartLine`/`NameStartCol`**: точные позиции имён функций/типов для LSP-навигации.
- **LSP: `handleImplementation`**: новый MCP-инструмент `lsp.implementation(file, line, character)`.
- **LSP: тесты**: `comprehensive_test.go` — тесты Implementation для JS/Python/Go/Java.
- **Symbols: LSP fallback для `find_implementations`**: при наличии LSP-менеджера делается запрос `textDocument/implementation` для точного поиска.
- **Symbols: fallback для методов**: поиск по последнему сегменту имени (`Logger.log` → `log`).
- **Symbols: `findCallees` fallback**: поиск целей вызовов по имени при неразрешённых `dst_id`.

### Changed
- **`ResolvePendingEdges`: 4-проходный алгоритм**: qualified → локальный name (по файлу) → относительные пути (JS/TS) → глобальный name. Временная таблица `tmp_resolved` для производительности.
- **`FindASTUnits`: двухфазный поиск**: сначала точное совпадение (использует индексы), затем LIKE для добора результатов.
- **LSP: обработка уведомлений**: `handleServerNotification` в горутине — не блокирует `readLoop`.
- **LSP: таймаут Call**: уменьшен с 120s до 30s.
- **LSP: `hoverString`**: улучшен парсинг `MarkupContent` и `MarkedString`.
- **LSP: Python Implementation**: явная проверка на отсутствие поддержки на протокольном уровне.
- **LSP: TypeScript capabilities**: `implementation.linkSupport = false`.
- **Go parser: ParentID для методов**: post-process для установки `ParentID` на тип-владелец.
- **Documentation**: уточнены описания `sym.get_semantic_neighborhood` (требует symbol_id) и `sym.get_dependency_graph` (для Go нужен полный путь).

### Fixed
- **LSP: `$/.progress` токен**: обработка `number` и `string` токенов (gopls/pyright).
- **LSP: гонка при обработке уведомлений**: уведомления обрабатываются в отдельной горутине.

---

## [v0.0.4] - 2026-05-24

### Added
- **Multi-repo workspace с auto-discovery (`internal/repos`, новый пакет)**. `repos.Discover(root)` определяет режим по содержимому `cfg.Root`:
  - single-repo, если в самом корне есть `.git` (имя = `basename(root)`);
  - multi-repo workspace, если в корне нет `.git`, но в его непосредственных поддиректориях есть `.git` — каждая такая поддиректория = отдельная репа; «соседние» поддиректории без `.git` тоже попадают в индекс как «прицепленные репы». Скрытые директории (`.*`) пропускаются.
  При коллизии имён к дубликату добавляется короткий sha1-суффикс. `repos.Resolver` сопоставляет абсолютный путь файла → имя репы (prefix-match).
- **LSP per-repo (`internal/lsp/manager.go`)**: кэш клиентов перешёл на composite-ключ `(repo, language)` — в multi-repo workspace каждая репа поднимает собственный LSP-инстанс с `rootURI = repo.Path`. Новый `Manager.SetRepoResolver(*repos.Resolver)` и `GetForRepo(ctx, repo, lang, root)`; `EnsureOpen` резолвит репу по abs-пути файла и выбирает соответствующего клиента.
- **Watcher routing (`internal/watcher/watcher.go`)**: метод `Watcher.SetRepoResolver(*repos.Resolver)` и поле `Event.Repo`, заполняемое prefix-match по `AbsPath`.
- **`repo` в `sym.*` MCP-тулах (`internal/mcp/symbol.go`)**: новый опциональный параметр `repo` (string | JSON-массив | CSV | `*`/пусто) у `find_callers`, `find_references`, `find_implementations`, `find_callees`, `expand_neighbors`, `get_call_graph`.
- **Поле `repo` в схеме SQLite (`internal/store/sqlite.go`)**: колонка `repo TEXT NOT NULL DEFAULT ''` в `ast_units` и `edges`, новые индексы `(repo, qualified)`, `(repo, name)`, `(repo, dst_name, kind)`. Новая служебная таблица `meta` для хранения `workspace_signature`.
- **`store.OpenFresh(path, signature)`**: открывает БД, предварительно удаляя файлы при несовпадении сохранённого workspace-signature (смена состава репо/корня инвалидирует старый граф).
- **Repo-aware API store**: `EdgesByDstNameForLangRepo(ctx, name, kind, lang, repo)` и `FindASTUnits(...)` с параметром `repo`.
- **`repo` в vector / BM25 / MCP**: все `vec.*` и `ts.search_symbols` инструменты принимают опциональный параметр `repo`.
- **MCP: новые семантические инструменты**: `sym.get_execution_context`, `sym.get_symbol_summary`, `sym.get_file_intent`, `sym.get_semantic_neighborhood`, `sym.get_call_graph` (с поддержкой `function`).
- **E2E-тест multi-repo (`internal/astindex/multirepo_e2e_test.go`)**: проверка изоляции репо при индексации и поиске.

### Changed
- **Граф никогда не пересекает границы репо.** `ResolvePendingEdges` резолвит `dst_id` в пределах одного языка И одной репы.
- **LSP без Docker**: LSP-серверы запускаются как локальные дочерние процессы через stdio.
- **Реорганизация архитектуры**: `main.go` и все `cli_*.go` перенесены в `cmd/ragota/` (стандартный Go-layout).
- **Производительность `astindex.FullScan`**: общий резолв рёбер в конце вместо N+1 проходов.

### Fixed
- **BFS-обход графа (`internal/store/neighbors.go`)**: дедупликация рёбер по `Edge.ID`.
- **jdtls: стабильность и производительность**: уведомление `language/status: ServiceReady`, лимит памяти JVM 4G.
- **Диагностика LSP**: логи в `.ragota/logs/lsp-debug.log`.

### Documentation
- **`ARCHITECTURE.md` в корне**: карта слоёв, потоки данных, правила для контрибьюторов.
- **AGENTS.md**: Scenario D (Symbol Context Investigation).

## [v0.0.3] - 2026-05-23

### Added
- **Гибридный поиск (RRF)**: Реализован алгоритм Reciprocal Rank Fusion для объединения результатов векторного и лексического (BM25) поиска, что значительно повышает релевантность выдачи.
- **Инструмент реранкинга**: Добавлен `vec.rerank` для вторичной сортировки результатов с использованием Cross-Encoder моделей (поддержка `bge-reranker` и генеративных моделей).
- **Индексация комментариев**: Добавлен сбор doc-комментариев для всех языков (Go, TS, JS, Python, Java). Комментарии теперь учитываются в векторном поиске и BM25.
- **Поддержка Cross-Encoders в Ollama**: Реализован механизм `embedding fallback` для моделей типа `bge-reranker-v2-m3`.
- **Улучшенный поиск ссылок**: `sym.find_references` теперь поддерживает квалифицированные имена (`package.symbol`).
- **Регистронезависимость**: Поиск символов в SQLite переведен на `COLLATE NOCASE`.
- **Новая политика для агентов**: В `docs/AGENTS.md` добавлена инструкция по приоритетному использованию `ts.search_symbols`.

### Changed
- **Переработка пайплайна индексации**:
  - Многопоточный парсинг и обработка файлов (Workers).
  - Глобальный батчинг эмбеддингов для ускорения работы с Ollama.
  - Инкрементальная индексация на основе хеширования файлов (SHA-1).
- **Синхронизация ID**: Инструменты `ts.*` и `sym.*` используют общую таблицу `ast_units`.
- **Модель по умолчанию**: В качестве реранкера установлена `qllama/bge-reranker-v2-m3`.
- **Обновление MCP инструментов**: Уточнены описания параметров (различие между `function` и `method` в Go).

### Fixed
- **Null-safety**: Гарантированный возврат пустых массивов `[]` вместо `null`.
- **LSP Pathing**: Исправлено дублирование путей в `lsp.definition` (ошибка в `SecureJoin`).
- **Ollama Rerank**: Устранена проблема бесконечной генерации за счет настройки стоп-токенов.

## [v0.0.2] - 2026-05-23
- Базовая поддержка Symbol Graph для Go и Java.
- Интеграция с Ollama для эмбеддингов.
- Базовая реализация BM25.

## [v0.0.1] - 2026-05-22
- Первый публичный релиз.
- Базовый функционал MCP серверов.
