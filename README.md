# ragota

An index over every repository you own, and the judgement to use it.

ragota discovers, indexes and watches your repositories, then answers
questions about the code — ranked search, symbol lookup, and a
**cross-repository graph** that joins services by the contracts they share:
HTTP routes, gRPC methods, Kafka topics, database tables. Programs get an
HTTP API; coding agents get an MCP server and a set of skills that teach
them when the index beats their own grep — and when it does not.

```
"who calls POST /charges?"        → the gateway repo, web/checkout.ts:41
"where does order_id end up?"     → PlaceOrder → payments → topic settled → db:settlements
"what talks to billing?"          → three services, with call counts and confidence
```

## The pieces

| Piece | What it is |
|---|---|
| `cmd/server` → `bin/ragota` | The server: indexing, watching, HTTP API, terminal dashboard |
| `cmd/mcp` → `bin/ragota-mcp` | Read-only MCP server over a running `ragota`; ten tools for any MCP client |
| [`skills/`](skills/README.md) | Agent Skills: when to use the index vs glob/grep/read, the graph tools, the empty-answer protocol |
| [`docs/`](docs/) | The documentation site (Docusaurus): `make docs` |
| `tools/eval` | The measured retrieval benchmark behind every quality claim |
| `e2e/` | Both shipped binaries driven from outside: `make e2e`, ~6 s |

## Quick start

```bash
make binary                                            # bin/ragota, bin/ragota-mcp
./bin/ragota --source ~/projects --watch --interactive run
```

Every repository under `--source` is discovered, indexed and kept fresh;
the dashboard shows progress. Then connect an agent:

```bash
claude mcp add ragota $(pwd)/bin/ragota-mcp -e RAGOTA_URL=http://localhost:8080
```

and install the skills into the workspace the agent analyzes code in:

```bash
mkdir -p .claude/skills && cp -R path/to/ragota/skills/ragota-* .claude/skills/
```

First questions, without an agent:

```bash
curl -s localhost:8080/api/v1/search -d '{"query":"where is the retry budget configured"}' | jq .
curl -s localhost:8080/api/v1/services | jq .
```

Full walkthrough, configuration, API and MCP reference: the
[documentation](docs/) (`make docs-serve` for a local live server).

## Quality, honestly

Retrieval claims here come from `tools/eval` — 103 questions over twelve
real repositories, ground truth pinned to file and line, deterministic
run-to-run. The baseline (AST + BM25 over SQLite, nothing external): search
answers with the right file first about half the time and in the top ten
far more often; symbol lookup on a known identifier resolves at MRR 0.71.
The tool descriptions, the docs and the skills are all written to that
operating point — scan the list, read the named range, never report an
empty answer as absence unchecked. Numbers, harness and history:
[Quality](docs/docs/quality.md).

## Development

```bash
make ci             # build, vet, fmt-check, lint, unit + integration tests
make e2e            # the product from outside: server + MCP over stdio on a fixture estate
make test-postgres  # storage conformance on a throwaway Postgres (Docker)
make eval-fast      # laptop-sized slice of the retrieval benchmark
make help           # everything else
```

Go 1.26+, cgo required (tree-sitter). PostgreSQL and the optional services
(embeddings, reranker, LSP containers, tracing ingestion) are exactly that —
optional; the default build and the whole e2e suite run with no Docker and
no network.

## Lineage

This branch (`v2`) is the unification of two projects: **ragota-core** (the
API-first indexing server) and **ragota-mcp** (the MCP server over it),
merged into one module under one name. Their commit histories live in their
original repositories; the v1 local tool this repository used to hold is on
`master`.
