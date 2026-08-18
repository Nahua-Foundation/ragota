#!/usr/bin/env python3
"""Estimate precision and recall of contract extraction over the corpus.

Both numbers are estimates, deliberately computed from evidence the extractor
did not produce:

  precision — for a sample of contract edges, read the source line the edge
    points at and look for an independent token that a call of that kind
    really happens there (an http/fetch/axios/client call for http, a
    grpc/stub for rpc, a publish/consume for messaging, a SQL verb for db).
    An edge whose own line shows no such token is counted as unsupported.
    This catches edges invented from a name that merely looks like a call.

  recall — scan the indexed sources for literals that look like an outbound
    contract at a call site (URLs and path templates, topic names next to a
    publish/subscribe call, table names inside SQL text) and ask how many of
    those lines carry a contract edge. What is missing here is what an
    operator would call "we did not find it".

Neither is ground truth: the tokens are heuristics, and both directions have
false positives (a URL in a comment, a `client` variable that is not a
client). They are stable across runs, which is what makes them useful — the
question the corpus answers is whether a change moved them.

    ./measure.py --corpus /data/corpus --db ~/.ragota/data/ragota.db
"""

import argparse
import json
import os
import re
import sys
from collections import defaultdict

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import corpuslib as cl  # noqa: E402

# --- precision ---------------------------------------------------------------

# A token on the call site's own line that independently indicates a call of
# this kind. "strong" is enough on its own; "verb" only counts together with a
# quoted path/topic/table literal, because words like get or send are far too
# common to mean anything by themselves.
# RestTemplate's operation names are in the http set for the same reason
# "resttemplate" is: nothing else is called getForEntity. Without them a client
# written entirely in those calls — conductor's is — reads as unsupported on
# every line, and any improvement to Java client extraction looks like a
# precision collapse.
#
# The messaging set had the same defect twice over, and it scored eShop's whole
# RabbitMQ integration layer at 0.000 — 43 correct edges, not one of them
# supported:
#
#   * the tokens were anchored on both sides, and messaging APIs write theirs
#     inside a compound name — BasicPublishAsync, PublishThroughEventBusAsync,
#     AddSubscription, xreadgroup, and "topics" plural. So the messaging
#     fragments below match anywhere in an identifier. Each one is a word that
#     appears in code about moving messages and nowhere else, which is what
#     licenses dropping the boundaries; the other three sets keep theirs,
#     because "from", "get" and "channel" are not that kind of word.
#   * a bus that routes on the message's own type names no verb at all. The
#     line that receives eShop's most-published contract is
#     `public async Task Handle(OrderStatusChangedToPaidIntegrationEvent @event)`
#     and the evidence in it is the type: IntegrationEvent, EventHandler,
#     eventBus, Mediator, Messenger.
MESSAGING_FRAGMENTS = (
    "publish|produce|subscri|consum|enqueue|dequeue|dispatch|kafka|nats|amqp|rabbit|"
    "sqs|sns|pubsub|servicebus|eventbus|eventhub|celery|broker|mediator|messenger|"
    "integrationevent|domainevent|eventhandler|messagehandler|topic|queue|exchange|emit"
)
STRONG = {
    "http": r"\b(https?|fetch|axios|httpclient|webclient|resttemplate|okhttp|resty|requests|urlopen|curl|superagent|got|ky|feign|httpx|restclient|url|uri|endpoint|(?:get|post|put|delete|patch)for(?:object|entity|location))\b",
    "rpc": r"\b(grpc|stub|rpc|channel|invoke|dial|protobuf|proto|servicestub|blockingstub)\b",
    "messaging": "(" + MESSAGING_FRAGMENTS + ")",
    "db": r"\b(select|insert|update|delete|upsert|from|join|query|execute|exec|repository|session|cursor|table|find|save|prepare)\b",
}
VERB = {
    "http": r"\b(get|post|put|patch|delete|head|options|call|do|send|request)\b",
    "rpc": r"\b(call|send|do|request|new)\b",
    "messaging": r"\b(send|write|read|push|pop|ack)\b",
    "db": r"\b(scan|rows|db|conn|tx)\b",
}
LITERAL = r"""["'`][^"'`]{2,}["'`]"""

STRONG_RE = {k: re.compile(v, re.I) for k, v in STRONG.items()}
VERB_RE = {k: re.compile(v, re.I) for k, v in VERB.items()}
LITERAL_RE = re.compile(LITERAL)


def line_supports(kind, line):
    if STRONG_RE[kind].search(line):
        return True
    return bool(VERB_RE[kind].search(line) and LITERAL_RE.search(line))


# --- recall ------------------------------------------------------------------

URL_RE = re.compile(r"""["'`]\s*https?://""", re.I)
PATH_RE = re.compile(r"""["'`](/[A-Za-z0-9_\-./{}:*$]{2,})["'`]""")
HTTP_CTX_RE = re.compile(STRONG["http"] + "|" + VERB["http"], re.I)
CALL_RE = re.compile(r"\w\s*\(")
# Filesystem paths and asset paths are the noise a "looks like a route"
# heuristic collects. Excluding them keeps the recall denominator closer to
# the call sites an operator would expect an edge from — the point of the
# measurement is the custom HTTP helper nobody recognizes, not open("/etc/…").
FS_PATH_RE = re.compile(r"^/(usr|etc|var|tmp|home|opt|dev|proc|sys|bin|sbin|lib|mnt|root)(/|$)")
ASSET_PATH_RE = re.compile(r"\.(go|java|py|ts|js|tsx|jsx|cs|rb|php|json|ya?ml|xml|html?|css|md|txt|png|jpe?g|svg|ico|sh)$", re.I)
TOPIC_CTX_RE = re.compile(r"\b(publish|produce|subscribe|consume|emit|topic|queue|channel|exchange)\w*\s*[\(\[]", re.I)
SQL_RE = re.compile(r"\b(select\s+.+\s+from|insert\s+into|update\s+\w+\s+set|delete\s+from|join)\b", re.I)

SKIP_DIRS = {".git", "node_modules", "vendor", "dist", "build", "target", "__pycache__", "testdata", "test-data"}


def line_signal(line):
    """Which contract kind a source line advertises at a call site, if any."""
    stripped = line.strip()
    if stripped.startswith(("//", "#", "*", "--")):
        # A URL in a comment is not a call site; it is the single largest
        # source of false "we missed it" in this measurement.
        return None
    if SQL_RE.search(line) and LITERAL_RE.search(line):
        return "db"
    if TOPIC_CTX_RE.search(line) and LITERAL_RE.search(line):
        return "messaging"
    if URL_RE.search(line):
        return "http"
    path = PATH_RE.search(line)
    if path and (HTTP_CTX_RE.search(line) or CALL_RE.search(line)):
        # A route literal handed to something that is called counts even when
        # the callee means nothing to us — that is exactly the case this
        # measurement exists for: a project that wraps every request in its own
        # helper looks contract-free from the edges alone.
        route = path.group(1)
        if not FS_PATH_RE.match(route) and not ASSET_PATH_RE.search(route):
            return "http"
    return None


# --- measurement -------------------------------------------------------------


def read_lines(path):
    try:
        with open(path, encoding="utf-8", errors="replace") as fh:
            return fh.read().splitlines()
    except OSError:
        return []


def indexed_files(db, repo_id, limit):
    rows = db.query(
        "SELECT path FROM files WHERE repo_id = %s ORDER BY path" % cl.quote(repo_id)
    )
    paths = [r[0] for r in rows]
    if limit and len(paths) > limit:
        # Deterministic stride rather than a random sample: two runs of the
        # corpus must look at the same files or their numbers cannot be
        # compared.
        stride = len(paths) / float(limit)
        paths = [paths[int(i * stride)] for i in range(limit)]
    return paths


def contract_edges(db, repo_id):
    rows = db.query(
        "SELECT kind, file_path, line FROM edges WHERE repo_id = %s AND kind IN (%s)"
        % (cl.quote(repo_id), ", ".join(cl.quote(k) for k in cl.CONTRACT_EDGE_KINDS))
    )
    out = []
    for kind, file_path, line in rows:
        out.append((cl.EDGE_KIND_CONTRACT[str(kind)], str(file_path), int(line)))
    return out


def measure_precision(root, edges, sample):
    """Sample edges per contract kind and check their source lines."""
    by_kind = defaultdict(list)
    for kind, path, line in edges:
        by_kind[kind].append((path, line))

    result = {}
    unsupported_examples = []
    for kind, sites in by_kind.items():
        sites.sort()
        picked = sites
        if sample and len(sites) > sample:
            stride = len(sites) / float(sample)
            picked = [sites[int(i * stride)] for i in range(sample)]

        cache = {}
        supported = 0
        checked = 0
        for path, line in picked:
            if line <= 0:
                continue
            if path not in cache:
                cache[path] = read_lines(os.path.join(root, path))
            lines = cache[path]
            if not lines or line > len(lines):
                continue
            checked += 1
            if line_supports(kind, lines[line - 1]):
                supported += 1
            elif len(unsupported_examples) < 20:
                unsupported_examples.append("%s:%d: %s" % (path, line, lines[line - 1].strip()[:120]))
        result[kind] = {
            "edges": len(sites),
            "checked": checked,
            "supported": supported,
            "precision": round(supported / checked, 3) if checked else None,
        }
    return result, unsupported_examples


def measure_recall(root, paths, edges, window):
    """Count call-site literals and how many of them carry an edge."""
    edge_lines = defaultdict(set)
    for kind, path, line in edges:
        for offset in range(-window, window + 1):
            edge_lines[(kind, path)].add(line + offset)
    any_lines = defaultdict(set)
    for (_, path), lines in edge_lines.items():
        any_lines[path] |= lines

    seen = defaultdict(int)
    covered = defaultdict(int)
    covered_any = defaultdict(int)
    missed_examples = []
    for path in paths:
        lines = read_lines(os.path.join(root, path))
        for no, line in enumerate(lines, start=1):
            kind = line_signal(line)
            if not kind:
                continue
            seen[kind] += 1
            if no in edge_lines.get((kind, path), ()):
                covered[kind] += 1
                covered_any[kind] += 1
            elif no in any_lines.get(path, ()):
                covered_any[kind] += 1
            elif len(missed_examples) < 20:
                missed_examples.append("%s:%d: %s" % (path, no, line.strip()[:120]))

    result = {}
    for kind in set(seen) | set(covered):
        result[kind] = {
            "candidate_lines": seen[kind],
            "with_edge": covered[kind],
            "with_any_contract_edge": covered_any[kind],
            "recall": round(covered[kind] / seen[kind], 3) if seen[kind] else None,
        }
    return result, missed_examples


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--corpus", default=os.environ.get("CORPUS_DIR", "corpus"))
    ap.add_argument("--db", default=os.environ.get("RAGOTA_DB", os.path.expanduser("~/.ragota/data/ragota.db")))
    ap.add_argument("--results", default="corpus-results",
                    help="bench.py output directory; repo ids are read from it")
    ap.add_argument("--out", default="corpus-results", help="where the measurement JSON goes")
    ap.add_argument("--sample", type=int, default=300, help="edges checked per contract kind (0 = all)")
    ap.add_argument("--max-files", type=int, default=4000, help="files scanned per repo for recall (0 = all)")
    ap.add_argument("--window", type=int, default=2,
                    help="lines of slack when matching an edge to a source line")
    ap.add_argument("repos", nargs="*")
    args = ap.parse_args()

    db = cl.open_db(args.db)
    repos = cl.load_repos(only=set(args.repos) or None)
    os.makedirs(args.out, exist_ok=True)

    rows = []
    for repo in repos:
        dump = os.path.join(args.results, repo.name + ".json")
        if not os.path.exists(dump):
            print("skip    %-32s no bench result" % repo.name)
            continue
        with open(dump, encoding="utf-8") as fh:
            bench = json.load(fh)
        repo_id, root = bench["repo_id"], bench["path"]

        edges = contract_edges(db, repo_id)
        precision, unsupported = measure_precision(root, edges, args.sample)
        paths = indexed_files(db, repo_id, args.max_files)
        recall, missed = measure_recall(root, paths, edges, args.window)

        out = {
            "name": repo.name,
            "pattern": repo.pattern,
            "repo_id": repo_id,
            "contract_edges": len(edges),
            "files_scanned": len(paths),
            "precision": precision,
            "recall": recall,
            "unsupported_edge_examples": unsupported,
            "uncovered_line_examples": missed,
        }
        with open(os.path.join(args.out, repo.name + ".measure.json"), "w", encoding="utf-8") as fh:
            json.dump(out, fh, indent=2, sort_keys=True)

        for kind in sorted(set(precision) | set(recall)):
            p = precision.get(kind, {})
            r = recall.get(kind, {})
            rows.append([repo.name, kind, p.get("edges", 0), p.get("precision", ""),
                         r.get("candidate_lines", 0), r.get("recall", "")])
        print("measure %-32s edges=%d files=%d" % (repo.name, len(edges), len(paths)), flush=True)

    table = os.path.join(args.out, "measure.tsv")
    with open(table, "w", encoding="utf-8") as fh:
        fh.write("repo\tkind\tedges\tprecision_est\tcandidate_lines\trecall_est\n")
        for row in rows:
            fh.write("\t".join(str(v) for v in row) + "\n")
    db.close()
    print("\nwrote %s" % table)


if __name__ == "__main__":
    main()
