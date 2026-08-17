---
sidebar_position: 8
title: Quality, measured
---

# Quality, measured

Claims about retrieval quality in this project come from a benchmark in the
repository, not from adjectives. `tools/eval` holds a 103-question set over
a corpus of twelve real open-source repositories (petclinic to
elasticsearch, ~2 GB of sources), with ground truth pinned to file and line
and **validated against the sources, never against the system under test**
(`make eval-validate`).

```bash
make corpus-clone   # clone the corpus (~15 GB with history)
make eval           # index, ask all 103, score recall@k / MRR / nDCG@10
make eval-fast      # the six small repositories, laptop-sized
make eval-compare   # A/B: two binaries or two configs on the same set
```

## The baseline line

The default configuration — AST + BM25 over SQLite, nothing external — on
the six-repository fast set:

| Metric | Value |
|---|---|
| recall@1 | 0.524 |
| MRR | 0.589 |
| Misses (not in top 10) | 5 / 21 |

Symbol lookup, measured separately on known identifiers: **recall@1 0.667,
MRR 0.714**. The same identifiers pushed through `/search` score 0.524 /
0.587 — this gap is why the MCP tool descriptions and the
[skills](./skills.md) insist on routing by input type.

Read the line as an honest operating point, not a boast: the top search hit
answers about half the questions; the right file is in the top ten far more
often; a few real answers are missed outright. Every consumer of these
answers — tool descriptions, skills, docs — is written to that reality:
scan the list, verify before editing, never report an empty answer as
absence without the [health protocol](./skills.md).

## Determinism

The line above is bit-stable: 16+ consecutive runs of the same binary on
the same corpus produce the identical metrics. That was made true, not
found true — index construction and score tie-breaking are pinned — and it
is what makes `make eval-compare` meaningful: a delta is a change, not a
coin flip.

## What moved the numbers historically

The eval exists to answer "did this help?" — a few answers it has given:

| Change | Effect |
|---|---|
| Symbol ranking tiers (exact-first, generated demoted) | symbol MRR 0.643 → 0.714, misses 7 → 4 |
| Graph-aware search + coverage work (full 103-question set) | recall@1 0.359 → 0.456, MRR 0.442 → 0.514 |
| Indexing pipeline overhaul | full-corpus pass 943 s → 431 s |
| `.gitignore` honoured in the walk | 107,758 → 374 candidate files on this repo |

## Grading answers, not just hits

`make eval-related` scores what `/context` adds around a hit (does the
expansion name the second file the answer needs?), and `make eval-answers`
puts a whole `/context` package in front of a local model and grades its
answer against the pinned ground truth — with the retrieval ceiling
reported separately, so model failures and retrieval failures do not blur.
That harness is the intended test bed for the small-model +
[skills](./skills.md) workflow this project targets.
