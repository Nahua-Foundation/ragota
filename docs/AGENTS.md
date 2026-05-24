# MCP Tool Integration Guidelines for Autonomous Agents

This documentation provides operational protocols and technical specifications for autonomous agents utilizing the provided Model Context Protocol (MCP) servers. These tools are designed to facilitate high-precision project analysis and navigation built around a **hybrid (vector + BM25) + reranker + code-graph** retrieval stack.

## 🚀 Objective: Core Tool Integration

For optimal analytical performance, agents MUST prioritize the use of these tools over manual file traversal or standard string-based searches. Integrating these capabilities into the primary reasoning loop is required for accuracy and context efficiency.

### 🧠 Strategic Prioritization
1. **Hybrid retrieval (`vec.search_hybrid`)**: Default entry point for locating relevant code. Combines vector similarity (qwen3-embedding for code, nomic-embed-text for markdown) with BM25 lexical search (Bleve) via Reciprocal Rank Fusion.
2. **Reranking (`vec.rerank`)**: Re-orders top-K hybrid candidates with the BGE Reranker (Ollama). When the model is unavailable the system gracefully falls back to the hybrid order.
3. **Symbol-aware navigation (`sym.*`)**: Definition / references / implementations / callers / callees + parent-child AST navigation.
4. **Code-graph expansion (`sym.expand_neighbors`, `sym.get_call_graph`, `sym.get_dependency_graph`)**: Walks `calls / imports / implements / extends / references` edges around a hit to widen context cheaply.
5. **Tree-sitter symbolic analysis (`ts.*`)**: Substring symbol search and file symbol listing.
6. **LSP intelligence (`lsp.*`)** — real-time, language-aware navigation when an LSP is available for the language. Supports Java, TypeScript, JavaScript, Python, and Go.

---

## 🛠 MCP Server Specifications

### 1. Retrieval & Reranking (`vector`)

Hybrid retrieval (vector + BM25) with optional reranking.

Base retrieval:
- `vec.search_semantic(query, top_k?, language?)` — pure vector search. Code uses **qwen3-embedding**, text/markdown uses **nomic-embed-text**.
- `vec.search_keyword(query, top_k?, language?, kind?)` — BM25 lexical search (Bleve).
- `vec.search_hybrid(query, top_k?, language?)` — vector + BM25 merged via RRF (or weighted sum if `hybrid.vector_weight`/`bm25_weight` are configured).
- `vec.rerank(query, candidates, top_n?)` — reranks a JSON array of candidates `[{id, content, score?}]` via the BGE Reranker (`qllama/bge-reranker-v2-m3`) in Ollama. **Graceful fallback**: if the reranker model is not loaded and `rerank.required = false`, returns the original ordering.
- `vec.search(query, limit?, language?)` — backward-compatible alias of `vec.search_hybrid`.

Infrastructure:
- `vec.reindex(path?)` — re-index a file or run a full scan.
- `vec.count()` — total chunks in the active Qdrant collection.

### 2. Symbol-aware retrieval (`symbol`)

Symbol-aware lookup and AST/graph navigation. Backed by `ast_units` and `edges` tables in SQLite, populated by language-specific extractors:

- **Go** — `go/parser` + `go/ast` (precise: functions, methods, types, imports, call expressions, embedded types).
- **Java / TypeScript / JavaScript** — tree-sitter (classes, interfaces, methods, functions, calls, imports, `extends`, `implements`).
- **Python** — AST units only (no edges yet).

Symbol lookup:
- `sym.find_definition(symbol)` — AST units defining the symbol.
- `sym.find_references(symbol)` — all reference edges to the symbol.
- `sym.find_implementations(interface)` — implementations of the interface.
- `sym.find_callers(function)` — direct callers.
- `sym.find_callees(function)` — direct callees.
- `sym.get_execution_context(symbol_id)` — **High-level context aggregator**. Returns definition, callers, callees, references, related types (implements/extends), and a list of important files. Use this for a quick 360° view of a symbol.
- `sym.traverse_graph(symbol_id, edge_types?, depth?)` — **Semantic navigation**. Walk the graph along specific edge types (call, import, etc.) to understand dependencies or flow.

AST / structure retrieval:
- `sym.get_file_symbols(path)` — AST units in a file with `parent_id` (parent-child).
- `sym.get_symbol(symbol_id)` — single AST unit by id.
- `sym.get_parent(symbol_id)` — parent AST unit.
- `sym.get_children(symbol_id)` — direct children.

Graph retrieval:
- `sym.expand_neighbors(node_id, depth?, kinds?)` — N-hop neighborhood. `kinds` is a comma-separated subset of `call,import,implements,extends,reference` (empty = all).
- `sym.get_dependency_graph(module, depth?)` — import-graph around a module/file.
- `sym.get_call_graph(function, depth?)` — call graph around a function.

Context retrieval:
- `sym.get_surrounding_context(symbol_id, before_lines?, after_lines?)` — parent body + adjacent units.
- `sym.get_related_files(symbol_id)` — files connected via `import/call/reference`.
- `sym.get_similar_code(symbol_id, limit?)` — embedding-similar AST units.

### 3. Structural Analysis (`treesitter`)
Tree-sitter based symbol index (substring lookup, fast).
- `ts.search_symbols(query, kind?, language?, limit?)` — substring search by symbol name. Use `kind` (`function`, `class`, `method`, …) for precise filtering. **Go-specific**: `kind="function"` finds only functions (e.g., `func foo()`), `kind="method"` finds only methods (e.g., `func (r Receiver) foo()`). Use the correct kind for your search target.
- `ts.list_symbols(file)` — full symbol hierarchy of a file. Execute BEFORE reading a source file > 100 lines.
- `ts.stats()` — diagnostics (files, symbols).
- `ts.reindex(path?)` — refresh the structural index.

### 4. Intelligence Services (`lsp`)
- `lsp.definition(file, line, character)`
- `lsp.references(file, line, character, include_declaration?)`
- `lsp.hover(file, line, character)`
- `lsp.languages()`

---

## 🧭 Operational Scenarios

### Scenario A: Feature/Logic Investigation (default)
1. **Discovery** — `vec.search_hybrid` with a conceptual query.
2. **Refine** — `vec.rerank` over top-K candidates if precise ranking matters (or if hybrid scores are close).
3. **Resolve symbols** — pick top hits, call `sym.find_definition` to anchor on canonical AST units.
4. **Expand graph** — `sym.expand_neighbors(node_id, depth=1, kinds="call,reference,import")` to widen context cheaply.
5. **Impact analysis** — `sym.find_callers`, `sym.find_references` for refactoring scope.

### Scenario B: Deep File Analysis
1. **Survey** — `sym.get_file_symbols(file)` (preferred over `ts.list_symbols` when AST units / parent-child are needed).
2. **Inspect** — `lsp.hover` for types/docs; `sym.get_surrounding_context` for parent body without loading the whole file.
3. **Navigate** — `lsp.definition` / `sym.find_definition` to chase external types and cross-file dependencies.

### Scenario C: Interface / Architectural Mapping
1. `sym.find_implementations(interface)` — concrete implementations.
2. `sym.get_dependency_graph(module, depth=2)` — surrounding modules.
3. `sym.get_call_graph(function, depth=2)` — execution flow.

### Scenario D: Symbol Context Investigation (Quick Deep Dive)
1. **Locate** — find target `symbol_id` via `sym.find_definition` or `ts.search_symbols`.
2. **Aggregated View** — `sym.get_execution_context(symbol_id)` to get definition, callers, callees, references, and related types in a single call.
3. **File Survey** — use `important_files` from the result to prioritize which files to read or list symbols for.

---

## 🤖 System Prompt Implementation (Hard-Wiring)

Integrate the following policy into your core operational instructions:

> **Autonomous Agent MCP Policy:**
> 1. PREFER `vec.search_hybrid` over `vec.search` / `vec.search_semantic` / `vec.search_keyword` alone — hybrid retrieval is the default.
> 2. APPLY `vec.rerank` on top of hybrid output whenever ordering quality matters or the top hits look ambiguous.
> 3. PRIORITIZE `ts.search_symbols` for locating functions, methods, or classes by name over text-based searches or `grep`. Use `grep` only as a last resort.
> 4. ANCHOR ranked hits to canonical AST units via `sym.find_definition` before deep reading.
> 5. EXPAND with `sym.expand_neighbors` (1–2 hops) instead of re-querying retrieval for related code.
> 6. MANDATE `sym.get_file_symbols` (or `ts.list_symbols`) before reading any source file exceeding 100 lines.
> 7. USE `sym.find_callers` / `sym.find_references` / `sym.find_implementations` as the primary mechanisms for impact analysis and refactoring scope.
> 8. PREFER `sym.get_execution_context` when you need a comprehensive overview of a symbol (callers, references, related types) to minimize round-trips.
> 9. USE `sym.traverse_graph` for directed semantic exploration (e.g., following a specific dependency chain or execution path).
> 10. APPLY `kind` and `language` filters where supported to minimize search noise.
> 11. MITIGATE context-window saturation: prefer `lsp.hover`, `sym.get_surrounding_context`, and `sym.get_similar_code` over multi-file ingestion.

---

## 📝 Technical Constraints
- **Path Resolution**: All tools require absolute paths or paths relative to the project root.
- **Index Referencing**: LSP implementations utilize **0-based** indexing for lines and characters. AST units and `ts.*` tools use **1-based** line numbers.
- **Embedding model migration**: changing `collections.code.embed_model` (default `qwen3-embedding`) or `collections.text.embed_model` triggers automatic reindex of the affected collection on next start (tracked via `embed_meta`). The other collection is left untouched.
- **Reranker availability**: if `rerank.enabled = true` but the Ollama model is not pulled, behaviour depends on `rerank.required`:
  - `false` (default): a warning is logged and hybrid order is returned untouched (graceful fallback).
  - `true`: the call fails with an explicit error.
- **Index Synchronization**: After significant codebase modifications, invoke `ts.reindex`, `vec.reindex`, and (when implemented) the symbol/graph reindex to maintain integrity.
