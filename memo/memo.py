#!/usr/bin/env python3
"""memo — a single-user memory workspace CLI.

Dumb storage for AI agents. Each workspace is a namespace with:
  - a SQLite database, persisted as a TEXT dump (db.sql) so git can diff/merge it
  - a files/ tree for prose/blobs
  - a scope.yaml manifest

The store directory is a git repo. Sync = git commit + push. History, undo,
offline edits, and reject-and-resolve-on-divergence come from git for free.

The binary SQLite is never written to disk or committed: db.sql (text) is the
source of truth, materialized into an in-memory SQLite for each operation and
dumped back. This is the "store SQLite as a dump, not a binary" approach.

No AI logic lives here. The calling agent decides what to store and how.
"""

import argparse
import json
import os
import shutil
import sqlite3
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path


# ---------------------------------------------------------------------------
# config / env
# ---------------------------------------------------------------------------

def load_env():
    """Load KEY=VALUE lines from a .env next to this script, without clobbering
    already-set environment variables."""
    env_path = Path(__file__).resolve().parent / ".env"
    if env_path.exists():
        for raw in env_path.read_text().splitlines():
            line = raw.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, val = line.split("=", 1)
            key, val = key.strip(), val.strip().strip('"').strip("'")
            os.environ.setdefault(key, val)


def store_dir() -> Path:
    d = os.environ.get("MEMO_DIR")
    if not d:
        # default: ./store next to this script
        d = str(Path(__file__).resolve().parent / "store")
    return Path(d).expanduser().resolve()


def remote_url() -> str:
    return os.environ.get("MEMO_REMOTE", "").strip()


# ---------------------------------------------------------------------------
# errors / output
# ---------------------------------------------------------------------------

class MemoError(Exception):
    def __init__(self, code: str, message: str):
        super().__init__(message)
        self.code = code
        self.message = message


def emit(data, meta=None, *, as_json: bool, human=None):
    """Print a success result in the selected mode and exit 0."""
    if as_json:
        print(json.dumps({"ok": True, "data": data, "error": None,
                          "meta": meta or {}}, indent=2, default=str))
    else:
        if human is not None:
            print(human)
        elif isinstance(data, str):
            print(data)
        else:
            print(json.dumps(data, indent=2, default=str))
    sys.exit(0)


def fail(err: MemoError, *, as_json: bool):
    if as_json:
        print(json.dumps({"ok": False, "data": None,
                          "error": {"code": err.code, "message": err.message},
                          "meta": {}}, indent=2))
    else:
        print(f"error [{err.code}]: {err.message}", file=sys.stderr)
    # distinct-ish exit codes per error class
    codes = {"WS_NOT_FOUND": 3, "WS_EXISTS": 4, "STORE_NOT_INIT": 5,
             "SQL_ERROR": 6, "FS_ERROR": 7, "GIT_ERROR": 8, "BAD_ARG": 2}
    sys.exit(codes.get(err.code, 1))


# ---------------------------------------------------------------------------
# git helpers
# ---------------------------------------------------------------------------

def git(args, cwd: Path, check=True):
    proc = subprocess.run(["git", *args], cwd=str(cwd),
                          capture_output=True, text=True)
    if check and proc.returncode != 0:
        raise MemoError("GIT_ERROR",
                        f"git {' '.join(args)} failed: {proc.stderr.strip()}")
    return proc


def ensure_store():
    d = store_dir()
    if not (d / ".git").exists():
        raise MemoError("STORE_NOT_INIT",
                        f"store not initialized at {d} (run: memo init)")
    return d


# ---------------------------------------------------------------------------
# workspace / db helpers
# ---------------------------------------------------------------------------

def ws_dir(name: str) -> Path:
    if not name or "/" in name or name.startswith(".") or "\\" in name:
        raise MemoError("BAD_ARG", f"invalid workspace name: {name!r}")
    return store_dir() / name


def require_ws(name: str) -> Path:
    p = ws_dir(name)
    if not (p / "scope.yaml").exists():
        raise MemoError("WS_NOT_FOUND", f"workspace not found: {name}")
    return p


def dump_path(name: str) -> Path:
    return ws_dir(name) / "db.sql"


def load_db(name: str) -> sqlite3.Connection:
    """Materialize the workspace DB from its text dump into memory."""
    con = sqlite3.connect(":memory:")
    dp = dump_path(name)
    if dp.exists():
        sql = dp.read_text()
        if sql.strip():
            try:
                con.executescript(sql)
            except sqlite3.Error as e:
                con.close()
                raise MemoError("SQL_ERROR", f"failed to load db.sql: {e}")
    return con


def save_db(name: str, con: sqlite3.Connection):
    """Dump the in-memory DB back to db.sql (deterministic text for git)."""
    lines = list(con.iterdump())
    dump_path(name).write_text("\n".join(lines) + "\n")


# ---------------------------------------------------------------------------
# commands
# ---------------------------------------------------------------------------

def cmd_init(a):
    d = store_dir()
    d.mkdir(parents=True, exist_ok=True)
    if not (d / ".git").exists():
        git(["init", "-q"], d)
        # identity for the POC so commits work in a clean environment
        git(["config", "user.email", "memo@localhost"], d)
        git(["config", "user.name", "memo"], d)
        (d / ".gitignore").write_text(
            "# binary SQLite is never committed; db.sql (text) is the source of truth\n"
            "**/db.sqlite\n**/db.sqlite-*\n")
        (d / "README.md").write_text(
            "# memo store\n\nGit-backed memory workspaces. "
            "Each `<name>/` holds `scope.yaml`, `db.sql`, and `files/`.\n")
        git(["add", "-A"], d)
        git(["commit", "-q", "-m", "chore: initialize memo store"], d)
    if remote_url():
        existing = git(["remote"], d).stdout.split()
        if "origin" not in existing:
            git(["remote", "add", "origin", remote_url()], d)
    emit({"store": str(d), "remote": remote_url() or None},
         as_json=a.json, human=f"initialized store at {d}")


def cmd_ws_create(a):
    ensure_store()
    p = ws_dir(a.name)
    if (p / "scope.yaml").exists():
        raise MemoError("WS_EXISTS", f"workspace already exists: {a.name}")
    (p / "files").mkdir(parents=True, exist_ok=True)
    now = datetime.now(timezone.utc).isoformat()
    (p / "scope.yaml").write_text(
        f"name: {a.name}\n"
        f"description: {json.dumps(a.description or '')}\n"
        f"created_at: {now}\n"
        f"manifest_version: 1\n")
    (p / "db.sql").write_text("")  # empty DB
    (p / "files" / ".gitkeep").write_text("")
    emit({"workspace": a.name, "path": str(p)},
         as_json=a.json, human=f"created workspace: {a.name}")


def cmd_ws_list(a):
    d = ensure_store()
    names = sorted(c.name for c in d.iterdir()
                   if c.is_dir() and (c / "scope.yaml").exists())
    emit(names, meta={"count": len(names)}, as_json=a.json,
         human="\n".join(names) if names else "(no workspaces)")


def cmd_ws_info(a):
    require_ws(a.name)
    manifest = (ws_dir(a.name) / "scope.yaml").read_text()
    con = load_db(a.name)
    tables = [r[0] for r in con.execute(
        "SELECT name FROM sqlite_master WHERE type='table' "
        "AND name NOT LIKE 'sqlite_%' ORDER BY name")]
    con.close()
    files_root = ws_dir(a.name) / "files"
    nfiles = sum(1 for _ in files_root.rglob("*") if _.is_file()
                 and _.name != ".gitkeep") if files_root.exists() else 0
    emit({"manifest": manifest, "tables": tables, "file_count": nfiles},
         as_json=a.json,
         human=f"{manifest}\ntables: {tables}\nfiles: {nfiles}")


def cmd_db_query(a):
    require_ws(a.name)
    con = load_db(a.name)
    try:
        cur = con.execute(a.sql)
        cols = [c[0] for c in cur.description] if cur.description else []
        rows = [dict(zip(cols, r)) for r in cur.fetchall()]
    except sqlite3.Error as e:
        con.close()
        raise MemoError("SQL_ERROR", str(e))
    con.close()
    emit(rows, meta={"columns": cols, "rows": len(rows)}, as_json=a.json,
         human=_table(cols, rows))


def cmd_db_exec(a):
    require_ws(a.name)
    con = load_db(a.name)
    try:
        con.executescript(a.sql)
        con.commit()
    except sqlite3.Error as e:
        con.close()
        raise MemoError("SQL_ERROR", str(e))
    save_db(a.name, con)
    con.close()
    emit({"applied": True}, as_json=a.json, human="ok")


def cmd_fs_write(a):
    require_ws(a.name)
    content = sys.stdin.read() if a.content is None else a.content
    dst = _safe_files_path(a.name, a.path)
    dst.parent.mkdir(parents=True, exist_ok=True)
    dst.write_text(content)
    emit({"path": a.path, "bytes": len(content.encode())},
         as_json=a.json, human=f"wrote {a.path} ({len(content.encode())} bytes)")


def cmd_fs_cat(a):
    require_ws(a.name)
    src = _safe_files_path(a.name, a.path)
    if not src.is_file():
        raise MemoError("FS_ERROR", f"no such file: {a.path}")
    emit(src.read_text(), as_json=a.json, human=src.read_text())


def cmd_fs_ls(a):
    require_ws(a.name)
    root = _safe_files_path(a.name, a.path or ".")
    if not root.exists():
        raise MemoError("FS_ERROR", f"no such path: {a.path}")
    if root.is_file():
        entries = [root.name]
    else:
        entries = sorted(p.name + ("/" if p.is_dir() else "")
                         for p in root.iterdir() if p.name != ".gitkeep")
    emit(entries, meta={"count": len(entries)}, as_json=a.json,
         human="\n".join(entries) if entries else "(empty)")


def cmd_fs_rm(a):
    require_ws(a.name)
    tgt = _safe_files_path(a.name, a.path)
    if not tgt.exists():
        raise MemoError("FS_ERROR", f"no such path: {a.path}")
    if tgt.is_dir():
        shutil.rmtree(tgt)
    else:
        tgt.unlink()
    emit({"removed": a.path}, as_json=a.json, human=f"removed {a.path}")


def cmd_sync(a):
    d = ensure_store()
    git(["add", "-A"], d)
    status = git(["status", "--porcelain"], d).stdout.strip()
    committed = False
    if status:
        msg = a.message or f"sync: {datetime.now(timezone.utc).isoformat()}"
        git(["commit", "-q", "-m", msg], d)
        committed = True
    pushed = None
    pull_note = None
    if remote_url() and "origin" in git(["remote"], d).stdout.split():
        branch = git(["rev-parse", "--abbrev-ref", "HEAD"], d).stdout.strip()
        remote_has_branch = bool(
            git(["ls-remote", "--heads", "origin", branch], d).stdout.strip())
        if remote_has_branch:
            # pull first so divergence surfaces as a clear reject-and-resolve
            pull = git(["pull", "--no-edit", "--no-rebase", "origin", branch],
                       d, check=False)
            if pull.returncode != 0:
                # leave the tree CLEAN (a conflicted db.sql would corrupt the
                # next load). Local commit is kept but unpushed; surface the
                # conflicting files for the agent/user to resolve.
                conflicts = git(["diff", "--name-only", "--diff-filter=U"],
                                d, check=False).stdout.split()
                git(["merge", "--abort"], d, check=False)
                raise MemoError(
                    "GIT_ERROR",
                    "diverged: remote changed the same data. Local commit kept "
                    "but NOT pushed (nothing lost). Resolve manually, then sync. "
                    f"conflicting: {conflicts or ['unknown']}")
            pull_note = pull.stdout.strip().splitlines()[-1] if pull.stdout.strip() else "up to date"
        push = git(["push", "-u", "origin", branch], d, check=False)
        if push.returncode != 0:
            raise MemoError(
                "GIT_ERROR",
                "push rejected (remote moved) — re-run sync to pull and retry. "
                f"details: {push.stderr.strip()}")
        pushed = True
    emit({"committed": committed, "pushed": pushed, "pull": pull_note},
         as_json=a.json,
         human=("committed; " if committed else "nothing to commit; ")
               + ("pushed" if pushed else "no remote"))


# ---------------------------------------------------------------------------
# small utils
# ---------------------------------------------------------------------------

def _safe_files_path(name: str, rel: str) -> Path:
    """Resolve a path strictly under <ws>/files/ — rejects traversal escapes."""
    root = (require_ws(name) / "files").resolve()
    target = (root / (rel or ".")).resolve()
    if root != target and root not in target.parents:
        raise MemoError("FS_ERROR", f"path escapes workspace: {rel}")
    return target


def _table(cols, rows):
    if not rows:
        return "(no rows)"
    widths = {c: len(c) for c in cols}
    for r in rows:
        for c in cols:
            widths[c] = max(widths[c], len(str(r.get(c, ""))))
    head = "  ".join(c.ljust(widths[c]) for c in cols)
    sep = "  ".join("-" * widths[c] for c in cols)
    body = "\n".join("  ".join(str(r.get(c, "")).ljust(widths[c])
                     for c in cols) for r in rows)
    return f"{head}\n{sep}\n{body}"


# ---------------------------------------------------------------------------
# arg parsing
# ---------------------------------------------------------------------------

def build_parser():
    p = argparse.ArgumentParser(prog="memo", description=__doc__.splitlines()[0])
    p.add_argument("--json", action="store_true", help="machine-readable output")
    sub = p.add_subparsers(dest="cmd", required=True)

    sub.add_parser("init", help="initialize the store (git repo)").set_defaults(fn=cmd_init)

    ws = sub.add_parser("workspace", aliases=["ws"], help="workspace ops")
    wss = ws.add_subparsers(dest="wcmd", required=True)
    c = wss.add_parser("create"); c.add_argument("name"); c.add_argument("--description", "-d"); c.set_defaults(fn=cmd_ws_create)
    c = wss.add_parser("list"); c.set_defaults(fn=cmd_ws_list)
    c = wss.add_parser("info"); c.add_argument("name"); c.set_defaults(fn=cmd_ws_info)

    db = sub.add_parser("db", help="database ops")
    dbs = db.add_subparsers(dest="dcmd", required=True)
    c = dbs.add_parser("query"); c.add_argument("name"); c.add_argument("sql"); c.set_defaults(fn=cmd_db_query)
    c = dbs.add_parser("exec"); c.add_argument("name"); c.add_argument("sql"); c.set_defaults(fn=cmd_db_exec)

    fs = sub.add_parser("fs", help="filesystem ops")
    fss = fs.add_subparsers(dest="fcmd", required=True)
    c = fss.add_parser("write"); c.add_argument("name"); c.add_argument("path"); c.add_argument("content", nargs="?", default=None); c.set_defaults(fn=cmd_fs_write)
    c = fss.add_parser("cat"); c.add_argument("name"); c.add_argument("path"); c.set_defaults(fn=cmd_fs_cat)
    c = fss.add_parser("ls"); c.add_argument("name"); c.add_argument("path", nargs="?", default="."); c.set_defaults(fn=cmd_fs_ls)
    c = fss.add_parser("rm"); c.add_argument("name"); c.add_argument("path"); c.set_defaults(fn=cmd_fs_rm)

    c = sub.add_parser("sync", help="commit and push to remote")
    c.add_argument("--message", "-m", default=None); c.set_defaults(fn=cmd_sync)
    return p


def main():
    load_env()
    args = build_parser().parse_args()
    try:
        args.fn(args)
    except MemoError as e:
        fail(e, as_json=getattr(args, "json", False))


if __name__ == "__main__":
    main()
