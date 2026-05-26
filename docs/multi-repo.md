# Multi-repo workspace

`ragota` автоматически определяет структуру директории:

- **Single-repo** — в корне есть `.git`, индексируется одна репа с именем = `basename(root)`.
- **Multi-repo workspace** — в корне нет `.git`, но среди поддиректорий есть директории с `.git`. Каждая — отдельная репа. Соседние поддиректории без `.git` тоже попадают в индекс как «прицепленные репы». Скрытые директории (`.*`) пропускаются.

## Правила индекса

- Индекс **единый** на весь workspace (одна Qdrant-коллекция, один SQLite, один BM25), но у каждого элемента есть поле `repo`.
- При коллизии имён к дубликату добавляется hash-суффикс (например, `myrepo-9f3a1c8e`).
- При смене состава репо/корня старый SQLite сносится автоматически (workspace-signature).

## Скоупирование поиска

| Инструмент | По умолчанию | Как заскоупить |
|---|---|---|
| `vec.*` | Все репы | `repo: "alpha"` \| JSON-массив \| `"*"` |
| `ts.search_symbols` | Все репы | тот же `repo`-параметр |
| `sym.*` граф-инструменты | Per-repo (репа узла) | `repo="*"` для всех реп |

**Граф строится строго в пределах одной репы.** `ResolvePendingEdges` фильтрует кандидатов по `dst.repo = src.repo` — одноимённые функции в разных репах не смешиваются.

## Инструменты с `repo`-параметром

```
sym.find_references(symbol, repo?)
sym.find_implementations(interface, repo?)
sym.find_callers(function, repo?)
sym.find_callees(function, repo?)
sym.expand_neighbors(node_id, depth?, kinds?, repo?)
sym.get_call_graph(function|symbol_id, depth?, repo?)
ts.search_symbols(query, kind?, language?, limit?, repo?)
```

Синтаксис `repo`: имя репы | JSON-массив `["a","b"]` | CSV `"a,b"` | `"*"`/пусто = все репы.

## Инструменты без `repo`

Работают по конкретному id/path и не нуждаются в фильтре:
`sym.find_definition`, `sym.get_symbol`, `sym.get_parent`, `sym.get_children`, `sym.get_file_symbols`, `sym.get_dependency_graph`, `sym.traverse_graph`, `sym.get_surrounding_context`, `sym.get_related_files`, `sym.get_similar_code`, `sym.get_execution_context`, `sym.get_symbol_summary`, `sym.get_file_intent`, `sym.get_semantic_neighborhood`.

## Правило для агентов

`vec.*` и `ts.search_symbols` — глобальны по умолчанию, скоупьте через `repo`.
`sym.*` граф-инструменты — per-repo по умолчанию, расширяйте до `repo="*"` только при необходимости.
Всегда смотрите поле `repo` в результатах при сравнении сущностей из разных реп.
