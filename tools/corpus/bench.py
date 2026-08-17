#!/usr/bin/env python3
"""Index the benchmark corpus against a running ragota and dump, per
repository, what came out of it: files, AST units by kind, edges by kind, how
many contract edges were resolved, and the coverage summary the server
reports.

    ./clone.sh -d /data/corpus
    ./bench.py --corpus /data/corpus --db ~/.ragota-core/data/ragota.db

The counts come from the metadata database rather than the API because no
endpoint reports per-repo unit and edge kinds; the coverage summary comes from
GET /repos/{id}/coverage. Results are written as one JSON file per repository
plus a summary TSV, so two runs of a changed extractor can be diffed.
"""

import argparse
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import corpuslib as cl  # noqa: E402


def count_repo(db, repo_id):
    """Per-repo counts straight out of the metadata store."""
    units = cl.group_counts(db, "ast_units", repo_id)
    edges = cl.group_counts(db, "edges", repo_id)
    files = cl.group_counts(db, "files", repo_id, column="language")

    resolved = {}
    for kind in cl.CONTRACT_EDGE_KINDS:
        rows = db.query(
            "SELECT COUNT(*) FROM edges WHERE repo_id = %s AND kind = %s AND dst_id <> 0"
            % (cl.quote(repo_id), cl.quote(kind))
        )
        resolved[kind] = int(rows[0][0]) if rows else 0

    return {
        "files_by_language": files,
        "files_total": sum(files.values()),
        "units_by_kind": units,
        "units_total": sum(units.values()),
        "edges_by_kind": edges,
        "edges_total": sum(edges.values()),
        "contract_edges": {k: edges.get(k, 0) for k in cl.CONTRACT_EDGE_KINDS},
        "contract_edges_resolved": resolved,
    }


def bench_one(api, db, repo, path, timeout, force):
    started = time.time()
    registered = api.add_repo(repo.name, path)
    repo_id = registered["id"]

    api.index(repo_id, force=force)
    state = api.wait_idle(repo_id, timeout=timeout)

    result = {
        "name": repo.name,
        "repo_id": repo_id,
        "pattern": repo.pattern,
        "stack": repo.stack,
        "path": path,
        "status": state.get("status"),
        "last_error": state.get("last_error", ""),
        "index_seconds": round(time.time() - started, 1),
    }
    result.update(count_repo(db, repo_id))
    try:
        result["coverage"] = api.coverage(repo_id)
    except RuntimeError as err:
        # An older server without the endpoint is still worth benchmarking;
        # the run then simply has no coverage column.
        result["coverage"] = {"error": str(err)}
    return result


SUMMARY_COLUMNS = [
    "name",
    "pattern",
    "status",
    "index_seconds",
    "files_total",
    "units_total",
    "edges_total",
    "http_call",
    "rpc_call",
    "produces",
    "consumes",
    "writes_to",
    "reads_from",
    "http_candidates",
    "http_edges",
]


def summary_row(res):
    contract = res.get("contract_edges", {})
    cov_http = {}
    for entry in (res.get("coverage") or {}).get("kinds", []) or []:
        if entry.get("kind") == "http":
            cov_http = entry
    row = [
        res["name"],
        res["pattern"],
        res.get("status", ""),
        res.get("index_seconds", ""),
        res.get("files_total", 0),
        res.get("units_total", 0),
        res.get("edges_total", 0),
    ]
    row += [contract.get(k, 0) for k in cl.CONTRACT_EDGE_KINDS]
    row += [cov_http.get("candidates", ""), cov_http.get("edges", "")]
    return [str(v) for v in row]


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--server", default=os.environ.get("RAGOTA_URL", "http://127.0.0.1:8080"))
    ap.add_argument("--api-key", default=os.environ.get("RAGOTA_API_KEY"))
    ap.add_argument("--corpus", default=os.environ.get("CORPUS_DIR", "corpus"),
                    help="directory holding the checkouts (see clone.sh)")
    ap.add_argument("--db", default=os.environ.get("RAGOTA_DB", os.path.expanduser("~/.ragota-core/data/ragota.db")),
                    help="metadata store: sqlite path or postgres DSN")
    ap.add_argument("--out", default="corpus-results", help="directory for the per-repo JSON dumps")
    ap.add_argument("--timeout", type=int, default=7200, help="seconds to wait for one repository")
    ap.add_argument("--no-force", action="store_true", help="do not re-index files whose hash is unchanged")
    ap.add_argument("repos", nargs="*", help="limit the run to these corpus entries")
    args = ap.parse_args()

    repos = cl.load_repos(only=set(args.repos) or None)
    api = cl.API(args.server, args.api_key)
    db = cl.open_db(args.db)
    os.makedirs(args.out, exist_ok=True)

    results = []
    for repo in repos:
        path = os.path.abspath(os.path.join(args.corpus, repo.name))
        if not os.path.isdir(path):
            print("skip    %-32s not cloned" % repo.name)
            continue
        print("index   %-32s %s" % (repo.name, path), flush=True)
        try:
            res = bench_one(api, db, repo, path, args.timeout, force=not args.no_force)
        except Exception as err:  # a repo that breaks must not end the run
            print("FAILED  %-32s %s" % (repo.name, err), file=sys.stderr)
            results.append({"name": repo.name, "pattern": repo.pattern, "status": "bench_error",
                            "last_error": str(err)})
            continue
        with open(os.path.join(args.out, repo.name + ".json"), "w", encoding="utf-8") as fh:
            json.dump(res, fh, indent=2, sort_keys=True)
        results.append(res)
        print("        %-32s files=%d units=%d edges=%d contract=%d in %ss"
              % (repo.name, res["files_total"], res["units_total"], res["edges_total"],
                 sum(res["contract_edges"].values()), res["index_seconds"]), flush=True)

    summary = os.path.join(args.out, "summary.tsv")
    with open(summary, "w", encoding="utf-8") as fh:
        fh.write("\t".join(SUMMARY_COLUMNS) + "\n")
        for res in results:
            if res.get("status") == "bench_error":
                fh.write(res["name"] + "\t" + res.get("pattern", "") + "\tbench_error\n")
                continue
            fh.write("\t".join(summary_row(res)) + "\n")
    db.close()
    print("\nwrote %s" % summary)


if __name__ == "__main__":
    main()
