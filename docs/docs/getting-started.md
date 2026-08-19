---
sidebar_position: 2
title: Getting started
---

# Getting started

Installing a release instead of building — and the optional services
(PostgreSQL, vector search, a reranker): [Install & run](./install.md).

## Build

Go 1.26+ with cgo enabled (the AST extractors use tree-sitter):

```bash
make binary        # → bin/ragota (server, repos and mcp in one binary)
```

## Index a directory of projects

Point `--source` at a directory; every repository under it (any directory
containing `.git`, to a shallow depth) is discovered, registered and indexed:

```bash
./bin/ragota --source ~/projects run
```

Useful flags, in the order people reach for them:

```bash
./bin/ragota --source ~/projects --watch run           # keep the index in step with saves
./bin/ragota --source ~/projects --watch --interactive run   # + terminal dashboard
```

- `--watch` follows file changes, creations and deletions under the indexed
  repositories and reindexes what moved. It budgets its file descriptors, so
  a huge tree degrades coverage gracefully instead of crashing the process.
- `--interactive` is a dashboard: per-repository indexing progress, the
  working set, warnings and errors, basic metrics. The process log goes to a
  file while the dashboard owns the terminal.

Without `--source` the server starts over whatever the config file
configures — including nothing, waiting for repositories to be added through
the API.

## The working set

`--source` also answers "which repositories is this run about". Repositories
from earlier runs stay in the index but go **dormant**: retrieval without an
explicit repository filter answers only from the current run's set, the
graph still sees everything, and naming a dormant repository in a request
reaches it. Point `--source` back at an old directory and its repositories
return to the working set after a freshness check.

Inspect and adjust it offline:

```bash
./bin/ragota repos list              # every repository, and which are active
./bin/ragota repos activate NAME    # put one back into the working set
./bin/ragota repos deactivate NAME  # take one out, keeping its index
```

## First questions

```bash
curl -s localhost:8080/api/v1/search \
  -d '{"query": "where is the retry budget configured", "limit": 5}' | jq .

curl -s localhost:8080/api/v1/nav/symbol \
  -d '{"symbol": "CaptureCharge"}' | jq .

curl -s localhost:8080/api/v1/services | jq .
```

Then connect an agent: [MCP](./mcp.md).

## Ignoring what should not be indexed

`.gitignore` is honoured automatically. On top of it, the server config and
each repository's own `.ragota.yaml` can exclude more — see
[Configuration](./configuration.md). The effect is not cosmetic: on this
project's own tree, gitignore support alone turned 107,758 candidate files
into 374.

## Verifying a setup

```bash
./bin/ragota --config config.yaml --check-config   # validate config, probe dependencies
make e2e                                           # the whole product, from outside, ~6s
```
