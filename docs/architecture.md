# Architecture

## The pipeline

```
discover → walk → extract → store → serve
```

1. **Discover.** A repository is a directory containing `.git`. `--source`
   scans a root to a shallow depth; the API registers paths directly; a git
   source clones and pulls.
2. **Walk.** One shared file walk feeds indexing and the watcher, applying
   the same exclusions everywhere: `.gitignore` (full semantics — nesting,
   negation, anchoring), server-config globs, and the repository's own
   `.ragota.yaml`.
3. **Extract.** Tree-sitter AST extractors per language produce **units**
   (functions, methods, classes, HTTP routes, gRPC methods, topics, tables,
   config keys) and **edges** (calls, imports, produces/consumes,
   reads/writes). Extractors also lift each unit's *contract key* — the
   piece that makes joining possible.
4. **Store.** SQLite or PostgreSQL, behind one storage interface with a
   shared conformance suite; BM25 in embedded bleve; vectors optionally.
5. **Serve.** Retrieval, the graph and the service map over HTTP;
   `ragota mcp` re-serves the same answers over MCP.

## Contract keys: the cross-repository join

Code that talks across services shares no symbols — only contracts. Every
outbound call site and every inbound declaration is normalized to a key:

```
http:POST /charges        grpc:Cart/AddItem
topic:order-events        db:orders
```

An outbound key meeting an inbound key — in whatever repository, whatever
language — becomes a cross-repository edge, and aggregated per (source
service, destination service, kind, key) they become the service map. This
is the part a per-repository tool cannot do at all.

**Confidence is graded, not implied.** A static call edge is fact; a
contract join is strong; name-similarity matches and runtime-observed edges
are leads. Confidence survives reindexing (it is recomputed from evidence,
not decayed), multiplies along `trace` chains, and is reported so a caller
can treat the tail of a long chain as a hypothesis.

Two optional refiners add edges the AST cannot see: **LSP servers**
(per-language containers under `deploy/lsp/`) resolve calls with compiler
knowledge, and **runtime ingestion** turns tracing data into
`runtime_call` edges — observed, and labelled as such.

## Retrieval

BM25 (bleve) is the backbone; a vector index, when configured, is fused
with it by reciprocal rank; an optional reranker reorders the fused top-N.
On top of ranking:

- **Intent.** "Who calls X" is not retrieval — the `callers` intent
  resolves X through the graph and answers with call sites.
- **Contract promotion.** A query that names a route, topic or table
  promotes the code carrying that contract.
- **Symbol lookup** is a separate, cheaper discipline: exact-name-first
  tiers, generated code and tests demoted — tuned for the "I hold an
  identifier" case that search ranking serves poorly.
- **Budgets.** Every answer can be capped in bytes; whole hits are dropped,
  weakest first, and truncation is declared.

**Determinism is a feature with tests.** Same corpus, same config → the
same index layout and the same eval line, run after run. The two historical
sources of flap — BM25 segment layout varying with compaction timing, and
score ties broken by unstable ids — are fixed and guarded (compaction is a
once-per-load policy; ordering tie-breaks on stable keys).

## The working set

Registration is permanent; *relevance* is per-run. `--source` computes the
set of repositories a run is about and marks the rest dormant — indexed,
reachable by name, followed by the graph, but excluded from default
retrieval. The flag lives in storage, flows through the service layer into
every retrieval query as a compiled filter, and surfaces in the API
(`active` on every repository), the TUI, `repos list` and `ragota_status`.
The [e2e suite](../e2e/e2e_test.go)
drives the whole lifecycle through both doors — HTTP and MCP.

## Process shape

One module, one binary, two processes. The server owns storage exclusively;
the MCP server is a stateless HTTP client of it and holds no index of its
own — which is why it is safe to run one per agent, locally, with a
read-scoped key.
