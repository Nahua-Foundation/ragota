---
sidebar_position: 3
title: Running a server
---

# Running a server

ragota as a long-lived service: a config file, API keys, PostgreSQL, and —
each one optional — the retrieval muscle: a vector index, a reranker, the
LSP precision pass. Every block on this page changes the
[measured line](./quality.md) when enabled, and every one is probed by
`--check-config` once configured. The laptop setup with none of this is
[Getting started](./getting-started.md).

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

## Vector search: Qwen3-Embedding over Ollama, Qdrant as the store

Two services: Ollama serves the embedder, Qdrant stores the vectors
(required as soon as `indexes.vector` is on).

```bash
ollama pull qwen3-embedding:0.6b        # 639 MB; :4b and :8b for GPU hosts
docker run -d --name ragota-qdrant -p 127.0.0.1:6333:6333 \
  -v ragota-qdrant:/qdrant/storage qdrant/qdrant:v1.12.4
```

| Model | Pull size | Dimensions |
|---|---|---|
| `qwen3-embedding:0.6b` | 639 MB | 1024 |
| `qwen3-embedding:4b` | 2.5 GB | 2560 |
| `qwen3-embedding:8b` | 4.7 GB | 4096 |

The three sized tags are in the built-in dimensions table; the bare
`qwen3-embedding` tag is deliberately not — which size it resolves to is
Ollama's choice, so it requires an explicit `dimensions:`.

```yaml
storage:
  qdrant: {url: http://localhost:6333}
indexes:
  vector:
    enabled: true
    embedder: {provider: ollama, model: "qwen3-embedding:0.6b"}
    chunking: {method: cards}   # symbol cards — measured better than line windows on every shape but one
```

Embeddings are built by an index pass: restart the `--source` run or
`POST /api/v1/repos/{id}/index` per repository after enabling.

## Reranker: Qwen3-Reranker

The largest single quality lever measured on `tools/eval` — larger than the
vector index it reorders (the measured table lives in
[`config.example.yaml`](https://github.com/Nahua-Foundation/ragota/blob/master/config.example.yaml); its conclusion:
`top_n: 25` and a small model, not a big one).

On a CPU host, llama.cpp serves the 0.6B in one line — the port matters
(ragota holds 8080) and so does `-ub`, without which any code snippet past
~500 tokens is rejected and search quietly keeps its original order:

```bash
llama-server -hf ggml-org/Qwen3-Reranker-0.6B-Q8_0-GGUF --rerank \
  --port 8090 -c 8192 -b 4096 -ub 4096
```

On a GPU host, vLLM serves the 4B (a causal LM scored from its yes/no
logits, hence the override):

```bash
vllm serve Qwen/Qwen3-Reranker-4B --port 8090 --hf_overrides \
  '{"architectures":["Qwen3ForSequenceClassification"],"classifier_from_token":["no","yes"],"is_original_qwen3_reranker":true}'
```

```yaml
search:
  rerank:
    enabled: true
    base_url: http://localhost:8090   # append /v1 for vLLM, Cohere or Jina; bare for llama.cpp and TEI
    model: Qwen/Qwen3-Reranker-4B     # required by vLLM; llama.cpp ignores it
    top_n: 25
    instruction: "Given a code search query, retrieve the most relevant code"
```

Qwen3-Reranker is instruction-aware and takes the task description inside
the query text — that is what `instruction:` renders. A reranker failure
never fails a search: the original order is kept and logged. (No GPU and no
llama.cpp? `text-embeddings-inference` with `BAAI/bge-reranker-base` is the
CPU fallback the [compose stack](#the-whole-thing-as-one-stack) ships.)

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
