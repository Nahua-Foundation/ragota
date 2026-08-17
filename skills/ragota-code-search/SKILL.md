---
name: ragota-code-search
description: Choose between the ragota MCP tools and your own glob/grep/read when locating code. Use whenever a coding task starts with finding something — a function, a route, a config, "where does X happen" — in a workspace served by ragota. Saves context and finds cross-repository answers plain file search cannot.
---

# Finding code: ragota vs your own file tools

ragota is an index over every repository in this workspace: ranked search,
symbol lookup, and a cross-repository graph of calls, HTTP routes, gRPC
methods, Kafka topics and database tables. Its answers are a few hundred
bytes. A grep-and-open-files hunt for the same answer is tens of kilobytes.
Use ragota to **locate**, then read **only the lines it names**. That is the
whole method.

If no `ragota_*` tools are listed, the MCP server is not connected; use your
file tools and say the index was unavailable.

## The decision table

Pick by **what you are holding**, not by what you want to end up with:

| You are holding | Do this | Not this |
|---|---|---|
| A question in words — "where is the retry budget configured" | `ragota_search` with the question **as a sentence** | grep of guessed keywords |
| An identifier — from a stack trace, diff, log, or earlier answer | `ragota_symbol` | `ragota_search` |
| An exact literal you know is in the source — an error message, a config key | your own grep | `ragota_search` |
| A file path you already know | read the file directly | any ragota call |
| A file+line, asking "who uses this" | `ragota_references` | reading every importer |
| "who calls X" by name | `ragota_search` with `intent: "callers"` | plain search |
| A question about services, topics, or cross-repo flow | see the **ragota-architecture** skill | grepping several repos |

The first two rows are measured, not stylistic: on the same benchmark,
`ragota_symbol` finds a known identifier at MRR 0.71 while `ragota_search`
manages 0.59 on it — and for prose questions the numbers reverse. Picking the
wrong door is the largest avoidable loss of answer quality.

## The method

1. **Locate** with one ragota call. Defaults are tuned: one line of code per
   hit, capped bytes. Do not raise `limit`, `max_bytes` or `snippet` "to be
   safe" — you will open the file anyway.
2. **Read a range, not a file.** A hit says `file:line`. Read ~40 lines
   around it. Never page a whole large file into context when the index
   already told you which lines matter.
3. **Widen through the graph, not by reading.** Need callers?
   `ragota_references` with that file and line. Need the surrounding flow?
   Then — and only then — `ragota_context` (it is the most expensive call
   here; keep `hops: 1`).

Worked example:

```
Task: "Discounts apply twice on checkout — fix it."

ragota_search {"query": "where is the discount applied during checkout"}
  → 1. shop-api pricing/discount.go:88 applyDiscount (function) ...
Read pricing/discount.go, lines 60–120          ← a range, not the file
ragota_references {"repo": "shop-api", "file_path": "pricing/discount.go", "line": 88}
  → checkout/total.go:41, cart/preview.go:73
Read the two ranges. Fix both call sites.
```

Four calls, a few KB of context, and the answer covered two files you had no
reason to guess.

## Trust, and its limits

- **Search**: the top hit answers the question roughly half the time; the
  right file is in the top 10 far more often. Scan the whole hit list before
  concluding anything. Hits carry a `Reason` — a hit promoted because it
  carries the contract the question names (a route, a topic, a table) is
  usually the answer even when ranked below a text match.
- **Symbol**: exact name matches come first, generated code and tests are
  demoted below hand-written code. First hit is usually right — but confirm
  by reading the range before editing.
- **A miss is not absence.** A small share of real symbols and answers are
  missed. Before reporting "X does not exist": follow the
  **ragota-index-health** skill (check status, working set, degraded flags),
  then grep — bounded to the directories the earlier answers pointed at.

## When your own tools win

- Editing a file you already have open — never ask the index about it.
- Exhaustive occurrence lists (renames, license sweeps): ranking truncates;
  grep does not.
- Files created or changed in the last moments, unless the server runs with
  `--watch` — the index follows saves, not keystrokes.
- Anything the status tool says is not indexed.

## Anti-patterns

- Do **not** decompose a question into keywords for `ragota_search`. It
  resolves what the question is about (routes, topics, tables) from the
  phrasing; keyword soup destroys that.
- Do not send identifiers to `ragota_search` or sentences to `ragota_symbol`.
- Do not open every hit. Read ranges for the top candidates only.
- Do not retry a failed call unchanged; change the input or change the tool.
- Do not treat an empty answer as proof of absence — see
  **ragota-index-health**.
