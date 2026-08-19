# ragota

ragota indexes every repository you point it at and answers questions about
the code — over HTTP for programs, over [MCP](./mcp.md) for coding agents.
Ranked search, symbol lookup, and the part a single-repository tool cannot
offer: a **cross-repository graph** that joins services by the contracts they
actually share — HTTP routes, gRPC methods, Kafka topics, database tables.

```
"who calls POST /charges?"          → the gateway repo, web/checkout.ts:41
"where does order_id end up?"       → PlaceOrder → payments → topic settled → db:settlements
"what services talk to billing?"    → three, with call counts and confidence
```

## Why it exists

A coding agent already has a filesystem, a grep and (maybe) a language
server. What it does not have:

- **Answers across repositories.** The caller of an HTTP route shares no
  text with its handler beyond a path string, and lives in another repo,
  possibly in another language. Contract joins find it anyway.
- **Ranking.** "Where is the retry budget configured" is not a grep — it is
  a retrieval problem, and ragota answers it with a scored, explained list.
- **A context budget.** Every answer is capped in bytes and defaults to one
  line of code per hit. An agent locates with ragota, then reads only the
  ranges the index named — a few kilobytes instead of tens. On small-context
  models that difference is the whole analysis.

## The shape of it

One Go module, one binary:

| Command | Role |
|---|---|
| `ragota` | The server: discovers, indexes and watches repositories; serves the HTTP API; optional terminal dashboard |
| `ragota mcp` | A thin, **read-only** MCP server over a running `ragota` — the same binary, exposing ten tools to any MCP client |

Around them, in this repository:

- [`skills/`](./skills.md) — Agent Skills that teach a model *when* to use
  the index and when to use its own file tools,
- `tools/eval` — a measured retrieval benchmark (the numbers in
  [Quality](./quality.md) come from it),
- `e2e/` — an end-to-end suite that drives the shipped binary through both
  doors (HTTP and MCP).

## The pages, in reading order

1. [Install & run](./install.md) — from a release archive to a connected
   agent, optional services (PostgreSQL, vector search, a reranker) included.
2. [Getting started](./getting-started.md) — build from source, index a
   directory, ask the first question.
3. [Configuration](./configuration.md) — the decisions that matter, not
   every knob.
4. [HTTP API](./http-api.md) — the map of the contract.
5. [Connecting an agent](./mcp.md) — wire `ragota mcp` into Claude Code or
   any MCP client.
6. [Agent skills](./skills.md) — what the skills teach and why.
7. [Architecture](./architecture.md) — the pipeline, the contract keys, the
   working set.
8. [Quality, measured](./quality.md) — what is measured, what the numbers
   are, and what they mean for trusting answers.
