# MCP-серверы

`ragota` объединяет четыре MCP-сервера. Каждый доступен по stdio или на отдельном порту.

## ts (Tree-Sitter) — структурный поиск символов

Разбирает код на символы (function/method/class/interface/type/enum/var/const) для Go, TypeScript/TSX, JavaScript/JSX, Python, Java.

| Метод | Описание |
|---|---|
| `ts.search_symbols(query, kind?, language?, limit?, repo?)` | Поиск по имени символа. **Go**: `kind="function"` находит только `func foo()`, `kind="method"` — только `func (r T) foo()`. `repo`: имя \| JSON-массив \| CSV \| `"*"`/пусто (по умолчанию — все репы). |
| `ts.list_symbols(file)` | Дерево символов файла. |
| `ts.reindex(path?)` | Переиндексация файла или всей директории. |
| `ts.stats()` | Статистика индекса (файлы, символы). |

## vec (Vector) — гибридный поиск

Гибридный (vector + BM25) поиск с опциональным реранкингом. Чанкинг через tree-sitter, две раздельные коллекции в Qdrant (код и текст).

| Метод | Описание |
|---|---|
| `vec.search(query, limit?, language?)` | Alias к `search_hybrid`. |
| `vec.search_semantic(query, top_k?, language?)` | Только векторный поиск (Qdrant). |
| `vec.search_keyword(query, top_k?, language?)` | Только лексический поиск (BM25/Bleve). |
| `vec.search_hybrid(query, top_k?, language?)` | Слияние vector + BM25 (RRF или weighted-sum). |
| `vec.rerank(query, candidates, top_n?)` | Реранк через BGE-Reranker (Ollama). Graceful fallback при недоступности. |
| `vec.reindex(path?)` | Обновление vector + BM25 индексов. |
| `vec.count()` | Число чанков в индексе. |

## sym (Symbol) — symbol-aware навигация

Навигация поверх AST units + code graph. Извлечение: Go через `go/ast`, Java/TS/JS через tree-sitter, Python через AST.

### Lookup

| Метод | Описание |
|---|---|
| `sym.find_definition(symbol)` | Найти определение символа. |
| `sym.find_references(symbol, repo?)` | Все ссылки на символ. |
| `sym.find_implementations(interface, repo?)` | Реализации интерфейса. |
| `sym.find_callers(function, repo?)` | Кто вызывает функцию. |
| `sym.find_callees(function, repo?)` | Кого вызывает функция. |

### AST / структура

| Метод | Описание |
|---|---|
| `sym.get_symbol(symbol_id)` | AST-узел по ID. |
| `sym.get_parent(symbol_id)` | Родительский узел. |
| `sym.get_children(symbol_id)` | Дочерние узлы. |
| `sym.get_file_symbols(path)` | Все символы файла. |

### Граф

| Метод | Описание |
|---|---|
| `sym.expand_neighbors(node_id, depth?, kinds?, repo?)` | BFS по рёбрам. `repo` по умолчанию = репа узла. |
| `sym.get_call_graph(function\|symbol_id, depth?, repo?)` | Граф вызовов. |
| `sym.get_dependency_graph(module, depth?)` | Граф import-связей. **Для Go нужен полный путь.** |
| `sym.traverse_graph(symbol_id, edge_types, depth)` | Навигация по связям графа. |

### Контекст

| Метод | Описание |
|---|---|
| `sym.get_execution_context(symbol_id)` | Полный контекст: definition, callers, callees, references, types, imports. |
| `sym.get_symbol_summary(symbol_id)` | Семантическое резюме через LLM (цель, роль, важность). |
| `sym.get_file_intent(path)` | Анализ назначения файла через LLM. |
| `sym.get_semantic_neighborhood(symbol_id)` | Кластеризованная окрестность символа (граф + LLM). **Нужен ID символа, не файла.** |
| `sym.get_surrounding_context(symbol_id)` | Исходный код вокруг узла. |
| `sym.get_related_files(symbol_id)` | Связанные файлы через import/call/reference. |
| `sym.get_similar_code(symbol_id)` | Семантически похожие фрагменты через vector. |

> **`repo`-параметр** для `sym.*`: имя \| JSON-массив \| CSV \| `"*"`/пусто. Граф-инструменты per-repo по умолчанию.

## lsp (Language Server Protocol) — навигация через родные LSP

Проксирует `gopls`, `typescript-language-server`, `pyright-langserver`, `jdtls`.

| Метод | Описание |
|---|---|
| `lsp.definition(file, line, character)` | Перейти к определению. |
| `lsp.references(file, line, character, include_declaration?)` | Найти ссылки. |
| `lsp.hover(file, line, character)` | Hover-информация. |
| `lsp.languages()` | Список настроенных LSP-языков. |

> Параметры `line` и `character` — **числовые** (не строки).

## Отдельный запуск

```bash
ragota serve-treesitter --root /path/to/project
ragota serve-vector     --root /path/to/project
ragota serve-symbol     --root /path/to/project
ragota serve-lsp        --root /path/to/project
```
