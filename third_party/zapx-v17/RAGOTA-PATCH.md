# Local zapx v17.1.2 + chunk-table fix

This directory is an exact copy of `github.com/blevesearch/zapx/v17@v17.1.2`
with **one** change: the `chunkedIntCoder.Write` chunk-table bug is fixed by
backporting the logic upstream ships in v17.2.x (see the marked block in
`intcoder.go`). The root `go.mod` points here with a `replace` directive.

## Why it exists

zapx v17.1.2 — the version bleve v2.6.0 pins and the first zap version bleve
uses by default — corrupts the postings chunk-offset table of any term whose
postings contain an *empty chunk before the last non-empty chunk*:

- `Write` converted the chunk-length array to end-offsets **in place**, then
  walked the chunks through the file-callback processor, *skipping empty
  chunks*. A skipped slot kept the end-offset from the first pass, and the
  second lengths-to-offsets pass treated it as a length. The written table
  then overruns the term's data, and readers walk into foreign bytes.
- Every v17 persist and merge writer is a `*FileWriter`, so the block always
  ran, callbacks configured or not.

In this service the archetypal victims are `repo_id` / `_all` repo terms and
language terms in segments merged across repositories (dense in one doc range,
absent for a full chunk, dense again; cardinality ≥ 1025). Symptoms ranged
from silently wrong search results to `memUvarintReader overflow` errors and
index-out-of-range panics during search — the intermittent BM25 corruption the
retrieval eval recorded. `internal/zapverify` holds the regression tests
(`chunktable_regression_test.go`) that reproduce the corruption
deterministically against the unpatched module, and `tools/zapcheck` verifies
existing indexes.

Indexes written before the fix stay corrupt on disk; they need a forced
reindex (or at least deletion of the bm25 directory) to be rebuilt cleanly.

## When to remove it

Upstream fixed the bug in zapx v17.2.x, but v17.2.x needs
`scorch_segment_api/v2 >= 2.4.8`, whose API bleve v2.6.0 does not compile
against. Once a bleve release requires zapx `>= v17.2.x`, upgrade bleve, drop
the `replace` directive and delete this directory; the regression tests in
`internal/zapverify` will confirm the upgrade still writes decodable tables.
