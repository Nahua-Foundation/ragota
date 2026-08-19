# Getting started

Everything local: install ragota, index your projects, connect a coding
agent. No config file, no Docker, no network — this page is the whole setup
for a laptop, and the configuration it runs is the one the
[quality numbers](./quality.md) measure. Running ragota as a shared server —
PostgreSQL, API keys, vector search, a reranker — is its own page:
[Running a server](./server.md).

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
tree-sitter):

```bash
make binary        # → bin/ragota
```

Either way the one file is the whole product: the server, `repos`
administration and the MCP server are subcommands, and the example config
and the agent skills are written by it (`ragota init`,
`ragota skills install`), always matching the version you run.

## Index your projects

```bash
ragota --source ~/projects --watch --interactive run
```

Every repository under `--source` (any directory containing `.git`, to a
shallow depth) is discovered, registered and indexed. `--watch` keeps the
index in step with saves; `--interactive` is a terminal dashboard —
per-repository progress, warnings, the working set — while the process log
goes to a file. Storage is SQLite under `~/.ragota`; there is nothing to
set up.

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
