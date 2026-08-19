#!/usr/bin/env python3
"""Run the retrieval query set against ragota and score the answers.

    ./run.py --corpus /data/corpus                    # build, index, measure
    ./run.py --corpus /data/corpus --shape route      # one question shape
    ./run.py --corpus /data/corpus --scope cross-repo # only the questions that
                                                      # cross a repository
    ./run.py --validate --corpus /data/corpus         # check the ground truth
    ./run.py --corpus /data/corpus --server http://127.0.0.1:8080 --no-index

The harness starts its own server on its own database under --work, indexes
only the repositories the selected queries need, asks every question through
POST /api/v1/search and POST /api/v1/context, and scores the ranked results
against tools/eval/queries.jsonl with recall@k, MRR and nDCG@10 — reported per
question shape, per scope, per repository and in total, plus a per-query table
so that a number that moved can be traced to the question that moved it.

A question is asked of the repositories its `repos` field names — one, several
or, with "all", of everything indexed — and results are scored as repository
plus path, so "which repository serves this route" is a question the harness
can put and mark.

The ground truth was written by reading the corpus sources, never by asking
this system what it returns; `--validate` re-checks it against those sources.
"""

import argparse
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import evallib as ev  # noqa: E402

REPO_ROOT = os.path.normpath(os.path.join(ev.HERE, "..", ".."))
DEFAULT_WORK = "/tmp/ragota-eval"


def add_common_args(ap):
    ap.add_argument("--corpus", default=os.environ.get("CORPUS_DIR", "corpus"),
                    help="directory holding the corpus checkouts (see tools/corpus/clone.sh)")
    ap.add_argument("--queries", default=ev.QUERIES_PATH, help="query set to run")
    ap.add_argument("--repo", action="append", default=[], help="restrict to this repository (repeatable)")
    ap.add_argument("--shape", action="append", default=[], help="restrict to this question shape (repeatable)")
    ap.add_argument("--exclude-tests", action="store_true",
                    help="do not index test, mock or fixture files. Measured on elasticsearch: "
                         "40079 files and 147 s become 28109 and 50 s, because test code carries "
                         "most of the call edges the linker resolves. No question in the set "
                         "expects a test file as its answer, but the keyword corpus changes with "
                         "them, so a run using this is not comparable with one that does not")
    ap.add_argument("--scope", action="append", default=[],
                    help="restrict to this question scope: %s (repeatable)" % ", ".join(ev.SCOPES))
    ap.add_argument("--id", action="append", default=[], help="restrict to this query id (repeatable)")

    ap.add_argument("--binary", help="ragota server binary to run (default: build cmd/ragota)")
    ap.add_argument("--server", help="use an already-running server instead of starting one")
    ap.add_argument("--api-key", default=os.environ.get("RAGOTA_API_KEY"))
    ap.add_argument("--work", default=os.environ.get("EVAL_WORK", DEFAULT_WORK),
                    help="throwaway directory for the database, the BM25 index and the logs")
    ap.add_argument("--work-subdir",
                    help="name the subdirectory of --work and the Qdrant collection (default: the label). "
                         "The collection prefix is derived from this name, not from --work, so two runs of "
                         "the same label on one machine share a collection unless they are named apart")
    ap.add_argument("--keep", action="store_true", help="keep --work after the run (default: keep only on failure)")
    ap.add_argument("--reuse", action="store_true", help="reuse repositories already indexed in --work")
    ap.add_argument("--no-index", action="store_true", help="do not index; assume --server already has the repos")
    ap.add_argument("--index-timeout", type=int, default=7200, help="seconds to wait for one repository")
    ap.add_argument("--index-workers", type=int, default=4,
                    help="parallel indexing workers (default 4). Lower it on a machine that would "
                         "otherwise swap: the parse stage holds a file's AST per worker, and the "
                         "three big repositories are where that adds up")

    ap.add_argument("--variant", action="append", default=[],
                    help="config overlay: %s (repeatable)" % ", ".join(sorted(ev.VARIANTS)))
    ap.add_argument("--mode", default="keyword", help="search mode: keyword, hybrid or semantic")
    ap.add_argument("--limit", type=int, default=20, help="hits requested from /search")
    # 20 is the server's cap and matches --limit, so the two endpoints are
    # scored over lists of the same length. Production callers ask for ~5.
    ap.add_argument("--context-limit", type=int, default=20, help="items requested from /context (server caps at 20)")
    ap.add_argument("--hops", type=int, default=1, help="graph expansion depth for /context")
    ap.add_argument("--no-context", action="store_true", help="skip /context and only measure /search")

    ap.add_argument("--rerank-url", help="rerank service base url (variant: rerank)")
    ap.add_argument("--rerank-model", default="", help="rerank model name")
    ap.add_argument("--rerank-top-n", type=int, default=50, help="candidates fed to the reranker")
    ap.add_argument("--rerank-instruction", default="", help="instruction for instruction-aware rerankers")
    ap.add_argument("--qdrant-url", help="qdrant url (variants: window, cards)")
    ap.add_argument("--embed-provider", default="ollama", help="embedding provider (variants: window, cards)")
    ap.add_argument("--embed-model", default="", help="embedding model (variants: window, cards)")
    ap.add_argument("--embed-url", default="", help="embedding endpoint override")
    ap.add_argument("--embed-dimensions", type=int, default=0,
                    help="embedding dimensions override for models outside the built-in table")
    ap.add_argument("--embed-query-instruction", default="",
                    help="query-side instruction for instruction-aware embedders (variant: qinstr)")
    ap.add_argument("--vector-weight", type=float, default=0.0,
                    help="the vector leg's share under convex fusion, 0..1 (variant: convex)")
    ap.add_argument("--split-boost", type=float, default=0.0,
                    help="weight of the code-aware clause against the literal one (variant: split)")
    ap.add_argument("--path-boost", type=float, default=0.0,
                    help="weight of the path clause against the text (variant: paths; 0 = the server's default)")
    ap.add_argument("--assistant-provider", default="ollama", help="assistant provider (variant: rewrite)")
    ap.add_argument("--assistant-url", help="assistant base url (variant: rewrite)")
    ap.add_argument("--assistant-model", default="", help="assistant model (variant: rewrite)")


def options_from(args):
    return {
        "rerank_url": args.rerank_url,
        "rerank_model": args.rerank_model,
        "rerank_top_n": args.rerank_top_n,
        "rerank_instruction": args.rerank_instruction,
        "qdrant_url": args.qdrant_url,
        "embed_provider": args.embed_provider,
        "embed_model": args.embed_model,
        "embed_url": args.embed_url,
        "embed_dimensions": args.embed_dimensions,
        "embed_query_instruction": args.embed_query_instruction,
        "path_boost": getattr(args, "path_boost", 0.0),
        "split_boost": getattr(args, "split_boost", 0.0),
        "vector_weight": getattr(args, "vector_weight", 0.0),
        "assistant_provider": args.assistant_provider,
        "assistant_url": args.assistant_url,
        "assistant_model": args.assistant_model,
        "exclude_tests": getattr(args, "exclude_tests", False),
    }


def label_of(args):
    parts = list(args.variant) or ["base"]
    return "%s/%s" % ("+".join(parts), args.mode)


def execute(args, queries=None):
    """Index, ask, score. Returns the result document written by --out."""
    queries = queries if queries is not None else ev.load_queries(
        args.queries, repos=set(args.repo) or None, shapes=set(args.shape) or None,
        ids=set(args.id) or None, scopes=set(args.scope) or None)
    if not queries:
        raise SystemExit("no queries selected")

    corpus = os.path.abspath(args.corpus)
    # A cross-repository question is asked of several repositories, so the run
    # has to index every one of them: the repository holding the answer is not
    # enough, and a question asked of a repository that was never indexed is a
    # zero that has nothing to do with retrieval.
    needed = ev.repos_needed(queries)
    # A cross-repository question is asked over checkouts its `repo` field does
    # not name, so a run filtered to one small repository can silently pull in
    # a 40k-file one and take an hour. Say so before indexing rather than after.
    named = {q.repo for q in queries}
    extra = [n for n in needed if n not in named]
    if extra:
        print("note: %d cross-repository question(s) also need %s"
              % (sum(1 for q in queries if set(q.search_repos) - named), ", ".join(extra)),
              flush=True)

    started = time.time()
    session = ev.open_session(REPO_ROOT, args, needed, options_from(args), label_of(args))
    try:
        rows = ask_all(session.api, queries, session.indexed, args)
    finally:
        session.close()

    doc = {
        "meta": {
            "created": time.strftime("%Y-%m-%dT%H:%M:%S"),
            "label": label_of(args),
            "variants": list(args.variant) or ["base"],
            "mode": args.mode,
            "limit": args.limit,
            "context_limit": args.context_limit,
            "hops": args.hops,
            "binary": session.binary,
            "binary_version": ev.binary_version(session.binary) if session.binary != "(external)" else "",
            "server": session.url,
            "corpus": corpus,
            "queries": os.path.abspath(args.queries),
            "query_count": len(queries),
            "repos": session.indexed,
            "index_seconds": session.index_seconds,
            "total_seconds": round(time.time() - started, 1),
        },
        "queries": rows,
    }
    doc["totals"] = summarize(rows)
    return doc


def ask_all(api, queries, indexed, args):
    # The query set names corpus checkouts; the API speaks repository ids.
    names = {info["id"]: name for name, info in indexed.items()}
    rows = []
    for i, q in enumerate(queries, 1):
        # Empty means "all": the request carries no repository filter and the
        # question is asked of everything this run indexed.
        repo_ids = [indexed[name]["id"] for name in q.search_repos] or None
        row = {"id": q.id, "repo": q.repo, "shape": q.shape, "scope": q.scope, "query": q.query,
               "search_repos": q.search_repos, "expect_file": q.expect_file,
               "expect_symbol": q.expect_symbol, "expect_line": q.expect_line,
               "alt_files": q.alt_files}

        t0 = time.time()
        try:
            res = api.search(q.query, repos=repo_ids, mode=args.mode, limit=args.limit)
            paths = ev.search_paths(res, names)
            row["search"] = ev.score_one(paths, q)
            row["search"]["span_rank"] = ev.span_rank(ev.spans(res, "hits", names), q)
            row["search"]["returned"] = len(paths)
            row["search"]["top"] = ev.dedup_paths(paths)[:10]
        except RuntimeError as err:
            row["search"] = {"error": str(err), "rank": 0, "mrr": 0.0, "ndcg@10": 0.0,
                             "span_rank": 0, "returned": 0, "top": []}
            for k in ev.RECALL_KS:
                row["search"]["recall@%d" % k] = 0.0
        row["search"]["ms"] = int((time.time() - t0) * 1000)

        if not args.no_context:
            t0 = time.time()
            try:
                res = api.context(q.query, repos=repo_ids, mode=args.mode,
                                  limit=args.context_limit, hops=args.hops)
                paths = ev.context_paths(res, names)
                row["context"] = ev.score_one(paths, q)
                row["context"]["span_rank"] = ev.span_rank(ev.spans(res, "items", names), q)
                row["context"]["returned"] = len(paths)
                row["context"]["top"] = ev.dedup_paths(paths)[:10]
                if res.get("rewritten_query"):
                    row["context"]["rewritten_query"] = res["rewritten_query"]
            except RuntimeError as err:
                row["context"] = {"error": str(err), "rank": 0, "mrr": 0.0, "ndcg@10": 0.0,
                                  "span_rank": 0, "returned": 0, "top": []}
                for k in ev.RECALL_KS:
                    row["context"]["recall@%d" % k] = 0.0
            row["context"]["ms"] = int((time.time() - t0) * 1000)

        rows.append(row)
        mark = "." if row["search"]["rank"] else "x"
        sys.stdout.write(mark)
        if i % 50 == 0 or i == len(queries):
            sys.stdout.write(" %d/%d\n" % (i, len(queries)))
        sys.stdout.flush()
    return rows


def summarize(rows):
    """Aggregate per endpoint, per shape and per repository."""
    out = {}
    for endpoint in ("search", "context"):
        present = [r[endpoint] for r in rows if endpoint in r]
        if not present:
            continue
        block = {"all": ev.aggregate(present)}
        by_shape, by_repo, by_scope = {}, {}, {}
        for row in rows:
            if endpoint not in row:
                continue
            by_shape.setdefault(row["shape"], []).append(row[endpoint])
            by_repo.setdefault(row["repo"], []).append(row[endpoint])
            by_scope.setdefault(row.get("scope") or "in-repo", []).append(row[endpoint])
        block["by_shape"] = {k: ev.aggregate(v) for k, v in sorted(by_shape.items())}
        block["by_repo"] = {k: ev.aggregate(v) for k, v in sorted(by_repo.items())}
        block["by_scope"] = {k: ev.aggregate(v) for k, v in sorted(by_scope.items())}
        block["median_ms"] = _median([s.get("ms", 0) for s in present])
        block["errors"] = sum(1 for s in present if s.get("error"))
        # A repository whose requests fail scores zero on every metric and is
        # indistinguishable from one that simply retrieves badly. Keep the
        # failures separate so nobody reads a broken index as a ranking result.
        failed = {}
        for row in rows:
            err = (row.get(endpoint) or {}).get("error")
            if err:
                failed.setdefault(row["repo"], {"count": 0, "example": err})["count"] += 1
        block["failed_repos"] = failed
        out[endpoint] = block
    return out


def _median(values):
    values = sorted(values)
    if not values:
        return 0
    mid = len(values) // 2
    return values[mid] if len(values) % 2 else (values[mid - 1] + values[mid]) // 2


def report(doc, per_query=True):
    meta, totals = doc["meta"], doc["totals"]
    lines = []
    lines.append("")
    lines.append("%s  —  %d queries, %d repositories, indexed in %ss"
                 % (meta["label"], meta["query_count"], len(meta["repos"]), meta["index_seconds"]))
    # A question with `repos: "all"` is only as hard as the set it was asked
    # of, so a run that contains one has to say what "all" meant.
    asked_all = [r["id"] for r in doc["queries"] if not r.get("search_repos", ["x"])]
    if asked_all:
        lines.append('  %d question(s) asked with no repository filter — "all" here means %s: %s'
                     % (len(asked_all), ", ".join(sorted(meta["repos"])), ", ".join(asked_all)))
    lines.append("")

    for endpoint in ("search", "context"):
        if endpoint not in totals:
            continue
        block = totals[endpoint]
        headers = ["/%s" % endpoint, "n"] + ev.METRIC_KEYS + ["span@10", "missed"]
        rows = []
        for shape, agg in block["by_shape"].items():
            rows.append([shape, agg["n"]] + [ev.fmt(agg[k]) for k in ev.METRIC_KEYS]
                        + [ev.fmt(agg["span@10"]), agg["missed"]])
        rows.append(["-- total", block["all"]["n"]]
                    + [ev.fmt(block["all"][k]) for k in ev.METRIC_KEYS]
                    + [ev.fmt(block["all"]["span@10"]), block["all"]["missed"]])
        lines.append(ev.table(rows, headers))
        lines.append("")
        # How far the question reaches is a different cut from what it asks;
        # printed only when the selection actually mixes scopes, so a run of
        # the single-repository set looks exactly as it always did.
        by_scope = block.get("by_scope") or {}
        if len(by_scope) > 1:
            rows = [[scope, agg["n"]] + [ev.fmt(agg[k]) for k in ev.METRIC_KEYS]
                    + [ev.fmt(agg["span@10"]), agg["missed"]]
                    for scope, agg in by_scope.items()]
            lines.append(ev.table(rows, ["/%s by scope" % endpoint, "n"]
                                  + ev.METRIC_KEYS + ["span@10", "missed"]))
            lines.append("")
        rows = [[repo, agg["n"]] + [ev.fmt(agg[k]) for k in ev.METRIC_KEYS]
                for repo, agg in block["by_repo"].items()]
        lines.append(ev.table(rows, ["/%s by repo" % endpoint, "n"] + ev.METRIC_KEYS))
        lines.append("  median latency %d ms%s"
                     % (block["median_ms"], ", %d request errors" % block["errors"] if block["errors"] else ""))
        for repo, info in sorted(block.get("failed_repos", {}).items()):
            lines.append("  ! %s: %d requests failed — scored zero for the wrong reason: %s"
                         % (repo, info["count"], info["example"][:120]))
        lines.append("")

    if per_query:
        headers = ["query", "shape", "repo", "s.rank", "s.span", "c.rank", "expected"]
        rows = []
        for row in doc["queries"]:
            srank = row["search"]["rank"] or "-"
            sspan = row["search"].get("span_rank") or "-"
            crank = (row.get("context") or {}).get("rank", "") or ("-" if "context" in row else "")
            rows.append([row["id"], row["shape"], row["repo"], srank, sspan, crank, row["expect_file"]])
        lines.append(ev.table(rows, headers, aligns=["<", "<", "<", ">", ">", ">", "<"]))
        lines.append("")
        lines.append("s.rank = rank of the expected file in /search; s.span = rank of the first chunk that")
        lines.append("actually covers the expected line; c.rank = rank in /context. '-' means not retrieved.")
        lines.append("")
    return "\n".join(lines)


def misses(doc, endpoint="search", top=3):
    """What came back instead, for the questions whose answer never appeared.

    An aggregate says retrieval is at 0.44; this says what it returned for the
    other 0.56, which is where the next improvement is designed.
    """
    lines = ["", "unanswered by /%s — expected file never returned:" % endpoint, ""]
    for row in doc["queries"]:
        score = row.get(endpoint) or {}
        if score.get("rank"):
            continue
        lines.append("  %s  [%s]" % (row["id"], row["shape"]))
        lines.append("    q:        %s" % row["query"])
        if (row.get("scope") or "in-repo") != "in-repo":
            lines.append("    asked of: %s  [%s]"
                         % (", ".join(row.get("search_repos") or []) or "everything indexed",
                            row["scope"]))
        lines.append("    expected: %s/%s%s" % (row["repo"], row["expect_file"],
                                                ":%d" % row["expect_line"] if row["expect_line"] else ""))
        if score.get("error"):
            lines.append("    error:    %s" % score["error"][:120])
        for i, path in enumerate(score.get("top", [])[:top], 1):
            lines.append("    got %d:    %s" % (i, path))
        lines.append("")
    return "\n".join(lines)


def write_tsv(doc, path):
    cols = ["id", "repo", "shape", "scope", "search_rank", "search_span_rank", "search_mrr", "search_ndcg10",
            "context_rank", "context_span_rank", "context_mrr", "context_ndcg10",
            "search_ms", "context_ms", "expect_file", "expect_symbol", "query"]
    with open(path, "w", encoding="utf-8") as fh:
        fh.write("\t".join(cols) + "\n")
        for row in doc["queries"]:
            s = row.get("search", {})
            c = row.get("context", {})
            fh.write("\t".join(str(v) for v in [
                row["id"], row["repo"], row["shape"], row.get("scope") or "in-repo",
                s.get("rank", 0), s.get("span_rank", 0), ev.fmt(s.get("mrr", 0.0)), ev.fmt(s.get("ndcg@10", 0.0)),
                c.get("rank", 0), c.get("span_rank", 0), ev.fmt(c.get("mrr", 0.0)), ev.fmt(c.get("ndcg@10", 0.0)),
                s.get("ms", 0), c.get("ms", 0),
                row["expect_file"], row["expect_symbol"], row["query"].replace("\t", " "),
            ]) + "\n")


def do_validate(args):
    queries = ev.load_queries(args.queries, repos=set(args.repo) or None,
                              shapes=set(args.shape) or None, ids=set(args.id) or None,
                              scopes=set(args.scope) or None)
    errors, warnings = ev.validate_queries(queries, os.path.abspath(args.corpus), strict_lines=args.strict_lines)
    by_shape, by_repo, by_scope = {}, {}, {}
    for q in queries:
        by_shape[q.shape] = by_shape.get(q.shape, 0) + 1
        by_repo[q.repo] = by_repo.get(q.repo, 0) + 1
        by_scope[q.scope] = by_scope.get(q.scope, 0) + 1
    print("%d queries over %d repositories" % (len(queries), len(by_repo)))
    print("  by shape: " + ", ".join("%s=%d" % kv for kv in sorted(by_shape.items())))
    print("  by scope: " + ", ".join("%s=%d" % kv for kv in sorted(by_scope.items())))
    print("  by repo:  " + ", ".join("%s=%d" % kv for kv in sorted(by_repo.items())))
    for w in warnings:
        print("warning: %s" % w)
    for e in errors:
        print("ERROR:   %s" % e)
    if errors:
        print("\n%d error(s): the ground truth no longer matches the corpus sources." % len(errors))
        return 1
    print("\nevery expected file exists and every anchor is on the recorded line.")
    return 0


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    add_common_args(ap)
    ap.add_argument("--validate", action="store_true",
                    help="check the query set against the corpus sources and exit (no server, no index)")
    ap.add_argument("--strict-lines", action="store_true",
                    help="with --validate, treat a drifted line number as an error")
    ap.add_argument("--out", help="write the full result JSON here (default: <work>/<label>.json)")
    ap.add_argument("--tsv", help="write the per-query table here (default: next to --out)")
    ap.add_argument("--quiet", action="store_true", help="totals only, no per-query table")
    ap.add_argument("--misses", action="store_true",
                    help="after the tables, print each unanswered question and what came back instead")
    args = ap.parse_args()

    if args.validate:
        return do_validate(args)

    doc = execute(args)
    out = args.out or os.path.join(args.work, "%s.json" % ev.slugify(label_of(args)))
    os.makedirs(os.path.dirname(os.path.abspath(out)), exist_ok=True)
    with open(out, "w", encoding="utf-8") as fh:
        json.dump(doc, fh, indent=2, sort_keys=True)
    tsv = args.tsv or (os.path.splitext(out)[0] + ".tsv")
    write_tsv(doc, tsv)

    print(report(doc, per_query=not args.quiet))
    if args.misses:
        print(misses(doc))
    print("wrote %s and %s" % (out, tsv))
    return 0


if __name__ == "__main__":
    sys.exit(main())
