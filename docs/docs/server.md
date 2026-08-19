---
sidebar_position: 3
title: Running a server
---

# Running a server

ragota as a long-lived service: a config file, API keys, PostgreSQL,
server-grade models, the LSP precision pass. Every block on this page
changes the [measured line](./quality.md) when enabled, and every one is
probed by `--check-config` once configured. The laptop setup — models
included — is [Getting started](./getting-started.md).

## The config file

```bash
ragota init          # writes the annotated example config as config.yaml
$EDITOR config.yaml
ragota --check-config
```

`init` writes [`config.example.yaml`](https://github.com/Nahua-Foundation/ragota/blob/master/config.example.yaml) verbatim —
every key documented with its reasoning, everything optional commented
out — and refuses to overwrite an existing file. `--check-config` validates
the result and probes every dependency it names: exit 0 all good, 1 the
config is invalid, 2 something configured is unreachable. Run it after each
block you enable below, and let a broken deploy say so before it starts.

## Auth: two keys, two roles

```yaml
server:
  auth:
    type: api_key
    api_keys: ["read:${RAGOTA_MCP_KEY}", "admin:${RAGOTA_API_KEY}"]
```

The agent-facing process gets the `read:` key and cannot be talked into a
DELETE; `admin:` adds the mutating routes. Secrets stay in the environment —
`${...}` in the YAML is resolved at startup. Details:
[Configuration](./configuration.md#auth-give-agents-the-read-key).

## PostgreSQL instead of SQLite

SQLite is the single-user default; PostgreSQL is for shared deployments.
Both pass the same conformance suite — behaviour, ordering and scoping are
identical by test.

```bash
docker run -d --name ragota-pg -p 127.0.0.1:5432:5432 \
  -e POSTGRES_USER=ragota -e POSTGRES_PASSWORD=ragota -e POSTGRES_DB=ragota \
  -v ragota-pgdata:/var/lib/postgresql/data postgres:16-alpine
```

```yaml
storage:
  postgres: {dsn: "postgres://ragota:ragota@localhost:5432/ragota?sslmode=disable"}
```

## The models on a server

The vector index and the reranker are configured exactly as in
[Getting started](./getting-started.md#start-the-models) — the same
`storage.qdrant`, `indexes.vector` and `search.rerank` blocks, pointing at
your model hosts instead of localhost. What changes at server scale:

**Bigger embedders.** The 0.6B is the laptop pick; a GPU host affords the
larger tags:

| Model | Pull size | Dimensions |
|---|---|---|
| `qwen3-embedding:0.6b` | 639 MB | 1024 |
| `qwen3-embedding:4b` | 2.5 GB | 2560 |
| `qwen3-embedding:8b` | 4.7 GB | 4096 |

The three sized tags are in the built-in dimensions table; the bare
`qwen3-embedding` tag is deliberately not — which size it resolves to is
Ollama's choice, so it requires an explicit `dimensions:`. Changing the
embedder changes the collection: reindex after switching.

**A GPU reranker.** vLLM serves the 4B (a causal LM scored from its
yes/no logits, hence the override):

```bash
vllm serve Qwen/Qwen3-Reranker-4B --port 8090 --hf_overrides \
  '{"architectures":["Qwen3ForSequenceClassification"],"classifier_from_token":["no","yes"],"is_original_qwen3_reranker":true}'
```

```yaml
search:
  rerank:
    base_url: http://your-gpu-host:8090/v1   # /v1 for vLLM, Cohere and Jina; bare for llama.cpp and TEI
    model: Qwen/Qwen3-Reranker-4B            # required by vLLM; llama.cpp ignores it
```

Before reaching for it, read the measured table in
[`config.example.yaml`](https://github.com/Nahua-Foundation/ragota/blob/master/config.example.yaml):
`top_n: 25` with the small model is the peak, and a bigger model is not
the upgrade it looks like.

**No GPU anywhere?** `text-embeddings-inference` with
`BAAI/bge-reranker-base` is the CPU fallback the
[compose stack](#the-whole-thing-as-one-stack) ships.

## LSP precision pass

Optional compiler-grade call edges through per-language language servers in
Docker. This is the one piece that wants a clone of the repository — the
containers build from `deploy/lsp/` (`make lsp-up`) — and the `lsp:` block
needs the `host_root`/`mount_root` mapping explained in
[`config.example.yaml`](https://github.com/Nahua-Foundation/ragota/blob/master/config.example.yaml).

## The whole thing as one stack

If you cloned the repository, the full estate is two files: app,
PostgreSQL, Qdrant and the LSP servers in one, Ollama and a reranker in the
other, wired together by `deploy/config.docker.yaml`:

```bash
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.models.yml up -d
docker compose exec ollama ollama pull qwen3-embedding:0.6b
```

The overlay's reranker is TEI with `bge-reranker-base` (a CPU-sized
default); to serve the Qwen pair instead, point `search.rerank` at a
llama.cpp or vLLM service as above and set the vector `model:` to the
pulled tag.

## Run

```bash
ragota --config config.yaml --source ~/projects --watch run
```

`--source` defines the run's **working set** — repositories from earlier
runs stay indexed but go dormant for unscoped retrieval;
[Getting started](./getting-started.md#the-working-set) explains the
semantics and the offline `ragota repos` commands. Operational knobs worth
knowing — timeouts, rate limiting, CORS, log levels:
[Configuration](./configuration.md#operational-keys-worth-knowing).
