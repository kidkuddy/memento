# CONTEXT — Memory Workspace Service (memo)

> **SUPERSEDED (2026-07-02): the project pivoted to a Go rewrite named `memento` — read
> `SPEC.md` (authoritative) first.** Headlines: CLI renamed memo → `memento`; Go instead
> of Python; one git repo = code + `store/`; per-write commits + periodic launchd push;
> migrations machinery (`db migrate`, subagent-owned schemas); `memento context <ns>`;
> a generalist Claude Code subagent as the SQL layer; adoption path replaces the old
> `rogue`/`xmen` system first, then adhd/engram/hub. The Python POC in `memo/` remains
> a working reference until the Go CLI reaches parity. Known POC defects (fixed in the
> Go spec, do NOT copy blindly): no locking (parallel writes lose data), `db query`
> silently discards writes, binary files unsupported in `fs`, no post-merge SQL
> validation, two vacuous test assertions in `test.sh` (lines 45/66).

Handoff doc for the **Python POC era**. Read this + `memory-workspace-service-spec.md` + `memo/` for the history of how we got here.

---

## 1. What this is

A **single-user, git-backed CLI** that is the one place all the user's structured data + prose lives (tasks, finances, book summaries, things learned, work tickets, ticket-refinement docs) instead of scattered across tools. Any AI tool or script the user builds talks to the same CLI, so "add a task", "log a transaction", "save this summary" all hit one store the user owns and controls.

**Core philosophy:** dumb storage, smart agent. The service exposes a SQLite DB + a filesystem per namespace through primitive commands. **No AI logic in the storage layer** — the calling agent decides what to store, the schema, and how to retrieve. Unix-composable primitives, not high-level workflows.

Project folder is `memento`. The CLI is called `memo`.

---

## 2. How we got here (decision trail)

Started from a vision doc (now rewritten as `memory-workspace-service-spec.md`). I stress-tested it; the user answered clarifying questions across several rounds. Locked decisions:

| Topic | Decision | Why |
|---|---|---|
| Users | **Single user**, many devices, parallel sessions per device | One key set, all workspaces open, no per-workspace authz |
| Server | **None allowed.** Strictly CLI + a place to store files | User constraint — no VM, no daemon |
| Substrate | **git + GitHub** (not GCS) | User's original approach; git already gives history/undo/offline/multi-device/reject-on-stale-push for free |
| SQLite storage | **Text dump (`db.sql`), never binary** | So git can diff/merge it; binary bloats git history and can't merge |
| Sync | `git commit` + `git push` | Reject-and-resolve = git's non-fast-forward push rejection |
| Divergence | **Reject-and-resolve** (refuse to silently overwrite) | User pick |
| Cross-workspace queries | **Not supported** — agent runs multiple queries | User pick |
| Retrieval | No semantic RAG / embeddings. Plain `grep` + optional user-created FTS5 is enough | User has no embedding model, no hard RAG need yet |
| Schema interop | Agents introspect via SQL (`sqlite_master`/`PRAGMA`); semantics live in an in-workspace `SCHEMA.md` | Storage stays schema-agnostic |
| Output | Two modes: `human` (TTY) + `json` envelope | User pick |
| DB size | "account for hundreds of MB" — stated as a hedge, likely KB–MB in reality | Drives the deferred changeset engine, see §6 |

### Key realization mid-conversation
My initial design put everything on a **GCS bucket** and hand-rebuilt versioning, history, and conflict-detection on top of raw object storage. Then the user revealed their old approach was **git+GitHub**. Git already does ~90% of what I was reinventing. The GCS design was over-engineered for a single user. **We pivoted to git.** The GCS/changeset-engine design survives only as the deferred path for the hundreds-of-MB-concurrently-edited corner (see §6). `memory-workspace-service-spec.md` still contains the fuller GCS/changeset writeup — treat it as the "if we ever outgrow git" reference, NOT the current implementation. The **current implementation is git-based** (§4).

---

## 3. Storage model

- The store dir (`MEMO_DIR`) is a **git repo**. `sync` = commit (+ push if `MEMO_REMOTE` set).
- Each workspace is a namespace directory:
  ```
  <store>/<name>/
    scope.yaml     # name, description, created_at, manifest_version
    db.sql         # SQLite as a TEXT dump — the source of truth (tracked)
    files/         # user-controlled tree for prose/blobs
  ```
- **No `db.sqlite` binary on disk.** `db.sql` is materialized into an **in-memory** SQLite for each op, then dumped back via `con.iterdump()`. `.gitignore` blocks `**/db.sqlite*` as belt-and-suspenders.
- Big blobs (PDFs/images) belong in `files/`, not the DB, to keep dumps small.

---

## 4. What's built (POC — all working)

```
memento/
├── CONTEXT.md                          # this file
├── memory-workspace-service-spec.md    # full design (incl. deferred GCS/changeset path)
└── memo/
    ├── memo.py        # the CLI, ~370 lines, stdlib only (sqlite3 + git via subprocess)
    ├── .env           # MEMO_DIR=./store, MEMO_REMOTE=  (working config)
    ├── .env.example
    ├── README.md
    └── test.sh        # 13-assertion end-to-end test, all green
```

### Commands implemented
| group | commands |
|---|---|
| store | `init`, `sync [-m msg]` |
| workspace | `ws create <name> [-d desc]`, `ws list`, `ws info <name>` (aliases: `workspace`) |
| db | `db query <ws> <sql>` (SELECT → rows), `db exec <ws> <sql>` (arbitrary/DDL, re-dumps) |
| fs | `fs write <ws> <path> [content|stdin]`, `fs cat`, `fs ls`, `fs rm` |

- Global `--json` flag → envelope `{ok, data, error, meta}`. Default human otherwise.
- `db exec` with multiple statements runs as one transaction (atomic within the DB).
- fs paths are jailed under `<ws>/files/` (traversal `../` rejected).
- Distinct exit codes per error class (see `fail()` in memo.py: WS_NOT_FOUND=3, WS_EXISTS=4, STORE_NOT_INIT=5, SQL_ERROR=6, FS_ERROR=7, GIT_ERROR=8, BAD_ARG=2).
- `init` sets a local git identity (`memo@localhost`) so commits work in a clean env; wires `origin` if `MEMO_REMOTE` set.

### Important behavior notes
- **`db exec`/`fs write` do NOT commit.** They only change the working tree. Committing is `sync`'s job (sync = the git step). This is intentional.
- **`sync` pull-before-push:** on first push (empty remote) it skips pull and does `push -u`. Otherwise it pulls first so divergence surfaces.
- **On pull conflict, sync aborts the merge** (`git merge --abort`), keeps the local commit unpushed, leaves the tree CLEAN (no conflict markers — critical, because a conflicted `db.sql` would fail to load as SQL), and returns a GIT_ERROR listing the conflicting files. This is the reject-and-resolve UX.

### Test coverage (`./test.sh`, 13/13 pass)
Uses a temp store + a local **bare repo as a stand-in for GitHub** (no network). Proves:
1. Basic ops (ws create/list, db exec/query, fs write/cat).
2. `db.sql` is text; no binary db exists.
3. Path traversal blocked.
4. Push to remote works.
5. **Fresh clone (device B) rebuilds the DB from the text dump** and sees files.
6. **Clean auto-merge** when two devices edit different files.
7. **Conflict surfaced, not lost/corrupted** when two devices edit the same data.

---

## 5. THE key finding (don't lose this)

My earlier claim "independent rows merge cleanly" was **wrong**. Concurrent `INSERT`s on two devices both grab the same autoincrement PK (`id=3`), so their `db.sql` dump lines land on the same line → **git conflict**, not a clean merge.

Implications (both handled in the POC):
- This is *correct* reject-and-resolve — the tool refuses to silently pick a winner. What genuinely merges cleanly is edits to **different files / well-separated parts** of the store (the realistic single-user multi-device case — you rarely edit the same table offline on two machines at once).
- Same-row / same-table-append concurrent edits require **manual resolution**. The long-term fix (semantic 3-way merge of `db.sql` at the SQL level, or a changeset engine) is **deferred** — not needed until the user actually hits it repeatedly.

---

## 6. Deferred / out of scope (spec calls this "v2")

Only justified if a workspace is **genuinely hundreds of MB AND concurrently edited offline on two devices** — unlikely for this user's data. Do NOT build preemptively:
- Changeset/session-extension-based incremental sync (ship row-level deltas instead of whole dumps).
- A custom git merge driver for `db.sql` (semantic SQL merge).
- Remote-direct mode (operate without a local copy).
- Also never-in-storage-layer: embeddings, semantic search, auto-tagging, knowledge graphs, auto-schema-gen, templates, conversation storage.

---

## 7. Next steps (pick up here)

Not yet done, roughly in priority order:
1. **Wire to a real GitHub repo** — test `MEMO_REMOTE=git@github.com:USER/repo.git` end-to-end (auth is whatever git already uses: SSH key / token; no separate API-key layer in POC).
2. **Missing commands:** `fs mv`, `fs mkdir`, `ws delete`, `ws rename`, `db migrate` (migrations tracked in a reserved `_meta_migrations` table — the one schema the service imposes), `db schema` (introspection convenience), `fs grep` (content search over `files/`).
3. **`sync` on plain `init` (no remote)** currently just commits — fine, but confirm agent ergonomics.
4. **Consider a real remote store other than GitHub?** Not needed; git remote satisfies the "no server, just a place to store files" constraint.
5. **Global install / entrypoint** — right now it's `./memo.py`. Add a wrapper or `pip`-installable console script if the user wants `memo` on PATH.
6. **Destructive-op guard** (spec mentions pre-op snapshot before DELETE/DROP/overwrite). Git history already provides undo via `git revert`/`checkout`, so this may be redundant — decide.

---

## 8. How to run

```bash
cd memento/memo
cp .env.example .env          # edit MEMO_DIR / MEMO_REMOTE
./memo.py init
./memo.py ws create tasks -d "personal + work"
./memo.py db exec tasks "CREATE TABLE tasks(id INTEGER PRIMARY KEY, title TEXT, done INT DEFAULT 0);"
./memo.py db exec tasks "INSERT INTO tasks(title) VALUES ('ship memo');"
./memo.py db query tasks "SELECT * FROM tasks WHERE done=0;"
./memo.py --json db query tasks "SELECT count(*) c FROM tasks;"
./memo.py sync -m "first task"
./test.sh                      # 13/13 should pass
```

## 9. Environment / constraints to remember
- Written in **Python 3.13**, stdlib only (sqlite3 3.50, git 2.50). No third-party deps on purpose.
- `memento` is NOT itself a git repo (only the *store* dir is). Don't confuse the two.
- User's global rules: no sycophancy / agreement language; conventional-commit messages; no Claude attribution in commits/PRs; never push without explicit confirmation; root-cause over band-aid fixes; `--release` for any cargo (N/A here, it's Python).
