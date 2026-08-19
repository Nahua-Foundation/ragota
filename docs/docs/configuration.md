---
sidebar_position: 3
title: Configuration
---

# Configuration

Configuration lives in one YAML file (`--config`, default `./config.yaml`,
env `RAGOTA_CONFIG`); [`config.example.yaml`](https://github.com/Nahua-Foundation/ragota/blob/master/config.example.yaml)
documents every key with its reasoning. This page covers the decisions that
matter, not every knob.

## The minimum that runs

No config file at all is a valid setup: `--source` enables the local source,
storage defaults to SQLite, and the AST + BM25 indexes are on. That baseline
— **AST + BM25 over SQLite, nothing external** — is also what the
[quality numbers](./quality.md) measure.

```yaml
server:
  host: 127.0.0.1
  port: 8080
storage:
  sqlite: {path: ~/.ragota/data/ragota.db}
indexes:
  ast:  {enabled: true}
  bm25: {enabled: true}
```

## Auth: give agents the read key

```yaml
server:
  auth:
    type: api_key
    api_keys: ["read:${RAGOTA_MCP_KEY}", "admin:${RAGOTA_API_KEY}"]
```

A key's `read:`/`admin:` prefix is the operator's grant, not part of the
credential the client sends. A **read** key reaches every retrieval and
inspection route; **admin** adds the mutating ones (add/delete/index a
repository, ingest commits, compact). Hand a model-facing process the read
key — then it cannot be talked into a DELETE, which is the reason the scopes
exist. A key with no prefix is admin, so configs from before scopes keep
working.

## What gets indexed

Three layers of exclusion, all additive:

1. **`.gitignore`** — honoured automatically, with real gitignore semantics
   (nesting, negation, anchoring, `.git/info/exclude`). Can be turned off
   with `repos.use_gitignore: false`.
2. **Server config** — `repos.ignore:` glob patterns applied to every
   repository (`**/node_modules/**` and friends).
3. **The repository itself** — a `.ragota.yaml` at a repository's root adds
   its own patterns, so a repo can carry the knowledge of what in it is not
   worth indexing:

```yaml
# .ragota.yaml, at the repository root
ignore:
  - "**/generated/**"
  - "**/fixtures/**"
```

## Storage: SQLite or PostgreSQL

```yaml
storage:
  sqlite:   {path: ~/.ragota/data/ragota.db}   # single-user, zero setup
  # postgres: {dsn: "${RAGOTA_PG_DSN}"}              # shared deployments
```

Both backends pass one shared conformance suite; behaviour, ordering and
scoping are identical by test, not by intention.

## Optional muscle

Each of these is off by default and changes the [measured line](./quality.md)
when enabled:

| Block | What it adds | Cost |
|---|---|---|
| `indexes.vector` | Embedding search fused with BM25 (Ollama-compatible endpoint, or Qdrant as the store) | An embedding service; indexing time |
| `search.rerank` | A reranker over the fused top-N | A model call per query |
| `lsp` | Call edges refined through per-language LSP servers (`deploy/lsp/`) | Containers; a slower second pass |
| `graph.runtime` | Edges observed in tracing data (OTel ingestion) | Your tracing pipeline |

## Operational keys worth knowing

```yaml
server:
  write_timeout_seconds: 120   # bounds /context end to end; raise before blaming the client
  rate_limit: {enabled: true, requests_per_minute: 120}
  cors: {enabled: false}
log:
  level: info                  # debug|info|warn|error; --log-level overrides
```

`--check-config` validates the file and probes every configured dependency —
exit 0 ok, 1 invalid config, 2 a dependency unreachable — so a broken deploy
says so before it starts.
