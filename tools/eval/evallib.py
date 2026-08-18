"""Shared plumbing for the retrieval evaluation harness: the query set, the
metrics, the throwaway server and the config variants it runs under.

Standard library only, like tools/corpus — the harness has to run on a machine
that has a checkout, a Go toolchain and nothing else. The API client and the
repository list come from tools/corpus/corpuslib.py rather than being written
twice.
"""

import json
import math
import os
import re
import shutil
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
CORPUS_TOOLS = os.path.normpath(os.path.join(HERE, "..", "corpus"))
sys.path.insert(0, CORPUS_TOOLS)

import corpuslib as cl  # noqa: E402

QUERIES_PATH = os.path.join(HERE, "queries.jsonl")

# The question shapes the product exists to answer. A query belongs to exactly
# one; the metrics are reported per shape because a change that helps one shape
# routinely hurts another, and a single average hides that.
SHAPES = (
    "implement",  # where is X implemented
    "callers",    # what calls X / who uses X
    "route",      # where does HTTP route M /path go
    "rpc",        # where is gRPC service/method M implemented
    "topic",      # which service publishes/consumes topic or queue Y
    "table",      # where is table/collection T written, which model maps to it
)

# How far the question reaches. The shape says what is being asked; the scope
# says how many code bases the answer has to be found across, which is the
# distinction the contract graph exists for and the one a single-repository
# query set cannot express.
SCOPES = (
    "in-repo",        # subject and answer in one repository, one service
    "cross-service",  # two services that deploy separately, one repository
    "cross-repo",     # the answer is in a different repository from the caller
)

REQUIRED_FIELDS = ("id", "repo", "shape", "query", "expect_file", "expect_line", "anchor", "why")


class Query:
    """One evaluation item: a question and the file that answers it."""

    def __init__(self, raw, source_line=0):
        self.raw = raw
        self.source_line = source_line
        self.id = raw.get("id", "")
        self.repo = raw.get("repo", "")
        self.shape = raw.get("shape", "")
        self.query = raw.get("query", "")
        self.expect_file = raw.get("expect_file", "")
        self.expect_symbol = raw.get("expect_symbol", "") or ""
        self.expect_line = int(raw.get("expect_line", 0) or 0)
        self.anchor = raw.get("anchor", "")
        self.alt_files = list(raw.get("alt_files") or [])
        # Files a complete answer needs *alongside* expect_file — the other end
        # of the contract the question is about (a route's registration, a
        # callee's definition, a queue's consumer). They are not acceptable
        # answers and never enter the retrieval metrics; related.py scores the
        # graph expansion against them. "repo:path" names another checkout.
        self.expect_related = list(raw.get("expect_related") or [])
        self.why = raw.get("why", "")
        self.scope = raw.get("scope") or "in-repo"
        # None (ask the repository that holds the answer), a list of repository
        # names, or the string "all" (ask without a repository filter).
        self.repos = raw.get("repos")

    @property
    def acceptable(self):
        """Every file that counts as a correct answer, gold first."""
        out = [self.expect_file]
        for alt in self.alt_files:
            if alt not in out:
                out.append(alt)
        return out

    @property
    def search_repos(self):
        """The corpus repositories this question is asked over.

        No `repos` field means the one repository that holds the answer, which
        is what every question written before cross-repository scoring did and
        what keeps those questions asked exactly as they were. A list names
        several. "all" returns the empty list, which the harness sends as *no*
        repository filter: the question is asked of everything indexed.
        """
        if self.repos == "all":
            return []
        if self.repos:
            return list(self.repos)
        return [self.repo]

    @property
    def acceptable_keys(self):
        """The acceptable answers as repo-qualified keys (see hit_key)."""
        return [hit_key(self.repo, path) for path in self.acceptable]

    @property
    def related_keys(self):
        """The companion files as repo-qualified keys.

        A bare path is in the repository that holds the answer; "repo:path"
        names another checkout, which is what a cross-repository contract
        needs — the far side of conductor's Elasticsearch client is a file in
        the elasticsearch checkout.
        """
        out = []
        for entry in self.expect_related:
            repo, path = entry.split(":", 1) if ":" in entry else (self.repo, entry)
            key = hit_key(repo, path)
            if key not in out:
                out.append(key)
        return out

    @property
    def expect_key(self):
        return hit_key(self.repo, self.expect_file)

    def __repr__(self):
        return "Query(%s)" % self.id


def load_queries(path=QUERIES_PATH, repos=None, shapes=None, ids=None, scopes=None):
    """Read the query set. Blank lines and lines starting with # are comments,
    so the file can carry section headers a reviewer can navigate by."""
    out = []
    with open(path, encoding="utf-8") as fh:
        for n, line in enumerate(fh, 1):
            stripped = line.strip()
            if not stripped or stripped.startswith("#"):
                continue
            try:
                raw = json.loads(stripped)
            except ValueError as err:
                raise SystemExit("%s:%d: not valid JSON: %s" % (path, n, err))
            q = Query(raw, n)
            if repos and q.repo not in repos:
                continue
            if shapes and q.shape not in shapes:
                continue
            if scopes and q.scope not in scopes:
                continue
            if ids and q.id not in ids:
                continue
            out.append(q)
    return out


def validate_queries(queries, corpus_dir, strict_lines=False):
    """Check the ground truth against the corpus *sources* — never against the
    index. Returns (errors, warnings) as lists of strings.

    The anchor is the load-bearing part: an expected answer is only worth
    something if the line it points at still says what the reviewer said it
    says. A drifted line number is a warning (the file moved on), a missing
    anchor is an error (the claim is no longer true).
    """
    errors, warnings = [], []
    seen = {}
    for q in queries:
        where = "%s (line %d)" % (q.id, q.source_line)
        for field in REQUIRED_FIELDS:
            if not q.raw.get(field):
                errors.append("%s: missing field %r" % (where, field))
        if q.id in seen:
            errors.append("%s: duplicate id, also on line %d" % (where, seen[q.id]))
        seen[q.id] = q.source_line
        if q.shape not in SHAPES:
            errors.append("%s: unknown shape %r (want one of %s)" % (where, q.shape, ", ".join(SHAPES)))
        if q.scope not in SCOPES:
            errors.append("%s: unknown scope %r (want one of %s)" % (where, q.scope, ", ".join(SCOPES)))
        if len(q.why or "") < 30:
            warnings.append("%s: 'why' is shorter than 30 characters; record the reasoning" % where)
        errors.extend(_check_repos(q, where, corpus_dir))

        root = os.path.join(corpus_dir, q.repo)
        if not os.path.isdir(root):
            warnings.append("%s: repository %s is not cloned at %s" % (where, q.repo, root))
            continue

        for path in q.acceptable:
            if not os.path.isfile(os.path.join(root, path)):
                errors.append("%s: %s does not exist in %s" % (where, path, q.repo))
        errors.extend(_check_related(q, where, corpus_dir))

        target = os.path.join(root, q.expect_file)
        if not os.path.isfile(target):
            continue
        lines = _read_lines(target)
        if q.expect_line < 1 or q.expect_line > len(lines):
            errors.append("%s: expect_line %d is outside %s (%d lines)"
                          % (where, q.expect_line, q.expect_file, len(lines)))
            continue
        if q.anchor in lines[q.expect_line - 1]:
            continue
        found = [n for n, text in enumerate(lines, 1) if q.anchor in text]
        if not found:
            errors.append("%s: anchor %r appears nowhere in %s" % (where, q.anchor, q.expect_file))
        elif strict_lines:
            errors.append("%s: anchor is on line %s of %s, not %d"
                          % (where, ",".join(str(f) for f in found[:3]), q.expect_file, q.expect_line))
        else:
            warnings.append("%s: anchor moved to line %s of %s (recorded %d)"
                            % (where, ",".join(str(f) for f in found[:3]), q.expect_file, q.expect_line))
    return errors, warnings


def _check_related(q, where, corpus_dir):
    """`expect_related` names the second file an answer needs. It is ground
    truth like everything else here, so it is checked against the sources: the
    file has to exist, and it has to be a *different* file from `expect_file` —
    a companion that is the answer scores the graph for reaching where it
    already stood.

    It may be an `alt_files` entry: a route's registration is both an equally
    defensible answer and the companion of its handler, and those pairs are the
    clearest cases the metric has.
    """
    errors = []
    if not q.raw.get("expect_related"):
        return errors
    if not isinstance(q.raw["expect_related"], list) or \
            not all(isinstance(r, str) for r in q.raw["expect_related"]):
        return ["%s: 'expect_related' must be a list of paths ('repo:path' for another checkout)" % where]
    for entry in q.expect_related:
        repo, path = entry.split(":", 1) if ":" in entry else (q.repo, entry)
        if not os.path.isdir(os.path.join(corpus_dir, repo)):
            continue  # repository not cloned; already reported as a warning
        if not os.path.isfile(os.path.join(corpus_dir, repo, path)):
            errors.append("%s: expect_related %s does not exist in %s" % (where, path, repo))
        if repo == q.repo and path == q.expect_file:
            errors.append("%s: expect_related %s is the answer itself" % (where, path))
    return errors


def _check_repos(q, where, corpus_dir):
    """The `repos` field decides which repositories the question is asked over,
    so it can make an answer unreachable — a mistake no amount of retrieval can
    recover from and one that would read as a ranking result."""
    errors = []
    if q.repos is None:
        pass
    elif q.repos == "all":
        pass
    elif not isinstance(q.repos, list) or not all(isinstance(r, str) for r in q.repos):
        errors.append("%s: 'repos' must be a list of repository names or the string \"all\"" % where)
    else:
        for name in q.repos:
            if not os.path.isdir(os.path.join(corpus_dir, name)):
                errors.append("%s: 'repos' names %s, which is not a corpus checkout" % (where, name))
        if q.repo not in q.repos:
            errors.append("%s: the answer is in %s, which 'repos' does not ask" % (where, q.repo))
    if q.scope == "cross-repo" and q.repos != "all" and len(q.repos or []) < 2:
        errors.append("%s: scope is cross-repo but the question is asked of one repository" % where)
    if q.scope != "cross-repo" and isinstance(q.repos, list) and len(q.repos) > 1:
        errors.append("%s: asked of %d repositories but scope is %r, not cross-repo"
                      % (where, len(q.repos), q.scope))
    return errors


def _read_lines(path):
    with open(path, "rb") as fh:
        raw = fh.read()
    return raw.decode("utf-8", errors="replace").splitlines()


# --- metrics -----------------------------------------------------------------
#
# Every query has exactly one file that answers it (plus, occasionally, a short
# list of equally defensible ones). That makes this a known-item retrieval
# task, and the metrics are the standard ones for it:
#
#   recall@k  1 if any acceptable file is in the top k, else 0. With a single
#             gold document this is the hit rate; averaged over the set it is
#             the fraction of questions whose answer was retrieved at all.
#   MRR       1 / (rank of the first acceptable file), 0 if never retrieved.
#   nDCG@10   binary gain over the acceptable set, discounted by 1/log2(rank+1)
#             and normalised by the ideal ordering of those same files.
#
# Results are chunks, not files: several chunks of one file arrive as separate
# hits. Ranks are therefore computed over the result list deduplicated by file,
# first occurrence winning, so a file that is chunked into ten pieces cannot
# outrank one that is chunked into two.
#
# A file is identified by repository *and* path. With one repository per
# question that is a relabelling; with several it is the difference between
# `src/main.go` in the service that calls a contract and `src/main.go` in the
# one that serves it, which is exactly the distinction a cross-repository
# question is asking about.

RECALL_KS = (1, 3, 5, 10, 20)
NDCG_K = 10


def hit_key(repo, path):
    """Identify one answer file: "<repository>/<path within it>"."""
    return "%s/%s" % (repo, path) if repo else path


def dedup_paths(paths):
    seen, out = set(), []
    for p in paths:
        if p in seen:
            continue
        seen.add(p)
        out.append(p)
    return out


def ranks_of(paths, acceptable):
    """1-based ranks of every acceptable file in a deduplicated result list."""
    index = {p: i + 1 for i, p in enumerate(paths)}
    return sorted(index[a] for a in acceptable if a in index)


def first_rank(paths, acceptable):
    got = ranks_of(paths, acceptable)
    return got[0] if got else 0


def mrr(rank):
    return 1.0 / rank if rank else 0.0


def recall_at(rank, k):
    return 1.0 if rank and rank <= k else 0.0


def ndcg_at(ranks, n_acceptable, k=NDCG_K):
    dcg = sum(1.0 / math.log2(r + 1) for r in ranks if r <= k)
    ideal = sum(1.0 / math.log2(i + 1) for i in range(1, min(n_acceptable, k) + 1))
    return dcg / ideal if ideal else 0.0


def score_one(keys, query):
    """Score one ranked list of repo-qualified file keys against one query."""
    paths = dedup_paths(keys)
    got = ranks_of(paths, query.acceptable_keys)
    rank = got[0] if got else 0
    out = {"rank": rank, "mrr": mrr(rank), "ndcg@10": ndcg_at(got, len(query.acceptable))}
    for k in RECALL_KS:
        out["recall@%d" % k] = recall_at(rank, k)
    return out


METRIC_KEYS = ["recall@%d" % k for k in RECALL_KS] + ["mrr", "ndcg@10"]


def mean(values):
    values = list(values)
    return sum(values) / len(values) if values else 0.0


def aggregate(scores):
    """Mean of every metric over a list of per-query score dicts."""
    out = {k: mean(s.get(k, 0.0) for s in scores) for k in METRIC_KEYS}
    out["n"] = len(scores)
    out["span@10"] = mean(1.0 if s.get("span_rank") and s["span_rank"] <= NDCG_K else 0.0
                          for s in scores)
    out["missed"] = sum(1 for s in scores if not s.get("rank"))
    return out


# --- API ---------------------------------------------------------------------


class EvalAPI(cl.API):
    """corpuslib's client plus the two retrieval endpoints under test."""

    def search(self, query, repos=None, mode="keyword", limit=20, filters=None):
        body = {"query": query, "mode": mode, "limit": limit}
        if repos:
            body["repos"] = repos
        if filters:
            body["filter"] = filters
        return self.post("/api/v1/search", body)

    def context(self, query, repos=None, mode="keyword", limit=10, hops=1):
        body = {"query": query, "mode": mode, "limit": limit, "hops": hops}
        if repos:
            body["repos"] = repos
        return self.post("/api/v1/context", body)


def _hit_key(hit, repo_names):
    """One hit as "<repository>/<path>". `repo_names` maps the server's repo id
    to the corpus checkout name the query set is written in terms of; a hit from
    a repository the run did not register keeps its raw id, which cannot match
    an expected answer and so cannot silently score as one.

    `file_path` is the only spelling since API 0.2.0. Hits used to carry the
    same value again as `path`, which is now gone; a related unit only ever had
    `file_path`, so the two views this file builds read the same field."""
    path = hit.get("file_path") or ""
    repo_id = hit.get("repo_id") or ""
    return hit_key((repo_names or {}).get(repo_id, repo_id), path)


def search_paths(response, repo_names=None):
    """Repo-qualified keys of a /search response, in rank order."""
    return [_hit_key(h, repo_names) for h in (response.get("hits") or [])]


def context_paths(response, repo_names=None):
    return [_hit_key(item.get("hit") or {}, repo_names) for item in (response.get("items") or [])]


def spans(response, key="hits", repo_names=None):
    """(key, start, end) triples, so a hit can be checked against the line the
    answer actually lives on rather than only against the file."""
    out = []
    if key == "hits":
        items = [{"hit": h} for h in (response.get("hits") or [])]
    else:
        items = response.get("items") or []
    for item in items:
        hit = item.get("hit") or {}
        start = int(hit.get("line") or 0)
        end = int(hit.get("end_line") or 0) or start
        out.append((_hit_key(hit, repo_names), start, max(start, end)))
    return out


def span_rank(triples, query, tolerance=0):
    """Rank (over files) of the first hit that covers the expected line.

    A file-level hit that lands 900 lines from the answer is still a hit for
    recall, but it is not what a reader needs; this reports how often the
    retrieved chunk actually contains the definition.
    """
    seen, rank = set(), 0
    for path, start, end in triples:
        if path not in seen:
            seen.add(path)
            rank += 1
        if path != query.expect_key:
            continue
        if start - tolerance <= query.expect_line <= end + tolerance:
            return rank
    return 0


# --- the graph expansion ------------------------------------------------------
#
# /context returns each hit with the AST unit it lands in and a `related` list
# built by walking the code graph out of that unit (callers, callees, contracts
# and their far sides). That list is a large part of what an LLM is handed and
# the retrieval metrics above score none of it: they read `items[].hit` and
# stop. These helpers give related.py one normalised view of the response so
# the expansion can be scored against the same ground truth as the hits.


def context_items(response, repo_names=None):
    """A /context response as a list of items in rank order.

    Each item carries the repo-qualified key of its hit, the unit the hit
    landed in, and its related units with the edge that produced them.
    """
    out = []
    for item in response.get("items") or []:
        hit = item.get("hit") or {}
        unit = item.get("unit") or {}
        related = []
        for rel in item.get("related") or []:
            runit = rel.get("unit") or {}
            related.append({
                "key": _hit_key(runit, repo_names),
                "name": runit.get("name") or "",
                "qualified": runit.get("qualified") or "",
                "kind": runit.get("kind") or "",
                "line": int(runit.get("start_line") or 0),
                "end_line": int(runit.get("end_line") or 0),
                "service": rel.get("service") or "",
                "via": rel.get("via") or "",
                "direction": rel.get("direction") or "",
                "distance": int(rel.get("distance") or 0),
            })
        out.append({
            "key": _hit_key(hit, repo_names),
            "line": int(hit.get("line") or 0),
            "end_line": int(hit.get("end_line") or 0) or int(hit.get("line") or 0),
            "symbol": hit.get("symbol") or unit.get("name") or "",
            "kind": hit.get("kind") or unit.get("kind") or "",
            "language": hit.get("language") or "",
            "snippet": hit.get("snippet") or "",
            "service": item.get("service") or "",
            "unit": unit.get("qualified") or unit.get("name") or "",
            "has_unit": bool(unit),
            "related": related,
        })
    return out


def related_keys(items, upto=None):
    """Every file the related lists name, over the first `upto` items."""
    out = []
    for item in items[:upto] if upto else items:
        for rel in item["related"]:
            if rel["key"] not in out:
                out.append(rel["key"])
    return out


def score_expansion(items, query):
    """Score one /context response's related lists against one question.

    Three things are asked of the expansion, and they are deliberately
    separate numbers:

    `companion` — for a question that carries `expect_related` (the second file
    a complete answer needs), does the related list of the item that *is* the
    answer name that file? The answer item is the first item on `expect_file`
    itself rather than on any acceptable file: the pairing is between one file
    and its far side, and anchoring it on the gold answer is what keeps that
    unambiguous where a question also carries an equally defensible second
    answer. Conditional on that item existing — an expansion cannot be blamed
    for a hit that never arrived — with the denominator printed next to it so
    the ceiling stays visible. `companion_in_hits` is the control: if retrieval
    already returned the second file, the graph added nothing by naming it.

    `reach` — over every question, whether the file that answers it is in the
    hits (`hit`), only in a related list (`rescue`: the expansion supplied an
    answer ranking did not), or in neither (`miss`).

    `shape` — what the lists look like at all: how many are empty, how large
    they are, how much of them points outside the file the reader already has.
    A metric that only rewards recall would be satisfied by returning the whole
    graph; these numbers are what make that visible.
    """
    acceptable = set(query.acceptable_keys)
    companions = set(query.related_keys)

    hits, seen = [], set()
    for item in items:
        if item["key"] not in seen:
            seen.add(item["key"])
            hits.append(item["key"])
    answer_rank = first_rank(hits, list(acceptable))

    out = {
        "items": len(items),
        "answer_rank": answer_rank,
        "n_related": sum(len(i["related"]) for i in items),
        "empty_items": sum(1 for i in items if not i["related"]),
        "off_file": sum(1 for i in items for r in i["related"] if r["key"] != i["key"]),
        "off_service": sum(1 for i in items for r in i["related"]
                           if r["service"] and i["service"] and r["service"] != i["service"]),
        "via": {},
    }
    for item in items:
        for rel in item["related"]:
            out["via"][rel["via"]] = out["via"].get(rel["via"], 0) + 1

    # reach: what the package holds, hits and expansion counted apart.
    all_related = set(related_keys(items))
    out["gold_in_hits"] = bool(answer_rank)
    out["gold_in_related"] = bool(all_related & acceptable)
    out["reach"] = "hit" if answer_rank else ("rescue" if out["gold_in_related"] else "miss")

    # companion: only for the questions that carry one, and anchored on the
    # gold file — validation guarantees a companion is never that file, so the
    # set below is never empty.
    out["has_companion"] = bool(companions)
    if companions:
        answer_item = next((i for i in items if i["key"] == query.expect_key), None)
        out["companion_in_hits"] = bool(set(hits) & companions)
        out["companion_in_any_related"] = bool(all_related & companions)
        out["companion_at_answer"] = (
            bool({r["key"] for r in answer_item["related"]} & companions) if answer_item else False)
        out["companion_reached"] = bool(answer_item)
    return out


def aggregate_expansion(scores):
    """Mean/rate of every expansion metric over a list of per-query scores."""
    n = len(scores)
    items = sum(s["items"] for s in scores)
    related = sum(s["n_related"] for s in scores)
    out = {
        "n": n,
        "items": items,
        "related_per_item": related / items if items else 0.0,
        "empty_items": (sum(s["empty_items"] for s in scores) / items) if items else 0.0,
        "off_file": (sum(s["off_file"] for s in scores) / related) if related else 0.0,
        "off_service": (sum(s["off_service"] for s in scores) / related) if related else 0.0,
        "hit": sum(1 for s in scores if s["reach"] == "hit"),
        "rescue": sum(1 for s in scores if s["reach"] == "rescue"),
        "miss": sum(1 for s in scores if s["reach"] == "miss"),
    }
    withc = [s for s in scores if s.get("has_companion")]
    reached = [s for s in withc if s.get("companion_reached")]
    out["companion_n"] = len(withc)
    out["companion_reached"] = len(reached)
    # Every rate below has the same denominator — the questions whose answer was
    # retrieved, so an expansion had something to expand from. Mixing that with
    # the full set would put the score and its own control on different bases.
    out["companion_at_answer"] = mean(1.0 if s.get("companion_at_answer") else 0.0 for s in reached)
    out["companion_in_any_related"] = mean(1.0 if s.get("companion_in_any_related") else 0.0 for s in reached)
    out["companion_in_hits"] = mean(1.0 if s.get("companion_in_hits") else 0.0 for s in reached)
    out["companion_absent"] = sum(1 for s in reached
                                  if not s.get("companion_in_any_related") and not s.get("companion_in_hits"))
    return out


# --- config ------------------------------------------------------------------


def yaml_dump(value, indent=0):
    """Minimal YAML emitter for the config files the harness generates.

    Only dict / list / str / bool / int / float appear in them, and every
    string is quoted, so nothing here has to guess about YAML's scalar rules.
    """
    pad = "  " * indent
    if isinstance(value, dict):
        lines = []
        for key, val in value.items():
            if isinstance(val, (dict, list)) and val:
                lines.append("%s%s:" % (pad, key))
                lines.append(yaml_dump(val, indent + 1))
            elif isinstance(val, (dict, list)):
                lines.append("%s%s: %s" % (pad, key, "{}" if isinstance(val, dict) else "[]"))
            else:
                lines.append("%s%s: %s" % (pad, key, _scalar(val)))
        return "\n".join(lines)
    if isinstance(value, list):
        lines = []
        for item in value:
            if isinstance(item, (dict, list)):
                lines.append("%s-" % pad)
                lines.append(yaml_dump(item, indent + 1))
            else:
                lines.append("%s- %s" % (pad, _scalar(item)))
        return "\n".join(lines)
    return pad + _scalar(value)


def _scalar(value):
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, (int, float)):
        return str(value)
    return '"%s"' % str(value).replace("\\", "\\\\").replace('"', '\\"')


# TEST_IGNORE keeps test, mock and fixture trees out of the index. It is opt-in
# (--exclude-tests): no question in the set expects a test file, and test code is
# where most of a repository's call edges live, but dropping those files changes
# the keyword corpus — its term statistics, and with them every score — so a run
# that uses it cannot be read next to one that does not.
TEST_IGNORE = [
    "**/src/test/**", "**/test/**", "**/tests/**", "**/testdata/**",
    "**/__tests__/**", "**/test/resources/**",
    "**/*_test.*", "**/*Test.java", "**/*Tests.java", "**/*IT.java",
    "**/*.test.*", "**/*.spec.*",
]


def base_config(workdir, port, corpus_dir):
    """The configuration every variant starts from: AST + BM25 over SQLite, no
    vector index, no reranker, no assistant. It is the setup a developer gets
    from a plain `make run`, which is what a baseline should measure."""
    return {
        "server": {
            "host": "127.0.0.1",
            "port": port,
            "auth": {"type": "none"},
            "read_timeout_seconds": 60,
            "write_timeout_seconds": 300,
        },
        "log": {"level": "warn", "format": "text"},
        "storage": {"sqlite": {"path": os.path.join(workdir, "ragota.db"), "pool_size": 10}},
        "indexes": {
            "workers": 4,
            "ast": {"enabled": True},
            # Compaction is asked for once, after every repository is in (see
            # index_repos): the layout it produces is the same, the rewrites are
            # one instead of one per repository.
            "bm25": {"enabled": True, "path": os.path.join(workdir, "bm25"), "no_compact": True},
        },
        "repos": {"sources": {"local": {"enabled": True, "paths": [corpus_dir]}}},
    }


# Variants are named config overlays. They exist so that the comparisons this
# harness is for — "did the reranker help?", "are symbol cards better than
# windows?", "is query rewriting worth an LLM call?" — are one flag, not a
# hand-edited YAML file that nobody can reproduce a month later.
#
# Each is a function of the CLI options (endpoints differ per machine), so a
# variant that needs a service it cannot reach fails loudly at startup rather
# than silently measuring the base configuration again.

def variant_rerank(cfg, opt):
    cfg.setdefault("search", {})["rerank"] = {
        "enabled": True,
        "base_url": opt["rerank_url"],
        "model": opt["rerank_model"],
        "top_n": opt["rerank_top_n"],
        "timeout_seconds": 60,
    }
    if opt.get("rerank_instruction"):
        cfg["search"]["rerank"]["instruction"] = opt["rerank_instruction"]
    return cfg


def variant_norerank(cfg, opt):
    cfg.get("search", {}).pop("rerank", None)
    return cfg


# What the vector variants keep out of the embedding channel. Ground truth
# never lives in test scaffolding, vendored trees or generated documentation
# sites, and those are up to half of a large repository (elasticsearch: 45%
# tests; medusa: 74% docs site) — embedding them buys noise at the corpus's
# highest price. BM25 and AST indexing still cover every excluded file, so
# they remain searchable by keyword; this trims the *vector* channel only,
# and it is the variant's documented condition, not a product default.
VECTOR_EXCLUDES = [
    "/test/", "/tests/", "/testing/", "/testdata/", "/__tests__/",
    "_test.", ".test.", ".spec.", "/qa/", "/e2e/", "/integration-tests/",
    "/fixtures/", "/mocks/", "/devenv/",
    "/vendor/", "/node_modules/", "/generated/", "/dist/", "/oas-output/",
    "/www/", "/docs/", "/doc/",
]


def _enable_vector(cfg, opt, method):
    cfg["storage"]["qdrant"] = {
        "url": opt["qdrant_url"],
        # Set per workdir by run.py; two runs that index differently must not
        # write into one collection.
        "collection_prefix": opt.get("collection_prefix") or "ragota_eval_",
        "mode": "docker_embedded",
    }
    cfg["indexes"]["vector"] = {
        "enabled": True,
        "embedder": {
            "provider": opt["embed_provider"],
            "model": opt["embed_model"],
            "base_url": opt["embed_url"],
            "batch_size": 64,
            "concurrency": 2,
            # ~500 tokens of code. Attention is quadratic in sequence length,
            # so halving the per-chunk budget roughly quadruples local-GPU
            # throughput; the stored chunk text is untouched (embedding-only
            # truncation, see EmbedderConfig.MaxChars).
            "max_chars": 2048,
        },
        "chunking": {"method": method, "window_lines": 60, "overlap": 10},
        "exclude": list(VECTOR_EXCLUDES),
    }
    if opt.get("embed_dimensions"):
        cfg["indexes"]["vector"]["embedder"]["dimensions"] = opt["embed_dimensions"]
    return cfg


def variant_window(cfg, opt):
    return _enable_vector(cfg, opt, "window")


def variant_cards(cfg, opt):
    return _enable_vector(cfg, opt, "cards")


def variant_qinstr(cfg, opt):
    """Query-side instruction for an instruction-aware embedder. Composes on
    top of window/cards, and changes only how the query is embedded — never a
    document — so a --no-reindex comparison against the bare side is valid."""
    vec = cfg["indexes"].get("vector")
    if not vec:
        raise SystemExit("variant 'qinstr' composes on top of 'window' or 'cards'")
    vec["embedder"]["query_instruction"] = opt["embed_query_instruction"]
    return cfg


def variant_symsum(cfg, opt):
    """One-line LLM summaries of boundary symbols, indexed with the symbol.

    File and service summaries are switched off so the comparison isolates the
    symbol pass; the generator is the same assistant endpoint the rewrite
    variant uses.
    """
    cfg.setdefault("models", {}).setdefault("providers", {})[opt["assistant_provider"]] = {
        "base_url": opt["assistant_url"]
    }
    cfg["summaries"] = {
        "enabled": True,
        "provider": opt["assistant_provider"],
        "model": opt["assistant_model"],
        "files": False,
        "symbols": True,
        "max_symbols": opt.get("max_symbols") or 500,
    }
    return cfg


def variant_rewrite(cfg, opt):
    cfg.setdefault("models", {}).setdefault("providers", {})[opt["assistant_provider"]] = {
        "base_url": opt["assistant_url"]
    }
    cfg["models"]["assistant"] = {
        "provider": opt["assistant_provider"],
        "base_url": opt["assistant_url"],
        "model": opt["assistant_model"],
        "recon": False,
        "disambiguate": False,
        "query_rewrite": True,
    }
    return cfg


def variant_norewrite(cfg, opt):
    models = cfg.get("models", {})
    if "assistant" in models:
        models["assistant"]["query_rewrite"] = False
    return cfg


VARIANTS = {
    "base": lambda cfg, opt: cfg,
    "symsum": variant_symsum,
    "rerank": variant_rerank,
    "norerank": variant_norerank,
    "window": variant_window,
    "cards": variant_cards,
    "qinstr": variant_qinstr,
    "rewrite": variant_rewrite,
    "norewrite": variant_norewrite,
}

# What each variant needs from the outside world. Checked before the server is
# started so a missing endpoint is an error, not a silently identical run.
VARIANT_REQUIRES = {
    "rerank": ["rerank_url"],
    "symsum": ["assistant_url", "assistant_model"],
    "window": ["qdrant_url", "embed_model"],
    "cards": ["qdrant_url", "embed_model"],
    "qinstr": ["embed_query_instruction"],
    "rewrite": ["assistant_url", "assistant_model"],
}


def apply_variants(cfg, names, opt):
    for name in names:
        if name not in VARIANTS:
            raise SystemExit("unknown variant %r (have: %s)" % (name, ", ".join(sorted(VARIANTS))))
        for need in VARIANT_REQUIRES.get(name, []):
            if not opt.get(need):
                raise SystemExit("variant %r needs --%s" % (name, need.replace("_", "-")))
        cfg = VARIANTS[name](cfg, opt)
    return cfg


# --- server ------------------------------------------------------------------


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


class Server:
    """A ragota process with its own database, started and stopped by the
    harness. Nothing it touches lives outside workdir, so a run can be thrown
    away by deleting one directory."""

    def __init__(self, binary, workdir, config, port):
        self.binary = binary
        self.workdir = workdir
        self.config = config
        self.port = port
        self.url = "http://127.0.0.1:%d" % port
        self.proc = None
        self._log = None
        self.log_path = os.path.join(workdir, "server.log")
        self.config_path = os.path.join(workdir, "config.yaml")

    def write_config(self):
        os.makedirs(self.workdir, exist_ok=True)
        with open(self.config_path, "w", encoding="utf-8") as fh:
            fh.write(yaml_dump(self.config) + "\n")

    def start(self, timeout=60):
        self.write_config()
        self._log = open(self.log_path, "ab")
        self.proc = subprocess.Popen(
            [self.binary, "-config", self.config_path],
            stdout=self._log, stderr=subprocess.STDOUT,
        )
        deadline = time.time() + timeout
        while time.time() < deadline:
            if self.proc.poll() is not None:
                raise SystemExit("server exited with %s; see %s" % (self.proc.returncode, self.log_path))
            try:
                with urllib.request.urlopen(self.url + "/health", timeout=2) as resp:
                    if resp.status == 200:
                        return self
            except (urllib.error.URLError, OSError):
                time.sleep(0.3)
        self.stop()
        raise SystemExit("server did not become healthy in %ds; see %s" % (timeout, self.log_path))

    def stop(self):
        if self.proc and self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=30)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=10)
        self.proc = None
        if self._log:
            self._log.close()
            self._log = None

    def __enter__(self):
        return self.start()

    def __exit__(self, *exc):
        self.stop()
        return False


def build_binary(repo_root, out_path):
    """Build cmd/server. Returns the path, or exits with the compiler output."""
    os.makedirs(os.path.dirname(out_path), exist_ok=True)
    env = dict(os.environ, CGO_ENABLED="1")
    proc = subprocess.run(
        ["go", "build", "-o", out_path, "./cmd/server"],
        cwd=repo_root, env=env, capture_output=True, text=True,
    )
    if proc.returncode != 0:
        raise SystemExit("go build failed:\n%s" % (proc.stderr or proc.stdout))
    return out_path


def binary_version(binary):
    """`ragota -version`, so a result file names what produced it."""
    try:
        proc = subprocess.run([binary, "-version"], capture_output=True, text=True, timeout=30)
    except (OSError, subprocess.SubprocessError):
        return ""
    return (proc.stdout or proc.stderr).strip().splitlines()[0] if (proc.stdout or proc.stderr) else ""


def index_repos(api, corpus_dir, names, timeout=7200, force=True, reuse=False):
    """Register and index each repository; returns name -> {id, seconds}."""
    existing = {}
    if reuse:
        try:
            for repo in api.get("/api/v1/repos") or []:
                existing[repo.get("name")] = repo
        except (RuntimeError, TypeError):
            existing = {}

    out = {}
    for name in names:
        path = os.path.join(corpus_dir, name)
        if not os.path.isdir(path):
            raise SystemExit("repository %s is not cloned at %s (see tools/corpus/clone.sh)" % (name, path))
        started = time.time()
        if reuse and name in existing and existing[name].get("status") == "idle":
            out[name] = {"id": existing[name]["id"], "seconds": 0.0, "reused": True}
            continue
        registered = api.add_repo(name, path)
        repo_id = registered["id"]
        api.index(repo_id, force=force)
        state = api.wait_idle(repo_id, timeout=timeout, poll=1.0)
        out[name] = {
            "id": repo_id,
            "seconds": round(time.time() - started, 1),
            "status": state.get("status"),
            "last_error": state.get("last_error", ""),
        }

    return out


# --- a run's server ------------------------------------------------------------


class Session:
    """A server with the repositories a question set needs, indexed and ready.

    run.py scores the ranked hits, related.py scores the graph expansion around
    them and answer.py puts the whole package in front of a model; all three
    need the same binary, the same throwaway database and the same repositories
    indexed the same way, so that lives here once. A session either starts its
    own server under --work or attaches to --server.
    """

    def __init__(self, api, indexed, binary, url, server=None, workdir=""):
        self.api = api
        self.indexed = indexed
        self.binary = binary
        self.url = url
        self.server = server
        self.workdir = workdir

    @property
    def names(self):
        """Server repository id -> the corpus checkout name queries are written in."""
        return {info["id"]: name for name, info in self.indexed.items()}

    @property
    def index_seconds(self):
        return round(sum(i["seconds"] for i in self.indexed.values()), 1)

    def repo_ids(self, query):
        """The repository filter for one question, or None for "everything"."""
        return [self.indexed[name]["id"] for name in query.search_repos] or None

    def close(self):
        if self.server:
            self.server.stop()
            self.server = None

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()
        return False


def compact_indexes(api):
    """Settle the keyword index layout once, after everything is indexed.

    Compaction rewrites the whole index, and the configuration this harness
    writes turns the per-pass one off: a twelve repository load would otherwise
    pay twelve whole-index rewrites to reach the layout the last one produces on
    its own. Measured on this corpus, indexing a 230 file repository after
    elasticsearch cost 7 s, of which 6.75 s was that rewrite.

    The final layout is what scores depend on, and it is the same either way —
    but a run whose compaction failed is a run whose scores can move between
    repetitions, so the failure is reported rather than swallowed.
    """
    # The server confirms the merge reached disk for up to ten minutes before
    # it answers (with a warning when it gives up). The client's default 600 s
    # is the same number, and the race was once lost by a millisecond — the
    # server answered 200 at 10m0.001s and the run died at its very last step.
    # The caller of a bounded wait must outwait it, with slack.
    prev, api.timeout = api.timeout, 900
    try:
        took = (api.post("/api/v1/admin/compact", {}) or {}).get("compacted_ms") or {}
    except (RuntimeError, TypeError, OSError) as exc:
        print("  compaction failed (%s): scores may vary between runs" % exc, flush=True)
        return
    finally:
        api.timeout = prev
    if took:
        print("  compacted %s" % ", ".join("%s %.1fs" % (k, v / 1000.0)
                                           for k, v in sorted(took.items())), flush=True)


def open_session(repo_root, args, needed, opts, label):
    """Build, start and index whatever `args` asks for.

    `needed` is the repositories the selected questions are asked over — the
    repository holding the answer is not enough, because a cross-repository
    question asked of a checkout that was never indexed is a zero that has
    nothing to do with retrieval.
    """
    corpus = os.path.abspath(args.corpus)
    if args.server:
        session = Session(EvalAPI(args.server, args.api_key), {}, "(external)", args.server)
    else:
        binary = args.binary or build_binary(repo_root, os.path.join(args.work, "bin", "ragota"))
        # compare.py --no-reindex pins both sides to one subdirectory so they
        # share an index; otherwise each label gets its own database.
        subdir = getattr(args, "work_subdir", None) or slugify(label)
        workdir = os.path.join(args.work, subdir)
        prefix = "ragota_eval_%s_" % slugify(subdir)
        if not args.reuse:
            rmtree(workdir)
            # The collection is part of the run's state, so it goes with the
            # workdir. Left behind, they accumulate silently and are the
            # heaviest thing a machine running many experiments carries:
            # eighteen of them held 2.7 GB of a laptop's RAM inside the Qdrant
            # container while every one of their workdirs was long gone.
            drop_collection(opts.get("qdrant_url"), prefix)
        os.makedirs(workdir, exist_ok=True)
        port = free_port()
        # The Qdrant collection belongs to the workdir, like the sqlite file
        # and the bm25 directory. A fixed prefix made every vector run share
        # one collection: `window` and `cards` index the same files into
        # differently-shaped documents, so the second run read the first run's
        # points as well as its own. Sides that share an index (compare.py
        # --no-reindex pins both to "shared") still share the collection.
        opts = dict(opts, collection_prefix=prefix)
        cfg = apply_variants(base_config(workdir, port, corpus), args.variant, opts)
        if opts.get("exclude_tests"):
            cfg["repos"]["ignore"] = list(TEST_IGNORE)
        server = Server(binary, workdir, cfg, port).start()
        session = Session(EvalAPI(server.url, args.api_key), {}, binary, server.url, server, workdir)

    try:
        if not args.no_index:
            print("indexing %d repositor%s: %s"
                  % (len(needed), "y" if len(needed) == 1 else "ies", ", ".join(needed)), flush=True)
            for name in needed:
                one = index_repos(session.api, corpus, [name], timeout=args.index_timeout,
                                  force=not args.reuse, reuse=args.reuse)
                session.indexed.update(one)
                info = one[name]
                print("  %-16s %-28s %6.1fs%s"
                      % (name, info["id"], info["seconds"], "  (reused)" if info.get("reused") else ""),
                      flush=True)
            compact_indexes(session.api)
        else:
            for repo in session.api.get("/api/v1/repos") or []:
                if repo.get("name") in needed:
                    session.indexed[repo["name"]] = {"id": repo["id"], "seconds": 0.0, "reused": True}

        missing = [n for n in needed if n not in session.indexed]
        if missing:
            raise SystemExit("repositories not present on the server: %s" % ", ".join(missing))
    except BaseException:
        session.close()
        raise
    return session


def repos_needed(queries):
    """Every checkout the selected questions are asked over, in query order."""
    needed = []
    for q in queries:
        for name in [q.repo] + q.search_repos:
            if name not in needed:
                needed.append(name)
    return needed


# --- reporting ---------------------------------------------------------------


def table(rows, headers, aligns=None):
    """Render a fixed-width text table."""
    cols = len(headers)
    widths = [len(str(h)) for h in headers]
    for row in rows:
        for i in range(cols):
            widths[i] = max(widths[i], len(str(row[i])))
    aligns = aligns or ["<"] + [">"] * (cols - 1)
    out = ["  ".join(("%-*s" % (widths[i], headers[i])) for i in range(cols))]
    out.append("  ".join("-" * widths[i] for i in range(cols)))
    for row in rows:
        cells = []
        for i in range(cols):
            fmt = "%*s" if aligns[i] == ">" else "%-*s"
            cells.append(fmt % (widths[i], row[i]))
        out.append("  ".join(cells))
    return "\n".join(out)


def fmt(value, places=3):
    return ("%%.%df" % places) % value


def slugify(text):
    return re.sub(r"[^a-z0-9]+", "-", text.lower()).strip("-")


def rmtree(path):
    if os.path.isdir(path):
        shutil.rmtree(path, ignore_errors=True)


def drop_collection(qdrant_url, prefix):
    """Delete the vector collection a run owns. Best effort: no Qdrant, no
    vector variant, or a collection that was never created are all fine."""
    if not qdrant_url or not prefix:
        return
    req = urllib.request.Request(
        qdrant_url.rstrip("/") + "/collections/" + prefix + "chunks", method="DELETE")
    try:
        urllib.request.urlopen(req, timeout=30).read()
    except (urllib.error.URLError, OSError):
        pass
