---
sidebar_position: 5
title: HTTP API
---

# HTTP API

The full contract lives in
[`internal/server/api/openapi.yaml`](https://github.com/Nahua-Foundation/ragota/blob/master/internal/server/api/openapi.yaml);
Go callers get the same surface typed in
[`client`](https://github.com/Nahua-Foundation/ragota/tree/master/client).
This page is the map.

Everything is under `/api/v1`, JSON in, JSON out. `GET /health` (no auth,
no prefix) reports version and API version — clients check compatibility
there before anything else. Beside it, also unprefixed: `GET /ready`
(readiness, probes dependencies), `GET /metrics` (Prometheus) and
`GET /openapi.yaml` (this contract, served by the binary).

## Retrieval

| Route | Question it answers |
|---|---|
| `POST /search` | Ranked code locations for a question in prose. Filters (`repos`, `languages`, `kinds`, `path_prefix`), snippet sizing, an `intent: callers` mode that answers with call sites, and opt-in `diagnostics` |
| `POST /context` | `/search` plus the graph expanded around each hit — the multi-file answer. The most expensive route; budget it |
| `POST /nav/symbol` | Definitions for an identifier, exact-first, generated code and tests demoted. With `kind` and no symbol it enumerates (all `http_route`s of a repo, all `db_table`s) |
| `POST /nav/definition` | The definition behind a file position |
| `POST /nav/references` | Graph edges pointing at the unit containing a file position — calls, imports, HTTP/gRPC, produces/consumes |

Two knobs shape every retrieval answer:

- **`max_bytes`** — a hard response budget. Weakest hits are dropped whole
  until the answer fits, and the response says when that happened.
- **`snippet`** — `chunk` (the whole indexed chunk), `line` (one anchor
  line), `none`. A chunk measured ~2.4 KB against ~120 B for the hit around
  it; clients that will open files themselves want `line`.

## Graph

| Route | Question |
|---|---|
| `POST /graph/neighbors` | Edges around one unit, both directions, with the far side where resolved |
| `POST /graph/path` | Does A reach B, through which hops (gRPC methods expand to their implementations, so paths cross services) |
| `POST /graph/trace` | Follow one parameter's value across calls, HTTP, Kafka, DB writes — service boundaries included |
| `GET /services` | The service map: detected deployables and the links between them, aggregated per contract |
| `GET /services/export` | The same graph as diagram text for a human — `?format=mermaid` (default) or `dot`; always the whole estate |
| `GET /topics` | Messaging topics with producers and consumers on each |

## Repositories and operations

| Route | Purpose |
|---|---|
| `GET /repos`, `POST /repos`, `GET /repos/{id}`, `DELETE /repos/{id}` | List (with `active` and indexing state), register, inspect, remove |
| `POST /repos/{id}/index` | Trigger a pass; `GET .../jobs` and `.../jobs/{job_id}` follow it |
| `POST /repos/{id}/reset` | Clear a stuck indexing claim so the next pass can start |
| `GET /repos/{id}/coverage` | How much of the repo's outbound call surface resolved into edges — the honesty metric behind "nothing calls this" |
| `POST /repos/{id}/commits`, `GET /repos/{id}/sync-state` | Commit ingestion for git sources, and the cursor it advances |
| `POST /webhooks/git` | Push notifications from CI (no prefix; its own shared secret instead of an API key) |
| `POST /otel/service-graph` | Runtime ingestion: tracing data becomes `runtime_call` edges — observed, and labelled as such |
| `POST /admin/compact` | One BM25 compaction — how a bulk load ends |
| `GET /stats` | Per-index document counts |

Mutating and administrative routes require an `admin:`-scoped key;
retrieval and inspection run with `read:`. See
[Configuration](./configuration.md#auth-give-agents-the-read-key).

## Semantics worth knowing before coding against it

- **Scoping.** `/search`, `/context` and `/nav/symbol` with no repository
  filter answer from the **working set** only; naming any repository reads
  it, active or not. The graph routes ignore the flag — the far side of a
  cross-service link is elsewhere by definition.
- **The working set is not settable over HTTP.** `--source` computes it at
  startup and `ragota repos activate|deactivate` adjusts it offline; the
  API reports it (`active` on every repository) and never mutates it.
- **Degradation is declared.** With `diagnostics: true`, `/search` reports
  whether every index answered; a zero-hit answer under degradation is not
  evidence of absence.
- **Unit ids are ephemeral.** They come from answers and do not survive a
  reindex; nothing lets a client compose one.
- Current footguns (accepted, documented): `/graph/trace` resets a
  `max_depth` above 24 to the default 16 instead of clamping; an unknown
  repository id in a filter answers zero hits rather than an error. The MCP
  server clamps and resolves names client-side; direct HTTP callers should
  do the same.
