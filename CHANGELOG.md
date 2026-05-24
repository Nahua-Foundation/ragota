# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased] - 2026-05-24

### Added
- **LSP per-repo (`internal/lsp/manager.go`)**: кэш клиентов перешёл на composite-ключ `(repo, language)` — в multi-repo workspace каждая репа поднимает собственный LSP-инстанс с `rootURI = repo.Path`. Новый `Manager.SetRepoResolver(*repos.Resolver)` и `GetForRepo(ctx, repo, lang, root)`; `EnsureOpen` резолвит репу по abs-пути файла и выбирает соответствующего клиента. Старые `Get`/`GetWithRoot` сохранены как обёртки (key с пустой репой) — обратная совместимость с single-workspace. Подключение в `cli_run.go` и `cli_serve.go` (serve-lsp/serve-symbol).
- **Watcher routing (`internal/watcher/watcher.go`)**: метод `Watcher.SetRepoResolver(*repos.Resolver)` и поле `Event.Repo`, заполняемое prefix-match по `AbsPath`. Подключено в `cli_watch.go` (с `repos.Discover` и выводом списка репо в multi-repo workspace) и `cli_run.go`.
- **`repo` в `sym.*` MCP-тулах (`internal/mcp/symbol.go`)**: новый опциональный параметр `repo` (string | JSON-массив | CSV | `*`/пусто) у `find_callers`, `find_references`, `find_implementations`, `find_callees`, `expand_neighbors`, `get_call_graph`. Дефолты: `expand_neighbors` и `get_call_graph` без явного `repo` ограничивают результат репой стартового узла (или единственной репой найденных определений в варианте `function`). Новые хелперы `repoMatcher`/`filterUnitsByRepo`/`filterEdgesByRepo` в `internal/mcp/util.go`.
- **E2E-тест multi-repo (`internal/astindex/multirepo_e2e_test.go`)**: создаёт tmp workspace с реальными маркерами `.git` (`alpha/`, `beta/`), в каждой — одноимённая функция `Save` + caller; прогоняет `repos.Discover` → `astindex.Indexer.FullScan` end-to-end и проверяет, что `FindASTUnits("Save","*")` видит обе репы, а `FindASTUnits(repo="alpha")` и `EdgesByDstNameForLangRepo(repo="alpha"/"beta")` не пересекают границ.
- **Multi-repo workspace с auto-discovery (`internal/repos`, новый пакет)**. `repos.Discover(root)` определяет режим по содержимому `cfg.Root`:
  - single-repo, если в самом корне есть `.git` (имя = `basename(root)`);
  - multi-repo workspace, если в корне нет `.git`, но в его непосредственных поддиректориях есть `.git` — каждая такая поддиректория = отдельная репа; «соседние» поддиректории без `.git` тоже попадают в индекс как «прицепленные репы». Скрытые директории (`.*`) пропускаются.
  При коллизии имён к дубликату добавляется короткий sha1-суффикс. `repos.Resolver` сопоставляет абсолютный путь файла → имя репы (prefix-match).
- **Поле `repo` в схеме SQLite (`internal/store/sqlite.go`)**: колонка `repo TEXT NOT NULL DEFAULT ''` в `ast_units` и `edges`, новые индексы `(repo, qualified)`, `(repo, name)`, `(repo, dst_name, kind)`. Новая служебная таблица `meta` для хранения `workspace_signature`.
- **`store.OpenFresh(path, signature)`**: открывает БД, предварительно удаляя файлы при несовпадении сохранённого workspace-signature (смена состава репо/корня инвалидирует старый граф). Подключено во всех точках входа CLI (`cli_run`, `cli_serve`).
- **Repo-aware API store**:
  - `EdgesByDstNameForLangRepo(ctx, name, kind, lang, repo)` — name-fallback с фильтром по репе источника;
  - `FindASTUnits(...)` получил параметр `repo` (`""`/`"*"` = все репо).
- **Multi-repo интеграционные тесты (`internal/store/multirepo_test.go`)**: два mock-репо с одноимённой функцией `Save`. Проверки:
  - `ResolvePendingEdges` резолвит caller'а alpha только в Save из alpha (не из beta);
  - `EdgesByDstNameForLangRepo` с `repo="alpha"` возвращает только рёбра из alpha;
  - `FindASTUnits` с `repo="alpha"` не видит Save из beta; `repo="*"` эквивалентно отсутствию фильтра.
- **`repo` в vector / BM25 / MCP**:
  - `internal/index/vector.go` + `fullscan.go` / `indexfile.go` — в Qdrant payload и `bm25.Doc` добавлено поле `repo` (резолв через `repos.Resolver`).
  - `internal/index/search.go` — `buildFilter` понимает значение `repo` как строку, JSON-массив или `"*"`.
  - `internal/bm25/bm25.go` — `Doc`/`Hit` получили поле `Repo`, `Query.Repos []string`, фильтр через disjunction по `repo`.
  - `internal/mcp/vector.go` — все `vec.*` инструменты принимают опциональный параметр `repo` (string `"name"`, JSON-массив `["a","b"]`, `"*"`/пусто = все репо). По умолчанию — глобальный поиск. `internal/mcp/treesitter.go` (`ts.search_symbols`) — то же.
- **CLI auto-discovery (`cmd/ai-tools/cli_run.go`, `cli_serve.go`)**: одна точка `repos.Discover` + `repos.NewResolver` + signature; резолвер пробрасывается в `astindex.Indexer.SetRepoResolver(...)` и `index.Vector.SetRepoResolver(...)`. При multi-repo в stderr выводится список найденных репо.

### Changed
- **Граф никогда не пересекает границы репо.** `ResolvePendingEdges` теперь делает резолв `dst_id` в пределах одного языка **И** одной репы (условие `dst.repo = src.repo` в обоих passes — qualified и name). Это значит: одноимённые функции в разных репах (например, `Save` в alpha и `Save` в beta) не смешиваются ни при инкрементальной индексации, ни при FullScan, ни при поиске callers/references/implementations.
- **`store.ASTUnit` и `store.Edge` получили поле `Repo`**; `scanASTUnit`, `scanEdges`, `astUnitColumns`, `edgeColumns` обновлены. `ReplaceASTUnits` и `ReplaceEdges` записывают `repo` в SQL. JOIN-запросы (`EdgesByDstNameForLangRepo`, ручной scan в `graph.findModuleNode`) используют явные алиасы `edges.repo` / `src.repo` — иначе после JOIN колонка `repo` становилась неоднозначной.

### Fixed
- **Кросс-языковые ложные привязки в graph (`internal/store/edges.go`, `internal/symbols/symbols.go`)**: `ResolvePendingEdges` теперь резолвит `dst_name → dst_id` только в пределах одного языка (JOIN с источником через `edges.src_id`, условие `dst.language = src.language`). Новый метод `EdgesByDstNameForLang` ограничивает name-fallback в `FindReferences` / `FindImplementations` / `FindCallers` языком найденных определений. Это устраняет, например, ситуацию, когда TS-вызов `log()` подтягивал Go-функцию `log`, а Java-`implements Foo` — Go-тип `Foo`. Резолв стал двухпроходным: сначала точный qualified-матч, затем name-матч по остатку.
- **SQL `ambiguous column name: id` в `EdgesByDstNameForLang`**: после JOIN с `ast_units` колонка `id` была неоднозначной — теперь SELECT явно квалифицирует все колонки префиксом `edges.`. Это разблокирует `find_implementations`, `find_callers`, `find_references`.

### Changed
- **Производительность `astindex.FullScan`**: внутренний `indexFile(ctx, path, resolveEdges bool)` позволяет отключать per-file `ResolvePendingEdges`. `FullScan` теперь делает один общий резолв в конце (с `defer`-страховкой на случай ранней отмены/ошибки), вместо N+1 проходов. Публичный `IndexFile` сохранил прежнее поведение (per-file резолв) — он используется watcher’ом и MCP-handler’ами для инкрементальной переиндексации.

### Added

#### MCP: новые семантические инструменты для агентов
- **`sym.get_execution_context(symbol_id)`** — агрегированный 360° контекст символа: definition, callers, callees, references, related_types (implements/extends), imports и список important_files за один вызов.
- **`sym.get_symbol_summary(symbol_id)`** — LLM-резюме символа (назначение, роль, важность) через локальную модель `phi3:mini` в Ollama.
- **`sym.get_file_intent(path)`** — LLM-анализ назначения и ответственности файла.
- **`sym.get_semantic_neighborhood(symbol_id)`** — кластеризованное представление окрестности символа (граф + LLM).
- **`sym.get_call_graph`**: помимо `symbol_id` теперь принимает `function` (имя) — соответствие README/AGENTS.md, с фильтрацией `function`/`method` и объединением окрестностей без дублей.

#### Документация и архитектура
- **`ARCHITECTURE.md` в корне**: карта слоёв `domain → service → transport`, потоки данных индексации и MCP-запросов, перечень пакетов с их ответственностью, правила для контрибьюторов и AI-агентов.
- **AGENTS.md**: добавлен Scenario D (Symbol Context Investigation) и обновлён System Prompt с правилами использования `sym.get_execution_context` и `sym.traverse_graph`.

#### Инфраструктура
- **Фолбэк LSP на tree-sitter**: автоматический поиск определений через индекс tree-sitter при недоступности LSP-сервера.
- **Автоматическая остановка Docker (Qdrant)**: контейнер Qdrant, запущенный через `--start-docker`, останавливается при завершении приложения.
- **Зависимость `phi3:mini` (Ollama)**: используется для семантических MCP-инструментов (`get_symbol_summary`, `get_file_intent`, `get_semantic_neighborhood`).

### Changed
- **LSP без Docker**: LSP-серверы запускаются как локальные дочерние процессы через stdio. Это упрощает маппинг путей, ускоряет старт и убирает оверхед на контейнер.
- **Реорганизация архитектуры**: `main.go` и все `cli_*.go` перенесены в `cmd/ai-tools/` (стандартный Go-layout).
- **Расширенная конфигурация**: в `config.yaml` добавлена секция `lsp` для гибкой настройки команд и аргументов запуска LSP-серверов.
- **Парсинг Go и tree-sitter**: расширено извлечение символов и связей (`internal/astindex/parse_go.go`, `treesitter_extractor.go`) для более полного графа.
- **Сервис `graph`**: значительно расширен (`internal/graph/graph.go`, +391 строк) — построение семантических окрестностей и агрегированных контекстов.
- **Документация `vec.search`**: в `docs/AGENTS.md` исправлено описание — это alias для `vec.search_hybrid` (а не `vec.search_semantic`), что соответствует фактической регистрации хендлера.

### Fixed
- **BFS-обход графа (`internal/store/neighbors.go`)**: добавлена дедупликация рёбер по `Edge.ID` в `ExpandNeighbors` и `TraverseGraph` — устраняет дублирование при симметричном in/out обходе и при достижении узла с нескольких сторон.
- **jdtls: стабильность и производительность**:
    - Заменено фиксированное ожидание на уведомление `language/status: ServiceReady`.
    - Устранена проблема SIGKILL после каждого запроса (процесс больше не привязан к контексту RPC).
    - Лимит памяти JVM поднят до 4G для стабильной работы на крупных проектах.
- **Диагностика LSP**: логи вынесены в `.ai-tools/logs/lsp-debug.log`, расширен захват stderr и информации о завершении процессов.

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
