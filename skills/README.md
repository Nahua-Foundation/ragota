# Agent skills for the ragota MCP

Three skills that teach a coding agent to use the `ragota_*` MCP tools well —
above all, **when to use the index and when to use its own glob/grep/read**.
They exist because the difference is measured, not stylistic: the right tool
for the input finds a known identifier at MRR 0.71 where the wrong one
manages 0.59, and a located-then-ranged read costs a few kilobytes of context
where a grep-and-open hunt costs tens. On models with small context windows
that budget is the analysis.

| Skill | Teaches |
|---|---|
| [`ragota-code-search`](ragota-code-search/SKILL.md) | The decision table: search vs symbol vs your own file tools, reading ranges instead of files, budget discipline |
| [`ragota-architecture`](ragota-architecture/SKILL.md) | Cross-repository questions: services, topics, references, traces; unit-id and confidence discipline |
| [`ragota-index-health`](ragota-index-health/SKILL.md) | What an empty answer means: working set and dormant repositories, degraded retrieval, when to fall back to grep |

They are written for the weakest model you would trust with code analysis:
prescriptive rules, one decision table each, worked examples to imitate, and
explicit anti-patterns. Stronger models lose nothing by following them.

## Installing

The skills are plain [Agent Skills](https://agentskills.io) directories — a
`SKILL.md` with YAML frontmatter — so any harness that supports the format
can load them as-is.

**Claude Code** — the binary carries them and writes them itself, so the
skill text always matches the tool descriptions of the `ragota mcp` you run:

```bash
# project: run in the workspace you analyze code in (not this repository)
ragota skills install

# or user-wide
ragota skills install ~/.claude/skills
```

Copying (or symlinking) from a checkout works too:
`cp -R path/to/ragota/skills/ragota-* .claude/skills/`.

**Any other harness** (opencode, crush, a bespoke qwen loop): either point
its skills directory at `skills/`, or inline the three `SKILL.md` bodies into
the system prompt. They total a few KB and are written to be pasted whole;
frontmatter `description` fields tell a router when each applies.

The skills assume the `ragota_*` tools are already connected — wiring the MCP
server itself is described in [docs](../docs/) (`docs/docs/mcp.md`).
