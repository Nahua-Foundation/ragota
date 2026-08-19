# Retrieval evaluation

`tools/corpus` answers *how much did the extractor find* — routes, edges,
services. It cannot answer the question that matters to a user: **did the
answer to a question get better?** Every ranking decision in this codebase —
RRF fusion by overlap, reranking after the limit instead of before, symbol
cards, query rewriting — was made without one. This harness is that number.

```sh
make corpus-clone CORPUS_DIR=/data/corpus       # once, ~15 GB
make eval-validate CORPUS_DIR=/data/corpus      # ground truth still true?
make eval          CORPUS_DIR=/data/corpus      # baseline
make eval-compare  CORPUS_DIR=/data/corpus \
     EVAL_ARGS="--a-mode keyword --b-mode hybrid --no-reindex"
```

Two further harnesses score what `make eval` reads past, and neither is on its
path — `make eval` behaves exactly as it always did:

```sh
make eval-related CORPUS_DIR=/data/corpus       # the graph expansion, scored
make eval-answers CORPUS_DIR=/data/corpus \
     EVAL_ARGS="--judge --control"              # what a model answers from it
```

`run.py` builds `cmd/ragota`, starts it on its own SQLite database under
`--work`, indexes only the repositories the selected queries need, asks every
question through `POST /api/v1/search` and `POST /api/v1/context`, and scores
the ranked results. Nothing it touches lives outside `--work`. Python 3
standard library only; the API client and the repository list are reused from
`tools/corpus/corpuslib.py`.

Past [Comparing two runs](#comparing-two-runs) this file stops being a
manual and becomes the measurement journal, in the order the findings were
made — which means a number is current only until a later section moves it.
The newest full-set line is the 103-question tables in
[Across a service boundary, and across a repository](#across-a-service-boundary-and-across-a-repository);
the newest `eval-fast` line is in
[Running this on a laptop](#running-this-on-a-laptop).

## The query set

`queries.jsonl` — 103 questions over 12 corpus repositories, one JSON object
per line, `#` comments and blank lines allowed so a reviewer can navigate it.

| field | meaning |
| --- | --- |
| `id` | unique and stable; result files and A/B deltas are keyed on it |
| `repo` | corpus checkout that holds the answer (`tools/corpus/repos.tsv`) |
| `shape` | `implement`, `callers`, `route`, `rpc`, `topic`, `table` |
| `scope` | `in-repo` (default), `cross-service`, `cross-repo` |
| `repos` | checkouts the question is asked over; absent means the one holding the answer, `"all"` means no repository filter |
| `query` | the question, as a developer would type it |
| `expect_file` | the file that answers it, relative to the repository root |
| `expect_symbol` | the symbol inside it, when there is one |
| `expect_line` | the line the answer starts on |
| `anchor` | exact text that must appear on `expect_line` |
| `alt_files` | other files that also count as correct (usually empty) |
| `expect_related` | the second file a complete answer needs — the far side of the contract the question is about. Not an acceptable answer and not part of any retrieval metric; it is what `related.py` scores the graph expansion against. `repo:path` names another checkout |
| `why` | how the answer was established, with file:line citations |

Coverage, by the shape of question the product exists to answer:

| shape | n | the question |
| --- | --- | --- |
| `route` | 30 | where does `POST /api/orders/cancel` go |
| `callers` | 26 | what calls X / who uses X |
| `implement` | 18 | where is X implemented |
| `table` | 12 | which model maps to table T, where is T written |
| `rpc` | 9 | where is gRPC method M implemented |
| `topic` | 8 | which service publishes/consumes queue Y |

`shape` says what is being asked; `scope` says how far the answer is from the
question, which is a different cut and the one this product exists for:

| scope | n | the answer is |
| --- | --- | --- |
| `in-repo` | 80 | in the same service as the question |
| `cross-service` | 19 | in another service of the same repository |
| `cross-repo` | 4 | in another checkout entirely |

Per repository: boutique 14, eshop 12, robotshop 11, petclinic 10,
elasticsearch 9, conductor 8, airflow 7, grafana 7, medusa 7, argocd 6,
consul 6, jellyfin 6 — six languages (Go, Java, C#, TypeScript, Python, PHP)
and the four contract families the corpus is built around (annotation routing,
explicit routing, gRPC, custom registries).

Roughly a third of the questions deliberately avoid the identifier they are
about ("which allocation decider stops shards from being allocated to nodes
that are low on disk space", "how does the ratings service check that a product
sku exists in the catalogue"). Without those the set would measure exact-token
lookup, which BM25 wins by construction and which is not what a user types.

### What it deliberately does not cover

- **Answer quality.** `run.py` measures whether the file that answers the
  question is retrieved and where it ranks. Whether an LLM handed that context
  writes a correct answer is a different experiment with a different harness —
  `answer.py`, which is off this script's path and has its own section below.
- **Multi-file answers.** Every question has one right file; nine of the 103
  carry a second equally defensible one in `alt_files` (a route's registration
  as well as its handler, a model as well as the code that writes it, a
  contract with exactly two callers). Questions whose honest answer is "these
  nine files together" are not in the set, because scoring them fairly needs
  graded relevance judgements that nobody has written. 57 questions now name
  *one* further file in `expect_related` — the far side of the contract — but
  only the graph expansion is scored against it, never the ranking.
- **Cross-repository breadth.** The corpus contains exactly *one* genuine
  cross-repository contract join (conductor's Elasticsearch client against
  elasticsearch's REST API), and all four `cross-repo` questions are on it.
  Four questions on one contract measure that contract, not the general case;
  the 19 `cross-service` questions carry the weight, and they cross a service
  boundary rather than a checkout boundary.
- **The graph expansion itself.** `/context` returns each hit with its unit,
  service and related units; `run.py` scores only the hits. A wrong or empty
  `related` list costs it nothing — which is why `related.py` exists, below.
- **Recall over the whole corpus.** 103 questions over 12 repositories is a
  sample, not a census. It is large enough that a five-point move is real and
  small enough that a human can read every row. Four corpus repositories are
  not represented at all — nats-server, kafka, umbraco and n8n — so the
  broker and custom-http-helper patterns are measured by `tools/corpus` only.
- **Latency under load.** Per-query wall time is recorded, single-client, warm.

## How the ground truth was established

By reading the corpus source code — never by asking ragota what it
returns. Ground truth derived from the system under test proves nothing: it
would make every change look like an improvement, because the answer key would
move with the index.

For each question the file was located by grep and by reading the surrounding
code, and three things were recorded: the path, the line, and an `anchor` — the
exact text on that line. Registration and definition were checked separately,
so a `route` answer names the function that *handles* the request, with the
line that binds the path cited in `why`:

```
POST /api/annotations/mass-delete
  pkg/api/api.go:523      apiRoute.Post("/annotations/mass-delete", ...)   ← registration
  pkg/api/annotations.go:338  func (hs *HTTPServer) MassDeleteAnnotations  ← the answer
```

For `callers`, symbols with exactly one non-test call site were preferred, so
"what calls X" has a single defensible answer rather than a judgement call.
Test files, mocks, generated code and vendored trees were never used as the
expected answer.

The cross-service and cross-repository questions were established the same way
but from both ends: the contract key was read off the serving side (a route
annotation, an rpc registration, a queue name, a `ToTable`), then every call
site of that key was found and read, and the question was only kept when the
callers were unambiguous. Where a contract has two equally defensible callers
both are recorded; where it has three in three languages, the serving side is
the question and the three are cited in `why`. Load generators and functional
tests call the same contracts and were excluded by hand, which is the same
distinction the product's contract lookup has to make at query time.

The `anchor` is what makes this maintainable. `make eval-validate` re-reads
every `expect_file` in the corpus checkout and checks that the anchor is still
on the recorded line:

```
103 queries over 12 repositories
  by shape: callers=26, implement=18, route=30, rpc=9, table=12, topic=8
  by scope: cross-repo=4, cross-service=19, in-repo=80
every expected file exists and every anchor is on the recorded line.
```

Validation also checks the fields that decide *where* a question is asked,
because those can put the answer out of reach in a way no amount of retrieval
recovers from and that would read as a ranking result: a `repos` list that
names a checkout the corpus does not have, or that leaves out the repository
holding the answer, is an error, as is a `cross-repo` question asked of one
repository.

A moved line is a warning with the new line number; a vanished anchor is an
error, because the recorded claim is no longer true of the code. Corpus
checkouts are `--depth 1` clones of moving branches, so this will fire — it is
how the set is kept honest rather than quietly rotting. Nothing in validation
touches the server or the index.

## The metrics

Every question has one correct file, so this is known-item retrieval and the
metrics are the standard ones for it. Results are chunks, not files, so ranks
are computed over the result list **deduplicated by file, first occurrence
winning** — otherwise a file split into ten chunks would outrank one split into
two for reasons that have nothing to do with relevance.

| metric | definition |
| --- | --- |
| `recall@k` | 1 if an acceptable file is in the top *k*, else 0, averaged over the set. With one gold document this is the hit rate: the fraction of questions whose answer was retrieved at all. k = 1, 3, 5, 10, 20. |
| `mrr` | mean of 1/(rank of the first acceptable file); 0 when it is never retrieved. |
| `ndcg@10` | binary gain over the acceptable set, discounted by 1/log2(rank+1), normalised by the ideal ordering of those same files. |
| `span@10` | fraction of questions where a chunk in the top 10 actually *covers* `expect_line`. A file-level hit 900 lines away is still a hit for recall; this says whether the retrieved text contains the answer. |
| `missed` | queries whose answer never appeared, at any rank. |

All of them are reported per shape, per repository and in total, for `/search`
and for `/context` separately, plus a per-query table (and a `.tsv`) so a
number that moved can be traced to the question that moved it.

## Running this on a laptop

A full run indexes twelve repositories, three of which are four fifths of the
corpus by size, and a vector run embeds every chunk of them. That is an hour
of wall time and several gigabytes of resident memory before the first
question is asked. Two things make it fit:

```sh
make eval-fast CORPUS_DIR=/data/corpus     # six repositories, in-repo only
```

How much that buys is worth stating, because it decides how often the harness
is worth running at all. The twelve-repository corpus takes 528 s to index with
AST and BM25, and one repository — elasticsearch — is 271 s of it; the three
heaviest are 84% of the indexing time and 22% of the questions. The six
`eval-fast` repositories index in **10 s**, and the whole run, questions
included, finishes in 17 s. That is the difference between a check you run
after every change and one you schedule.

What it costs is coverage: 40 of the 103 questions, and the six easiest
repositories, so its numbers are **not comparable with the full-corpus tables
below** — a higher recall here means the questions are easier, not that
anything improved. It is a regression check against itself. Measured on
2026-08-16, binary `dev (4deada3)`, base/keyword, 0 request errors:

| /search, base/keyword, eval-fast | n | recall@1 | recall@5 | recall@10 | MRR | span@10 | never found |
| --- | --- | --- | --- | --- | --- | --- | --- |
| **total** | **40** | **0.500** | **0.675** | **0.750** | **0.577** | **0.550** | **8** |

`/context` returns the same ranks at every k on this set; only its span differs
(0.475), which is the graph expansion changing which chunk covers the line
rather than which file is found.

### Two things that made indexing slower than the work in it

Both were found by reading the per-pass line the server logs
(`index repo … indexers="ast=81s bm25=39s" total_sec=147`) rather than by
guessing, and both are measured on this corpus with the keyword configuration.

**The layout was settled once per repository instead of once.** Compaction
rewrites the whole keyword index, so its cost is the size of everything indexed
so far, not of what the pass added. Indexing boutique — 230 files — after
elasticsearch was in the index took 7 s, of which **6.75 s was that rewrite**
and 0 s was indexing boutique:

```
compacted index  indexer=bm25  took_ms=6753
index repo boutique  indexers="ast=0s bm25=0s"  total_sec=7
```

Twelve repositories paid that twelve times to arrive where the last pass would
have arrived alone. The harness now indexes with `no_compact` and asks for one
compaction when everything is in (`POST /api/v1/admin/compact`). The final
layout is the same, which is what scores depend on — `eval-fast` returns the
same rank for every one of its 40 questions, and its indexing drops from 10.0 s
to 9.0 s (six compactions become one; on a corpus holding elasticsearch the
same change is worth seconds per repository).

That equality is the point of the check. The first attempt at this called the
compaction from the wrong branch, so nothing was compacted at all — and the
timing looked *better*. What caught it was the scores: MRR 0.577 → 0.571 and one
more question never found, which is the segment-layout dependence this whole
mechanism exists to remove.

**Test code costs three times what its file count suggests.** elasticsearch is
40 079 indexed files, 13 504 of them tests. Excluding them (`--exclude-tests`):

| elasticsearch, one pass | files | ast | total | local edges linked |
| --- | --- | --- | --- | --- |
| everything | 40 079 | 81 s | 147 s | 2 653 866 |
| tests excluded | 28 109 | 25 s | **50 s** | 769 671 |

Thirty per cent fewer files, two thirds less time: test code carries most of the
call edges, and resolving them is what the linker spends its pass on. **No
question in the set expects a test file as its answer** — that was checked, all
103 of them — and on `eval-fast` excluding tests moved recall@1 0.500 → 0.525
and MRR 0.577 → 0.591, which is one question finding its answer and is reported
as such rather than as an improvement. It stays opt-in: dropping those files
changes the keyword corpus and with it every score, so a run using the flag
cannot be read next to one that does not.

and knowing where the memory actually goes, which is not where it looks:

- **The Qdrant collections outlive their runs.** A run's collection is part of
  its state and is now dropped when its workdir is, but collections created
  before that fix — or by a run given `--keep` — stay. Eighteen of them held
  2.7 GB inside the container on the machine this was written on, every one
  belonging to an experiment finished days earlier. `curl
  localhost:6333/collections` lists them; deleting the ones you cannot name is
  safe.
- **A filtered run can still index a repository you did not name.** The
  cross-repository questions are asked over two checkouts, so `--repo
  conductor` pulls in elasticsearch and turns a five-minute run into an hour.
  The harness now prints which extra checkouts a selection needs before it
  indexes anything; `--scope in-repo` avoids them entirely.
- **Each run holds a server process with its indexes resident.** Two
  comparison sides run sequentially, but an abandoned run leaves its server
  alive — `pgrep -f ragota` after a cancelled harness is worth a look.

## Comparing two runs

`compare.py` runs the same set twice and prints the delta per shape and the
list of queries whose rank changed, worst regression first. An aggregate that
did not move can still hide five improvements and five regressions, which is
why the per-query table is the point.

```sh
# search modes (one index, two query-time settings)
tools/eval/compare.py --corpus /data/corpus --a-mode keyword --b-mode hybrid --no-reindex

# reranker on/off — needs a rerank service (TEI, vLLM, Cohere, Jina)
tools/eval/compare.py --corpus /data/corpus --b-variant rerank \
    --rerank-url http://localhost:8090 --rerank-model BAAI/bge-reranker-v2-m3

# window chunks vs symbol cards — needs qdrant and an embedder
tools/eval/compare.py --corpus /data/corpus --a-variant window --b-variant cards \
    --qdrant-url http://localhost:6333 --embed-model nomic-embed-text --a-mode hybrid --b-mode hybrid

# query rewriting on/off — needs an assistant LLM
tools/eval/compare.py --corpus /data/corpus --b-variant rewrite \
    --assistant-url http://localhost:11434 --assistant-model qwen2.5

# before/after of a code change
tools/eval/compare.py --corpus /data/corpus --a-binary /tmp/before --b-binary /tmp/after

# two runs that already happened
tools/eval/compare.py --a-results base.json --b-results rerank.json
```

The variants are config overlays (`evallib.VARIANTS`), so a comparison is one
flag rather than a hand-edited YAML file nobody can reproduce next month. A
variant whose service is unreachable fails at startup instead of silently
measuring the base configuration twice. `--no-reindex` shares one indexed
database between the two sides and is only valid when they differ at query
time; `window` vs `cards` changes what is indexed and must not use it.

A worked example, 52 queries over the eight repositories that index in under a
minute, one index shared by both sides:

```
A = base/keyword   B = base/hybrid

/search    n   A recall@10  B recall@10  delta  A mrr  B mrr  delta
callers     8        0.125        0.125      .  0.042  0.042      .
implement  11        0.545        0.545      .  0.324  0.324      .
route      15        0.600        0.600      .  0.285  0.285      .
rpc         7        0.286        0.286      .  0.076  0.076      .
table       6        0.833        0.833      .  0.267  0.267      .
topic       5        0.800        0.800      .  0.383  0.383      .
-- total   52        0.519        0.519      .  0.235  0.235      .

/search: no query changed rank.
```

Not a null result: it is the harness saying that on a deployment without a
vector index, `mode: hybrid` is `mode: keyword` — RRF over a single searcher
returns that searcher's order, for all 52 questions, at every rank. Anyone
choosing `hybrid` for a BM25-only install is paying nothing and getting
nothing, and that is now checkable rather than arguable.

## The 80-question baseline (2026-08-12)

> Superseded on 2026-08-13, when the query set grew to 103 — the current
> full-set line is in [Across a service boundary, and across a
> repository](#across-a-service-boundary-and-across-a-repository). Kept
> because the sections after it compare against these numbers.

Measured on 2026-08-12 on an Apple-silicon laptop, binary
`ragota dev (b76eb21) darwin/arm64 go1.26.5` built from `cmd/ragota` on
`feat/graph-search-and-vector-eval` — the first run with the zapx chunk-table
fix and the graph-backed callers intent in the binary.

```sh
tools/eval/run.py --corpus <corpus> --work <work> --mode keyword --keep --misses
```

Configuration: AST + BM25 indexers over SQLite, `indexes.workers: 4`, **no
vector index, no reranker, no assistant** — the setup a developer gets from a
plain `make run`. Search mode `keyword`, `--limit 20`, `/context` with
`limit 20, hops 1`. 80 queries, 12 repositories, indexed in 760 s (the machine
was concurrently embedding the vector-variant index; the 2026-08-11 run did it
in 530 s solo); 0 request errors, no failed repositories — the first full
12-repo index-and-merge pass since the zapx fix, the exact workload that
previously corrupted two runs out of three. Median query latency 12 ms
(`/search`) and 37 ms (`/context`).

`/search`, by question shape:

| shape | n | recall@1 | recall@5 | recall@10 | recall@20 | MRR | nDCG@10 | span@10 | never found |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| callers | 15 | 0.133 | 0.267 | 0.267 | 0.400 | 0.197 | 0.209 | 0.200 | 9 |
| implement | 18 | 0.444 | 0.667 | 0.667 | 0.722 | 0.533 | 0.562 | 0.389 | 5 |
| route | 24 | 0.083 | 0.458 | 0.667 | 0.750 | 0.242 | 0.322 | 0.500 | 6 |
| rpc | 7 | 0.143 | 0.286 | 0.429 | 0.429 | 0.196 | 0.221 | 0.429 | 4 |
| table | 11 | 0.273 | 0.455 | 0.727 | 0.727 | 0.385 | 0.470 | 0.545 | 3 |
| topic | 5 | 0.600 | 0.600 | 0.800 | 0.800 | 0.629 | 0.641 | 0.600 | 1 |
| **total** | **80** | **0.237** | **0.463** | **0.588** | **0.650** | **0.339** | **0.386** | **0.425** | **28** |

Against the 2026-08-11 baseline (`a00963c`): total recall@10 0.537 → 0.588,
MRR 0.280 → 0.339, recall@1 0.175 → 0.237, unanswered 32 → 28. The movement is
where the graph intent applies: `callers` 0.067 → 0.267 at ten (and 0.133 →
0.400 at twenty) with MRR 0.026 → 0.197; `topic` MRR 0.351 → 0.629 (the
publisher's call site now leads instead of trailing); `table` 0.636 → 0.727.
`implement` and `route` are flat — no intent fires on them, which is the
detector doing its job. The three shifts share one cause: search now resolves
what the question is about and promotes the incoming call, rpc, http, topic
and table edges' call sites ahead of the text ranking.

`/context` now matches `/search` per shape at k ≤ 10 and is slightly better at
20 (0.675 vs 0.650); its `span@10` stays lower (0.375 vs 0.425) — unit
deduplication sometimes keeps a different chunk of the right file than the one
containing the answer.

Per repository, `/search` recall@10, best first: robotshop 0.857, consul
0.833, airflow 0.714, jellyfin 0.667, petclinic 0.571, grafana 0.571, eshop
0.571, boutique 0.571, conductor 0.500, argocd 0.500, elasticsearch 0.429,
medusa 0.286. Repository size still does not explain the spread: the two
largest checkouts (elasticsearch, medusa) trail, but grafana (≈15 k source
files) climbed to mid-table on the strength of the graph answers.

What this run says:

1. **"What calls X" went from broken to half-answered, and the remaining half
   is named.** 9 of 15 `callers` questions still never return the calling
   file. The recovered six come straight from the graph (four in the top ten,
   two at rank 1). Of the nine misses, the reproducible causes are extractor
   gaps, not ranking: robotshop's PHP client concatenates its URL so no
   `http_call` edge exists to promote, and medusa's workflow steps compose at
   runtime so the TS pass has no call edge either; several others describe the
   callee by synonyms ("fetch an owner" for `getOwner`), which exact-name and
   BM25 retrieval both miss — the vector index's case.
2. **Prose still outranks code.** For 12 of the 28 unanswered questions the
   top hit is documentation, config or data — `README.md`, `llms-full.txt`,
   `package.json`, generated OAS pages. Documentation repeats the vocabulary
   of the question, implementations do not, and BM25 has no reason to prefer
   the code. (Run `--misses` to see this for the whole set.)
3. **The right file at the wrong place is still a quarter of the wins.** 47 of
   80 questions have the answering file in the top ten, but only 34 have a
   chunk covering the answer's line (span@10 0.425). Callers hits point at the
   call-site line itself, which is why their span now tracks their recall;
   for text hits this remains the chunking signal it was.

### The vector index, measured

The `window` variant — Qdrant + bge-small-en-v1.5 (384d) with the eval's
`VECTOR_EXCLUDES` (tests, vendored trees, generated doc sites kept out of the
embedding channel) — against this baseline's keyword mode. Same binary, all
80 queries, all 12 repositories, zero errors:

| /search, 80 q | keyword | hybrid | delta |
| --- | --- | --- | --- |
| recall@5 | 0.463 | 0.550 | +0.088 |
| recall@10 | 0.588 | 0.675 | +0.088 |
| recall@20 | 0.650 | 0.738 | +0.088 |
| MRR | 0.339 | 0.376 | +0.037 |
| span@10 | 0.425 | 0.475 | +0.050 |
| never found | 28 | 21 | −7 |

Every shape but one gains: `callers` 0.267 → 0.467 (with the graph promotion
this puts the answer in the top ten for 7 of 15 questions that started the
day at 1 of 15), `table` 0.727 → 0.909, `rpc` 0.429 → 0.571, `implement` and
`route` up a step; `topic` gives back one query of five. The repositories
that move are the ones that were failing: medusa 0.286 → 0.571, argocd and
conductor +0.333 each, grafana and petclinic +0.143. Elasticsearch alone
stays at 0.429 — its misses are vocabulary-free questions ("which allocation
decider stops shards…") that neither channel reaches; that is reranker and
query-rewrite territory.

Two runs earlier, a mid-size model over the unfiltered corpus (nomic-768, no
exclusions) scored **0.644** recall@10 on the 59-query subset it covered;
this configuration scores **0.712** on the same 59. A model a quarter the
size, embedding less than half the text, retrieves better — the corpus you
embed matters more than the megabytes of the model embedding it. (The two
knobs changed together, so their individual shares are unmeasured; the pair
is the recommended configuration.)

Getting this number surfaced four product defects, each now fixed and each
invisible without the harness:

1. **The default fusion weights disabled BM25.** With k=60 and 20 candidates
   a side, weighting BM25 at 0.7 put its entire contribution range under the
   vector side's last rank: every vector hit outscored every unshared keyword
   hit, "hybrid" was vector-only, and keyword's rank-1 answers vanished from
   the top twenty. Defaults are now equal (plain RRF); weighting remains a
   config choice.
2. **Embedder input was unbounded.** Sixty minified lines can be megabytes; a
   strict server (llama.cpp) rejects the whole batch, one data file failed
   its repository, and `status=error` blocked `--reuse`. Chunks are truncated
   to `embedder.max_chars` for the embedder call only.
3. **Bytes cannot guarantee tokens.** Code runs ~4 bytes/token, Arabic
   localization ~2, a JSON of floats under 2 — every fixed byte cap left some
   file over a 2048-token serving context. The indexer now halves the budget
   and retries before failing that one file.
4. **Qdrant answered filtered deletes with full scans.** The per-file
   delete-before-upsert had no payload index to use, went quadratic in
   corpus size, and throttled the whole pipeline to 5 points/s at 120k
   points; the collection now carries keyword payload indexes for repo_id,
   file_path and language (5 → 100 points/s live, one core freed).

Practicalities for whoever repeats this, in the order they were learned: CPU
inference cannot embed this corpus (0.4 chunks/s in Docker ≈ days); llama.cpp
on Metal reaches ~9 texts/s and does not scale with parallel requests; torch
fp16 on MPS reaches ~15, but only if every GPU call runs on one dedicated
thread; padding dominates mixed-length batches (sort by length, sub-batch
small); a 33M model embeds ~5× faster than a 137M one and, on the filtered
corpus, retrieves better. End to end the full run costs ~50 minutes on an
M-series laptop; the same job on a GPU host (deploy/docker-compose.models.yml)
is minutes.

### The rest of the pipeline, measured

Every remaining stage that was built but never measured, on one index per
configuration and one query set. The 59 questions are the nine repositories
that index in ten minutes; `cards` changes what is indexed, so it cannot share
an index with `window` and the subset is what keeps all five rows comparable.

| /search, 59 q | recall@1 | recall@10 | MRR | span@10 | never found |
| --- | --- | --- | --- | --- | --- |
| keyword | 0.254 | 0.644 | 0.367 | 0.475 | 18 |
| + vectors (window chunks) | 0.237 | 0.712 | 0.379 | 0.542 | 12 |
| + symbol cards instead of windows | 0.237 | 0.780 | 0.403 | 0.661 | 9 |
| window + reranker | 0.356 | 0.831 | 0.496 | 0.644 | 10 |
| **cards + reranker** | **0.441** | **0.847** | **0.588** | **0.695** | **9** |

On the full 80 queries the reranker over window chunks gives recall@10 0.787
(from 0.675), MRR 0.447, span@10 0.600, 17 never found.

- **The reranker is the largest single lever measured so far** — bigger than
  the vector index it reorders. It also lifts `span@10` by more than it lifts
  `recall@10`, which means a cross-encoder picks the *chunk containing the
  answer* out of a file's chunks: part of the chunking problem is a ranking
  problem. It costs the request: median /search goes from 13 ms to 629 ms
  (856 ms with cards) for 50 candidates through a 278M cross-encoder on a
  laptop GPU, so `top_n` is the dial between the two.
- **Symbol cards beat line windows on every shape but one** and index *faster*
  (fewer, more meaningful documents): `route` 0.765 → 0.941, `implement`
  0.750 → 0.833, `callers` 0.556 → 0.667, span@10 +0.119. Only `rpc` gives
  one query back. Cards and the reranker are independent and stack.
- **Query rewriting is a regression and should stay off**: /context recall@10
  0.613 → 0.512, `table` −0.273, `callers` −0.200 (it never touches /search,
  which is the only reason the totals there did not move). Two distinct
  failures, both visible in the rewrites: the model drops the one literal
  token that could match ("rows inserted into the **login_attempt** table" →
  "SQL, MySQL, ORM"), and it destroys the interrogative phrasing the callers
  intent is detected from ("what calls the argo helper…" → "calls ArgoHelper
  to validate application repository"), so a rank-1 graph answer becomes a
  miss. Intent must be resolved on the user's own words, before any rewrite;
  and an LLM should *add* to the query rather than replace it — or write into
  the index instead, where the vocabulary gap is one-time rather than
  per-request.

### Reading the question against the graph, not the graph against the question

Two defects, both found by asking the index why a question failed rather than
by reasoning about what might help, and both about the *same* mistake in
opposite directions.

**A name is not a callee.** "What calls the shipping service ShipOrder rpc"
returned nothing while the answering edge sat in the graph, resolved. A
vendored `.proto` puts one method into every service's generated package —
17 units named `ShipOrder` in boutique — the linker resolves the call edge to
exactly one of them, and a lookup that took the first five units by name
asked the wrong five. Units sharing a qualified name are one callee with
several ids: `callers` recall@10 0.778 → 0.889, MRR 0.541 → 0.652.

**A question does not have to carry the key.** Three rpc questions named
their contract in ways no pattern extracts — service and method with no slash
("the ApplicationService Sync grpc method"), method alone, service alone —
while the `implements_rpc` edges carried the key all along, argocd's on
exactly the expected line. So the match runs from the edges to the question:
read the repository's implementation edges, keep the ones whose key the
question describes by word component. `rpc` recall@10 0.571 → **1.000**,
MRR 0.351 → 0.708.

The scaffolding has to be dropped from both sides or the comparison is
asymmetric: "the grpc shipping service" leaves "shipping", while the key
`ShippingService` still carries the component "service". And the lookup must
not fire for a callers question — the same contract, the opposite side of it.

| /search, 59 q | this morning | now |
| --- | --- | --- |
| recall@1 | 0.254 | **0.559** |
| recall@10 | 0.644 | **0.932** |
| MRR | 0.367 | **0.680** |
| span@10 | 0.475 | **0.746** |
| never found | 18 | **4** |

Per shape: `route` 1.000, `rpc` 1.000, `implement` 0.917, `callers` 0.889,
`table` 0.889, `topic` 0.800. The four that remain are not ranking problems —
a table named only in prose, a consumer whose handler is registered through a
generic dispatcher, a PHP client that concatenates its URL, and a jellyfin
method whose file the retrieval never surfaces.

### Where an LLM does not belong

Two ways of spending an LLM on retrieval were built and measured, and both
lost. Together they say something more useful than either alone.

**Query rewriting** (rephrase the question before retrieving): /context
recall@10 0.613 → 0.512. It drops the literal token that would have matched
and destroys the phrasing intent detection reads.

**Symbol summaries** (one LLM line per symbol, written at index time and
embedded with it, scoped by the graph to the 2 118 contract-boundary symbols
out of 146 000): nothing. Measured twice with qwen2.5:1.5b — 1 628 summaries
over the nine small repositories (recall@1 0.508 → 0.492, MRR 0.621 → 0.610,
+24 min of indexing) and 1 217 over elasticsearch, grafana and medusa, where
the vocabulary-gap questions actually live (every metric identical to three
decimals, MRR −0.002, the same four questions unanswered). The summaries are
not bad — "marks indices as frozen based on cluster state metadata" is a fair
description of a method called `frozenIndices` — they simply change no
ranking.

The common explanation is the reranker. A cross-encoder is *already* the
vocabulary bridge, and a better one than either of these, because it reads
the question and the candidate together at query time. A rewrite guesses
which words will retrieve; a summary guesses which question will be asked;
the reranker has to guess nothing. And by the time it landed, the questions
these two were designed for were largely answered anyway: on the three big
repositories the unanswered vocabulary questions had already gone from nine
to four without either.

The place an LLM has earned so far is where the graph is uncertain rather
than where the text is thin: disambiguating an edge with several plausible
destinations, and pre-index recon. Both are index-time decisions with a
verifiable right answer — unlike guessing at a future question.

### Questions that carry their own answer

A question naming a contract contains the key the linker already built for
it: "where does POST /orders/cancel go" is `http:POST /orders/cancel`, "the
login_attempt table" is `db:login_attempt`. Resolving the key and putting the
graph's answer first, instead of ranking text that merely shares the words:

| /search, 59 q | ranking only | + contract lookup |
| --- | --- | --- |
| recall@1 | 0.458 | **0.508** |
| recall@10 | 0.847 | **0.864** |
| MRR | 0.578 | **0.621** |
| never found | 9 | **8** |

Per shape, the movement is where the keys are: `route` recall@10 0.941 →
**1.000** (one question went from never found to rank 1, and recall@1 0.471 →
0.529), `table` recall@1 0.556 → **0.778**. Nothing regressed.

The two guards this needed were found by measuring, not by thinking. A load
generator posting to `/cart/checkout` and a functional test calling `PUT
/api/orders/cancel` match the key exactly and answer a different question:
promoting them put `locustfile.py` and `OrderingApiTests.cs` above the
handler. So the far side of a contract is never promoted, and neither is
anything under a test path — putting a hit ahead of the ranked results
asserts that it *is* the answer, which is a much stronger claim than ranking
it highly.

### One thing the reranker must not touch

With retrieval and ranking split into separate stages, the code graph's call
sites can join the candidate list *before* the rerank stage instead of being
placed in front of a finished result — letting the cross-encoder arbitrate
between "the graph says this line calls X" and "this text looks like the
question". Measured, that is worse on exactly the shape the feature exists
for: `callers` recall@10 0.778 → 0.667, MRR 0.556 → 0.370, and one answer
that was rank 1 disappeared from the results entirely.

The reason is not that the reranker is weak — it is the largest lever in the
table above. It is that a cross-encoder scores a call site by how much its
text resembles the question, while "X is called here" is a structural fact
that no phrasing makes more or less true. Asked to judge something it cannot
verify, it demotes correct answers. **What the graph knows, ranking cannot
improve on**; the reranker's job starts where the graph's certainty ends.

Three methodological notes from the same experiment:

- The rerank service returns **bit-identical scores for identical input**, so
  a rank change between two runs is a behaviour change, never noise. This was
  checked directly, and it is what made the finding above readable: six
  queries moved between two runs that were supposed to be equivalent, and
  every one of them turned out to be a query where the intent detector fires.
- Differences of a single query (±0.017 at n = 59) are *not* noise here, but
  they are also not evidence. The `top_n` table's fine distinctions (25 vs 50
  differ by 0.008 MRR) are in that range; its coarse ones (off vs 25, 0.403 →
  0.580 MRR) are not.
- The sentence above — that a rank change is never noise — held only because
  those runs shared an index. A comparison that **re-indexes** used to carry a
  per-query noise floor of its own: two builds of the same sources with the
  same binary scored them differently, and `petclinic-visits-table` ("which
  entity maps to the visits table") moved between rank 1 and rank 3 across
  runs, `Visit.java` trading places with the two `schema.sql` files. The same
  comparison with `--no-reindex` moved nothing. This is fixed rather than
  quantified; the next section says how.

The nomic run's 59-query slice remains in `window-hybrid-part1d.json` for
model comparisons.

### Why two builds of the same sources used to disagree

The instability was in index construction, and it was not the ranker's
tie-break (`internal/search/hybrid.go` `sortHits` already orders by score, then
repo, path and line). The scores themselves differed, and they differed for a
reason that has nothing to do with the corpus:

- Bleve scores BM25 against an average document length it derives as
  `ceil(FieldCardinality(field) / DocCount())` — v2.6.0,
  `search/searcher/search_term.go`, `bm25ScoreMetrics`.
- `DocCount` is the live document count and depends on nothing else.
  `FieldCardinality` is a **sum over segments** of each segment's distinct-term
  count (`index/scorch/snapshot_index.go`, `newIndexSnapshotFieldDict`; zapx's
  `Dictionary.Cardinality()` is `fst.Len()`). A term present in eight segments
  is counted eight times, so the number describes how the documents were split
  into segments at least as much as it describes the corpus.
- That average appears in both the IDF and the length-normalising denominator,
  so **every score in the index moves with it**.

Segment layout is not a property of the input. Scorch persists and merges on
its own goroutines, so two passes over the same files end differently, and the
scores of an index nobody is writing to change underneath a reader. Measured on
this repository's own sources (166 files, 1067 chunks), `bm25 keyword search`
scored 0.872 with two segments and **0.757 a tenth of a second later**, after
the background merger reached one — a 13% move with no write in between.
Indexed in windows of 512, 128 and 32 files, the same 1065 chunks gave term
counts of 16677, 19655 and 20295, average document lengths of 16, 19 and 20,
and top scores spread over 20%.

The `ceil` is what makes the symptom intermittent rather than constant: the
average is a whole number, so a layout difference either lands in the same
rounded band and changes nothing, or crosses into the next one and moves every
score at once. Most runs agree exactly; the ones that do not disagree about
everything, which is why a near-tie like `petclinic-visits-table` flips between
rank 1 and rank 3 instead of jittering.

**The fix.** A full index pass now ends by merging the BM25 index into a single
segment (`bm25.Indexer.Compact`, driven by `Service.compactIndexes`). One
segment is one segment however it was reached, and its dictionary holds each
term once, so the count becomes the corpus's own distinct-term count — a
function of the content and nothing else. Four different batchings of the same
600 documents report term counts that disagree before compaction — 2059, 2318,
2297 and 2621 on one run, and different numbers on the next, which is the bug
stated as a measurement — and exactly 2059 with identical per-chunk scores
after (`internal/index/bm25/compact_test.go`). Deleting a repository compacts
too: its documents stop counting the moment they are removed, but the words
they contributed stay in the term dictionary until the segments holding them
are rewritten.

Compacting also has to reach disk before the pass is called done, which is a
second deadline and not the same one. Bleve's `ForceMerge` returns once the
merged segment is in the root snapshot — enough for every query this process
will serve, and not enough for the next one: writing that root is the
persister's own goroutine's job, and a close before it gets there leaves the
last recorded snapshot pointing at the segments the merge replaced. Reopening
then reads back the layout that was just compacted away, along with the scores
it produces; this was measured at 1689 terms against the 1459 the compacted
index reported. `Compact` now waits for bleve's `index_bgthreads_active` to go
false, which is the merger and the persister both caught up.

Compaction costs about 4% of the pass that filled the index (180 ms against
4.8 s for 8536 chunks) and returns in microseconds when the index is already
one segment. It transiently needs room for a second copy of the index;
`indexes.bm25.no_compact` turns it off, at the price of the drift above.

A forced pass over an index that already holds the repository ends where a
fresh build on an empty directory ends — same term count, same score for every
chunk — even though it gets there through a layout a fresh build never has
(rewritten chunks in new segments, the ones they replaced left behind as
deletes). That is the shape of every `run.py` invocation, and it is what makes
a re-indexing A/B comparable to a `--no-reindex` one.

Two residues worth knowing before reading a re-indexing A/B as exact:

- **Incremental commit passes do not compact.** Rewriting the whole index for a
  handful of changed files is not worth it, so a repository kept current by
  `apply commits` alone is only as reproducible as its last full pass. Every
  `run.py` and `compare.py` invocation does a full pass, so the harness is
  unaffected.
- **Exact score ties are returned in segment order** by the keyword leg —
  bleve's internal document order, which is the order the indexing goroutines
  wrote the documents in. `search.settleSearcherOrder` now re-sorts each
  searcher's hits by score, repo, path and line *before* anything reads their
  positions, so the tie no longer decides the ranking. What is left is the
  searcher's own cut: the sort runs after bleve has already truncated to
  `candidateQuery`'s limit, so a tie straddling that boundary can still put a
  different member of it on the page. Nothing in this baseline does, and the
  window is 50 candidates deep against a limit of 10 or 20.

  The claim this bullet used to make — that real prose and code tie exactly only
  rarely, and that only the synthetic corpus in `compact_test.go` did — was
  wrong, and it is why the tie was left unsettled. In boutique alone,
  `src/frontend/rpc.go` ties `src/frontend/handlers.go` and the four vendored
  copies of `demo.proto` tie each other, on an ordinary question.

### The second cause: what the row's id decided

Compaction settled the scores and the harness still disagreed with itself.
Eight runs of identical code over an identical corpus, fresh work directory
each time, split six/two:

```
recall@1 0.5238  MRR 0.5894  missed 5    x6
recall@1 0.4762  MRR 0.5577  missed 5    x2
```

One question moved: `petclinic-visits-table`, between rank 1 and rank 3. It is
not scored by BM25 at all — it is a `table` question answered by promoting the
graph's declarations of `db:visits` ahead of the text hits — so no amount of
score stability could have reached it.

Three units declare that key: the `@Table(name = "visits")` entity and the
hsqldb and mysql `schema.sql`. They tie on every term the unit ranking has an
opinion about — same repository, same path penalty (none is generated or test
code), same `LENGTH(name)`, all three named `visits` — so the ranking fell
through to its last term, `id`. That is the autoincrement row key, handed out
in the order the `indexes.workers` goroutines committed rows, and comparing the
two databases directly shows it is the whole story:

```
run A   Visit.java 5959   hsqldb 6050   mysql 6055   -> rank 1
run B   hsqldb 5571   mysql 5576   Visit.java 7018   -> rank 3
```

Identical row counts, identical rows, different ids. `ORDER BY ..., id` is
stable within one database and says nothing across two.

**The fix** is that no ordering ends on a row id. `storage.UnitTieBreak` ends
the unit ranking on where a unit is — repository, path, position in the file,
then kind, name and qualified name — and `storage.EdgeOrder` does the same for
edges, which mattered more than it looks: the promoters read the first 50 edges
of a contract key and the first 2000 client-side edges of a repository, so
insertion order chose the *set* that reached ranking and not merely its
arrangement. `internal/graph`'s linker had the same defect one layer down, where
it is worse — `sortUnits` and `routeCandidates` broke ties on `unit.ID` and the
winner is written back to `edges.dst_id`, outliving the pass that chose it.

Nothing about scoring or index content changed, and the ordering the fix
settles on is the one the majority of runs already produced: ten consecutive
runs are identical to each other and per-query identical to a pre-fix run that
landed on 0.5238 / 0.5894. The conformance suite's `InsertionOrder` subtest
stores the same rows in two orders under two repositories and demands the same
answer, with and without a limit, so this cannot come back unnoticed on either
backend.

Widening the same three repositories from the 21 `in-repo` questions to all 35
found a **third** cause, which the id fix does not touch and the bullet above
now describes: `fuseRRF` scores a hit by its *position* in the searcher's list,
so a BM25 score tie is converted into a difference in the fused score before
`sortHits` — which runs after fusion — can settle it. It moved nDCG@10 by 0.001
and one question's span rank between 8 and 9. Both halves are needed: the id
fix alone leaves the 35-question slice unstable, and this one alone leaves
`petclinic-visits-table` flipping.

One intermittent failure showed up while producing this baseline and is worth
recording: on two earlier runs, `/api/v1/search` returned HTTP 500 with
`search: error reading frequency: memUvarintReader overflow` for whole
repositories (conductor once on a freshly built index; grafana and medusa after
a second server process indexed two more repositories into the same on-disk
BM25 index). The run above is clean. Because a failed request scores zero on
every metric, `run.py` and `compare.py` now print failed repositories
separately — a broken index must never be read as a ranking result.

That failure has since been traced to its root cause: zapx v17.1.2 (the segment
writer bleve v2.6.0 pins) corrupts the postings chunk-offset table of any term
whose postings skip a full chunk mid-range — for this corpus, repo_id and
language terms in segments merged across repositories. The visible overflow was
the rare case; most affected terms silently return wrong results. The fix
shipped upstream in zapx v17.1.9, which go.mod now requires directly (an
interim local patch under `third_party/zapx-v17` carried the same fix until
then); the regression tests live in `tools/zapcheck/zapverify`, and `tools/zapcheck`
verifies an index on disk. Indexes written before the fix remain corrupt and need a forced
reindex; a clean baseline should be re-measured after rebuilding them, since
"the run above is clean" only means the queries asked did not cross a poisoned
chunk boundary.

### Across a service boundary, and across a repository

Every number above is a question asked and answered inside one service. The
reason this product exists is the other kind — follow a route, an rpc, a queue
or a table from the code that uses it to the code that serves it, across a
service boundary and, where there is one, across a repository boundary — and
nothing measured that.

23 questions were added, established the same way as the first 80 (by reading
the corpus, never by asking the system) but from both ends of each contract:
the key was read off the serving side, every call site of that key was found
and read, and a question was kept only when the answer was unambiguous.

- **19 `cross-service`** — one repository, several services, usually a
  different language at each end. boutique 7 (a Go frontend into Java, C# and
  Python services, and a Python recommender into a Go catalog), robotshop 4
  (Java→Node, Python→Node, and one route that three services in three
  languages call), eshop 5 (a gRPC client, three RabbitMQ integration events,
  and a table one service reads out from under another), petclinic 3.
- **4 `cross-repo`** — conductor's `es6-persistence` is a client of
  elasticsearch's REST API, and the two are separate checkouts. That is the
  only genuine cross-repository contract join this corpus contains. One of the
  four is asked with no repository filter at all — "which repository serves
  `GET /_cluster/health`" — against all twelve.

Scoring them needed two changes to the harness, neither of which may move an
existing question. A query now carries `repos`, so it can be asked of several
checkouts or of everything; and a result is identified by **repository plus
path** rather than path alone, because `src/main.go` in the service that calls
a contract and `src/main.go` in the one that serves it are different answers.
With one repository per question the second is a relabelling, and it is: the
80 original questions score identically on every field — rank, span, MRR,
nDCG, recall at every k, and the returned file list itself — before and after.

Measured on 2026-08-13, binary built from `cmd/ragota` at `c882882`, the same
configuration as the baseline above (AST + BM25 over SQLite, no vector index,
no reranker, no assistant), 12 repositories indexed in 943 s, 0 request
errors, median `/search` 8 ms:

| /search, base/keyword | n | recall@1 | recall@5 | recall@10 | MRR | span@10 | never found |
| --- | --- | --- | --- | --- | --- | --- | --- |
| in-repo | 80 | 0.388 | 0.575 | 0.662 | 0.471 | 0.475 | 23 |
| cross-service | 19 | 0.158 | 0.421 | 0.474 | 0.256 | 0.368 | 8 |
| cross-repo | 4 | 0.750 | 0.750 | 0.750 | 0.750 | 0.500 | 1 |
| **total** | **103** | **0.359** | **0.553** | **0.631** | **0.442** | **0.456** | **32** |

The same index and the same candidates with the reranker in front of them
(`top_n` 25, median 820 ms):

| /search, +rerank | n | recall@1 | recall@5 | recall@10 | MRR | span@10 | never found |
| --- | --- | --- | --- | --- | --- | --- | --- |
| in-repo | 80 | 0.388 | 0.613 | 0.688 | 0.475 | 0.512 | 22 |
| cross-service | 19 | 0.368 | 0.632 | 0.632 | 0.487 | 0.526 | 7 |
| cross-repo | 4 | 0.750 | 0.750 | 0.750 | 0.750 | 0.500 | 1 |
| **total** | **103** | **0.398** | **0.621** | **0.680** | **0.488** | **0.515** | **30** |

What this says:

1. **Most of the cross-service deficit is ranking, and the reranker takes it
   back.** The gap to `in-repo` at ten closes from 19 points to 6. No
   cross-service question gets worse and ten of the twelve that are retrieved
   at all end at rank 1 or 2: the Java→Node cart post 11 → 1, eshop's gRPC
   basket caller 13 → 2, boutique's two-caller shipping quote 5 → 1,
   petclinic's genai pet POST from never found to 1. Only petclinic's genai
   call into the vets service stays outside the top two (6 → 4).
   Crossing a service boundary is not, by itself, hard to retrieve
   — it is hard to *rank*, because the answering file talks about the caller's
   domain and not the callee's.

2. **What is left is eight questions no ranking can reach, and the graph says
   why — three different reasons, and only one of them is "missing edge".**
   Each was checked against the extracted graph in the run's own database, not
   inferred from the results:

   - **The key is on both sides but spelled differently.** robotshop's payment
     service produces `http_call http:GET /check/{id}` at `payment.py:64`, and
     the Express handler is an `http_route` unit keyed `http:GET /check/:id`
     at `user/server.js:85` — the exact answer, one placeholder syntax apart.
     petclinic is worse: the gateway's call is extracted as
     `http:GET /visits` from a URI whose real path is `pets/visits`, against
     an `http:GET /pets/visits` route unit. Two sides, two keys, no join.
   - **The key is on both sides, correctly, and the lookup does not use it.**
     boutique carries `implements_rpc grpc:PaymentService/Charge` and
     `rpc_call grpc:PaymentService/Charge` — the latter at
     `checkoutservice/main.go:370`, which is the expected answer line. "Which
     service calls the payment service Charge rpc" returns three copies of
     `demo.proto` instead. The Go→Go equivalent (`ShipOrder`) answers at rank
     1; the difference is that the Node implementation's symbol is not named
     after the rpc, so resolving the callee *by name* fails and the edge that
     already carries the key is never consulted. eshop's
     `ProductPriceChangedIntegrationEvent` is the same story with both halves
     present — `produces` at `CatalogApi.cs:356`, `consumes` at
     `ProductPriceChangedIntegrationEventHandler.cs:5`, which is the expected
     answer line — and "which service subscribes to catalog product price
     changes" still never returns it, because the question describes the topic
     instead of naming it. The `rpc` shape was fixed by running the match from
     the edges to the question rather than the other way round; `callers` and
     `topic` have since had the same treatment on the client side of the
     contract, and both of these questions now answer at rank 1 — see "The
     other side of a contract, measured" below.
   - **The key is genuinely missing, and the pattern that would produce it is
     nameable.** `Route.builder(PUT, "/_template/{name}")…build()` yields no
     route unit where `new Route(GET, "/_cluster/health")` yields two — the
     one cross-repo failure, and the reason its *caller* comes back at rank 2
     in the same result list while its handler never appears. eshop publishes
     through a wrapper: the same `PublishThroughEventBusAsync` call yields a
     `produces` edge at `CatalogApi.cs:356` and none at
     `OrderStatusChangedToAwaitingValidationIntegrationEventHandler.cs:32`,
     where the event is assigned by a ternary — so
     `topic:OrderStockConfirmedIntegrationEvent` has a consumer in the graph
     and no publisher anywhere, and both of its questions fail. `FROM
     ordering.orders` inside a C# raw string gives OrderProcessor no
     `reads_from db:orders`, so all four of that table's edges sit in the
     service that owns it.

3. **The one cross-repository contract in this corpus is not joined by the
   graph at all — the three questions that answer, answer by string match.**
   elasticsearch has the `http:GET /_cluster/health` route unit; conductor has
   *no* `http_call` edge for it, because `performRequest("GET", path, params)`
   is not a recognised client call. Both files contain the literal
   `/_cluster/health`, BM25 finds them, and rank 1 across twelve repositories
   is the result. That is a real answer to a real question and it is worth
   having — but it is not evidence that cross-repository tracing works, and
   the `cross-repo` row scoring above `in-repo` should be read as four
   questions about one distinctive string, not as a property of the boundary.

4. **One repository accounts for half the remaining failures.** Over the new
   questions alone, at ten with the reranker: petclinic 3/3, robotshop 3/4,
   boutique 5/7, conductor 2/2, elasticsearch 1/2 — and eshop **1/5**. eshop
   is last because its whole integration layer is generic:
   `AddSubscription<TEvent, THandler>()` names the contract in a type
   parameter and the routing key is read at runtime from
   `@event.GetType().Name`. Its three `topic` questions are about that layer
   and all three fail; the fourth failure is the `orders` table read through a
   raw SQL string; only its gRPC question answers. A contract expressed only
   in type parameters is invisible to a text index and, today, half-visible to
   the extractor — which is what makes `topic` the one shape that got *worse*
   when the new questions joined it (0.800 → 0.500 at ten).

The obvious objection to point 2 is that those seven are not graph failures at
all but vocabulary failures — the question describes the contract in words the
index does not hold, and a vector channel would bridge it. It does not. The 19
cross-service questions were run again on their own index of the four
repositories they live in, with symbol cards, Qdrant and bge-small-en-v1.5
under the same reranker: `recall@10` 0.579, **the same seven still unanswered**
(and eshop's gRPC basket caller lost, 2 → never found, because a one-line C#
call does not survive card chunking as well as it survives a line window).
Aggregate comparison with the tables above is not meaningful — a four-repository
BM25 index has different term statistics — but the identity of the failures is,
and it is unchanged. Keyword, keyword plus a cross-encoder, and vectors plus
cards plus the same cross-encoder all miss exactly the questions whose contract
key is absent, misspelt or unconsulted. **None of the three is a retrieval
problem, and no ranking work will fix them.**

What this section does not cover: `"repos": "all"` means whatever the run
indexed, so the one question that uses it is only comparable between runs that
indexed the same set — the result file records which, and the run prints it.
And four questions on one contract is not a measurement of cross-repository
retrieval; it is the most this corpus can support, and a second checkout pair
would be worth more here than another ten cross-service questions.
### Compiler-grade call edges, measured

Every call edge in this system comes from a name: the tree-sitter pass records
the callee's name at the call site and the linker resolves that name to a unit.
Over the nine repositories below, **273 404 of 783 618 call edges (35 %) name a
symbol two or more definitions carry** — the linker has to guess, and says so
with `ConfHeuristic`. A language server does not guess. `lsp.calls` spends a
bounded number of `textDocument/references` requests on exactly those callees
and applies the answer to the edges that already exist (see the README section
"Correcting the call graph").

Measured as a pure A/B on **one** index: 59 questions, nine repositories,
`cards+rerank/hybrid`, binary `dev (e9eac7f)`. The A side was scored, the call
pass was then run over that same database with `tools/lspcalls`, and the B side
was scored without reindexing — so BM25, the vector collection and the reranker
saw byte-identical inputs and the only difference is the graph.

| /search, 59 q | A: name matching | B: + LSP call edges |
| --- | --- | --- |
| `callers` recall@10 | 0.889 | 0.889 |
| `callers` MRR | 0.602 | **0.657** |
| total recall@10 | 0.915 | 0.915 |
| total MRR | 0.655 | 0.664 |

**One question moved, and nothing regressed.** `boutique-shiporder-caller`
2 → 1: gopls found that `checkoutservice/main.go:387` references the generated
`ShipOrder` client, where the Go extractor had recorded only the surrounding
`rpc_call` — a call site that did not exist in the graph before. Every other
question is unchanged, including the five where the pass rewrote hundreds of
edges.

What it cost, per repository (warm containers, contended laptop):

| repo | languages | candidates | requests | files | refs | confirmed | re-pointed | added | contradicted | s |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| airflow | go, ts | 416 | 416 | 157 | 1155 | 370 | 100 | 278 | 267 | 55 |
| boutique | go | 510 | 510 | 20 | 890 | 432 | 112 | 20 | 26 | 8 |
| jellyfin | csharp | 4056 | 742 | 210 | 1283 | 917 | 92 | 15 | 438 | 119 |
| eshop | csharp | 485 | 485 | 202 | 215 | 97 | 25 | 7 | 31 | 163 |
| petclinic | java | 60 | 59 | 21 | 10 | 1 | 5 | 4 | 0 | 30 |
| robotshop | go, java | 20 | 20 | 9 | 3 | 2 | 1 | 0 | 0 | 88 |
| conductor | java | 2970 | 1493 | 200 | **0** | 0 | 0 | 0 | 0 | 52 |
| argocd | go | 519 | 98 | 28 | 300 | — | — | — | — | 439 |
| consul | go | 8600 | 0 | 0 | 0 | — | — | — | — | — |

3 725 requests, 8.6 minutes of wall clock, 139 ms mean per request. That
rewrote **3 240 of 783 942 call edges (0.4 %)**.

The cost is not in the requests. Once a workspace is loaded, gopls answers a
references request in 1-100 ms, tsserver in ~150 ms, jdtls in ~250 ms and
OmniSharp in ~640 ms. The cost is the session: **60-90 s for a large Go module
(cold, downloading the module graph; ~7 s warm), ~3 min for a Maven reactor,
~3.5 min for an MSBuild solution.** And `socat`'s fork mode starts a fresh
server per connection, so that load is paid on every pass, not amortised.

Three rows in that table are the real finding.

- **conductor: 1 493 requests, zero references.** jdtls was up, answered every
  request promptly, and knew nothing — it never imported the Gradle build.
- **argocd and consul: no result at all.** gopls died mid-pass on both (439 s
  in, after 98 of argocd's requests), and a language whose session dies has its
  whole plan discarded, including the 300 references it had already collected.
- **Java and C# need a writable workspace.** jdtls writes `.classpath` /
  `.project` / `.settings` and OmniSharp writes `obj/` next to each project;
  against the documented read-only mount both return zero symbols for every
  file, which is indistinguishable from "this code has no symbols".

That last shape — a server that answers, quickly, with nothing — is what makes
this pass dangerous rather than merely useless when it goes wrong. Reading an
empty `references` answer as "nothing calls this definition" unresolved **44 of
petclinic's 144 resolved call edges** on the first run, two of them correct
(jdtls does not resolve `Pet.getOwner`, so `Pet.toString`'s two `this.getOwner()`
calls lost their destination). An empty answer now denies nothing, and only a
file the server demonstrably resolved something in during the pass may be
contradicted at all; `internal/lsp` pins all three failure modes — errored,
empty, unreachable — as tests, each checked by removing the guard.

**The verdict: behind a flag, off by default.** One question of 59, +0.056 MRR
on the one shape it targets, for a language-server deployment, a writable
checkout, minutes of session warm-up per repository per pass, and two of nine
repositories where the Go server did not survive. It is worth its cost only
where the graph itself is the product — an "who calls X" API, a call-graph
export — and not as the price of a `make run`. `lsp.calls.enabled` therefore
defaults to false even when `lsp.enabled` is set.

Two things this measurement says that are worth more than the number:

1. **The remaining `callers` failures are not edge failures.** petclinic's
   `ApiGatewayController.java:56 → CustomersServiceClient.getOwner` edge now
   exists, at `ConfExact`, and the question still answers at rank 3 — because
   the callee is never resolved: "what calls the customers service client to
   fetch an owner" contains no identifier-shaped token, and the leading hit is
   the `Owner` model. Same for jellyfin's `TryConnect`. The graph is right and
   the question never reaches it; that is `calleeUnits`' problem, and it is
   cheaper to fix than any of this.
2. **A correct graph is not automatically a better answer.** 3 240 edges were
   corrected and one ranking changed. The edges that matter for a question are
   the handful the intent actually looks up, and most corrections land far from
   any question anyone asked.

Provenance: these numbers come from the **80-question set as it stands on this
branch**, scored over its 59-question nine-repository subset. `master` has since
grown the set to 103 questions with cross-service and cross-repository scoring,
and `callers` is where most of the new failures land — so the table above is
comparable with the other 59-question rows in this document and with nothing on
the larger set.

### A bigger cross-encoder is not the upgrade it looks like

Every reranker number above was measured with the *smallest* usable model
(`BAAI/bge-reranker-base`, 278M). The obvious next step is a stronger one, so
`BAAI/bge-reranker-v2-m3` (568M, the same TEI-shaped service, fp16 on the same
laptop GPU) was measured against it — **one shared index**, seven runs, no
re-indexing between them, so BM25 segments and Qdrant points were byte-identical
and the only difference is the rerank configuration. 59 questions, nine
repositories, symbol cards, `hybrid`, binary `dev (d2a6667)`:

| /search, 59 q | top_n | recall@1 | recall@10 | MRR | span@10 | never found | median |
| --- | --- | --- | --- | --- | --- | --- | --- |
| no reranker | — | 0.339 | 0.864 | 0.506 | 0.695 | 5 | 579 ms |
| bge-reranker-base 278M | 10 | 0.508 | 0.881 | 0.628 | 0.712 | 4 | 858 ms |
| **bge-reranker-base 278M** | **25** | **0.542** | **0.932** | **0.669** | 0.763 | **4** | 1048 ms |
| bge-reranker-base 278M | 50 | 0.525 | 0.915 | 0.652 | 0.763 | 5 | 1965 ms |
| bge-reranker-v2-m3 568M | 10 | 0.492 | 0.881 | 0.616 | 0.712 | 4 | 1511 ms |
| bge-reranker-v2-m3 568M | 25 | 0.475 | 0.915 | 0.632 | 0.763 | 4 | 1663 ms |
| bge-reranker-v2-m3 568M | 50 | 0.508 | 0.881 | 0.650 | 0.763 | 5 | 5133 ms |

**Twice the model is a loss at every top_n.** Same span@10, lower recall@1 and
MRR at each setting, and 1.6× to 3× the latency. Giving it its best shot does
not rescue it either: served at a 1024-token window instead of 512 — the length
its architecture is for, and enough to stop truncating a 40-line card — it
scores *identically* (recall@1 0.475, recall@10 0.915, MRR 0.631) for one extra
covering chunk (span@10 0.780) at 4550 ms, 2.7× its own 512-token cost.

`top_n` 25 is a **peak, not a plateau**, which the older table (measured before
the graph lookups landed) could not show: 50 retrieves twice as many candidates
and reranks twice as many, and scores worse on every metric than 25 — one answer
leaves the top ten entirely. More candidates is more chances to promote the
wrong one.

The cross-encoder alone, timed outside the search pipeline on documents the
length it actually receives (a card with 40 body lines, so the worst case):

| documents | base 278M | v2-m3 @512 | v2-m3 @1024 |
| --- | --- | --- | --- |
| 10 | 884 ms | 1486 ms | 1920 ms |
| 25 | 1209 ms | 3631 ms | 4684 ms |
| 50 | 1853 ms | 7308 ms | 13899 ms |
| 100 | 3313 ms | 8162 ms | 29799 ms |

Two caveats a reader should carry: the machine was shared with other work
throughout, so every latency here is an upper bound and the quality numbers are
the reliable half; and reproducibility was checked rather than assumed — the
same configuration run twice over the same index scored identically on every
metric to three decimals, so a moved rank in this section is a behaviour change.

### The document the reranker is handed, and where its path belongs

Candidates reach the rerank stage as three different kinds of document. A vector
hit carries a symbol card; a keyword hit carries a window of raw file text; a
graph hit carries a sentence about an edge — and that third kind never reaches
the reranker at all, because the graph's call sites are added after ranking (see
"One thing the reranker must not touch"). What the cross-encoder actually
compares is cards against raw windows, and **neither of them said where it
lived**.

That matters more than it sounds. A Go service's entry point is
`function main.main`; the word "checkout" appears nowhere in the card for
`src/checkoutservice/main.go` except in a path the card did not carry.

Prefixing every rerank document with its path and symbol, at query time:

| /search, 59 q | recall@1 | recall@10 | MRR | span@10 |
| --- | --- | --- | --- | --- |
| base 278M, snippet only | **0.542** | 0.932 | **0.669** | 0.763 |
| base 278M, + path header | 0.508 | 0.932 | 0.645 | **0.797** |
| v2-m3 568M, snippet only | 0.475 | 0.915 | 0.632 | 0.763 |
| v2-m3 568M, + path header | **0.576** | **0.932** | **0.702** | 0.763 |

**The same information helps the larger model and hurts the smaller one.** For
v2-m3, 15 queries move and 12 of them improve, on every shape at once (`callers`
recall@1 +0.222, `implement` +0.167, `table` +0.111, MRR +0.070) — the one
configuration in this document where the 568M model is worth its cost. For the
278M model, 20 queries move and 12 get worse: `route` recall@1 0.529 → 0.412. It
gains covering chunks and loses rank-1 answers, which is what a model being
distracted by tokens it cannot use looks like.

So the path is worth having and the rerank stage is the wrong place to add it —
it is a property of the document, not of the query. Putting it in the card
instead is the next section, and it settles the question: with the path in the
card, adding the header again at query time moves nothing (base MRR 0.675 →
0.658, m3 0.687 → 0.696, span@10 identical in both). The card change subsumes
it, reaches the embedder as well as the reranker, and costs nothing per request.

### What goes in a symbol card

`cardBodyLines` and the card's fields were designed once and never tuned. A card
is `<kind> <qualified>`, the signature, the doc comment and the first 40 lines of
the body — everything the parser knows about the symbol and nothing about where
it is. Adding one line, the repository-relative path, in front of the rest:

| /search, 59 q | recall@1 | recall@10 | MRR | span@10 | never found |
| --- | --- | --- | --- | --- | --- |
| card as designed, no reranker | 0.339 | 0.864 | 0.506 | 0.695 | 5 |
| **card leads with its path**, no reranker | **0.424** | **0.881** | **0.561** | **0.763** | 6 |
| card as designed + rerank (base, 25) | 0.542 | **0.932** | 0.669 | 0.763 | 4 |
| **card leads with its path** + rerank (base, 25) | **0.559** | 0.915 | **0.675** | **0.797** | 4 |
| card as designed + rerank (v2-m3, 25) | 0.475 | 0.915 | 0.632 | 0.763 | 4 |
| **card leads with its path** + rerank (v2-m3, 25) | 0.542 | **0.932** | **0.687** | **0.814** | 4 |

One line of text per card buys **more span@10 than the reranker does** — +0.068
with no reranker at all, and it stacks: the best span@10 measured on this project
is 0.814, the path card under the larger cross-encoder. That is the number this
work was aimed at. `recall@10` barely moves, because the answering file was
mostly already being found; what changes is *which chunk of it* comes back.

The mechanism is visible per query. 17 questions move without a reranker, 11 of
them better, and the winners are the ones whose subject is a directory:
`argocd-compare-app-state` 7 → 1, `consul-kv-http-route` 5 → 1,
`boutique-shiporder-caller` 3 → 1. Per shape the gain is where the vocabulary
gap was — `implement` MRR 0.431 → 0.534, `route` 0.501 → 0.585, `callers` 0.356
→ 0.420 — and `table` and `topic` do not move at all, because their questions
name a key rather than a place.

Two honest costs. One question, `petclinic-gateway-owner-call`, goes from rank 13
to never found — the path text crowds a short method's card. And under the
reranker the trade is not free per shape: `route` MRR 0.695 → 0.744 and span@10
0.706 → 0.824, while `implement` gives back recall@10 0.917 → 0.833. The totals
are a win and the per-shape table says who paid for it.

This is an index-time change: it needs a re-index, and both sides of the
comparison above re-indexed. That is only readable because a full pass now
compacts the BM25 index to one segment (see "Why two builds of the same sources
used to disagree") — the keyword channel is identical across the two builds by
construction, so the movement is the vector channel's.

**The enclosing type's context was the other obvious candidate, and it is a
loss.** A method's card says what the method is called and not what owns it, so
`getOwner` was given `in class …CustomersServiceClient` plus that type's first
doc line, resolved from the smallest declaration whose byte span contains the
member. Measured on its own index against the path card above, it moves nothing
and costs a question:

| /search, 59 q | recall@1 | recall@10 | MRR | span@10 |
| --- | --- | --- | --- | --- |
| path card, no reranker | 0.424 | **0.881** | 0.561 | **0.763** |
| + enclosing type, no reranker | 0.424 | 0.864 | 0.561 | 0.746 |
| path card + rerank (base, 25) | 0.559 | **0.915** | **0.675** | 0.797 |
| + enclosing type + rerank (base, 25) | 0.542 | 0.898 | 0.660 | 0.797 |
| path card + rerank (v2-m3, 25) | 0.542 | **0.932** | **0.687** | 0.814 |
| + enclosing type + rerank (v2-m3, 25) | 0.525 | 0.915 | 0.672 | 0.814 |

Five queries move without a reranker, three of them worse, and MRR is identical
to three decimals; with either reranker one answer leaves the top ten. The
reason is that the context was already there: in Java, C# and TypeScript the
qualified name *is* `<package>.<Class>.<member>`, so the added line repeats it
and contributes only the type's doc sentence — and in Go, the one language whose
qualified name would not carry it, the method sits beside its type rather than
inside it, so the byte-span lookup finds nothing to add. It buys a duplicate
where the name is qualified and nothing where it is not. Reverted.

The shape of these two results together is worth more than either: **the card
was missing a fact about the symbol, not more words about it.** The path is the
one thing the parser never records and the file system always knows; the
enclosing type was already in the name.

## Scoring the graph expansion

`/context` hands back more than ranked hits. Each item carries the AST unit the
hit landed in, that unit's service, and a `related` list — the units the code
graph reaches from it, in both directions, with the contract indirections
followed (`internal/graph`, `Expand`). That list is a large part of what an LLM
is actually given, and nothing scored it: `run.py` reads `items[].hit` and
stops, so a wrong or empty `related` list cost nothing.

`related.py` scores it, against ground truth of the same kind as the rest of
this set and established the same way — by reading the corpus, never by asking
the system what it returns.

### The second file

57 of the 103 questions now carry `expect_related`: the file a complete answer
needs *besides* `expect_file`, the other end of the contract the question is
about. None of it was invented for this metric. Establishing each answer already
meant reading both ends, and the far side is cited in the question's `why`:

| the question | the answer | its companion |
| --- | --- | --- |
| where does POST /cart/checkout go in the frontend | `frontend/handlers.go` — the handler | `frontend/main.go` — where the path is bound |
| what calls the argo helper that validates a repository | `application.go` — the caller | `util/argo/argo.go` — the callee |
| which service publishes orders to the robot-shop exchange | `payment/rabbitmq.py` — the producer | `dispatch/main.go` — the consumer |
| where do rows get inserted into the login_attempt table | `store.go` — the insert | `login_attempt_mig.go` — where the table is created |
| which service serves the check the payment service makes | `user/server.js` — the handler | `payment/payment.py` — the client |

The other 46 have no second file an honest reader would need — "which allocation
decider stops shards from being allocated to nodes that are low on disk space"
is answered by one class — and they carry no annotation rather than a
manufactured one.

`expect_related` is **not** an acceptable answer. It never enters recall, MRR,
nDCG or span, `run.py` does not read the field, and `--validate` rejects a
companion that is the `expect_file` itself. It may be an `alt_files` entry: a
route's registration is both an equally defensible answer and the companion of
its handler, and those pairs are the clearest cases the metric has.

### Three numbers, kept apart

**companion** — for a question that carries one, does the related list of the
item that *is* the answer name the second file? The answer item is the first
item on `expect_file` itself, not on any acceptable file, because the pairing is
between one file and its far side and that has to stay unambiguous. Reported
conditional on that item existing — an expansion cannot be blamed for a hit that
never arrived — with the count that reached it beside it, so the ceiling is
never folded into the score. And reported next to its control, **already a
hit**: if retrieval returned the second file on its own, the graph added nothing
by naming it.

The obvious alternative — "the related list of the *top* item should contain the
second file" — was rejected after reading the data. When the top item is not the
answer, a companion in its related list is a coincidence between two files the
ranking happened to like, not evidence of anything a reader needs.

**reach** — over all 103, whether the answering file is among the hits, only in
some item's related list (a **rescue**: the expansion supplied an answer the
ranking did not), or in neither. This needs no annotation: it reuses
`expect_file`, so it covers the whole set and is directly comparable with
`run.py`'s `/context` recall.

**shape** — what the lists look like at all: the share of items whose related
list is empty, the mean size, the share of related units naming a file other
than the item's own, and the same for services. None of it is scored. It is
there because a metric with a recall term and no precision term is satisfied by
returning the whole graph, and this is what makes that visible.

### The first numbers

Measured on 2026-08-13 on the same index as the 103-question baseline above
(AST + BM25 over SQLite, no vector index, no reranker, keyword mode, `/context`
`limit 20`), 2 040 items over 103 questions, `hops 1`:

| /context related lists, 103 q | value |
| --- | --- |
| items whose `related` list is **empty** | **0.787** |
| related units per item | 1.58 |
| related units naming another file | 0.810 |
| related units naming another service | 0.179 |
| edge kinds | `call` 3 073, `http_call` 82, `handled_by` 20, `writes_to` 17, `reads_from` 16, `rpc_call` 12, `kafka_flow` 3 |

| companion, 57 q | n | answer retrieved | at the answer item | anywhere in the package | already a hit | absent |
| --- | --- | --- | --- | --- | --- | --- |
| callers | 26 | 17 | 0.118 | 0.412 | 0.765 | 3 |
| route | 13 | 9 | 0.333 | 0.444 | 0.444 | 3 |
| rpc | 8 | 7 | 0.286 | 0.429 | 0.714 | 1 |
| topic | 5 | 2 | 0.500 | 1.000 | 1.000 | 0 |
| table | 4 | 3 | 0.000 | 0.000 | 0.667 | 1 |
| implement | 1 | 1 | 0.000 | 0.000 | 1.000 | 0 |
| in-repo | 34 | 24 | 0.250 | 0.458 | 0.625 | 5 |
| cross-service | 19 | 12 | 0.167 | 0.417 | **1.000** | 0 |
| cross-repo | 4 | 3 | **0.000** | 0.000 | **0.000** | 3 |
| **total** | **57** | **39** | **0.205** | **0.410** | **0.692** | **8** |

The three rates share one denominator — the 39 questions whose answer was
retrieved, so that the score and its own control stand on the same base.

| reach, 103 q | in hits | rescued by `related` | neither |
| --- | --- | --- | --- |
| in-repo (80) | 58 | 2 | 20 |
| cross-service (19) | 12 | 1 | 6 |
| cross-repo (4) | 3 | 0 | 1 |
| **total** | **73** | **3** | **27** |

Four things this says, in the order they matter:

1. **Four out of five items get no expansion at all.** 0.787 of items come back
   with an empty `related` list, and that is the dominant failure — not a wrong
   list, an absent one. It is not the retrieval's doing: a hit that lands in no
   AST unit, or in a unit with no edges, expands to nothing. Three of the
   companion failures are exactly this and are worth naming, because in each the
   answer *was* found and the item beside it was blank: argocd's
   `/auth/callback` handler, jellyfin's `JellyfinDbContext`, and conductor's
   `waitForHealthyCluster` — the cross-repository question the whole corpus is
   built around.

2. **When the graph does answer, it usually says what retrieval already said.**
   The companion is in the answer item's related list for 8 of the 39 questions
   whose answer was retrieved (0.205), while retrieval had already returned that
   same file as a hit for 0.692 of them — and for `cross-service`, where the far
   side is the whole point, the control is **1.000**: BM25 returned both ends of
   all twelve. The expansion is not what puts the second file in front of the
   reader on those; it is only what says the two are *joined*. The failures are
   specific rather than diffuse: consul's route question reaches its registration
   through a `handled_by` edge into `agent/http_register.go`, while grafana's
   identical question — same shape, same explicit-routing pattern,
   `routing.Wrap(hs.GetDashboardSnapshot)` in `pkg/api/api.go` — comes back with
   sixteen related units that are all outgoing `call` edges and no `handled_by`
   at all. One extractor pattern short, and now visible as a number.

3. **The expansion does rescue answers, and it is three questions.** For
   `argocd-compare-app-state`, `argocd-validaterepo-callers` and
   `robotshop-user-check-route` the answering file is in *no* hit and in some
   item's related list — the graph supplying an answer the ranking did not, which
   is the feature working exactly as intended. Read against 27 questions the
   package misses entirely, it is also the size of the effect: 73 → 76 of 103.

4. **The one cross-repository contract is not joined, and now it costs
   something.** The `cross-repo` row is 0.000 in every column, control included:
   for all three questions whose answer was retrieved, the far side is in no
   related list *and* in no hit — the package simply does not contain it. The
   prose above already said conductor's `performRequest` produces no `http_call`
   edge; this is the same fact with a number on it, and it moves when the
   extractor does.

`hops` is the obvious dial and it was measured, on the same index: at `hops 2`
the companion at the answer item goes 0.205 → **0.256** (8 of 39 → 10) and
anywhere-in-the-package 0.410 → 0.462, for **2.55× the payload** (1.58 → 4.03
related units per item) and no change at all to reach (still 73 hits, 3 rescues)
or to the empty share (0.787 — an item with no edges has none at any depth).
Two more companions for two and a half times the tokens is a bad trade at the
default, which is why the default stays 1.

### What this does not capture

- **No precision term.** A related list of 24 units, 23 of them irrelevant,
  scores exactly like a list of one right unit. There are no graded relevance
  judgements for the other 23 and nobody has written them; `empty items`,
  `rel/item` and `off-file` are the only pressure in the other direction and
  they are descriptive, not scored. `consul-kv-http-route` scores a hit with a
  list of 24 that also contains a minified JavaScript asset from `dist/`.
- **No ordering.** `Expand` sorts by distance and truncates at `maxRelated` =
  24. A companion late in the list counts the same as one at the top, though a
  reader — and a prompt budget — will not treat them the same. Of the eight
  companions this run does find, four are past position five (9 of 10, 13 of
  24, 11 of 24, 13 of 13) and only two lead their list.
- **File-level matching**, like every other metric here: the right file reached
  through the wrong symbol counts.
- **One companion per question, and only 57 questions.** The rate is not
  comparable with recall over the full 103, and a question whose honest answer
  needs three files is still not expressible.
- **Nothing about whether the list helps.** That is the next harness.

## Answering the question, not retrieving the file

Everything above measures a proxy. `recall@10` says the file that answers the
question came back in the top ten; it does not say that anyone reading the
result learns the answer. The product exists to serve the second thing, and
`answer.py` is the smallest honest harness for it: take the `/context` package
for each question, render it the way a client would put it in a prompt, ask one
local model, and grade what comes back.

```sh
tools/eval/answer.py --corpus <corpus> --model qwen2.5:1.5b --judge --control
tools/eval/answer.py --corpus <corpus> --no-related     # the graph's A/B
```

The model is **qwen2.5:1.5b on a laptop**, and that has to be repeated next to
every number it produces: it is not a statement about what a good model would
do with this context, it is a fixed, cheap, reproducible reader that changes
only when the context does. Temperature 0 and a fixed seed, five context items,
1 200 characters of snippet each, the related lists rendered as one line per
unit.

### Grading, mechanically first

The ground truth already names the answering file, the line, the symbol and an
`anchor` — the exact text on that line — so the grade does not need a model:

| | |
| --- | --- |
| `cited` | the answer names the answering file. The prompt shows every candidate as `<repository>/<path>`, and a model answers with the tail of it, so a tail counts only when it belongs to exactly one file in the package: robotshop has four `server.js` and "server.js" names none of them. |
| `grounded` | the answer also carries the symbol, or a distinctive word off the anchor line — the difference between naming a file and saying what is in it. |
| `correct` | both. |

`cited` is the headline and `grounded` is the weaker of the two: an anchor word
like "visits" can be arrived at from the question. Neither reads the prose for
correctness — an answer that names the right file and describes it wrongly
scores as correct, and one that describes it perfectly while citing nothing
scores as wrong. That is the trade for a grade that cannot drift.

The path rule is the fiddly part and it was tuned against the answers, not
guessed: matching is on **whole path segments**, so `connectca_server.go` is not
a citation of `.../connectca/server.go` (that file does not exist, and an
earlier substring rule scored it as correct), while tails of what the model
wrote are tried in turn, because a small model routinely writes the right file
under an invented prefix — this one turns boutique into
`blocq/src/shippingservice/main.go`. `answer.py --regrade <result.json>`
re-grades a finished run with no server and no model, which is why every run
records the file names it offered each question; the 15 questions that the
final rule marks as retrieved-but-not-cited were read by hand, and every one of
them names a genuinely different file.

### The ceiling, kept separate

A question whose file was never retrieved cannot be answered, so the report
never adds the two failures together. Every question falls into one of:

- the answering file was among the items the model was shown, and the answer
  named it — or did not, which is the interesting number;
- the file was retrieved but past the prompt's item budget, cut before the model
  saw it — a cheaper failure to fix than either of the others;
- the file was not retrieved at all.

`--control` asks every question a second time with no context whatsoever. If a
model answered from memory the whole table would be meaningless, and the control
is what rules that out rather than assuming it.

### The first numbers

Same index, same binary and the same 103 questions as everything above,
`/context` `limit 20`, five items in the prompt, `qwen2.5:1.5b`, median 5.0 s
per answer:

| answers, 103 q | n | file in the prompt | cited it | cited + grounded | correct / all |
| --- | --- | --- | --- | --- | --- |
| callers | 26 | 10 | 0.600 | 0.400 | 0.154 |
| implement | 18 | 12 | 0.667 | 0.500 | 0.333 |
| route | 30 | 15 | 0.667 | 0.467 | 0.233 |
| rpc | 9 | 7 | 0.857 | 0.857 | 0.667 |
| table | 12 | 8 | 0.875 | 0.875 | 0.583 |
| topic | 8 | 3 | 1.000 | 1.000 | 0.375 |
| in-repo | 80 | 44 | 0.750 | 0.636 | 0.350 |
| cross-service | 19 | 8 | 0.500 | 0.375 | 0.158 |
| cross-repo | 4 | 3 | 1.000 | 0.667 | 0.500 |
| **total** | **103** | **55** | **0.727** | **0.600** | **0.320** |

The whole point of the exercise is the decomposition, so here it is without an
average on top of it. Of 103 questions:

- **55** had the answering file among the five items the model was shown. Of
  those, **33** answers named it *and* carried what is on the line — so **22
  questions were retrieved and still answered wrong**.
- **48** did not. Of those, **18** had the file retrieved at rank 6-20 and cut
  by the prompt budget, not missed by retrieval — the cheapest of the three
  failures to fix, and invisible to every metric above.
- **0** of the 48 were answered correctly anyway.
- 61 answers cited a file from the package that is not an answer.

Read against the retrieval numbers on the same index — `/context` `recall@10`
0.631, `recall@5` 0.553 — the product delivers **0.320**. About a third of the
gap is the prompt budget and two thirds is the reader: a retrieval score of
0.631 corresponds to a third of questions actually answered, with this model.

**The control settles the obvious objection.** Asked the same 103 questions with
no context at all, the model cited the right file **0 times**. Nothing in the
table above is memory; every point of it came from the retrieval.

### What a 1.5B judge can and cannot tell you

`--judge` ran on all 103 answers, the same model that wrote them:

| | |
| --- | --- |
| judge says correct | **1.000** (103 of 103) |
| mechanical grade says correct | 0.320 |
| the two agree | 33 of 103 (**0.320**) |
| judged correct that the mechanical grade rejects | 70 |
| judged incorrect that the mechanical grade accepts | **0** |

It agreed with everything. Handed the question, the documented file and line,
and an answer naming a *different* file, it affirmed the answer and wrote a
fluent reason for doing so — for "where is the ApplicationService Sync grpc
method implemented in the api server" it accepted the generated client stub
`pkg/apiclient/application/application.pb.go` in place of the server
implementation, and explained that the method name matched. Its agreement rate
with the mechanical grade is exactly the mechanical grade's own pass rate,
which is what agreement looks like when one side is a constant: the judge
carries no information about which answers are right.

So, plainly:

- **It cannot grade.** Not "it is noisy" — it is biased in one direction, to the
  floor. Any score built on it would read 1.000 no matter what the retrieval
  did, which is the exact failure mode this harness exists to avoid.
- **It cannot be trusted about its own family's output**, and this is the
  measurement of that rather than the assumption: 70 false accepts, 0 false
  rejects.
- **It can follow the output format** — 103 of 103 verdicts parsed — which is
  the only thing it did reliably.
- **What it is useful for is the calibration itself.** The disagreement rate is
  cheap to produce and it tells you whether a judge may be used at all. On this
  hardware, with this model, the answer is no; on a larger judge the same
  three lines of table are how you would find out.

The reader is a 1.5B model on a laptop, so the absolute numbers describe *that
reader*. What the harness is for is comparison: the same reader over two
contexts is a fair test of the contexts, and `--no-related` is the first such
comparison this makes possible — the graph expansion measured by whether it
changes an answer rather than by whether it is present.

### The other side of a contract, measured

The section above ends with two questions the graph could already answer and
the lookup would not ask: boutique's `rpc_call grpc:PaymentService/Charge` sits
on the expected line and "which service calls the payment service Charge rpc"
returned three copies of `demo.proto`; eshop's `consumes
topic:ProductPriceChangedIntegrationEvent` sits on the expected line and "which
service subscribes to catalog product price changes" never returned it at all.
Neither question can be answered by extracting a key from it — the first names
a contract whose implementation is called `ChargeServiceHandler`, so resolving
the callee by name finds nothing, and the second never spells the topic.

So the same reversal the `rpc` shape got, applied to the client side: read the
repository's `rpc_call`, `http_call`, `produces` and `consumes` edges and keep
the ones whose key the question describes, component by component. Which of the
four to read is decided by the question — the callers intent asks for the call
side, and for a topic the verb picks the end, because "which service publishes
X" and "which service subscribes to X" name one key and have opposite answers.

A pure A/B on one index, measured 2026-08-13: 103 questions, 12 repositories,
`base/keyword`, binary `dev (d0ba64b)`, 0 request errors. Both sides asked the
same questions of the same database; the only difference is the query-time
lookup:

| /search, 103 q | A: before | B: after |
| --- | --- | --- |
| `topic` recall@10 (n=8) | 0.500 | **0.750** |
| `topic` MRR | 0.330 | **0.750** |
| `callers` recall@10 (n=26) | 0.423 | **0.462** |
| `callers` MRR | 0.320 | **0.358** |
| `cross-service` recall@10 (n=19) | 0.474 | **0.632** |
| `cross-service` MRR | 0.283 | **0.440** |
| total recall@1 | 0.388 | **0.437** |
| total recall@10 | 0.621 | **0.650** |
| total MRR | 0.459 | **0.502** |
| total span@10 | 0.447 | **0.476** |
| never found | 32 | **29** |

**Five questions moved, all of them forward, and nothing regressed** — on
`/search` and on `/context`, at every k, for every other shape. Both recorded
failures answer at rank 1 on their exact line (`checkoutservice/main.go:370`,
`ProductPriceChangedIntegrationEventHandler.cs:5`), and three more came with
them: eshop's stock-confirmed consumer (never found → 1), robotshop's orders
publisher (7 → 1) and its orders consumer (2 → 1, ahead of the publisher the
literal-key lookup had put first — the verb says which end was asked for).

The rules that keep it from answering a different question were not designed;
each one is a regression that showed up in this table and was traced to the
promotion that caused it. The first version accepted any key whose components
the question described, and moved 13 queries, 7 of them backwards:

- **A one-word key is not a name.** `Create`, `Sync`, `GET /owners`,
  `GET /product/{}` are one common word each, and a question that happens to
  contain that word has not named a contract. This cost four rank-1 answers
  (grafana's snapshot handler 1 → 6 behind four unrelated `Create` rpcs, argocd's
  `AugmentSyncMsg` 1 → 3 behind `ApplicationService/Sync`, boutique's catalog
  loader and conductor's cassandra dao 1 → 2) and lost petclinic's gateway owner
  call entirely, 18 → not retrieved. A single-word rpc method now needs its
  service named as well, and a route needs two path segments.
- **A path parameter is not a path word.** "Which endpoint reads a shopping cart
  by its id" describes every component of `http:DELETE /cart/{id}`, including
  the `{id}`, and that promotion pushed the answer 1 → 3. Parameter segments are
  per-request data — the same reason the key drops the query string — and are
  dropped before the comparison.
- **A load generator is not a caller.** `GET /api/catalogue/products` has
  exactly one caller in robotshop's graph and it is `load-gen/robot-shop.py`;
  the ratings service's own call is the PHP client that concatenates its URL and
  produces no edge. Nothing scored differently — that question is unanswered
  either way — but rank 1 asserted that synthetic traffic is the code that uses
  the contract. Load generators are excluded by name, the way test paths already
  were. This is the same distinction that had to be made by hand when the query
  set was written.

Two more things this measurement says:

1. **The strictness is the feature.** Instrumented over the whole set, the
   lookup fires on **8 of the 103 questions and promotes exactly one line each
   time** — seven of those eight are the file the query set records as the
   answer. Two of the eight `topic` questions still fail and both fail
   honestly: eshop's stock-confirmed *publisher* has no `produces` edge to find
   (the ternary case above), and "which handler reacts to an order being paid
   in the catalog service" describes `OrderStatusChangedToPaid` as "an order
   being paid", leaving `status` and `changed` undescribed — that topic has
   three consumers and the question names the service that tells them apart,
   which is not something a key match can use.
2. **The eighth promotion fires without answering.** Conductor's "which class
   asks the workflow executor to decide after an async system task finishes"
   describes `grpc:WorkflowService/DecideWorkflow` completely, and the graph's
   answer — `WorkflowClient.java:176` — is a gRPC client, not the internal
   executor the question means. It is recorded here rather than patched: the
   question does name that contract, and the rank it displaces is a text hit
   that was not the answer either (that question is unanswered in both runs).

The lookup costs one indexed edge query per repository per request. It first
cost 3.9 s on elasticsearch's callers question, because "the contract edges of
this repository" had only `idx_edges_repo` to work from and SQLite walked all
2.8 M edges to find 2 660 `rpc_call` rows; migration 10 adds `(repo_id, kind)`
and drops the index it supersedes, and the same question now takes 31 ms —
median `/search` 6 ms, unchanged from the A side.

Rebuilding all twelve repositories from scratch on the new schema returns
**the same rank for all 103 questions** and indexes in 260 s against the A
side's 289 s, so neither the ranking above nor the indexing cost depends on
which database the two sides shared.

## Reading the numbers

- The set is a fixed 103 questions; two runs of it are comparable, one run of it
  is only a starting point. Runs from before 2026-08-13 measured the first 80,
  so their totals are not comparable with these — compare per shape, per scope
  or per query instead.
- `recall@10` is the headline: an answer that is not in the top ten is not an
  answer a user will find.
- Per-shape numbers matter more than the total. A change that lifts `route` and
  sinks `callers` averages out to nothing and is not nothing.
- Per-scope numbers say something the shapes cannot: whether an answer that
  lives in another service, or another checkout, is reachable at all. A total
  that improves while `cross-service` sinks is a product moving away from what
  it is for.
- `span@10` moving apart from `recall@10` is the chunking signal: the right
  file, the wrong part of it.
- The three harnesses measure three different things and must not be summed.
  `run.py` says the file was retrieved; `related.py` says the package also held
  the file the answer needs beside it; `answer.py` says a reader given the
  package wrote the answer down. Each one's ceiling is the one before it, which
  is why every table here prints the ceiling next to the rate rather than
  folding it in.
- Every `answer.py` number is a **1.5B model on a laptop**. It is a fixed reader
  for comparing two contexts, not an estimate of what a production model would
  do with the same package.
