# Connecting an agent

`ragota mcp` speaks MCP on stdio — the shape every MCP client launches — and
forwards to a running `ragota` over HTTP. It is deliberately **read-only**:
no tool reaches a mutating route, and there is no flag that adds one, so the
key you hand it can be a `read:` key and a model cannot be talked into
deleting a repository.

## Launch block

Claude Code (or any client with the standard JSON config):

```json
{
  "mcpServers": {
    "ragota": {
      "command": "/path/to/bin/ragota",
      "args": ["mcp"],
      "env": {
        "RAGOTA_URL": "http://localhost:8080",
        "RAGOTA_MCP_KEY": "…read-scoped key, if auth is on…"
      }
    }
  }
}
```

```bash
claude mcp add ragota -e RAGOTA_URL=http://localhost:8080 -- /path/to/bin/ragota mcp
```

At startup it proves the whole path — server reachable, API version
compatible, key accepted, configured scope valid — and refuses to start with
a reason on stderr rather than failing ten times inside a model's turn.
`ragota mcp -check` runs the same probe and exits.

## Configuration (environment)

| Variable | Meaning | Default |
|---|---|---|
| `RAGOTA_URL` | Base URL of the ragota server | `http://localhost:8080` |
| `RAGOTA_MCP_KEY` | API key; the read-scoped one is the point | — |
| `RAGOTA_API_KEY` | Fallback if `RAGOTA_MCP_KEY` is unset | — |
| `RAGOTA_AUTH_STYLE` | `api-key` (X-API-Key) or `bearer` (for gateways) | `api-key` |
| `RAGOTA_TIMEOUT` | Budget for one whole tool call | `120s` |
| `RAGOTA_MAX_BYTES` | Default response budget | `16384` |
| `RAGOTA_REPOS` | Default repository scope, comma-separated, by id or name | all |

`RAGOTA_REPOS` exists because the launch block is the one place that knows
which repositories a workspace is about; a model cannot guess ids.

## The ten tools

| Tool | Answers |
|---|---|
| `ragota_search` | A question in prose → ranked locations |
| `ragota_symbol` | An identifier you hold → definitions, exact-first; enumerates kinds |
| `ragota_context` | Search + graph expansion — the multi-file answer (expensive) |
| `ragota_references` | file+line → what uses the unit there |
| `ragota_neighbors` | unit id → edges around it |
| `ragota_path` | two unit ids → does A reach B, through what |
| `ragota_trace` | function + parameter → where the value ends up, across services |
| `ragota_services` | The service map, or mermaid/dot diagram text |
| `ragota_topics` | Topics with producers and consumers |
| `ragota_status` | What is indexed, the working set, and how far to trust an empty answer |

Tool descriptions carry the routing rule (question → search, identifier →
symbol) with the measured numbers behind it, because choosing wrong is the
largest avoidable quality loss on this server. The
[skills](./skills.md) teach the same judgement at the agent level.

## Design notes

- **Budgeted by default.** One line of code per hit, 16 KB per answer; a
  default `ragota_context` call had measured over ten thousand tokens
  before the budget existed. Agents open files themselves — answers here
  are for deciding *which*.
- **Names accepted everywhere.** Every `repo` argument takes an id or a
  name; ids come back in answers. A wrong id would read as "no such code"
  (the server answers zero hits, not an error), so the resolver runs
  client-side.
- **Empty answers are explained.** Search carries retrieval diagnostics on
  every call; an empty `ragota_context` answer triggers a health probe, so
  "degraded" is never silently readable as "absent".
- **Traps clamped.** The server resets a too-deep trace to its default
  instead of clamping; the tool clamps, so "as deep as possible" means what
  it says.
