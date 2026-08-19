---
sidebar_position: 9
title: Quality, measured
---

# Quality, measured

Claims about retrieval quality in this project come from a benchmark in the
repository, not from adjectives. `tools/eval` holds a 103-question set
spanning twelve of the sixteen repositories in the benchmark corpus
(petclinic to elasticsearch — `tools/corpus` documents why each is there),
with ground truth pinned to file and line and **validated against the
sources, never against the system under test** (`make eval-validate`).

```bash
make corpus-clone   # clone the corpus (shallow, ~15 GB)
make eval           # index, ask all 103, score recall@k / MRR / nDCG@10
make eval-fast      # the six small repositories, laptop-sized
make eval-compare   # A/B: two binaries or two configs on the same set
```

## The baseline line

The default configuration — AST + BM25 over SQLite, nothing external — as
last recorded in the journal
([`tools/eval/README.md`](https://github.com/Nahua-Foundation/ragota/blob/master/tools/eval/README.md)):
the full set measured 2026-08-13, the `eval-fast` subset 2026-08-16.

| Metric | Full set (103 questions) | eval-fast (40 questions, six repos) |
|---|---|---|
| recall@1 | 0.359 | 0.500 |
| recall@10 | 0.631 | 0.750 |
| MRR | 0.442 | 0.577 |
| Never found | 32 | 8 |

Symbol lookup is measured separately, on known identifiers: **recall@1
0.667, MRR 0.714**, against 0.524 / 0.587 for the same identifiers pushed
through `/search` — the gap is why the MCP tool descriptions and the
[skills](./skills.md) insist on routing by input type. One provenance
caveat: those four numbers (and the symbol row in the history below) come
from a 21-identifier measurement that predates this repository — it arrived
with the ragota-core merge, and `run.py` does not re-measure `/nav/symbol`
yet — so unlike everything else on this page they are a recorded result,
not a re-runnable one.

Read the table as an honest operating point, not a boast: on the full set
the top hit answers about a third of the questions (half, on the easier
fast subset), the right file is in the top ten for two out of three, and
roughly three questions in ten are not found at all. Every consumer of
these answers — tool descriptions, skills, docs — is written to that
reality: scan the list, verify before editing, never report an empty answer
as absence without the [health protocol](./skills.md).

## Determinism

The line above is bit-stable: after the tie-break and compaction fixes, ten
consecutive runs of the same binary on the same corpus produced the
identical metrics, and the once-per-load compaction change was accepted
only because every one of `eval-fast`'s 40 questions kept its exact rank.
That was made true, not found true — index construction and score
tie-breaking are pinned — and it is what makes `make eval-compare`
meaningful: a delta is a change, not a coin flip.

## What moved the numbers historically

The eval exists to answer "did this help?" — a few answers it has given:

| Change | Effect |
|---|---|
| Symbol ranking tiers (exact-first, generated demoted) | symbol MRR 0.643 → 0.714, misses 7 → 4 |
| Graph-backed `callers` intent (80-question set) | recall@1 0.175 → 0.237, MRR 0.280 → 0.339, unanswered 32 → 28 |
| Reranker over the fused candidates (full set) | recall@1 0.359 → 0.398, MRR 0.442 → 0.488; the cross-service deficit at ten closes from 19 points to 6 |
| Excluding test files from the index (opt-in) | one elasticsearch pass 147 s → 50 s |
| `.gitignore` honoured in the walk | 107,758 → 374 candidate files on this repo |

## Grading answers, not just hits

`make eval-related` scores what `/context` adds around a hit (does the
expansion name the second file the answer needs?), and `make eval-answers`
puts a whole `/context` package in front of a local model and grades its
answer against the pinned ground truth — with the retrieval ceiling
reported separately, so model failures and retrieval failures do not blur.
That harness is the intended test bed for the small-model +
[skills](./skills.md) workflow this project targets.
