# ragota-core

A configurable API service for code indexing, search and cross-service code graphs.

## What is ragota-core?

`ragota-core` is an **API-first** code indexing service that:
- Accepts configuration via YAML (indexes, models, storage)
- Accepts repositories via API (local paths, Git clone) and keeps them fresh
  via **push webhooks** (GitHub/GitLab)
- Indexes code with AST, Vector, and BM25 indexers
- Parses **Go, Java, C#, TypeScript/JavaScript, Python, Protobuf, SQL
  migrations, OpenAPI specs and config files** (yaml/json/.env/.properties)
- Builds a **cross-repository code graph**: call edges, HTTP routes/clients,
  gRPC contracts and implementations, Kafka producers/consumers
  (env/config-driven topic names are resolved through indexed config files),
  database tables written/read by code
- Detects **services inside a repository** (monorepo-aware), aggregates
  service-to-service links and exports the graph as **Mermaid/DOT**
- Traces a **parameter's flow** across function calls and service boundaries
  (HTTP / gRPC / Kafka) down to **database column sinks**, with branch
  alternatives
- Serves **graph-expanded retrieval** (`/context`): search hits enriched with
  the code graph around them — a ready-made context package for an LLM
- Ingests **runtime service graphs** from tracing (OTel/Jaeger) to confirm
  statically discovered links
- Optionally generates **LLM summaries** of files and services (Ollama/OpenAI)
- Stores metadata in **PostgreSQL** (primary backend, sqlc + native pgx) or
  **SQLite** (lightweight embedded/dev option); exposes **Prometheus metrics**

**Key difference from ragota**: configurable storage/models and API-first approach instead of CLI + built-in MCP server.

## Architecture

```
┌──────────────────────────────────────────────────────────────┐
│                       HTTP REST API                          │
│  /repos /search /nav /graph /services /topics /stats /health │
└──────────────────────────────────────────────────────────────┘
                    │
┌─────────────────────────────────────────────┐
│              Index Engine                   │
│  AST (multi-lang + contracts) │ Vector │ BM25│
└─────────────────────────────────────────────┘
                    │
┌─────────────────────────────────────────────┐
│        Graph: service detection + linker    │
│  call / http_call / rpc_call / kafka_flow   │
└─────────────────────────────────────────────┘
                    │
┌─────────────────────────────────────────────┐
│               Storage                       │
│  PostgreSQL  │  SQLite  │  Qdrant           │
└─────────────────────────────────────────────┘
                    │
┌─────────────────────────────────────────────┐
│               AI Models                     │
│  Ollama  │  OpenAI                          │
└─────────────────────────────────────────────┘
```

### How the cross-service graph works

Indexing is two-phase:

1. **Per-repo parse (tree-sitter + proto parser).** Each file yields symbol
   units (functions, methods, classes, types) and edges. Framework heuristics
   additionally emit *contract* units and edges:
   - HTTP routes (net/http, chi/gin-style, Spring `@GetMapping`, ASP.NET
     attributes, express) become `http_route` units;
   - `.proto` files yield `proto_service` / `rpc_method` / `proto_message` /
     `proto_field` units;
   - HTTP clients (http.Get/NewRequest, RestTemplate, HttpClient, axios/fetch),
     gRPC client stubs (`New<Service>Client`) and Kafka produce/consume calls
     become `http_call` / `rpc_call` / `produces` / `consumes` edges keyed by
     `http:METHOD /path`, `grpc:Service/Method` and `topic:<name>`.

2. **Global linking.** After every repo index the linker resolves `call`
   edges within the repo, matches contract edges against contract units of
   *all* repos (path templates for HTTP, qualified-name suffixes for gRPC) and
   joins Kafka `produces`×`consumes` pairs into derived `kafka_flow` edges.
   Every resolution records a confidence score — heuristics never pretend to
   be exact.

Services are detected per repository (`.ragota.yaml` override,
docker-compose build contexts, `cmd/*` convention, per-directory build
manifests) and stored as `service` units; every symbol maps to a service by
path prefix.

## Quick Start

### 1. Build

```bash
cd ~/projects/ragota-core
make binary                # -> bin/ragota-core, --version reports the git describe
# or: go build -o ragota-core ./cmd/server
# or: make install         # into $GOBIN
```

### 2. Configure

Copy `config.example.yaml` to `config.yaml` and edit:

```yaml
repos:
  sources:
    local:
      enabled: true
```

### 3. Check the config before running it

```bash
./bin/ragota-core --config config.yaml --check-config
```

`--check-config` loads and validates the file, prints the warnings, then probes
every endpoint the configuration points at — reranker, embedder, assistant LLM,
Qdrant and each LSP server — **without touching the database**. Exit codes:
`0` all good, `1` the config is invalid, `2` the config is valid but something
it points at is unreachable. Run it in the deploy pipeline: it catches the
silent misconfigurations (a typo in `server.auth.type`, a vector index without
Qdrant, a dead reranker) before they reach production.

### 4. Run

```bash
make run                   # honours CONFIG=..., defaults to config.yaml
# or: ./bin/ragota-core --config config.yaml
```

Server starts on `http://127.0.0.1:8080`

### Or: point it at a directory of projects

Steps 2–4 are for a deployment. To index the code on your own machine there is
nothing to configure and nothing to `curl`:

```bash
./bin/ragota-core --source ~/projects run          # discover, register, index
./bin/ragota-core --source ~/projects --watch run  # ...and keep it fresh
./bin/ragota-core --source ~/projects --watch --interactive run  # ...and watch it work
```

`run` is optional: an invocation without a subcommand starts the server exactly
as it always has, so `--config config.yaml` and `make run` are unaffected. The
other subcommand is `repos` (below). An unrecognized word is rejected with a
usage message instead of being ignored.

**What `--source` counts as a repository.** A directory containing `.git` —
git's own marker, and deliberately not the `go.mod`/`package.json` test that
service detection applies *inside* a repository, since a monorepo full of those
is one repository and not forty. In order:

- the directory itself, if it is a repository — `--source ./my-project` needs no
  wrapper, and a submodule or vendored checkout inside it stays part of it;
- otherwise every repository below it, to a depth of **3** (`~/projects/repo`,
  `~/src/org/repo`, `~/code/github.com/org/repo`), stopping at each one it
  finds. Directories whose name starts with a dot are skipped;
- otherwise the directory itself anyway, on the assumption that somewhere you
  pointed the indexer at holds code even without version control. The log line
  says which of the three happened.

Registration is idempotent — ids come from name and path — so running the same
command twice adds nothing and resets nothing. The repositories are then indexed
one after another, not all at once: a pass holds a window of file contents in
memory while its indexers work through it.

**`--source` also says what the run is *about*.** The repositories it finds
become the working set, and every other registered repository goes dormant.
Repositories persist, so without this a `--source ~/projects/one-project` after
a `--source ~/projects` would still be answering out of all twenty: dormant is
how the other nineteen stay indexed without being in the way.

Dormant is a view and not a lifecycle state. Nothing is deleted and nothing is
unindexed — files, symbols, edges, coverage and the repository's place in the
cross-repository graph all survive — and naming it again, with `--source` or
with `repos activate`, brings it back as it was. What changes is the default
scope of retrieval: `/search`, `/context` and `/nav/symbol` answer from the
working set unless the request names repositories itself, and a request that
names one reaches it whether it is active or not. `/nav/symbol` is in that list
because it and `/search` are two ways of asking one question — a sentence goes
to one, an identifier to the other — and one question must not answer
differently depending on which door it came through. The graph endpoints, the
linker and indexing are deliberately not narrowed; the cross-repository graph is
the point of the system.

Two runs that redefine nothing: a run with no `--source` (including
`--check-config`) leaves the working set exactly as the last one left it, and a
`--source` that matched nothing warns and leaves it alone rather than reading a
mistyped path as a request for an index that answers nothing.

**It composes with configuration rather than replacing it.** `--source` is
appended to `repos.sources.local.paths`, so ignore patterns, per-repository
`.ragota.yaml` settings and every indexer treat what it finds exactly as they
treat a configured path. Two consequences worth knowing:

- with no config file at all, a `--source` run uses a built-in local profile
  (SQLite + AST + keyword index, and the ignore list `config.example.yaml`
  ships). No vector index — that one needs an embedder and Qdrant, which is
  what a config file adds. A `--config` you *name* and that is missing is still
  an error;
- `local.paths` doubles as the allowlist for paths an API client may add, and an
  empty list means "anything". A run with `--source` therefore has an allowlist
  where a bare config file had none.

**`--watch`** re-indexes a file when it changes, adds one that appears and drops
one that goes away, through the same incremental pass a pushed commit takes. It
follows the local repositories in the working set (a git-sourced one is
refreshed by pulling it, so watching one would turn an editor save into a `git
pull`; a dormant one is not what this run is about, and on macOS every watched
directory costs a file descriptor the run's own repositories need), watches only
the directories the indexing walk descends into — an
excluded `node_modules` is neither indexed nor watched — and debounces bursts:
changes are applied once a repository has been quiet for 400ms, or after 5s if
it never goes quiet. A directory it cannot watch is a warning, not a crash.

**`--interactive`** replaces the scrolling log with a dashboard: the working
set and the phase each repository is in, a progress bar for the pass in flight,
which repositories the watcher is following, the recent warnings and errors, and
the running totals. The dormant repositories are one line under the table
(`+17 dormant in the index, not in this run`) rather than seventeen rows showing
a dash where a progress bar goes, which read as "queued" — but they are counted,
so nothing is left out silently. `q` (or `Esc`, or `Ctrl-C`) quits, `w` narrows
the log pane to warnings and errors. Long paths and long messages are cut to fit
rather than wrapped, columns are dropped as the terminal narrows, and nothing in
the frame needs colour to be read.

Four things about it are worth knowing:

- **the log moves, it does not stop.** Records on stderr and a full-screen
  renderer on one terminal interleave into garbage, so for as long as the
  dashboard is up the whole log goes to `~/.ragota-core/logs/ragota-core.log`
  (appended, one marker line per run; the path is in the dashboard's footer, so
  `tail -f` it from another pane). Warnings and errors are on screen as well —
  they reach the dashboard through a slog handler that tees them, not through
  that file. If the file cannot be opened, everything below warn is dropped for
  the duration rather than written over the frame;
- **quitting is a shutdown, not an exit.** `q` returns from the renderer and the
  process then takes exactly the path `SIGTERM` takes — HTTP server drained,
  watcher stopped, index pass in flight ended, storage closed — and the log
  reappears on stderr for it. `SIGTERM` from outside does the same in the other
  direction: the terminal is handed back first, then the shutdown runs;
- **a non-terminal stdout falls back.** Piped, redirected or under CI there is no
  dashboard: the run logs to stderr as it always did and says once, as a warning,
  that it could not draw. `--interactive` never turns a working pipeline into an
  error;
- **it does not need `--source`.** On its own it shows the working set as the
  last run left it, and the passes the API starts against those repositories.

### Which repositories the index knows about: `repos`

The working set outlives the run that chose it, so there is a subcommand for
reading and editing it without re-running `--source` and without `curl`:

```bash
ragota-core repos list                 # every repository, marked active or dormant
ragota-core repos deactivate beta      # take one out of the working set
ragota-core repos activate  beta       # ...and put it back
ragota-core repos deactivate .         # a path works too, so this means "this project"
```

`list` prints every registered repository — dormant ones included, since this is
the command that says what the dashboard is leaving out — with its state, its
id, its path and the totals underneath. `activate` and `deactivate` move one
repository across the boundary and leave the rest of the set where it is; they
take an id, a name or a path, and an ambiguous name is refused with the ids that
resolve it rather than guessed at. Neither indexes, unindexes or deletes
anything: deactivating is exactly as reversible as it sounds.

Like `run`, these need no config file — with none, the same built-in local
profile the `--source` runs use is what they read. A server that is already
running picks the change up on its next request, since retrieval resolves the
working set per request; its watcher does not, having been given its
repositories at startup.

### CLI flags

| Flag | Meaning |
|---|---|
| `--config PATH` | config file; overrides `RAGOTA_CONFIG`, default `config.yaml` |
| `--check-config` | validate + dependency dry-run, then exit (see above) |
| `--version` | version, revision, platform and Go version |
| `--log-level LEVEL` | override `log.level` for this run (`debug`, `info`, `warn`, `error`) |
| `--pprof HOST:PORT` | serve `net/http/pprof` on a separate listener; empty (default) disables it |
| `--source DIR` | directory *holding* projects: discover, register and index the repositories under it on startup (added to `repos.sources.local.paths`), and make exactly those the working set |
| `--watch` | keep the index in step with changes under the local repositories in the working set |
| `--interactive` | draw the indexing status as a terminal dashboard; the log goes to `~/.ragota-core/logs/ragota-core.log` while it is on screen, and a non-terminal stdout falls back to plain logging |

Subcommands go after the flags (`--source ./dir run`). `run` starts the HTTP API
and may be omitted; `repos` reads and edits the working set and exits.

### Profiling a slow index pass

`--pprof` is off unless you pass an address. It listens separately from the API
so the profiles never inherit the API's CORS or auth settings; bind it to
loopback (a non-loopback bind is allowed but logged as a warning, since the
endpoints dump heap contents, goroutine stacks and the process command line).

```bash
./bin/ragota-core --config config.yaml --pprof 127.0.0.1:6060 &
curl -X POST localhost:8080/api/v1/repos/$ID/index -d '{"force":true}'

go tool pprof -http :8081 http://127.0.0.1:6060/debug/pprof/profile?seconds=60   # where CPU goes
curl -s 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=2' > stacks.txt       # what is blocked
```

An index pass that looks stalled is usually visible in `goroutine?debug=2`: the
worker pool parks on a channel while one goroutine sits in a storage call.

**With the vector index on, the embedder is the pass.** A CPU profile of an
Elasticsearch pass (40k files, AST + BM25 + vector) accounted for 16s of samples
in 30s of wall time on a ten-core machine — the process was using half of one
core, and a goroutine dump taken at the same moment had 22 goroutines in it, of
which the two that mattered were both parked in the embedder's HTTP call. The
per-indexer tally the pass logs said the same thing from the other side:

```
index repo ... indexers="ast=224s bm25=117s vector=692s" total_sec=834
```

The CPU stages are not the problem, and parallelising them further buys nothing:
they finish inside every window and then wait. What is worth attention is
whether the embedding endpoint ever has an empty queue, because a local
accelerator serves requests one at a time and idle time there is time nothing
can give back. Two things used to empty it, both now fixed — the embed workers
wrote each file's points to Qdrant themselves instead of embedding
(`indexes/vector.storeWorkers`), and every write re-checked the collection,
which cost four Qdrant round trips per file for every one that carried data
(`storage/qdrant.ensureCollection`). Sampling the same dump for how many embed
workers are inside `Embed` is the quickest way to see it: 1.27 of 2 before,
1.99 of 2 after.

The two binaries over the same repository, back to back on the same machine:

| Elasticsearch, 40001 files | before | after |
| --- | --- | --- |
| `total_sec` | 973 | 809 |
| `vector` | 866 | 718 |
| `ast` | 263 | 269 |
| `bm25` | 157 | 159 |

Read the last two rows first. This machine is shared, so neither half ran
alone, and a wall time from it means nothing on its own — but the CPU stages
are the control: they did the same work at the same speed in both halves, to
within two percent, so the machine was the same machine and the seventeen
percent is the vector channel. Consul, measured the same way, went 263s ->
149s with its vector stage 256s -> 131s; the smaller repository gains more
because its files chunk into fewer texts each, so it paid the per-file Qdrant
overhead more often per second of embedding.

### Running the whole stack in Docker

```bash
docker compose -f deploy/docker-compose.yml up -d --build          # app + postgres + qdrant + LSP servers
docker compose -f deploy/docker-compose.yml \
               -f deploy/docker-compose.models.yml up -d           # + ollama and a reranker
# or: make compose-up
```

The app container reads `deploy/config.docker.yaml`. Repositories live in the
`repos` volume, mounted at `/workspace` in the app *and* in every language
server, so `lsp.host_root` and `lsp.mount_root` are both `/workspace` — an
identity mapping. When ragota-core runs on the host instead (`make lsp-up`),
`host_root` is the host directory and `mount_root` is `/workspace`; a wrong
mapping is now reported instead of silently producing an empty LSP pass.

## API

### Repositories

```bash
# Add local repository
curl -X POST http://localhost:8080/api/v1/repos \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-project",
    "source": "local",
    "path": "/Users/tema/projects/my-app"
  }'

# List repositories
curl http://localhost:8080/api/v1/repos

# Index repository (async - returns 202)
curl -X POST http://localhost:8080/api/v1/repos/{id}/index

# Check repo status (poll until status is "idle")
curl http://localhost:8080/api/v1/repos/{id}

# Reset a repo: drop its indexed data and its commit cursor, keeping the repo
# registered (the supported way out of "search is silently empty")
curl -X POST http://localhost:8080/api/v1/repos/{id}/reset

# Delete repository
curl -X DELETE http://localhost:8080/api/v1/repos/{id}
```

### Incremental indexing and the reindex trap

Indexing skips files whose content hash is unchanged, so a second `/index` call
only processes what moved. That has one consequence worth stating plainly:

> **Adding or removing an index store requires `force: true`.** Turning on the
> vector or BM25 index (or pointing `indexes.bm25.path` at a new directory)
> leaves every unchanged file unindexed *in the new store*, because the hash
> still matches. The store stays empty and searches against it return nothing —
> silently, with no error anywhere. After any such change run
> `POST /repos/{id}/index -d '{"force": true}'` for every repo, or
> `POST /repos/{id}/reset` followed by a normal index.

The same applies after a change to how a stored key is spelled. The current one:
ORM table names are now derived identically on both sides of the join
(`contract.TableName`), which corrects entity names carrying an underscore or a
package qualifier — `User_Profile` was stored as `user__profiles` and looked up
as `user_profiles`, so the ORM writes never met their table. Repos indexed
before that keep the old spelling until a forced reindex rewrites it.

### Commit ingestion (external client)

An external client that owns the git history can push commits with per-file
diffs; only the affected paths are re-indexed. Each repo keeps a commit cursor
(`last_commit`); the first commit of a batch must reference the cursor in its
`parents`, otherwise the API answers `409 {"error": "commit gap", "last_commit": ...}`
and indexes nothing — the client then resends the missing range or requests a
full reindex via `/index`.

```bash
# Push a batch of commits (returns 202; file statuses: A/M/D/R).
# "content" is optional: when omitted, the file is read from the repo's
# working tree on disk (the server runs a source update first).
curl -X POST http://localhost:8080/api/v1/repos/{id}/commits \
  -H "Content-Type: application/json" \
  -d '{
    "commits": [
      {
        "sha": "3f2a1b...",
        "parents": ["9c8d7e..."],
        "files": [
          {"path": "src/orders.ts", "status": "M", "content": "export async function ..."},
          {"path": "src/legacy.ts", "status": "D"},
          {"path": "src/api/new.ts", "old_path": "src/old.ts", "status": "R"}
        ]
      }
    ]
  }'

# Check the commit cursor and indexing status
curl http://localhost:8080/api/v1/repos/{id}/sync-state
# => {"repo_id": "...", "last_commit": "3f2a1b...", "status": "idle"}
```

### Search

```bash
# Keyword search (BM25)
curl -X POST http://localhost:8080/api/v1/search \
  -H "Content-Type: application/json" \
  -d '{
    "query": "email validation",
    "mode": "keyword",
    "limit": 10
  }'

# Semantic search (requires vector index + embedder configured)
curl -X POST http://localhost:8080/api/v1/search \
  -H "Content-Type: application/json" \
  -d '{
    "query": "function to validate email",
    "mode": "semantic",
    "limit": 10
  }'

# Hybrid search (requires both BM25 and vector indexes)
curl -X POST http://localhost:8080/api/v1/search \
  -H "Content-Type: application/json" \
  -d '{
    "query": "email validation",
    "mode": "hybrid",
    "limit": 10
  }'

# "What calls X" — answered from the code graph, not text ranking. The intent
# is detected from the phrasing ("what calls...", "who uses...", "callers of
# ...") or forced with "intent": "callers"; call sites come first, each with
# reason "calls <symbol>". "intent": "none" turns the handling off per query,
# `search.intent: off` in the config disables detection server-wide.
curl -X POST http://localhost:8080/api/v1/search \
  -H "Content-Type: application/json" \
  -d '{
    "query": "what calls validateEmail",
    "mode": "keyword",
    "limit": 10
  }'
```

#### Bounding the response

`limit` and `hops` count elements, and an element has no size of its own: one
snippet has measured 2,420 bytes, and `/search` with `limit: 20` has measured
34 KB — roughly 8,400 tokens out of the context window of whichever model asked.
Two request fields bound it, on both `/search` and `/context`:

```bash
# Locations only, and never more than 4 KB of them
curl -X POST http://localhost:8080/api/v1/search \
  -H "Content-Type: application/json" \
  -d '{"query": "email validation", "limit": 20, "snippet": "none", "max_bytes": 4096}'
```

- `snippet`: `chunk` (default — the indexed chunk, what search has always
  returned), `line` (the snippet's first line, the one the hit is anchored at),
  or `none` (no code body at all). Locations survive every mode, so an agent
  that will open the file itself pays for the addresses and nothing else: on the
  same query, `none` is ~620 tokens against ~8,400.
- `max_bytes`: a cap on the response body. Whole hits are dropped from the end
  of the ranked list — the best answers survive and the JSON still parses — and
  the response then carries `"truncated": true`. `total` still counts what the
  query retrieved, so the gap between it and `hits` is what the budget cost.

Both default to today's behaviour exactly: unset `snippet` is `chunk`, and
`max_bytes: 0` caps nothing.

A hit is `repo_id`, `file_path`, `line`, `end_line`, `symbol`, `kind`,
`language`, `score`, `snippet` and `reason`. (Before API 0.2.0 it also repeated
`file_path` as `path`.)

### Navigation (Code Intelligence)

```bash
# Find symbols by name
curl -X POST http://localhost:8080/api/v1/nav/symbol \
  -H "Content-Type: application/json" \
  -d '{
    "repo_id": "my-project",
    "name": "validateEmail",
    "kind": "function",
    "limit": 10
  }'

# Go to definition
curl -X POST http://localhost:8080/api/v1/nav/definition \
  -H "Content-Type: application/json" \
  -d '{
    "repo_id": "my-project",
    "file_path": "src/auth.go",
    "position": {"line": 42, "character": 10}
  }'

# Find references
curl -X POST http://localhost:8080/api/v1/nav/references \
  -H "Content-Type: application/json" \
  -d '{
    "repo_id": "my-project",
    "file_path": "src/auth.go",
    "position": {"line": 42, "character": 10},
    "limit": 50
  }'
```

### Graph

```bash
# Services detected across all repos + aggregated service-to-service links
curl http://localhost:8080/api/v1/services

# One repository's services, capped. `repo` is repeatable and also accepts a
# comma-separated list; a link is kept when *either* end is in the selection,
# because the far side of a cross-service call lives elsewhere by definition.
# `limit` caps each list and sets "truncated": true when it cuts.
curl "http://localhost:8080/api/v1/services?repo=orders&repo=billing&limit=50"

# Kafka topics with producers and consumers
curl http://localhost:8080/api/v1/topics

# Edges in/out of a unit (unit ids come from /nav/symbol responses)
curl -X POST http://localhost:8080/api/v1/graph/neighbors \
  -H "Content-Type: application/json" \
  -d '{"unit_id": "42"}'

# Directed path between two units (crosses gRPC/HTTP/Kafka boundaries)
curl -X POST http://localhost:8080/api/v1/graph/path \
  -H "Content-Type: application/json" \
  -d '{"from_unit_id": "42", "to_unit_id": "137", "max_depth": 10}'

# Trace where a parameter flows, across services:
# "method1 has param1 — where does it go?"
curl -X POST http://localhost:8080/api/v1/graph/trace \
  -H "Content-Type: application/json" \
  -d '{
    "repo_id": "my-monorepo",
    "symbol": "CreateOrderHandler",
    "param": "user_id"
  }'
```

A trace response is a chain of steps with the tracked identifier at each hop:

```
web/checkoutHandler          (start: parameter user_id)
→ web/submitOrder            (call, tracked: userId)
→ gateway POST /api/v1/orders (http_call, field user_id)
→ gateway/CreateOrderHandler (handled_by)
→ orders.OrderService/CreateOrder (rpc_call, field UserId)
→ orders/CreateOrder          (implements_rpc)
→ orders/publishOrderCreated  (call)
→ billing/onOrderCreated      (kafka_flow, topic orders.created, field UserID)
→ billing/chargeUser          (call)
→ notifier POST /api/notify/send (http_call)
→ notifier/Send → SaveNotification → Save
```

Each step carries a confidence value; matching is heuristic (normalized
identifier comparison: `UserId` == `user_id` == `userId`). Branches appear in
`alternatives` — e.g. the same `user_id` also flows into the `analytics`
consumer and ends in the `analytics_events.user_id` database column
(`writes_to` edges are extracted from SQL statements in code and linked to
tables parsed from migration files).

### Retrieval context for LLMs

```bash
# Search + N-hop graph expansion in one call
curl -X POST http://localhost:8080/api/v1/context \
  -H "Content-Type: application/json" \
  -d '{"query": "order creation flow", "mode": "keyword", "limit": 5, "hops": 2}'
```

Each item carries the search hit (snippet), its enclosing unit, its service,
and the related units reachable through the graph — callers, callees, and the
far side of gRPC/HTTP/Kafka contracts.

This is the largest response the API produces (`limit: 20` with `hops: 3` has
measured 57 KB, ~14,200 tokens), so it takes the same `snippet` and `max_bytes`
bounds as `/search` — see [Bounding the response](#bounding-the-response). Items
are dropped whole, so a surviving hit always keeps the graph expansion that
explains it.

### Reranking & assistant LLM

Two optional LLM stages sharpen retrieval:

- **Reranking** (`search.rerank` in the config): after BM25/vector/RRF
  retrieval, the top `top_n` candidates (default 50) are re-scored by an
  external rerank service — any TEI- or Cohere-compatible endpoint answering
  `POST {base_url}{path}` with `{"query": ..., "documents": [...], "model": ...}`,
  which covers TEI, Infinity, vLLM, Cohere and Jina. Optional `api_key` is sent
  as a bearer token and `timeout_seconds` (default 30) bounds the call.
  Reranked hits get `"+rerank"` appended to their `Reason`. If the reranker
  fails, search logs a warning and returns the original order — it never breaks
  retrieval.

  Reranking is not part of the OpenAI API: an OpenAI-compatible server exposes
  it as `/v1/rerank`, not through `/v1/chat/completions`, so `base_url` must
  carry the `/v1` segment for vLLM, Cohere and Jina and omit it for a bare TEI
  container. Instruction-aware rerankers such as Qwen3-Reranker take their task
  description inside the query text, since the request has no field for it —
  setting `instruction` renders the query as
  `<Instruct>: {instruction}\n<Query>: {query}`, and `query_template` /
  `document_template` override that for models needing their full prompt.
  `config.example.yaml` carries a worked Qwen3-Reranker-4B + vLLM example,
  including the `hf_overrides` the model needs to be served at all.

  Budget the timeouts together: a dead reranker costs
  `search.rerank.timeout_seconds` per attempt times the client's retries, and
  that has to fit inside `server.write_timeout_seconds` (default 120) or the
  client sees a truncated response instead of results in the original order.
  `--check-config` probes the reranker endpoint before you deploy.
- **Assistant query rewrite** (`models.assistant` with `query_rewrite`,
  default **off** — measured as a retrieval regression, so opt in explicitly):
  when enabled, `/api/v1/context` first asks the assistant LLM to turn a
  natural-language question into a short keyword-style query; the rewritten
  query is used for retrieval and echoed back as `rewritten_query`. Failures
  and timeouts (10s) fall back to the original query.

Related retrieval upgrades: `indexes.vector.chunking.method: cards` switches
the vector index to symbol cards (one embedding document per function/class:
kind, qualified name, signature, doc comment and body head — files without
symbols keep window chunking), and LLM summaries (when `summaries` is enabled)
are also pushed into the vector index as `.ragota/summaries/*.md` documents,
so semantic search can answer "what does this service do" style questions.

### Diagram export, webhooks, runtime graph, metrics

```bash
# Service graph as a diagram
curl "http://localhost:8080/api/v1/services/export?format=mermaid"   # or format=dot

# Topics a service participates in
curl "http://localhost:8080/api/v1/topics?service=orders"

# Reindex on push (point GitHub/GitLab webhooks here). RAGOTA_WEBHOOK_SECRET is
# mandatory — with it unset the endpoint answers 503. A request authenticates
# with any one of: GitHub's HMAC signature (X-Hub-Signature-256, set the secret
# as the GitHub webhook secret), GitLab's X-Gitlab-Token, or a manual
# X-Webhook-Token. The secret is never read from the query string.
curl -X POST http://localhost:8080/webhooks/git \
  -H "Content-Type: application/json" \
  -H "X-Webhook-Token: $RAGOTA_WEBHOOK_SECRET" \
  -d '{"repository": {"full_name": "acme/billing"}}'


# Ingest an observed service graph from tracing (confirms static links, and
# records the calls static analysis cannot see: a URL concatenated at runtime,
# a workflow step wired by a dispatcher). Service names are matched to detected
# services by word component, so "checkout-service", "checkout_service" and
# "checkoutservice" are the same service.
curl -X POST http://localhost:8080/api/v1/otel/service-graph \
  -H "Content-Type: application/json" \
  -d '{"edges": [{"client": "gateway", "server": "orders", "calls": 1200}]}'
# -> {"received":1,"stored":1}
#
# The reply names what it could not place, because a tracing backend's service
# names are its own and a silent "stored: 0" is unactionable:
# -> {"received":2,"stored":0,"unmatched":["cartservice","frontend"],
#     "known":["checkoutservice","shippingservice"]}
# A payload that matches nothing leaves the existing runtime graph alone; a
# payload that matches replaces it wholesale.

# Prometheus metrics
curl http://localhost:8080/metrics

# OpenAPI 3.0 spec for all endpoints (served from the binary, no auth)
curl http://localhost:8080/openapi.yaml
```

Besides HTTP counters and indexer gauges, `/metrics` exposes indexing
observability: `ragota_index_repo_seconds` and `ragota_link_seconds`
(summaries, `_count`/`_sum`), plus `ragota_link_resolved_total` and
`ragota_link_errors_total` counters from the cross-repo linking pass.

### Per-repository settings: `.ragota.yaml`

A repository can describe itself to the indexer with a `.ragota.yaml` at its
root. Both sections are optional and independent — a repository that only
wants to exclude a directory does not have to describe its services:

```yaml
# Overrides the layout heuristics (docker-compose, cmd/*, build manifests)
services:
  - name: gateway
    root: services/gateway
  - name: orders
    root: services/orders

# Added to the server's repos.ignore for this repository only
ignore:
  - "**/generated/**"
  - "**/*.pb.go"
```

`ignore` uses the same glob syntax as the server's `repos.ignore`, and the two
are combined: the repository's patterns are **added** to the server's, never
substituted for them. The file is content of the indexed repository, so it may
only narrow what is indexed — a repository cannot re-enable a path the
operator excluded. A manifest that fails to parse is logged and the server's
patterns stay in force, rather than the repository silently losing its
exclusions.

**Pattern form matters.** Prefer `**/dir/**`:

| pattern | `node_modules/x.js` | `web/node_modules/x.js` | `node_modules_old/x.js` |
| --- | --- | --- | --- |
| `node_modules/**` | ignored | **kept** | **ignored** |
| `**/node_modules/**` | ignored | ignored | kept |

`dir/**` is a string-prefix match anchored at the repository root: it misses
nested copies and catches directories that merely start with the same letters.
`**/dir/**` matches whole path components at any depth, which is what these
patterns almost always mean. The same applies to file patterns — `*.min.js`
matches only at the root, `**/*.min.js` matches everywhere.

### What the checkout excludes: `.gitignore`

Every repository already says what is not its code, and the indexer reads it:
each checkout's `.gitignore` files and its `.git/info/exclude` apply on top of
the configured patterns. Build outputs, virtualenvs, vendored trees and
`node_modules` are excluded without anyone listing them a second time — as are
the surprises, such as a benchmark corpus of foreign repositories sitting in a
gitignored directory of the project being indexed.

These are git's rules, not a glob list resembling them:

| | |
| --- | --- |
| **nesting** | a `.gitignore` governs its own directory and below, and outranks the ones above it |
| **negation** | `!pattern` re-includes, and the last matching line wins — including one in a nested file overriding a parent |
| **anchoring** | `/foo` and `a/foo` match at that level only; `foo` matches at any depth below the file that declared it |
| **directory-only** | `foo/` matches a directory and never a file |
| **`**`** | `**/foo` at any depth, `foo/**` everything inside `foo`, `a/**/b` across any number of levels |
| **the parent rule** | a file cannot be re-included if a parent *directory* is excluded — git never looks inside one, so a negation there has nothing to override |

Two deliberate departures, both towards indexing more rather than less: the
user's global excludesfile (`core.excludesFile`) is not read — it is one
developer's machine configuration, and a server or container has nobody's, so
honouring it would make the same commit index differently depending on who ran
it — and matching is case-sensitive whatever `core.ignoreCase` says.

**A tracked file is never excluded by this.** A file git has in its index is in
the repository and shows up in every listing, whatever the ignore rules say
about it, so an index that lacked it would answer questions about that
repository wrongly: force-added files (23 across the twelve repositories of the
benchmark corpus — 7 in consul, under an ignored `vendor/`, and 16 in grafana)
stay indexed, and a directory holding one is still walked into, while its
untracked neighbours are left out.
The exception applies only to `.gitignore`; `repos.ignore` and `.ragota.yaml`
mean what they say. When the index cannot be read at all — no git on `PATH` in
a checkout that has one — the exclusions are applied unverified and a warning
names the repository.

**Order.** The configured patterns (`repos.ignore` plus the repository's
`.ragota.yaml`) are checked first and are final; `.gitignore` is consulted only
for the paths they keep. So the three compose in one direction: each can
exclude more, none can re-include what an earlier one excluded. A `!vendor/`
in a repository's `.gitignore` does not defeat `**/vendor/**` in the operator's
config, and neither does a file being tracked.

**Turning it off.** `repos.use_gitignore: false` (default `true`) for a
repository whose `.gitignore` hides something worth indexing;
`RAGOTA_USE_GITIGNORE=0` for a run with no config file to edit, which is what
`--source DIR` is. It applies everywhere the patterns do — the full index pass,
`--watch`, and the incremental pass a pushed commit takes — so the three cannot
disagree about what the index should hold. `--watch` also re-reads a
`.gitignore` that changes under it, rather than using the one it read at
startup.

### Stats

```bash
# Indexer statistics
curl http://localhost:8080/api/v1/stats
```

### Health

```bash
curl http://localhost:8080/health
# => {"status": "ok", "version": "v0.4.1-3-gacf3b5f", "api_version": "0.2.0"}
```

See [Versioning](#versioning) for what the two versions mean.

### Error responses, limits and probes

Errors carry a machine-readable code alongside the message:

```json
{"error": "repo is already being indexed", "code": "repo_busy"}
```

Codes: `repo_busy`, `commit_gap`, `payload_too_large`, `invalid_path`,
`not_found`, `validation_failed`, `rate_limited`, `unauthorized`, `forbidden`,
`internal_error`, `not_ready`, `index_damaged`. Request bodies over
`server.max_body_bytes` (1 MiB; commits: `server.max_commit_body_bytes`,
64 MiB) are rejected with `413`, and the rate limiter answers `429`.

```bash
# Liveness: answers as soon as the process is up
curl http://localhost:8080/health

# Readiness: checks the storage backend; 503 with code "not_ready" until it is usable
curl http://localhost:8080/ready
```

`GET /repos/{id}/sync-state` additionally reports `pending_commit` (the SHA a
running batch is applying), `indexed_at` and `last_error`.

## Authentication

Configure authentication in `config.yaml` under `server.auth`:

```yaml
server:
  auth:
    type: api_key       # none | api_key
    api_keys: ["${RAGOTA_API_KEY}"]
```

When `type` is `api_key`, include the key in requests:

```bash
curl http://localhost:8080/api/v1/repos \
  -H "X-API-Key: your-secret-key"
```

The `Authorization: Bearer` header is also supported (maps to the same API key check).

### Scopes

A key carries a scope, written as a prefix in `api_keys`:

```yaml
server:
  auth:
    type: api_key
    api_keys:
      - "read:${RAGOTA_MCP_KEY}"     # retrieval and inspection only
      - "admin:${RAGOTA_ADMIN_KEY}"  # everything
```

`read` reaches `/search`, `/context`, `/nav/*`, `/graph/*`, `/services`,
`/topics`, `/stats` and every `GET` under `/repos`. `admin` adds the routes that
change something: adding, deleting, indexing and resetting a repository,
pushing commits, ingesting a runtime service graph, and `/admin/compact`. A read
key calling one of those gets `403` with code `forbidden`.

Give a client that acts for a language model — an MCP server, an agent — a
`read` key. It then holds no delete rights at all, so no prompt can reach one.

The prefix is configuration, not credential: the client still sends
`RAGOTA_MCP_KEY` itself, never `read:` in front of it. A key with **no** prefix
is `admin`, which is what every key granted before scopes existed, so an
existing single-key config keeps working unchanged and restricting a key is an
edit that opts in. Only the two exact prefixes are scopes — a key that merely
contains a colon (`team:orders:s3cret`) is still the whole string.

## Versioning

`GET /health` reports what a client is talking to:

```bash
curl http://localhost:8080/health
# => {"status": "ok", "version": "v0.4.1-3-gacf3b5f", "api_version": "0.2.0"}
```

`version` is the binary, stamped at build time (`make binary`; `dev` when it was
not stamped). `api_version` is the wire contract — the `info.version` of
`/openapi.yaml` — and is the one a client with its own release cycle can log or
refuse to talk to. A test keeps the two spellings of it from drifting apart.

## Directory Structure

```
cmd/server/main.go       — flags, the `run`/`repos` subcommands, config, setup.Build, http.Server
cmd/server/checkconfig.go — --check-config validation + dependency dry-run
cmd/waitport/            — tiny TCP readiness probe used by the Makefile
cmd/lspprobe/            — what one language server can answer for a repo, and what it costs
cmd/lspcalls/            — run the LSP call-edge pass over an existing index (no reindex)
deploy/                  — service image, compose stacks, LSP server images
internal/
  api/                   — HTTP handlers, routes, auth, ratelimit, types
  obs/                   — process-local metrics registry behind /metrics
  service/               — Service with business logic
  setup/                 — Build(ctx, cfg) component initialization
  config/                — types, Load(), Validate(), ignore patterns
  gitignore/             — git's own exclusion rules for one checkout (.gitignore, info/exclude, tracked files)
  chunking/              — WindowChunker, GoChunker
  search/                — RRF search service
  indexing/              — Indexer interface + ast/ (Go/Java/C#/TS/Python/proto/sql/config parsers), bm25/, vector/
  graph/                 — cross-repo linker, traversal, parameter tracing, context expansion
  svcdetect/             — service detection (monorepo layouts, compose, cmd/*)
  models/                — Embedder + Generator interfaces (ollama, openai)
  repos/                 — RepoSource interface + git/, local/, manifest/ (.ragota.yaml), --source discovery, the shared walk
  watch/                 — filesystem watcher behind --watch (debounced, feeds the incremental pass)
  status/                — bounded in-process status surface for an interactive front end
  tui/                   — the --interactive dashboard (bubbletea) over that surface
  storage/               — Storage interface + sqlite/, postgres/, qdrant/
  httpx/                 — HTTP client with retries
  e2e/                   — end-to-end tests over testdata microservice fixtures
testdata/
  microservices/         — Go monorepo: gateway (HTTP+gRPC client), orders (gRPC server + Kafka producer), proto contract
  billing-java/          — Spring: @KafkaListener consumer + RestTemplate client
  notifier-dotnet/       — ASP.NET Core attribute-routed controller
  web-ts/                — express route + axios client
  analytics-py/          — FastAPI + Kafka consumer (env-driven topic) + SQL migrations
```

## Configuration

See `config.example.yaml` for full configuration options, and validate any
change with `ragota-core --check-config` before deploying it.

Environment variables:

| Variable | Effect |
|---|---|
| `RAGOTA_CONFIG` | config file path when `--config` is not given (default `config.yaml`) |
| `RAGOTA_BM25_PATH` | overrides `indexes.bm25.path` for one process |
| `RAGOTA_MAX_BODY_BYTES` | overrides `server.max_body_bytes` |
| `RAGOTA_MAX_COMMIT_BODY_BYTES` | overrides `server.max_commit_body_bytes` |
| `RAGOTA_TRUSTED_PROXIES` | overrides `server.trusted_proxies` (comma-separated) |
| `RAGOTA_WEBHOOK_SECRET` | shared secret for `POST /webhooks/git` |
| `GITHUB_TOKEN`, `GITLAB_TOKEN` | fallback for `repos.sources.git.auth.*` when the config leaves a token empty |
| `RAGOTA_LSP_HOST_ROOT` | host directory the `deploy/lsp` containers mount at `/workspace` |
| `RAGOTA_LSP_BIND` | interface the LSP ports are published on (default `127.0.0.1`) |

Values in the YAML may reference the environment as `${NAME}`. A literal `$` is
written `$$`; a bare `$NAME` is rejected rather than silently expanded, so a DSN
password or API key containing `$` no longer corrupts the file (`pa$$word`).
Comments are never expanded, so commented-out examples may reference variables
nobody has set.

Key sections:
- `server`: Host, port, auth (`none` or `api_key` — a typo is now a validation
  error instead of silently disabling authentication), rate limiting, CORS
  (`origins: ["*"]` allows every origin), trusted proxies, HTTP timeouts
  (`write_timeout_seconds` defaults to 120: `/context` and a synchronous clone
  legitimately outlive the old 15s) and request body caps
- `log`: `level` (`debug` shows the LSP and linker diagnostics) and `format`
  (`text` or `json`)
- `storage`: PostgreSQL is the primary relational backend (sqlc-generated
  queries over native pgx/pgxpool; `internal/storage/postgres`, regenerate with
  `make sqlc`). SQLite remains available as a lightweight embedded/dev option
  (postgres takes precedence when both are configured). Qdrant is optional, for
  vectors.
- `indexes`: AST, BM25 (`path` for the on-disk index) and vector indexers.
  `indexes.ast.languages` restricts which parsers are registered — files in
  other languages are skipped; omit the key to register all of them. The vector
  index requires an embedder *and* `storage.qdrant` (validation rejects the
  combination that used to die at startup with `vector store not available`).
  `chunking.method: semantic|hybrid` is implemented for Go only — every other
  language falls back to window chunking
- `models`: AI model providers (Ollama, OpenAI). The `openai` provider targets
  any OpenAI-compatible endpoint — chat completions go to
  `{base_url}/v1/chat/completions` and embeddings to `{base_url}/v1/embeddings`,
  and a `base_url` already ending in `/v1` is accepted rather than doubled, so
  a vLLM or gateway URL works whichever form it is published in. The vector
  embedder may override the endpoint with its own `base_url`.
- `repos`: Repository sources (local, git) and ignore patterns. Each checkout's
  own `.gitignore` applies on top of `repos.ignore` unless
  `repos.use_gitignore: false` (see [What the checkout
  excludes](#what-the-checkout-excludes-gitignore)). Git tokens come from
  `repos.sources.git.auth`, each falling back to `GITHUB_TOKEN` /
  `GITLAB_TOKEN`

## Assistant LLM: recon pass & edge disambiguation

An optional auxiliary LLM (`models.assistant` in the config) helps in two
places where the heuristics run out of evidence. It is fully optional: without
it, indexing and linking behave exactly as before.

- **Recon pass (before first indexing).** Once per repository — before service
  detection, and only while no `recon` unit exists for the repo — the
  assistant receives a compact overview (directory tree up to depth 3 with
  build manifests, first 2KB of the README, the manifest list; at most 8KB
  total) and answers with strict JSON:
  `{"services":[{"name","root","purpose","language"}],"config_paths":[...],"notes":"..."}`.
  The raw answer is stored as a unit of kind `recon`
  (`recon:<repoID>`, path `.ragota/recon`), and the suggested services are
  merged into service detection as *hints*: heuristically detected services
  always win per root directory, LLM hints only add services whose root
  nothing else claimed (marked `detected_by: llm`). Recon failures are logged
  and never fail indexing.

- **Edge disambiguation (linker).** When contract matching is ambiguous —
  an `http_call` whose two best route candidates score within 0.05 of each
  other, or an `rpc_call`/`implements_rpc` matched by method name only with
  more than one candidate — the assistant is shown the call site and the
  candidate list and asked for the index of the right one (or -1). A chosen
  candidate is applied with high confidence and the edge meta gets
  `"source":"llm"`; a declined choice keeps the heuristic result. Answers are
  cached per join key within a run, and at most 20 LLM calls are made per
  linking run.

Metrics: `ragota_recon_total`, `ragota_disambig_total`. See the `assistant`
block in `config.example.yaml`.

## LSP refinement pass

The tree-sitter parsers are heuristic; the optional LSP pass refines their
output with real language servers. When `lsp.enabled` is set, indexing runs an
extra step after the AST indexer (`internal/lsp`, registered by `setup.Build`
chained behind the AST indexer so it always sees the stored units):

- `textDocument/documentSymbol` — functions/methods the parsers missed are
  added as units (marked with hash `lsp`).

### Correcting the call graph (`lsp.calls`)

A call edge records the callee's *name*; the linker resolves that name to a
unit. That is language-independent and approximate in one way: when several
definitions share a name the linker has to guess, and says so by resolving at
`ConfHeuristic`. A language server does not guess.

`lsp.calls.enabled` turns on a second, **repository-scoped** pass
(`lsp.CallRefiner`, run by the service right after linking). For each selected
callee definition it asks `textDocument/references` and spends the answer on
the call edges that already exist:

| what the server says | what happens to the edge |
| --- | --- |
| references the definition at a line that already has a matching call edge | confirmed, confidence `ConfExact`, meta `source: lsp` |
| references it at a line whose edge points at another same-named definition | re-pointed to the definition the server names |
| references it at a line with no parsed call at all | a new `call` edge from the enclosing unit |
| does *not* reference it from a line whose edge claims it does | the resolution is dropped (`dst_id` cleared, `ConfWeak`) |

It is repository-scoped and not an indexer because a session costs a whole
workspace load — 60-90 s for a large Go module, minutes for Maven or MSBuild —
and indexing hands the indexers 512 files at a time, which would pay that per
batch.

**The bound is the design.** A `references` request per function over a large
repository is tens of thousands of requests, so `lsp.calls.scope` selects only
the definitions where name resolution is weakest:

- `boundary` — endpoints of contract edges (`handled_by` destinations;
  `implements_rpc` / `http_call` / `rpc_call` / `produces` / `consumes` /
  `writes_to` / `reads_from` sources). Few, and what cross-service questions
  are about.
- `ambiguous` — definitions whose name another definition in the same
  repository also carries *and* that some call edge names. An ambiguous name
  nothing calls costs a request for nothing.
- `both` (default) — the union, boundary first.

Test scaffolding and vendored trees are excluded, `max_symbols` caps the
requests per repository (reaching it is logged and counted), and a symbol with
more than `max_refs_per_symbol` reference sites is left alone entirely — it is
not an answer to "what calls X", and a symbol whose list was truncated may not
contradict anything either.

The pass is validated rather than assumed:

- `lsp.enabled: true` with an empty `servers` map is a config error (it used to
  skip every file and report success);
- `host_root` and `mount_root` must be set together and be absolute — a wrong
  mapping makes the servers return empty results;
- every configured server is dialed once at startup and the result is logged;
- a reachable server that returns no symbols for *any* file is counted as a
  failed language, not as an empty success.

Metrics: `ragota_lsp_pass_total`, `ragota_lsp_files_refined`,
`ragota_lsp_files_failed`, `ragota_lsp_files_skipped`,
`ragota_lsp_server_failures`, `ragota_lsp_empty_languages`,
`ragota_lsp_reference_errors`, `ragota_lsp_dial_failures`,
`ragota_lsp_pass_seconds`; and for the call pass
`ragota_lsp_call_requests`, `ragota_lsp_call_confirmed`,
`ragota_lsp_call_repointed`, `ragota_lsp_call_added`,
`ragota_lsp_call_contradicted`, `ragota_lsp_call_truncated`,
`ragota_lsp_call_pass_seconds`.

Unavailable servers are logged and skipped — indexing never fails because of
the LSP pass.

Each supported language (`go`, `typescript`, `java`, `csharp`) runs its own
language server in Docker, bridged from stdio to TCP with `socat` (fork mode:
one server process per connection, reconnect-safe). Images and compose file
live in `deploy/lsp/` (gopls, typescript-language-server, Eclipse JDT LS,
OmniSharp; host ports 7301-7304, published on `127.0.0.1` — these servers are
unauthenticated and can read, for C# write, the entire mounted tree, so set
`RAGOTA_LSP_BIND` only behind a firewall). All server versions are pinned in
the Dockerfiles; Compose v2 is required, because v1 ignores the memory limit
the .NET server needs to size its GC heap.

```bash
make lsp-up        # build + start the four LSP containers ($PWD mounted at /workspace)
make lsp-down      # stop them
make test-e2e-lsp  # full cycle: up, wait for ports, run LSP e2e tests, down
```

Repositories must live under `lsp.host_root`, which the compose file mounts
read-only at `lsp.mount_root` (`/workspace`); set `RAGOTA_LSP_HOST_ROOT` when
starting the containers. See the `lsp` section in `config.example.yaml`.

## Distributed indexing

Multiple ragota-core instances can share one PostgreSQL database and split
indexing work. With `indexes.distributed: true`, `POST /repos/{id}/index`
enqueues a job into a shared queue instead of indexing in-process; each
instance runs a worker that claims jobs atomically (`FOR UPDATE SKIP LOCKED`),
heartbeats while indexing, and requeues jobs whose heartbeat went stale
(crashed workers). Config:

```yaml
indexes:
  distributed: true
  job_poll_seconds: 3     # worker poll & heartbeat interval
  stale_job_seconds: 120  # requeue running jobs with a heartbeat older than this
```

SQLite implements the same queue, so a pair of instances over one shared file
works for development; startup logs a warning, because two instances with their
own database file each silently run two separate queues.

## Upgrading past the zapx fix

The BM25 index is written by zapx, and v17.1.2 — the version bleve v2.6.0
pins — corrupts the postings chunk-offset table of any term whose postings
skip a full chunk mid-range. For a code index that means the repo_id and
language terms in segments merged across repositories: the visible symptom was
`memUvarintReader overflow` on some searches, but the quiet case is worse,
because an affected term simply returns the wrong documents. The patched
module lives in `third_party/zapx-v17` (see its RAGOTA-PATCH.md), with
regression tests in `internal/zapverify`.

**A BM25 index written before this fix stays corrupt** — the patch changes the
writer, not what is already on disk. After upgrading:

```bash
# 1. check an index on disk; it exits non-zero and names the affected terms
go run ./cmd/zapcheck ~/.ragota-core/data/bm25

# 2. if it reports damage, force a full reindex of every repository
curl -X POST http://localhost:8080/api/v1/repos/{id}/index -d '{"force": true}'
```

Worth running `zapcheck` from cron on a long-lived index: the failure is
silent by nature, and a search that quietly returns the wrong documents is not
something a health check notices. A damaged index that *is* detected at query
time answers 503 with code `index_damaged` rather than a bare 500.

## Development

```bash
make binary            # bin/ragota-core, version-stamped from git describe
make install           # go install ./cmd/server
make run               # run with CONFIG=config.yaml (override CONFIG=...)
make check-config      # validate the config and probe its dependencies
make test              # unit tests
make test-integration  # -tags integration
make test-e2e          # end-to-end suite
make test-postgres     # storage tests against a throwaway postgres container
make test-e2e-lsp      # e2e with the four LSP containers (no `nc` needed)
make lint              # golangci-lint; `make lint-install` if it is missing
make ci                # build vet fmt-check lint test test-integration
make compose-up        # deploy/docker-compose.yml stack
make help              # all targets
```

Two measurement harnesses sit next to the code, both Python 3 standard library
only:

```bash
make corpus-clone  CORPUS_DIR=/data/corpus   # 16 real repositories, ~15 GB
make corpus-bench  CORPUS_DIR=/data/corpus   # what the extractor found: routes, edges, services
make eval-validate CORPUS_DIR=/data/corpus   # re-check the eval ground truth against the sources
make eval          CORPUS_DIR=/data/corpus   # what retrieval answered: recall@k, MRR, nDCG@10
make eval-compare  CORPUS_DIR=/data/corpus EVAL_ARGS="--b-variant rerank --rerank-url ..."
```

`tools/corpus` answers *how much did the extractor find*; `tools/eval` answers
*did the answer to a question get better*. The second is 80 questions over 12
of those repositories — "where is X implemented", "what calls X", "where does
this route go", "which service publishes this queue", "where is this table
written" — each with the file that answers it, established by reading the code
rather than by asking the index. `tools/eval/README.md` has the query set, the
metrics and the current baseline.

`make lint` fails when golangci-lint is absent instead of passing silently, so
`.golangci.yml` is live configuration rather than decoration. `make ci` is the
full local gate; `cmd/server/checkconfig_test.go` loads `config.example.yaml`
and `deploy/config.docker.yaml` through `Load` + `Validate`, which catches
documentation drift against the schema.

## Project Status

- [x] Core interfaces (Storage, Indexer, Embedder, RepoSource)
- [x] HTTP server with REST API endpoints
- [x] Local and Git repository sources
- [x] SQLite storage implementation (files, AST units, edges, repos)
- [x] Qdrant vector storage implementation
- [x] AST indexer (tree-sitter based): Go, Java, C#, TypeScript/JavaScript + proto contract parser
- [x] Edge extraction: calls, HTTP routes/clients, gRPC clients/servers, Kafka producers/consumers
- [x] Service detection (docker-compose, cmd/*, build manifests, .ragota.yaml override)
- [x] Global linker: cross-repo resolution of gRPC/HTTP/Kafka contract edges with confidence scores
- [x] Graph API: /graph/neighbors, /graph/path, /graph/trace, /services, /topics
- [x] Parameter flow tracing across service boundaries (HTTP/gRPC/Kafka)
- [x] Vector indexer (semantic/hybrid chunking, embeddings)
- [x] BM25 indexer (Bleve, with chunking and stats)
- [x] Ollama and OpenAI embedders
- [x] Navigation endpoints (symbol search, definition, references)
- [x] Hybrid search service (RRF fusion)
- [x] Component initialization via setup.Build
- [x] Ignore patterns support
- [x] `.gitignore` and `.git/info/exclude` applied per checkout (`repos.use_gitignore`)
- [x] Indexer statistics
- [x] Async indexing (POST /repos/{id}/index returns 202)
- [x] Auth middleware (API key)
- [x] Rate limiting middleware
- [x] RRF fusion with correct weights (bm25=0.7)
- [x] Incremental indexing (skip unchanged files by content hash)
- [x] Repository persistence via SQLite repos table
- [x] End-to-end tests: multi-repo, monorepo services, cross-service graph, parameter trace
- [x] Python parser (FastAPI/Flask routes, requests/httpx, kafka-python/confluent)
- [x] SQL migration parser + writes_to/reads_from edges + database sinks in traces
- [x] Config-file indexing (yaml/json/.env/.properties) with env-driven Kafka topic resolution
- [x] OpenAPI spec import as HTTP route contracts
- [x] Graph-expanded retrieval endpoint (/context)
- [x] Trace alternatives (branching flows: Kafka + DB at once)
- [x] Stale file cleanup on reindex
- [x] Git push webhooks (GitHub/GitLab) with update + reindex
- [x] Mermaid/DOT service graph export; per-service topic filter
- [x] OTel/Jaeger runtime service graph ingest (runtime_call edges)
- [x] Prometheus metrics endpoint (dependency-free)
- [x] LLM summaries of files and services (Ollama/OpenAI, optional)
- [x] PostgreSQL storage backend (integration-tested)
- [x] Assignment-level aliasing in traces (`x := userID; g(x)` is followed via edge alias metadata)
- [x] AsyncAPI channel import (declared channels surface in /topics with descriptions and spec/code drift)
- [x] Indexed + incremental contract linking (hash-bucket matching, valid resolutions skipped, kafka flows rebuilt per touched topic)
- [x] Declarative framework detectors (new HTTP/Kafka framework support = one data row, see internal/indexing/ast/frameworks.go)
- [x] Shared SQL filter layer for SQLite/PostgreSQL backends (internal/storage/sqlutil)
- [x] OpenAPI spec of the service's own API served at /openapi.yaml
- [ ] Transitive alias chains and cross-function dataflow (current aliasing is one level, file-scoped)
- [x] Distributed indexing workers (shared job queue; see "Distributed indexing")
- [x] `--check-config` wiring dry-run, compose stack for the service and its models
- [x] LSP call-edge correction (`lsp.calls`): bounded `textDocument/references`
      over contract-boundary and ambiguously-named callees, applied to the call
      edges the name matcher resolved

## License

MIT
