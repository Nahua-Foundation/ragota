"""Shared plumbing for the corpus scripts: the repository list, the metadata
database and the ragota API.

Standard library only, on purpose: the corpus has to be runnable on a machine
that has a checkout and nothing else installed.
"""

import json
import os
import shutil
import subprocess
import sqlite3
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
REPOS_TSV = os.path.join(HERE, "repos.tsv")

# Edge kinds that represent an outbound contract, i.e. the ones the corpus
# measures. Kinds derived by the linker (kafka_flow) or ingested from tracing
# (runtime_call) are excluded: they are conclusions drawn from these, not
# observations of a call site.
CONTRACT_EDGE_KINDS = [
    "http_call",
    "rpc_call",
    "produces",
    "consumes",
    "writes_to",
    "reads_from",
]

# Which contract kind an edge kind belongs to, matching storage.ContractKind*.
EDGE_KIND_CONTRACT = {
    "http_call": "http",
    "rpc_call": "rpc",
    "produces": "messaging",
    "consumes": "messaging",
    "writes_to": "db",
    "reads_from": "db",
}


class Repo:
    def __init__(self, name, url, pattern, stack):
        self.name = name
        self.url = url
        self.pattern = pattern
        self.stack = stack

    def __repr__(self):
        return "Repo(%s)" % self.name


def load_repos(path=REPOS_TSV, only=None):
    """Parse repos.tsv, optionally filtered to the given names."""
    out = []
    with open(path, encoding="utf-8") as fh:
        for line in fh:
            line = line.rstrip("\n")
            if not line.strip() or line.lstrip().startswith("#"):
                continue
            parts = line.split("\t")
            while len(parts) < 4:
                parts.append("")
            repo = Repo(*parts[:4])
            if only and repo.name not in only:
                continue
            out.append(repo)
    return out


# --- metadata database -------------------------------------------------------


def quote(value):
    """Render a Python value as a SQL literal.

    The corpus scripts build their SQL as text because the same statements run
    against SQLite (via the sqlite3 module) and PostgreSQL (via the psql
    client, which takes no bind parameters on the command line). Every value
    that reaches this function is a repository id or a kind produced by this
    tooling, never external input.
    """
    if isinstance(value, bool):
        return "TRUE" if value else "FALSE"
    if isinstance(value, (int, float)):
        return str(value)
    return "'" + str(value).replace("'", "''") + "'"


class SQLiteDB:
    def __init__(self, path):
        self.path = path
        self.conn = sqlite3.connect(path)

    def query(self, sql):
        cur = self.conn.execute(sql)
        try:
            return cur.fetchall()
        finally:
            cur.close()

    def close(self):
        self.conn.close()


class PsqlDB:
    """Postgres access through the psql client, so the scripts stay
    dependency-free."""

    def __init__(self, dsn):
        if not shutil.which("psql"):
            raise SystemExit("a postgres DSN needs the psql client on PATH")
        self.dsn = dsn

    def query(self, sql):
        proc = subprocess.run(
            ["psql", self.dsn, "-At", "-F", "\x1f", "-c", sql],
            capture_output=True,
            text=True,
        )
        if proc.returncode != 0:
            raise SystemExit("psql: %s" % proc.stderr.strip())
        rows = []
        for line in proc.stdout.splitlines():
            if not line:
                continue
            rows.append(tuple(line.split("\x1f")))
        return rows

    def close(self):
        pass


def open_db(target):
    """Open the metadata store: a postgres DSN or a path to the sqlite file."""
    if target.startswith("postgres://") or target.startswith("postgresql://"):
        return PsqlDB(target)
    if not os.path.exists(target):
        raise SystemExit("no metadata database at %s" % target)
    return SQLiteDB(target)


def group_counts(db, table, repo_id, column="kind", where=""):
    """kind -> count for one repository."""
    sql = "SELECT %s, COUNT(*) FROM %s WHERE repo_id = %s %s GROUP BY %s" % (
        column,
        table,
        quote(repo_id),
        where,
        column,
    )
    return {str(row[0]): int(row[1]) for row in db.query(sql)}


# --- API ---------------------------------------------------------------------


class API:
    def __init__(self, base, api_key=None, timeout=600):
        self.base = base.rstrip("/")
        self.api_key = api_key
        self.timeout = timeout

    def _call(self, method, path, body=None):
        url = self.base + path
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(url, data=data, method=method)
        req.add_header("Content-Type", "application/json")
        if self.api_key:
            req.add_header("Authorization", "Bearer " + self.api_key)
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                raw = resp.read()
        except urllib.error.HTTPError as err:
            detail = err.read().decode(errors="replace")
            raise RuntimeError("%s %s -> %s %s" % (method, path, err.code, detail.strip()))
        return json.loads(raw) if raw else {}

    def get(self, path):
        return self._call("GET", path)

    def post(self, path, body=None):
        return self._call("POST", path, body if body is not None else {})

    def add_repo(self, name, path):
        return self.post("/api/v1/repos", {"name": name, "source": "local", "path": path})

    def index(self, repo_id, force=True):
        return self.post("/api/v1/repos/%s/index" % repo_id, {"force": force})

    def coverage(self, repo_id):
        return self.get("/api/v1/repos/%s/coverage" % repo_id)

    def wait_idle(self, repo_id, timeout, poll=5.0):
        """Block until the repo leaves the indexing state. Returns the repo."""
        deadline = time.time() + timeout
        while True:
            repo = self.get("/api/v1/repos/%s" % repo_id)
            if repo.get("status") != "indexing":
                return repo
            if time.time() > deadline:
                raise TimeoutError("repo %s still indexing after %ds" % (repo_id, timeout))
            time.sleep(poll)
