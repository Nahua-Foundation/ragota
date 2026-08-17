---
name: ragota-index-health
description: What an empty or surprising ragota answer means and what to do next — working set, dormant repositories, indexing in progress, degraded retrieval. Use before concluding "this code does not exist" or "nothing calls this" from a ragota result, and whenever ragota answers look inconsistent with the files on disk.
---

# Empty answers: diagnose before you conclude

An empty ragota answer has four ordinary causes, and only one of them is
"the code is not there". Reporting the wrong one produces confident false
statements — "nothing calls this", "there is no such handler" — that a whole
task then builds on. Two minutes of diagnosis beats that every time.

## The protocol

**1. Read the flags already in the answer.** Retrieval tools report when the
answer was `degraded` (one of the indexes did not answer) and when a byte
budget truncated the hit list. A zero-hit answer under degradation is
evidence of nothing — retry, or say the index was unwell. `ragota_context`
answers that came back empty include a retrieval health note for exactly this
reason: read it.

**2. `ragota_status` — is the repository even in the answer set?** It lists
every repository with id, indexing state, last-indexed time, and the line
that catches most surprises:

```
repositories (20, 2 in the working set):
   active  a1b2c3  name shop-api     idle  indexed 2026-08-18 10:12
   dormant d4e5f6  name billing      idle  indexed 2026-08-17 22:03
   ...
```

- **dormant** — the server was started about a different set of
  repositories (`--source`). A dormant repository is indexed and reachable,
  but **retrieval without a `repos` argument does not answer from it**. Name
  it: `ragota_search {"query": ..., "repos": ["billing"]}`. The graph and
  service-map tools reach dormant repositories either way.
- **indexing** — the pass is still running; retrieval sees a moving corpus.
  Wait, or say the answer is provisional.
- **error** — that repository's last pass failed; its index is stale from
  before the failure. Say so instead of trusting it.
- **missing entirely** — it was never registered; ragota knows nothing about
  it. Fall back to file tools for that repository.

**3. Wrong door?** An identifier sent to `ragota_search` meets generated
stubs and ranking built for prose; a sentence sent to `ragota_symbol`
matches on names only, never meaning. Re-ask through the right tool before
concluding (see **ragota-code-search**).

**4. Rephrase once.** For search: ask the question the way a colleague would,
with the domain words the code likely uses. For symbol: try the bare name if
you sent a qualified one — it matches both, but a *mistyped* qualifier
matches neither.

**5. Then grep — bounded.** If ragota still answers nothing, search files
directly, but scope it: the service map and earlier hits tell you which
repository and directory to grep, so even the fallback stays cheap.

**6. Report the distinction.** "Not in the index" (and why: dormant, error,
never registered, degraded) is a different finding from "not in the code".
State which one you established. If you exhausted step 5, you may say the
code is absent — and only then.

## Surprising non-empty answers

- **Stale hits** — a line number that does not match the file on disk means
  the index predates recent edits (no `--watch`, or the pass has not run).
  Trust the file, mention the staleness.
- **Duplicate-looking services** — service names are unique per repository,
  not across the estate; two `api` services are two different things.
  Carry the repository name.
- **A repository you did not expect in the answers** — repositories from
  earlier runs stay registered and dormant, and appear when the graph tools
  cross into them, or when a caller names them. That is by design; scope
  with `repos` when it matters.

## When to stop trusting the index entirely

All of these are visible in `ragota_status`, and each one means "use file
tools and say why": the server is unreachable (every tool errors), the
repository you need is missing or in error, or its last index predates the
changes you are analyzing. The index is a cache of understanding — when it
cannot be fresh, the files are the truth.
