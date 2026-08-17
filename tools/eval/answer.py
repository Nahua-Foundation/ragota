#!/usr/bin/env python3
"""Put the `/context` package in front of a model and grade the answer.

    ./answer.py --corpus /data/corpus --model qwen2.5:1.5b
    ./answer.py --corpus /data/corpus --reuse --work-subdir shared --judge --control
    ./answer.py --corpus /data/corpus --no-related   # what the graph is worth

Everything else in this directory asks *was the right file retrieved and where
did it rank*. Whether a model handed that context writes a **correct answer** is
a different question and the one the product exists to serve. This harness asks
it: `/context` for each question, rendered as an LLM would be given it, one
answer per question, graded.

Grading is where this goes wrong if it is careless, so it is mechanical first.
The ground truth already names the answering file, the line and an `anchor` —
the exact text on that line — so the grade is:

  cited      the answer names the answering file. The prompt shows every
             candidate as "<repository>/<path>" and a model answers with the
             tail of it, so a tail counts only when it belongs to exactly one
             file in the package: robotshop has four server.js and "server.js"
             names none of them.
  grounded   the answer also carries the symbol, or a word off the anchor line
             — the difference between naming a file and saying what is in it.
  correct    both.

And the ceiling is reported apart from the score, which is the entire point of
the exercise: a question whose file was never retrieved cannot be answered, so
"retrieval failed" and "retrieval worked and the answer was still wrong" are
never added together.

`--judge` adds an LLM judge over the same answers. It grades its own family's
output — with `--model` and `--judge-model` both defaulting to the same local
model it grades *its own* output — so it is reported only as a disagreement
rate against the mechanical grade, never as the number.

`--control` asks every question a second time with no context at all. A small
model that answers from memory would make the whole table meaningless; the
control is what rules that out.
"""

import argparse
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import evallib as ev  # noqa: E402
import run as runner  # noqa: E402

REPO_ROOT = os.path.normpath(os.path.join(ev.HERE, "..", ".."))

# Words that appear on every second line of source in some language and would
# make "grounded" mean nothing.
ANCHOR_STOPWORDS = {
    "public", "private", "protected", "static", "final", "void", "class", "const", "return",
    "string", "func", "function", "def", "self", "this", "new", "true", "false", "null", "none",
    "import", "from", "package", "value", "name", "type", "async", "await", "await;", "override",
    "var", "let", "int", "bool", "float", "double", "with", "that", "then", "else", "elif",
    "session", "request", "response", "params", "args", "kwargs", "error", "err",
}


# --- the model ---------------------------------------------------------------


class Ollama:
    """The smallest possible client for a local ollama server.

    Standard library only, like the rest of tools/eval. Temperature 0 and a
    fixed seed, so two runs of the same questions over the same index differ
    only where the server's own output differs.
    """

    def __init__(self, base_url, model, timeout=300, seed=7, num_ctx=8192, num_predict=320):
        self.base_url = base_url.rstrip("/")
        self.model = model
        self.timeout = timeout
        self.options = {"temperature": 0, "seed": seed, "num_ctx": num_ctx, "num_predict": num_predict}

    def ask(self, prompt):
        body = json.dumps({
            "model": self.model, "stream": False, "options": self.options,
            "messages": [{"role": "user", "content": prompt}],
        }).encode("utf-8")
        req = urllib.request.Request(self.base_url + "/api/chat", data=body,
                                     headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=self.timeout) as resp:
            payload = json.loads(resp.read().decode("utf-8"))
        return (payload.get("message") or {}).get("content", "").strip()

    def check(self):
        """Fail at startup rather than after the first thirty questions."""
        try:
            with urllib.request.urlopen(self.base_url + "/api/tags", timeout=10) as resp:
                tags = json.loads(resp.read().decode("utf-8"))
        except (urllib.error.URLError, OSError) as err:
            raise SystemExit("no model server at %s: %s" % (self.base_url, err))
        have = [m.get("name", "") for m in tags.get("models") or []]
        if self.model not in have and self.model + ":latest" not in have:
            raise SystemExit("model %r is not loaded at %s (have: %s)"
                             % (self.model, self.base_url, ", ".join(have) or "nothing"))


# --- the prompt --------------------------------------------------------------


ASK = """You are a code assistant answering a question about an unfamiliar code base.
Use only the excerpts below. If they do not contain the answer, say so.

Answer in at most four sentences. Name the file that answers the question, \
copying its path exactly as it appears below, and name the function, method or \
class inside it.

QUESTION
%s

EXCERPTS
%s

ANSWER"""

ASK_NO_CONTEXT = """You are a code assistant answering a question about a code base you \
have not been shown. Answer in at most four sentences, naming the file and the \
function you believe answers it, or say that you do not know.

QUESTION
%s

ANSWER"""


def render(items, max_chars=1200, with_related=True):
    """The context package as a model gets to see it."""
    out = []
    for n, item in enumerate(items, 1):
        head = "[%d] %s:%d-%d" % (n, item["key"], item["line"], item["end_line"])
        facts = [f for f in (item["service"] and "service %s" % item["service"],
                             item["symbol"] and "symbol %s" % item["symbol"],
                             item["language"]) if f]
        if facts:
            head += "  (%s)" % ", ".join(facts)
        out.append(head)
        snippet = (item["snippet"] or "").strip()
        if len(snippet) > max_chars:
            snippet = snippet[:max_chars] + "\n..."
        if snippet:
            out.append(snippet)
        if with_related:
            for rel in item["related"]:
                out.append("    related: %s  (%s %s%s)"
                           % (rel["key"], rel["direction"], rel["via"],
                              ", " + rel["name"] if rel["name"] else ""))
        out.append("")
    return "\n".join(out).strip()


# --- the mechanical grade ----------------------------------------------------


# Anything that looks like a file path with an extension, wherever the model
# put it — inside backticks, in a bullet, at the end of a sentence.
PATH_TOKEN = re.compile(r"[\w./-]*\w[\w./-]*\.\w+")


def answer_paths(text):
    """The file paths an answer wrote, normalised."""
    out = []
    for raw in PATH_TOKEN.findall(text or ""):
        token = raw.lower().lstrip("./").strip("/")
        if token and token not in out:
            out.append(token)
    return out


def is_tail_of(token, key):
    """Whole path segments only: `connectca_server.go` is not a tail of
    `.../connectca/server.go`, and reading it as one credits an answer that
    named a file which does not exist."""
    return key == token or key.endswith("/" + token)


def tails(token):
    """A written path and its own tails, longest first.

    A small model routinely writes the right file under a wrong prefix — this
    one turns boutique into "blocq/src/shippingservice/main.go" — and the part
    that identifies the code is the tail. Dropping segments from the front
    recovers it; the ambiguity test below is what stops that from turning into
    credit for "main.go".
    """
    parts = token.split("/")
    return ["/".join(parts[i:]) for i in range(len(parts))]


def cited(text, gold_keys, universe):
    """Did the answer name one of `gold_keys`, unambiguously?

    A path the answer wrote counts when some tail of it is a whole-segment tail
    of an acceptable answer and of nothing else the package offered: robotshop
    has four `server.js`, so "server.js" names none of them, while
    "user/server.js" names one.
    """
    golds = [g.lower() for g in gold_keys]
    others = [u.lower() for u in universe if u not in gold_keys]
    for token in answer_paths(text):
        for tail in tails(token):
            if not any(is_tail_of(tail, g) for g in golds):
                continue
            return not any(is_tail_of(tail, o) for o in others)
    return False


def anchor_words(anchor):
    words = set()
    for token in re.split(r"[^A-Za-z0-9_]+", anchor or ""):
        if len(token) >= 4 and token.lower() not in ANCHOR_STOPWORDS and not token.isdigit():
            words.add(token.lower())
    return words


def grounded(text, query):
    """Does the answer carry what is *on* the answering line, not just its path?

    The symbol counts, and so does any distinctive word off the anchor — the
    table name, the route path, the method being called.
    """
    lowered = text.lower()
    symbol = (query.expect_symbol or "").lower()
    if symbol:
        if symbol in lowered:
            return True
        tail = symbol.split(".")[-1]
        if len(tail) >= 3 and tail in lowered:
            return True
    return any(word in lowered for word in anchor_words(query.anchor))


def grade(text, query, universe):
    keys = list(query.acceptable_keys)
    out = {"cited": cited(text, keys, universe), "grounded": grounded(text, query)}
    out["correct"] = bool(out["cited"] and out["grounded"])
    out["cited_other"] = bool(not out["cited"] and any(
        cited(text, [u], universe) for u in universe if u not in keys))
    return out


def regrade(doc):
    """Grade a stored run again, with no server and no model.

    The grading rule is the part of this harness most likely to be wrong, and
    the answers are the expensive part; a run therefore records the file names
    it offered each question (`universe`) so that fixing the grader is a second
    of work rather than another pass over the model.
    """
    queries = {q.id: q for q in ev.load_queries(doc["meta"]["queries"])}
    missing = [r["id"] for r in doc["queries"] if "universe" not in r]
    if missing:
        raise SystemExit("this result file predates --regrade (no `universe` on %d rows)" % len(missing))
    for row in doc["queries"]:
        q = queries.get(row["id"])
        if q is None:
            raise SystemExit("query %s is no longer in %s" % (row["id"], doc["meta"]["queries"]))
        row["grade"] = grade(row["answer"], q, row["universe"])
        if "control_answer" in row:
            row["control_grade"] = grade(row["control_answer"], q, row["universe"])
    doc["totals"] = summarize(doc["queries"])
    return doc


# --- the judge ---------------------------------------------------------------


JUDGE = """You are grading one answer to a question about a code base.

QUESTION
%s

THE DOCUMENTED ANSWER
file: %s
line %d reads: %s
%s

THE ANSWER TO GRADE
%s

Does the answer point at that same code? Reply with exactly one word on the \
first line, CORRECT or INCORRECT, and then one short sentence of reason."""


def judge_one(llm, query, text):
    prompt = JUDGE % (query.query, query.expect_key, query.expect_line, query.anchor.strip(),
                      ("symbol: %s" % query.expect_symbol) if query.expect_symbol else "",
                      text.strip() or "(no answer)")
    verdict = llm.ask(prompt)
    return {"verdict": read_verdict(verdict), "text": verdict}


def read_verdict(verdict):
    """CORRECT / INCORRECT, wherever the model put it.

    A small model does not always follow "one word on the first line", and a
    verdict that failed to parse must not be silently counted as a rejection —
    it is a third outcome and the judge's disagreement rate has to carry it.
    """
    lines = [line for line in (verdict or "").upper().splitlines() if line.strip()]
    for scope in (lines[:1], lines):
        for line in scope:
            if "INCORRECT" in line:
                return "incorrect"
            if "CORRECT" in line:
                return "correct"
    return "unparsed"


# --- the run -----------------------------------------------------------------


def execute(args, queries=None):
    queries = queries if queries is not None else ev.load_queries(
        args.queries, repos=set(args.repo) or None, shapes=set(args.shape) or None,
        ids=set(args.id) or None, scopes=set(args.scope) or None)
    if not queries:
        raise SystemExit("no queries selected")

    llm = Ollama(args.model_url, args.model, timeout=args.model_timeout)
    llm.check()
    judge = Ollama(args.model_url, args.judge_model or args.model, timeout=args.model_timeout) \
        if args.judge else None
    if judge:
        judge.check()

    started = time.time()
    session = ev.open_session(REPO_ROOT, args, ev.repos_needed(queries),
                              runner.options_from(args), runner.label_of(args))
    rows = []
    try:
        names = session.names
        for i, q in enumerate(queries, 1):
            row = {"id": q.id, "repo": q.repo, "shape": q.shape, "scope": q.scope,
                   "query": q.query, "expect_file": q.expect_file, "expect_key": q.expect_key,
                   "expect_symbol": q.expect_symbol, "expect_line": q.expect_line}
            res = session.api.context(q.query, repos=session.repo_ids(q), mode=args.mode,
                                      limit=args.context_limit, hops=args.hops)
            retrieved = ev.context_items(res, names)
            items = retrieved[:args.items]
            universe = ev.dedup_paths([it["key"] for it in items]
                                      + ev.related_keys(items) + list(q.acceptable_keys))
            row["universe"] = universe

            # Where the answer stood in what the model was given. This is the
            # ceiling: the rest of the row is only interesting against it.
            shown = ev.dedup_paths([it["key"] for it in items])
            row["ctx_rank"] = ev.first_rank(shown, list(q.acceptable_keys))
            row["ctx_related"] = bool(set(ev.related_keys(items)) & set(q.acceptable_keys))
            row["ctx_items"] = len(items)
            # Retrieved but past the prompt's budget is a third outcome, and a
            # cheaper one to fix than either of the other two.
            row["retrieved_rank"] = ev.first_rank(
                ev.dedup_paths([it["key"] for it in retrieved]), list(q.acceptable_keys))

            t0 = time.time()
            row["answer"] = llm.ask(ASK % (q.query, render(items, args.max_chars, not args.no_related)))
            row["ms"] = int((time.time() - t0) * 1000)
            row["grade"] = grade(row["answer"], q, universe)

            if args.control:
                row["control_answer"] = llm.ask(ASK_NO_CONTEXT % q.query)
                row["control_grade"] = grade(row["control_answer"], q, universe)
            if judge:
                row["judge"] = judge_one(judge, q, row["answer"])

            rows.append(row)
            sys.stdout.write("." if row["grade"]["correct"] else ("o" if row["ctx_rank"] else "x"))
            if i % 50 == 0 or i == len(queries):
                sys.stdout.write(" %d/%d\n" % (i, len(queries)))
            sys.stdout.flush()
    finally:
        session.close()

    doc = {
        "meta": {
            "created": time.strftime("%Y-%m-%dT%H:%M:%S"),
            "label": "answers/%s" % runner.label_of(args),
            "model": args.model,
            "model_url": args.model_url,
            "judge_model": (args.judge_model or args.model) if args.judge else "",
            "items": args.items,
            "max_chars": args.max_chars,
            "related_in_prompt": not args.no_related,
            "control": bool(args.control),
            "mode": args.mode,
            "context_limit": args.context_limit,
            "hops": args.hops,
            "binary": session.binary,
            "binary_version": ev.binary_version(session.binary) if session.binary != "(external)" else "",
            "corpus": os.path.abspath(args.corpus),
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


def summarize(rows):
    def block(subset):
        n = len(subset)
        got = [r for r in subset if r["ctx_rank"]]
        return {
            "n": n,
            "ctx_hit": len(got),
            "cited": ev.mean(1.0 if r["grade"]["cited"] else 0.0 for r in subset),
            "correct": ev.mean(1.0 if r["grade"]["correct"] else 0.0 for r in subset),
            # Conditional on the file having been in the package at all.
            "correct_given_ctx": ev.mean(1.0 if r["grade"]["correct"] else 0.0 for r in got),
            "cited_given_ctx": ev.mean(1.0 if r["grade"]["cited"] else 0.0 for r in got),
            "correct_with_ctx": sum(1 for r in got if r["grade"]["correct"]),
            "correct_without_ctx": sum(1 for r in subset if not r["ctx_rank"] and r["grade"]["correct"]),
            "retrieved_not_shown": sum(1 for r in subset
                                       if not r["ctx_rank"] and r.get("retrieved_rank")),
            "cited_other": sum(1 for r in subset if r["grade"]["cited_other"]),
            "median_ms": sorted(r["ms"] for r in subset)[n // 2] if n else 0,
        }

    out = {"all": block(rows)}
    for name, keyfn in (("by_shape", lambda r: r["shape"]),
                        ("by_scope", lambda r: r.get("scope") or "in-repo"),
                        ("by_repo", lambda r: r["repo"])):
        groups = {}
        for row in rows:
            groups.setdefault(keyfn(row), []).append(row)
        out[name] = {k: block(v) for k, v in sorted(groups.items())}

    if any("control_grade" in r for r in rows):
        ctl = [r for r in rows if "control_grade" in r]
        out["control"] = {
            "n": len(ctl),
            "cited": ev.mean(1.0 if r["control_grade"]["cited"] else 0.0 for r in ctl),
            "correct": ev.mean(1.0 if r["control_grade"]["correct"] else 0.0 for r in ctl),
        }
    if any("judge" in r for r in rows):
        judged = [r for r in rows if "judge" in r]
        agree = sum(1 for r in judged if (r["judge"]["verdict"] == "correct") == r["grade"]["correct"])
        out["judge"] = {
            "n": len(judged),
            "correct": ev.mean(1.0 if r["judge"]["verdict"] == "correct" else 0.0 for r in judged),
            "unparsed": sum(1 for r in judged if r["judge"]["verdict"] == "unparsed"),
            "agree": agree,
            "agreement": agree / len(judged) if judged else 0.0,
            "judge_yes_mech_no": sum(1 for r in judged
                                     if r["judge"]["verdict"] == "correct" and not r["grade"]["correct"]),
            "judge_no_mech_yes": sum(1 for r in judged
                                     if r["judge"]["verdict"] != "correct" and r["grade"]["correct"]),
        }
    return out


def report(doc, per_query=True):
    meta, totals = doc["meta"], doc["totals"]
    all_ = totals["all"]
    lines = ["", "%s  —  %d questions, %d context items each, model %s%s"
             % (meta["label"], meta["query_count"], meta["items"], meta["model"],
                "" if meta["related_in_prompt"] else ", related lists withheld"), ""]

    headers = ["answers", "n", "file in context", "cited it", "cited + grounded", "correct/all"]
    rows = []
    for name in ("by_shape", "by_scope"):
        for key, agg in totals[name].items():
            rows.append([key, agg["n"], agg["ctx_hit"], ev.fmt(agg["cited_given_ctx"]),
                         ev.fmt(agg["correct_given_ctx"]), ev.fmt(agg["correct"])])
        rows.append(["--", "", "", "", "", ""])
    rows.append(["-- total", all_["n"], all_["ctx_hit"], ev.fmt(all_["cited_given_ctx"]),
                 ev.fmt(all_["correct_given_ctx"]), ev.fmt(all_["correct"])])
    lines.append(ev.table(rows, headers))
    lines.append("  'file in context' is the ceiling: questions whose answering file the retrieval put in")
    lines.append("  the package at all. The two middle columns are rates over those; the last is over all n.")
    lines.append("")

    lines.append("the gap, stated plainly:")
    lines.append("  %d of %d questions had the answering file in the context package."
                 % (all_["ctx_hit"], all_["n"]))
    lines.append("    of those, %d answers named it and quoted what is on the line (%s)."
                 % (all_["correct_with_ctx"], ev.fmt(all_["correct_given_ctx"])))
    lines.append("    so %d questions were retrieved and still answered wrong."
                 % (all_["ctx_hit"] - all_["correct_with_ctx"]))
    lines.append("  %d had no answering file to work from; %d were answered correctly anyway."
                 % (all_["n"] - all_["ctx_hit"], all_["correct_without_ctx"]))
    lines.append("    of those %d, %d had it retrieved past item %d and cut by the prompt budget,"
                 % (all_["n"] - all_["ctx_hit"], all_["retrieved_not_shown"], meta["items"]))
    lines.append("    which is a cheaper failure to fix than either of the other two.")
    lines.append("  %d answers cited a file from the package that is not an answer." % all_["cited_other"])
    lines.append("  median %d ms per answer." % all_["median_ms"])
    lines.append("")

    if "control" in totals:
        ctl = totals["control"]
        lines.append("control — the same questions with no context at all:")
        lines.append("  cited the right file %s, correct %s (n=%d). Whatever the table above measures,"
                     % (ev.fmt(ctl["cited"]), ev.fmt(ctl["correct"]), ctl["n"]))
        lines.append("  this is how much of it the model already knew.")
        lines.append("")

    if "judge" in totals:
        j = totals["judge"]
        lines.append("judge — %s grading answers written by %s:" % (meta["judge_model"], meta["model"]))
        lines.append("  says correct %s where the mechanical grade says %s;"
                     % (ev.fmt(j["correct"]), ev.fmt(all_["correct"])))
        lines.append("  the two agree on %d of %d (%s): %d judged correct that the mechanical grade"
                     % (j["agree"], j["n"], ev.fmt(j["agreement"]), j["judge_yes_mech_no"]))
        lines.append("  rejects, %d rejected that it accepts%s."
                     % (j["judge_no_mech_yes"],
                        ", %d verdicts unparseable" % j["unparsed"] if j["unparsed"] else ""))
        lines.append("  It is the same family of model that wrote the answers; read it as a second"
                     " opinion, not as the grade.")
        lines.append("")

    if per_query:
        headers = ["query", "shape", "ctx", "cited", "grounded", "judge", "expected"]
        rows = []
        for row in doc["queries"]:
            rows.append([row["id"], row["shape"], row["ctx_rank"] or ("rel" if row["ctx_related"] else "-"),
                         "y" if row["grade"]["cited"] else ".",
                         "y" if row["grade"]["grounded"] else ".",
                         (row.get("judge") or {}).get("verdict", "")[:1].upper() or "-",
                         row["expect_file"]])
        lines.append(ev.table(rows, headers, aligns=["<", "<", ">", "^", "^", "^", "<"]))
        lines.append("")
        lines.append("ctx = rank of the answering file among the items the model was shown"
                     " ('rel' = only named in a")
        lines.append("related list, '-' = absent). cited/grounded are the mechanical grade;"
                     " judge is C/I when asked for.")
        lines.append("")
    return "\n".join(lines)


def transcript(doc, only_wrong=True):
    """The answers themselves. A grade nobody reads the output of is a number
    nobody should trust."""
    lines = ["", "answers%s:" % (" that the mechanical grade rejected" if only_wrong else ""), ""]
    for row in doc["queries"]:
        if only_wrong and row["grade"]["correct"]:
            continue
        lines.append("  %s  [%s]  ctx rank %s" % (row["id"], row["shape"], row["ctx_rank"] or "-"))
        lines.append("    q:        %s" % row["query"])
        lines.append("    expected: %s:%d  %s" % (row["expect_key"], row["expect_line"], row["expect_symbol"]))
        for n, line in enumerate((row["answer"] or "(empty)").splitlines()):
            lines.append("    %s %s" % ("a:       " if n == 0 else "         ", line))
        if "judge" in row:
            lines.append("    judge:    %s" % row["judge"]["text"].replace("\n", " ")[:200])
        lines.append("")
    return "\n".join(lines)


def write_tsv(doc, path):
    cols = ["id", "repo", "shape", "scope", "ctx_rank", "ctx_related", "cited", "grounded",
            "correct", "judge", "ms", "expect_file"]
    with open(path, "w", encoding="utf-8") as fh:
        fh.write("\t".join(cols) + "\n")
        for row in doc["queries"]:
            g = row["grade"]
            fh.write("\t".join(str(v) for v in [
                row["id"], row["repo"], row["shape"], row["scope"], row["ctx_rank"],
                int(bool(row["ctx_related"])), int(bool(g["cited"])), int(bool(g["grounded"])),
                int(bool(g["correct"])), (row.get("judge") or {}).get("verdict", ""),
                row["ms"], row["expect_file"],
            ]) + "\n")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    runner.add_common_args(ap)
    ap.add_argument("--model", default="qwen2.5:1.5b", help="the model that answers (ollama tag)")
    ap.add_argument("--model-url", default=os.environ.get("OLLAMA_URL", "http://127.0.0.1:11434"))
    ap.add_argument("--model-timeout", type=int, default=300)
    ap.add_argument("--items", type=int, default=5, help="context items put in the prompt")
    ap.add_argument("--max-chars", type=int, default=1200, help="snippet budget per item")
    ap.add_argument("--no-related", action="store_true",
                    help="withhold the graph expansion from the prompt (the A/B for related.py)")
    ap.add_argument("--judge", action="store_true", help="also grade every answer with an LLM judge")
    ap.add_argument("--judge-model", default="", help="model for the judge (default: --model)")
    ap.add_argument("--control", action="store_true",
                    help="also ask every question with no context, to see what the model already knew")
    ap.add_argument("--out", help="write the full result JSON here (default: <work>/answers-<label>.json)")
    ap.add_argument("--tsv", help="write the per-query table here (default: next to --out)")
    ap.add_argument("--quiet", action="store_true", help="totals only, no per-query table")
    ap.add_argument("--transcript", action="store_true", help="print the rejected answers in full")
    ap.add_argument("--all-transcript", action="store_true", help="print every answer in full")
    ap.add_argument("--regrade", help="re-grade this stored result file instead of running "
                                      "(no server, no model — for when the grader changes)")
    args = ap.parse_args()

    if args.regrade:
        with open(args.regrade, encoding="utf-8") as fh:
            doc = regrade(json.load(fh))
        with open(args.regrade, "w", encoding="utf-8") as fh:
            json.dump(doc, fh, indent=2, sort_keys=True)
        print(report(doc, per_query=not args.quiet))
        if args.transcript or args.all_transcript:
            print(transcript(doc, only_wrong=not args.all_transcript))
        print("re-graded %s" % args.regrade)
        return 0

    doc = execute(args)
    out = args.out or os.path.join(args.work, "answers-%s.json" % ev.slugify(runner.label_of(args)))
    os.makedirs(os.path.dirname(os.path.abspath(out)), exist_ok=True)
    with open(out, "w", encoding="utf-8") as fh:
        json.dump(doc, fh, indent=2, sort_keys=True)
    tsv = args.tsv or (os.path.splitext(out)[0] + ".tsv")
    write_tsv(doc, tsv)

    print(report(doc, per_query=not args.quiet))
    if args.transcript or args.all_transcript:
        print(transcript(doc, only_wrong=not args.all_transcript))
    print("wrote %s and %s" % (out, tsv))
    return 0


if __name__ == "__main__":
    sys.exit(main())
