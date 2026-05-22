# MCP Tool Integration Guidelines for Autonomous Agents

This documentation provides operational protocols and technical specifications for autonomous agents utilizing the provided Model Context Protocol (MCP) servers. These tools are designed to facilitate high-precision project analysis and navigation.

## 🚀 Objective: Core Tool Integration

For optimal analytical performance, agents MUST prioritize the use of these tools over manual file traversal or standard string-based searches. Integrating these capabilities into the primary reasoning loop is required for accuracy and context efficiency.

### 🧠 Strategic Prioritization
1.  **Semantic Search (`vec.search`)**: Primary entry point for locating relevant code segments based on conceptual or natural language queries.
2.  **Symbolic Analysis (`ts.search_symbols`)**: Mechanism for precise location of identified entities (functions, classes, variables) by name.
3.  **Code Navigation (`lsp.*`)**: Mechanism for resolving dependencies, definitions, and usage patterns within specific technical contexts.

---

## 🛠 MCP Server Specifications

### 1. Structural Analysis (`treesitter`)
Provides Abstract Syntax Tree (AST) based code comprehension for rapid symbol location.
- `ts.search_symbols(query, kind?, language?, limit?)`: Executes a substring search on symbol names. Use the `kind` parameter (e.g., `function`, `class`, `method`) for precise filtering.
- `ts.list_symbols(file)`: Retrieves the complete symbol hierarchy of a specified file. Execute this tool BEFORE reading file contents exceeding 100 lines to establish a structural map.
- `ts.stats()`: Returns diagnostic data regarding the indexed codebase (total files and symbols).
- `ts.reindex(path?)`: Forces synchronization of the structural index.

### 2. Semantic Search (`vector`)
Provides natural language search capabilities across the entire codebase using vector embeddings.
- `vec.search(query, limit?, language?)`: Executes a conceptual query (e.g., "implementation of request validation logic").
- `vec.count()`: Returns the total count of vectorized code segments.
- `vec.reindex(path?)`: Forces synchronization of the vector index.

### 3. Intelligence Services (`lsp`)
Provides real-time, language-aware intelligence via the Language Server Protocol.
- `lsp.definition(file, line, character)`: Resolves the exact location of a symbol's definition.
- `lsp.references(file, line, character, include_declaration?)`: Identifies all cross-references of a symbol. Essential for impact analysis and refactoring.
- `lsp.hover(file, line, character)`: Retrieves type signatures and documentation without full-file content expansion.
- `lsp.languages()`: Lists the currently active Language Servers and supported languages.

---

## 🧭 Operational Scenarios

### Scenario A: Feature/Logic Investigation
1.  **Discovery**: Invoke `vec.search` with a conceptual query to identify relevant modules.
2.  **Extraction**: Identify key symbols from the search results.
3.  **Verification**: Confirm precise symbol locations via `ts.search_symbols`.
4.  **Tracing**: Map dependencies and implementation details using `lsp.references`.

### Scenario B: Deep File Analysis
1.  **Survey**: Invoke `ts.list_symbols(file)` to establish a structural overview.
2.  **Inspection**: Use `lsp.hover` to resolve types and documentation for unknown entities within the file.
3.  **Navigation**: Utilize `lsp.definition` to trace external types and cross-file dependencies.

---

## 🤖 System Prompt Implementation (Hard-Wiring)

Integrate the following policy into your core operational instructions:

> **Autonomous Agent MCP Policy:**
> 1. PREFER `vec.search` over sequential file reading for locating specific feature implementations.
> 2. MANDATE `ts.list_symbols` execution before reading any source file exceeding 100 lines.
> 3. UTILIZE `lsp.definition` and `lsp.references` as the primary mechanisms for cross-file navigation.
> 4. APPLY `kind` and `language` filters in `ts.search_symbols` to minimize search noise and improve precision.
> 5. MITIGATE context window saturation: Use `lsp.hover` for rapid information retrieval instead of multi-file ingestion.

---

## 📝 Technical Constraints
- **Path Resolution**: All tools require absolute paths or paths relative to the project root.
- **Index Referencing**: LSP implementations utilize **0-based** indexing for lines and characters.
- **Index Synchronization**: Following significant codebase modifications, invoke `ts.reindex` and `vec.reindex` to maintain data integrity.
