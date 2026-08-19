#!/usr/bin/env python3
"""Run the query set twice and print the difference, so a ranking change can be
defended or rejected with a number instead of an anecdote.

Two configurations of the same binary:

    ./compare.py --corpus /data/corpus --b-variant rerank --rerank-url http://localhost:8090
    ./compare.py --corpus /data/corpus --a-mode keyword --b-mode hybrid --no-reindex
    ./compare.py --corpus /data/corpus --a-variant window --b-variant cards \
                 --qdrant-url http://localhost:6333 --embed-model nomic-embed-text
    ./compare.py --corpus /data/corpus --b-variant rewrite \
                 --assistant-url http://localhost:11434 --assistant-model qwen2.5

Two binaries — the before/after of a change:

    ./compare.py --corpus /data/corpus --a-binary /tmp/before/ragota --b-binary /tmp/after/ragota

Two runs that already happened:

    ./compare.py --a-results base.json --b-results rerank.json

A and B are the same query set asked in the same order, so the per-query table
at the bottom is the whole point: it names the questions that moved and in
which direction. Aggregates hide a change that helped five queries and broke
five others.
"""

import argparse
import copy
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import evallib as ev  # noqa: E402
import run as runner  # noqa: E402


def side_args(args, side):
    """Build a run.py argument namespace for one side of the comparison."""
    ns = copy.deepcopy(args)
    for field in ("binary", "server", "variant", "mode"):
        value = getattr(args, "%s_%s" % (side, field))
        if value not in (None, [], ""):
            setattr(ns, field, value)
    ns.variant = list(ns.variant or [])
    ns.out = None
    ns.tsv = None
    ns.quiet = True
    ns.validate = False
    return ns


def label(doc):
    return doc["meta"]["label"]


def delta_table(a, b, endpoint):
    """Aggregate deltas per question shape, then the total."""
    if endpoint not in a["totals"] or endpoint not in b["totals"]:
        return None
    ablock, bblock = a["totals"][endpoint], b["totals"][endpoint]
    keys = ["recall@1", "recall@5", "recall@10", "mrr", "ndcg@10"]
    headers = ["/%s" % endpoint, "n"]
    for k in keys:
        headers += ["A " + k, "B " + k, "delta"]
    rows = []
    shapes = sorted(set(ablock["by_shape"]) | set(bblock["by_shape"]))
    for shape in shapes:
        aa = ablock["by_shape"].get(shape, {})
        bb = bblock["by_shape"].get(shape, {})
        row = [shape, aa.get("n", bb.get("n", 0))]
        for k in keys:
            av, bv = aa.get(k, 0.0), bb.get(k, 0.0)
            row += [ev.fmt(av), ev.fmt(bv), signed(bv - av)]
        rows.append(row)
    row = ["-- total", ablock["all"]["n"]]
    for k in keys:
        av, bv = ablock["all"].get(k, 0.0), bblock["all"].get(k, 0.0)
        row += [ev.fmt(av), ev.fmt(bv), signed(bv - av)]
    rows.append(row)
    return ev.table(rows, headers)


def signed(value, places=3):
    if abs(value) < 10 ** (-places) / 2:
        return "."
    return ("%+." + str(places) + "f") % value


def moved(a, b, endpoint):
    """Queries whose rank changed, worst regression first."""
    bidx = {row["id"]: row for row in b["queries"]}
    out = []
    for arow in a["queries"]:
        brow = bidx.get(arow["id"])
        if not brow or endpoint not in arow or endpoint not in brow:
            continue
        ar = arow[endpoint].get("rank", 0)
        br = brow[endpoint].get("rank", 0)
        if ar == br:
            continue
        # Not retrieved sorts behind every retrieved rank.
        akey = ar if ar else 10 ** 6
        bkey = br if br else 10 ** 6
        out.append((bkey - akey, arow["id"], arow["shape"], arow["repo"],
                    ar or "-", br or "-", arow["expect_file"]))
    out.sort(key=lambda t: -t[0])
    return out


def compare(a, b, endpoints=("search", "context"), show_moved=True):
    lines = ["", "A = %s   (%s)" % (label(a), a["meta"]["created"]),
             "B = %s   (%s)" % (label(b), b["meta"]["created"]), ""]

    aids = [r["id"] for r in a["queries"]]
    bids = [r["id"] for r in b["queries"]]
    if aids != bids:
        only_a = sorted(set(aids) - set(bids))
        only_b = sorted(set(bids) - set(aids))
        lines.append("warning: the two runs did not ask the same questions"
                     " (%d only in A, %d only in B); comparing the intersection."
                     % (len(only_a), len(only_b)))
        lines.append("")

    # A request that failed scores zero on every metric. Left unsaid, that
    # reads as a ranking regression instead of a broken server.
    for endpoint in endpoints:
        for side, doc in (("A", a), ("B", b)):
            block = doc["totals"].get(endpoint) or {}
            for repo, info in sorted((block.get("failed_repos") or {}).items()):
                lines.append("warning: %s /%s: %d requests failed on %s — those queries score zero"
                             " for a reason that is not ranking: %s"
                             % (side, endpoint, info["count"], repo, info["example"][:100]))
    if lines and lines[-1].startswith("warning:"):
        lines.append("")

    for endpoint in endpoints:
        rendered = delta_table(a, b, endpoint)
        if rendered:
            lines.append(rendered)
            lines.append("")

    if not show_moved:
        return "\n".join(lines)

    for endpoint in endpoints:
        rows = moved(a, b, endpoint)
        if not rows:
            if endpoint in a["totals"] and endpoint in b["totals"]:
                lines.append("/%s: no query changed rank." % endpoint)
                lines.append("")
            continue
        regressions = [r for r in rows if r[0] > 0]
        improvements = [r for r in rows if r[0] < 0]
        lines.append("/%s: %d queries moved — %d worse, %d better"
                     % (endpoint, len(rows), len(regressions), len(improvements)))
        table_rows = [[r[1], r[2], r[3], r[4], r[5], r[6]] for r in rows]
        lines.append(ev.table(table_rows,
                              ["query", "shape", "repo", "A rank", "B rank", "expected"],
                              aligns=["<", "<", "<", ">", ">", "<"]))
        lines.append("")
    return "\n".join(lines)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    runner.add_common_args(ap)
    ap.add_argument("--a-results", help="use this result JSON as side A instead of running it")
    ap.add_argument("--b-results", help="use this result JSON as side B instead of running it")
    ap.add_argument("--a-binary", help="server binary for side A (default: --binary)")
    ap.add_argument("--b-binary", help="server binary for side B (default: --binary)")
    ap.add_argument("--a-server", help="already-running server for side A")
    ap.add_argument("--b-server", help="already-running server for side B")
    ap.add_argument("--a-variant", action="append", default=[], help="config overlay for side A")
    ap.add_argument("--b-variant", action="append", default=[], help="config overlay for side B")
    ap.add_argument("--a-mode", help="search mode for side A (default: --mode)")
    ap.add_argument("--b-mode", help="search mode for side B (default: --mode)")
    ap.add_argument("--no-reindex", action="store_true",
                    help="both sides share one indexed workdir; only valid when they differ solely at query time")
    ap.add_argument("--out-dir", help="write both result JSONs here")
    ap.add_argument("--no-moved", action="store_true", help="aggregates only, no per-query movement table")
    args = ap.parse_args()

    docs = {}
    for side in ("a", "b"):
        existing = getattr(args, "%s_results" % side)
        if existing:
            with open(existing, encoding="utf-8") as fh:
                docs[side] = json.load(fh)
            continue
        ns = side_args(args, side)
        if args.no_reindex:
            # One database, indexed once: the sides must then differ only in
            # query-time settings (mode, reranker), never in what was indexed.
            ns.work_subdir = "shared"
            ns.reuse = side == "b" or args.reuse
        print("=== running side %s: %s" % (side.upper(), runner.label_of(ns)), flush=True)
        docs[side] = runner.execute(ns)
        if args.out_dir:
            os.makedirs(args.out_dir, exist_ok=True)
            path = os.path.join(args.out_dir, "%s-%s.json" % (side, ev.slugify(runner.label_of(ns))))
            with open(path, "w", encoding="utf-8") as fh:
                json.dump(docs[side], fh, indent=2, sort_keys=True)
            runner.write_tsv(docs[side], os.path.splitext(path)[0] + ".tsv")
            print("wrote %s" % path)

    print(compare(docs["a"], docs["b"], show_moved=not args.no_moved))
    return 0


if __name__ == "__main__":
    sys.exit(main())
