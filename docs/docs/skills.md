---
sidebar_position: 6
title: Agent skills
---

# Agent skills

The MCP tools tell an agent *what* they do; the skills in
[`skills/`](https://github.com/Nahua-Foundation/ragota/tree/master/skills) teach
it *when* — above all, when the index beats the agent's own glob/grep/read
and when it does not. They are the distilled judgement from measuring this
system, packaged as [Agent Skills](https://agentskills.io) (a directory with
a `SKILL.md`), and they are the difference between an agent that has the
tools and an agent that uses them well.

| Skill | Teaches |
|---|---|
| `ragota-code-search` | The decision table keyed on what the agent is holding (question → search, identifier → symbol, known literal → grep, known path → read); locate-then-read-ranges; budget discipline |
| `ragota-architecture` | Cross-repository questions through the graph: services, topics, references, traces; unit-id and confidence discipline |
| `ragota-index-health` | The empty-answer protocol: degraded flags, the working set and dormant repositories, when to fall back to bounded grep, and how to report "not in the index" vs "not in the code" |

## Why skills at all

Two reasons, both measured:

1. **Routing is worth ~20% of answer quality.** A known identifier through
   `ragota_symbol` resolves at MRR 0.71; the same identifier through
   `ragota_search` manages 0.59 — and for prose questions the numbers
   reverse. The rule fits in one table; without it, every model relearns it
   per task or never does.
2. **Context is the budget.** Locate-with-ragota then read-a-range costs a
   few KB; grep-and-open costs tens. The skills exist to make the cheap
   path the habitual one — written for small-context models
   (prescriptive rules, worked examples, explicit anti-patterns), and
   stronger models lose nothing by following them.

## Installing

**Claude Code** — into the workspace where the agent analyzes code:

```bash
mkdir -p .claude/skills && cp -R path/to/ragota/skills/ragota-* .claude/skills/
```

(or `~/.claude/skills/` for user-wide.)

**Other harnesses** — point the skills directory at `skills/`, or paste the
three `SKILL.md` bodies into the system prompt; they total a few KB and are
written to survive that. The frontmatter `description` is the trigger a
skill router matches on.

See [`skills/README.md`](https://github.com/Nahua-Foundation/ragota/blob/master/skills/README.md)
for the full text.
