---
sidebar_position: 2
title: Getting started
---

# Getting started

The full local setup on one page: ragota with the Qwen embedder and
reranker — the configuration that answers best — and a coding agent on
top. Every model step is skippable:
[Without the models](#without-the-models) is the zero-dependency fallback.
Running ragota as a shared service — PostgreSQL, API keys, GPU-grade
models — is its own page: [Running a server](./server.md).

## Install

Grab the archive for your platform from the
[releases page](https://github.com/Nahua-Foundation/ragota/releases)
(`checksums.txt` sits next to the archives), unpack, put `ragota` on your
PATH:

```bash
tar -xzf ragota_v*_darwin_arm64.tar.gz && sudo mv ragota_v*_darwin_arm64/ragota /usr/local/bin/
ragota --version
```

Or build from source — Go 1.26+ with cgo enabled (the AST extractors use
tree-sitter): `make binary` → `bin/ragota`. Either way the one file is the
whole product: the server, `repos` administration and the MCP server are
subcommands, and the example config and the agent skills are written by it
(`ragota init`, `ragota skills install`), always matching the version you
run.

## Start the models

Three services, three commands, all laptop-sized:

```bash
# The embedder — install Ollama (ollama.com), then:
ollama pull qwen3-embedding:0.6b

# The vector store:
docker run -d --name ragota-qdrant -p 127.0.0.1:6333:6333 \
  -v ragota-qdrant:/qdrant/storage qdrant/qdrant:v1.12.4

# The reranker — llama.cpp (brew install llama.cpp), one line:
llama-server -hf ggml-org/Qwen3-Reranker-0.6B-Q8_0-GGUF --rerank \
  --port 8090 -c 8192 -b 4096 -ub 4096
```

Two flags on the reranker line are load-bearing: `--port 8090` because
ragota holds 8080, and `-ub 4096` because the default batch rejects any
code snippet past ~500 tokens — search would then quietly keep its
original order. The reranker is the largest single quality lever measured
on `tools/eval`, larger than the vector index it reorders; a failure of
either never fails a search, retrieval just falls back and says so.

## Point ragota at them

`ragota init` writes the full annotated example config; for this page the
whole file is:

```yaml
# config.yaml
storage:
  sqlite: {path: ~/.ragota/data/ragota.db}
  qdrant: {url: http://localhost:6333}
indexes:
  vector:
    enabled: true
    embedder: {provider: ollama, model: "qwen3-embedding:0.6b"}
    chunking: {method: cards}   # symbol cards — measured better than line windows
search:
  rerank:
    enabled: true
    base_url: http://localhost:8090
    top_n: 25                   # the measured peak; 50 is slower and worse
    instruction: "Given a code search query, retrieve the most relevant code"
```

```bash
ragota --check-config   # exit 0 all good, 1 invalid config, 2 a dependency unreachable
```

`--check-config` probes every service the file names, so a typo'd port
says so here, not as silently worse answers later.

## Index your projects

```bash
ragota --source ~/projects --watch --interactive run
```

Every repository under `--source` (any directory containing `.git`, to a
shallow depth) is discovered, registered and indexed. `--watch` keeps the
index in step with saves; `--interactive` is a terminal dashboard —
per-repository progress, warnings, the working set — while the process log
goes to a file.

First questions, before any agent:

```bash
curl -s localhost:8080/api/v1/search \
  -d '{"query": "where is the retry budget configured", "limit": 5}' | jq .

curl -s localhost:8080/api/v1/nav/symbol -d '{"symbol": "CaptureCharge"}' | jq .

curl -s localhost:8080/api/v1/services | jq .
```

## Connect an agent

```bash
claude mcp add ragota -e RAGOTA_URL=http://localhost:8080 -- $(which ragota) mcp
ragota mcp -check      # proves the whole path: server reachable, API version, key
```

Then install the skills into the workspace the agent analyzes code in —
they teach it when the index beats its own grep, and they are versioned
with the binary:

```bash
cd ~/that-workspace && ragota skills install
```

The ten tools and the launch-block reference:
[Connecting an agent](./mcp.md). What the skills teach and why:
[Agent skills](./skills.md).

## Without the models

No Ollama, no Docker, no reranker: skip the two model sections and run
with no config file at all —

```bash
ragota --source ~/projects --watch --interactive run
```

— SQLite under `~/.ragota`, AST + BM25, the exact configuration the
[quality numbers](./quality.md) measure. Add the models later by writing
the config above and triggering an index pass (restart the `--source` run,
or `POST /api/v1/repos/{id}/index` per repository) so the embeddings get
built. Bigger model sizes and GPU serving:
[Running a server](./server.md#the-models-on-a-server).

## The working set

`--source` also answers "which repositories is this run about".
Repositories from earlier runs stay in the index but go **dormant**:
retrieval without an explicit repository filter answers only from the
current run's set, the graph still sees everything, and naming a dormant
repository in a request reaches it. Point `--source` back at an old
directory and its repositories return after a freshness check.

Inspect and adjust it offline:

```bash
ragota repos list              # every repository, and which are active
ragota repos activate NAME     # put one back into the working set
ragota repos deactivate NAME   # take one out, keeping its index
```

## What gets indexed

`.gitignore` is honoured automatically — on this project's own tree that
alone turned 107,758 candidate files into 374. The server config and each
repository's own `.ragota.yaml` can exclude more:
[Configuration](./configuration.md).
