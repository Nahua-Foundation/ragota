#!/usr/bin/env python3
"""Score the graph expansion that `/context` returns around every hit.

    ./related.py --corpus /data/corpus                    # index, ask, score
    ./related.py --corpus /data/corpus --reuse --work-subdir shared
    ./related.py --corpus /data/corpus --scope cross-service --misses

`run.py` scores `items[].hit` and stops there. Each item also carries a
`related` list — the units the code graph reaches from the unit the hit landed
in (callers, callees, contracts and their far sides, `internal/graph`
`Expand`) — and that list is a large part of what an LLM is actually handed.
Until this script existed a wrong or empty `related` list cost nothing.

Three numbers, deliberately separate:

  companion   For a question that carries `expect_related` — the second file a
              complete answer needs, the other end of the contract it is about
              — does the related list of the item that *is* the answer name
              that file? Reported conditional on the answer being retrieved,
              because an expansion cannot be blamed for a hit that never
              arrived, and next to the control: whether retrieval had already
              returned the second file on its own, in which case the graph
              added nothing by naming it.

  reach       Over every question, whether the answering file is in the hits,
              only in a related list (a rescue: the expansion supplied an
              answer that ranking did not), or in neither.

  shape       What the lists look like at all: how many items get an empty one,
              how large they are, how much of them points outside the file the
              reader already has. A metric that only rewarded recall would be
              satisfied by returning the entire graph; these are what make that
              visible, and they are descriptive rather than scored.

What this does not measure is written down in tools/eval/README.md, in the
section "Scoring the graph expansion" — most importantly that there is no
precision term: 24 irrelevant related units score exactly like one right one.
"""

import argparse
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import evallib as ev  # noqa: E402
import run as runner  # noqa: E402

REPO_ROOT = os.path.normpath(os.path.join(ev.HERE, "..", ".."))


def execute(args, queries=None):
    queries = queries if queries is not None else ev.load_queries(
        args.queries, repos=set(args.repo) or None, shapes=set(args.shape) or None,
        ids=set(args.id) or None, scopes=set(args.scope) or None)
    if not queries:
        raise SystemExit("no queries selected")

    started = time.time()
    label = "related/%s" % runner.label_of(args)
    session = ev.open_session(REPO_ROOT, args, ev.repos_needed(queries),
                              runner.options_from(args), runner.label_of(args))
    rows = []
    try:
        names = session.names
        for i, q in enumerate(queries, 1):
            row = {"id": q.id, "repo": q.repo, "shape": q.shape, "scope": q.scope,
                   "query": q.query, "expect_file": q.expect_file,
                   "expect_related": list(q.expect_related)}
            try:
                res = session.api.context(q.query, repos=session.repo_ids(q), mode=args.mode,
                                          limit=args.context_limit, hops=args.hops)
                items = ev.context_items(res, names)
                row["score"] = ev.score_expansion(items, q)
                # What the answer item was actually given, so a zero can be
                # read rather than guessed at.
                answer = next((it for it in items if it["key"] in set(q.acceptable_keys)), None)
                row["at_answer"] = ([{"key": r["key"], "via": r["via"], "direction": r["direction"],
                                      "name": r["name"]} for r in answer["related"]]
                                    if answer else [])
                row["answer_service"] = answer["service"] if answer else ""
                row["top_related"] = ev.related_keys(items, upto=3)
            except RuntimeError as err:
                row["error"] = str(err)
                row["score"] = ev.score_expansion([], q)
            rows.append(row)
            sys.stdout.write("." if row["score"]["reach"] != "miss" else "x")
            if i % 50 == 0 or i == len(queries):
                sys.stdout.write(" %d/%d\n" % (i, len(queries)))
            sys.stdout.flush()
    finally:
        session.close()

    doc = {
        "meta": {
            "created": time.strftime("%Y-%m-%dT%H:%M:%S"),
            "label": label,
            "mode": args.mode,
            "context_limit": args.context_limit,
            "hops": args.hops,
            "binary": session.binary,
            "binary_version": ev.binary_version(session.binary) if session.binary != "(external)" else "",
            "corpus": os.path.abspath(args.corpus),
            "queries": os.path.abspath(args.queries),
            "query_count": len(queries),
            "annotated": sum(1 for q in queries if q.expect_related),
            "repos": session.indexed,
            "index_seconds": session.index_seconds,
            "total_seconds": round(time.time() - started, 1),
        },
        "queries": rows,
    }
    doc["totals"] = summarize(rows)
    return doc


def summarize(rows):
    scores = [r["score"] for r in rows]
    out = {"all": ev.aggregate_expansion(scores), "via": {}}
    for name, keyfn in (("by_shape", lambda r: r["shape"]),
                        ("by_scope", lambda r: r.get("scope") or "in-repo"),
                        ("by_repo", lambda r: r["repo"])):
        groups = {}
        for row in rows:
            groups.setdefault(keyfn(row), []).append(row["score"])
        out[name] = {k: ev.aggregate_expansion(v) for k, v in sorted(groups.items())}
    for score in scores:
        for via, n in score["via"].items():
            out["via"][via] = out["via"].get(via, 0) + n
    out["errors"] = sum(1 for r in rows if r.get("error"))
    return out


def report(doc, per_query=True):
    meta, totals = doc["meta"], doc["totals"]
    lines = ["", "%s  —  %d queries (%d with a companion file), %d repositories, hops %d"
             % (meta["label"], meta["query_count"], meta["annotated"], len(meta["repos"]), meta["hops"]), ""]

    # 1. What comes back at all.
    headers = ["expansion", "n", "items", "rel/item", "empty items", "off-file", "off-service"]
    rows = []
    for shape, agg in list(totals["by_shape"].items()) + [("-- total", totals["all"])]:
        rows.append([shape, agg["n"], agg["items"], ev.fmt(agg["related_per_item"], 2),
                     ev.fmt(agg["empty_items"]), ev.fmt(agg["off_file"]), ev.fmt(agg["off_service"])])
    lines.append(ev.table(rows, headers))
    lines.append("  empty items = share of returned items whose related list is empty;"
                 " off-file = share of related")
    lines.append("  units naming a file other than the item's own;"
                 " off-service = other service. Neither is scored.")
    if totals["via"]:
        lines.append("  edge kinds: " + ", ".join("%s=%d" % kv for kv in
                                                  sorted(totals["via"].items(), key=lambda kv: -kv[1])))
    lines.append("")

    # 2. The companion file.
    headers = ["companion", "n", "answer found", "at the answer", "anywhere", "already a hit", "absent"]
    rows = []
    for name, block in (("by_shape", totals["by_shape"]), ("by_scope", totals["by_scope"])):
        for key, agg in block.items():
            if agg["companion_n"]:
                rows.append([key, agg["companion_n"], agg["companion_reached"],
                             ev.fmt(agg["companion_at_answer"]), ev.fmt(agg["companion_in_any_related"]),
                             ev.fmt(agg["companion_in_hits"]), agg["companion_absent"]])
        rows.append(["--", "", "", "", "", "", ""])
    agg = totals["all"]
    rows.append(["-- total", agg["companion_n"], agg["companion_reached"],
                 ev.fmt(agg["companion_at_answer"]), ev.fmt(agg["companion_in_any_related"]),
                 ev.fmt(agg["companion_in_hits"]), agg["companion_absent"]])
    lines.append(ev.table(rows, headers))
    lines.append("  answer found = questions whose answering file was retrieved at all — the ceiling, and")
    lines.append("  the denominator of all three rates: 'at the answer' is the share of those whose answer")
    lines.append("  item names the companion, 'anywhere' the share whose package names it in any related")
    lines.append("  list, 'already a hit' the control — retrieval returned the second file without the")
    lines.append("  graph. 'absent' counts the packages, of those same questions, that hold it nowhere.")
    lines.append("")

    # 3. Reach.
    headers = ["reach", "n", "in hits", "rescued by related", "neither"]
    rows = []
    for key, agg in list(totals["by_scope"].items()) + [("-- total", totals["all"])]:
        rows.append([key, agg["n"], agg["hit"], agg["rescue"], agg["miss"]])
    lines.append(ev.table(rows, headers))
    lines.append("  rescued = the answering file appears in no hit and in some item's related list.")
    lines.append("")

    if per_query:
        headers = ["query", "shape", "scope", "reach", "companion", "rel/item"]
        rows = []
        for row in doc["queries"]:
            s = row["score"]
            if not s["has_companion"]:
                comp = "-"
            elif not s.get("companion_reached"):
                comp = "unreached"
            elif s.get("companion_at_answer"):
                comp = "at answer"
            elif s.get("companion_in_any_related"):
                comp = "elsewhere"
            elif s.get("companion_in_hits"):
                comp = "hit only"
            else:
                comp = "absent"
            rows.append([row["id"], row["shape"], row["scope"], s["reach"], comp,
                         ev.fmt(s["n_related"] / s["items"], 2) if s["items"] else "0.00"])
        lines.append(ev.table(rows, headers, aligns=["<", "<", "<", "<", "<", ">"]))
        lines.append("")
    return "\n".join(lines)


def misses(doc):
    """For every question whose companion is absent, what the answer item's
    related list held instead — which is where the next fix is designed."""
    lines = ["", "companion file absent — the answer item's related list, in full:", ""]
    for row in doc["queries"]:
        score = row["score"]
        if not score["has_companion"] or score.get("companion_in_any_related") or score.get("companion_in_hits"):
            continue
        lines.append("  %s  [%s/%s]" % (row["id"], row["shape"], row["scope"]))
        lines.append("    q:         %s" % row["query"])
        lines.append("    wanted:    %s" % ", ".join(row["expect_related"]))
        if not score.get("companion_reached"):
            lines.append("    (the answering file was never retrieved, so nothing expanded from it)")
        elif not row["at_answer"]:
            lines.append("    got:       nothing — the related list is empty")
        for rel in row["at_answer"][:8]:
            lines.append("    got:       %s  (%s, %s)" % (rel["key"], rel["via"], rel["direction"]))
        lines.append("")
    return "\n".join(lines)


def write_tsv(doc, path):
    cols = ["id", "repo", "shape", "scope", "reach", "items", "related", "empty_items",
            "has_companion", "companion_reached", "companion_at_answer",
            "companion_in_any_related", "companion_in_hits", "expect_file"]
    with open(path, "w", encoding="utf-8") as fh:
        fh.write("\t".join(cols) + "\n")
        for row in doc["queries"]:
            s = row["score"]
            fh.write("\t".join(str(v) for v in [
                row["id"], row["repo"], row["shape"], row["scope"], s["reach"], s["items"],
                s["n_related"], s["empty_items"], int(bool(s["has_companion"])),
                int(bool(s.get("companion_reached"))), int(bool(s.get("companion_at_answer"))),
                int(bool(s.get("companion_in_any_related"))), int(bool(s.get("companion_in_hits"))),
                row["expect_file"],
            ]) + "\n")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    runner.add_common_args(ap)
    ap.add_argument("--out", help="write the full result JSON here (default: <work>/related-<label>.json)")
    ap.add_argument("--tsv", help="write the per-query table here (default: next to --out)")
    ap.add_argument("--quiet", action="store_true", help="totals only, no per-query table")
    ap.add_argument("--misses", action="store_true",
                    help="after the tables, print what the related list held instead of the companion")
    args = ap.parse_args()

    doc = execute(args)
    out = args.out or os.path.join(args.work, "related-%s.json" % ev.slugify(runner.label_of(args)))
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
