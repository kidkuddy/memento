# memento — Test Plan & Spec Stress-Test

Companion to `SPEC.md`. Three layers: scriptable CLI tests (bash, like the POC's
`test.sh`), end-to-end scenarios, and agent-level scenarios (manual/eval — exercise
the subagent, not just the CLI). Section 5 records where stress-testing these
scenarios **broke the spec** and what was amended.

Conventions: every test runs against a temp clone of the repo with `MEMENTO_STORE`
pointed at a temp store and a local bare repo as the GitHub stand-in (no network),
same harness style as `memo/test.sh`.

---

## 1. CLI tests (scriptable)

### 1.1 store / init / config

| id | scenario | expected |
|---|---|---|
| ST-01 | `init` on a fresh machine | creates the **store repo** at the configured path (`git init`, or `git clone` when remote set), `.gitignore` (blocks `**/db.sqlite*`), writes config with **absolute** `store_path`, commits scaffold |
| ST-02 | `init` twice | idempotent, exit 0, no duplicate commit |
| ST-03 | any command with no config and no `MEMENTO_STORE` | exit 5 STORE_NOT_INIT, message says run `init` |
| ST-04 | `status` on clean, synced store | clean tree, ahead 0 / behind 0, last-push timestamp |

### 1.2 namespaces

| id | scenario | expected |
|---|---|---|
| NS-01 | `ns create tasks -d "..."` | dir + scope.yaml + empty db.sql + files/ + one commit `data(tasks): create namespace` |
| NS-02 | create duplicate | exit 4 NS_EXISTS |
| NS-03 | invalid names: `a/b`, `.hidden`, `..`, empty, `store`, `_meta` | exit 2 BAD_ARG (reserved names include anything starting with `.` or `_`) |
| NS-04 | case collision on APFS: create `Tasks` when `tasks` exists | exit 4 NS_EXISTS (case-insensitive FS makes them the same dir) |
| NS-05 | `ns rename tasks todo` | `git mv` + commit; old name gone, data intact |
| NS-06 | `ns delete` without `--force` | refused; with `--force`: tag `pre-delete-<ns>-<date>` created, then removal committed |
| NS-07 | `ns list` / `ns info` ignore stray non-namespace dirs in `store/` | only dirs with scope.yaml listed |

### 1.3 db — query (read-only enforcement)

| id | scenario | expected |
|---|---|---|
| DBQ-01 | plain SELECT | rows in envelope, `meta.columns`, `meta.rows` |
| DBQ-02 | `WITH cte AS (...) SELECT ...` | allowed |
| DBQ-03 | `UPDATE`, `INSERT`, `DELETE`, `DROP`, `CREATE` via `db query` | exit 6, **nothing executed, nothing committed** (POC defect: silently discarded) |
| DBQ-04 | `PRAGMA table_info(x)` via query | rejected; message points at `db schema` |
| DBQ-05 | multi-statement `SELECT 1; SELECT 2` | rejected (single statement only) |
| DBQ-06 | SELECT against empty/new ns | exit 6 "no such table", db.sql untouched |

### 1.4 db — exec & dump round-trip

| id | scenario | expected |
|---|---|---|
| DBE-01 | CREATE + INSERT + SELECT round-trip | data persists across separate invocations |
| DBE-02 | multi-statement exec where the last statement fails | db.sql **byte-identical to before** (dump only written on full success) |
| DBE-03 | exec commits exactly one commit touching only `<ns>/db.sql` | `git show --stat` proves staging scope |
| DBE-04 | values: emoji, newlines-in-TEXT, quotes, NULL, REAL, negative INT, BLOB (`X'...'`) | dump → load → dump is byte-identical |
| DBE-05 | AUTOINCREMENT table (`sqlite_sequence`) | round-trips; sequence continues correctly after reload |
| DBE-06 | FTS5 virtual table + content + `MATCH` query after reload | round-trips; MATCH works on the rebuilt DB (retrieval plan depends on this) |
| DBE-07 | hand-corrupt db.sql (invalid SQL), then any db op | exit 6 "failed to load"; recovery documented: `git checkout -- store/<ns>/db.sql` |
| DBE-08 | kill -9 the process mid-exec (loop until hit) | db.sql is always either old or new version, never partial (atomic temp+rename) |

### 1.5 fs

| id | scenario | expected |
|---|---|---|
| FS-01 | write/cat/ls/rm/mv/mkdir round-trips; nested paths auto-created | per-write commit each, staged to that ns only |
| FS-02 | traversal: `../x`, absolute paths, `a/../../x` | exit 7 |
| FS-03 | symlink inside files/ pointing outside the jail, then `cat`/`write` through it | exit 7 (jail resolves symlinks) |
| FS-04 | binary round-trip: PNG via stdin → `fs cat` (raw stdout) and `--json` (base64) | byte-identical (POC defect: text-only) |
| FS-05 | `fs rm <ns> .` and `fs rm <ns> ""` | refused — never remove the files/ root |
| FS-06 | `fs mv` onto an existing target | refused without `--force` |
| FS-07 | `fs grep` skips binary files; result count capped with `meta.truncated` |
| FS-08 | filenames with spaces + unicode | all ops work |

### 1.6 migrations

| id | scenario | expected |
|---|---|---|
| MG-01 | `migrate new` scaffolds `NNNN_slug.sql` with next number; `migrate status` lists it pending |
| MG-02 | `migrate up` applies pending in order; **one commit per applied file** (`data(ns): migrate 0002_x`) |
| MG-03 | `up` with file 2 of 3 failing | file 1 applied+committed, files 2–3 not; exit 11; re-run after fix resumes at 2 |
| MG-04 | re-running `up` | no-op (tracked in `_meta_migrations`) |
| MG-05 | edit an already-applied migration file | `migrate status` flags checksum drift; `up` refuses with exit 11 |
| MG-06 | migration + `fs write <ns> SCHEMA.md` in one agent task | two commits, SCHEMA.md matches new DDL |
| MG-07 | agent bypass: DDL via raw `db exec` | allowed (dumb storage) — convention enforcement is the subagent's job, asserted in agent-level tests |

### 1.7 sync / divergence / validation

| id | scenario | expected |
|---|---|---|
| SY-01 | sync with nothing to push | exit 0, "up to date" |
| SY-02 | fresh clone on "device B" | rebuilds DBs from db.sql, sees files (POC test carried over) |
| SY-03 | non-overlapping edits on A and B | clean merge, both survive |
| SY-04 | same-table concurrent INSERTs | merge conflict → abort, tree clean (no `<<<<<<<` ever in db.sql), local commits kept, exit 9 |
| SY-05 | **git-clean but SQL-broken merge** (both devices create the same table name at different dump positions → duplicate CREATE TABLE merges cleanly) | post-merge validation fails → `git reset --hard ORIG_HEAD`, exit 9, `status` explains |
| SY-06 | remote moves between pull and push | push rejected, exit 9, re-run sync succeeds |
| SY-07 | sync with no network (remote unreachable) | exit 8 GIT_ERROR; `status` distinguishes *offline* from *diverged* |
| SY-08 | crash after db.sql written but before commit, then sync | sync auto-commits stray changes as `data(<ns>): recover uncommitted changes` before pulling |
| SY-09 | manual edits dropped directly into the store repo, bypassing the CLI | picked up by the same recovery commit on next sync; nothing lost, nothing left dangling |
| SY-10 | `status` after each of SY-04/05/07 | shows the recorded failure, conflicting files, ahead/behind |

### 1.8 locking / concurrency

| id | scenario | expected |
|---|---|---|
| LK-01 | two concurrent `db exec` on the **same** ns (loop 50×) | both writes present after; no lost update (POC defect: last-writer-wins) |
| LK-02 | concurrent exec on two **different** ns | both succeed, two commits, no `index.lock` collision (global lock serializes) |
| LK-03 | `sync` concurrent with a long `db exec` | serialized by the global lock; no torn read of a half-written db.sql |
| LK-04 | lock holder killed -9 | flock released by OS; next command proceeds (no stale-lockfile state) |
| LK-05 | lock wait exceeds timeout (e.g. 30s) | exit 10 LOCKED, message says who/what likely holds it |

---

## 2. End-to-end scenarios

- **E2E-1 Cold start:** clone the tool repo on a fresh mac → build → `init` (clones
  the store repo from its remote) → `ns create tasks` → subagent logs a task →
  launchd plist installed → within one interval the commit is on the store origin.
  Proves the whole loop with zero manual git.
- **E2E-2 Surgical undo (the per-write-commit payoff):** 10 writes across 2 ns →
  accidentally `DELETE FROM tasks` → `git log --oneline -- tasks/` shows one
  commit per write → `git revert <sha>` → `db query` shows rows back. If this is
  clumsy in practice, per-write commits lose their main justification — evaluate.
- **E2E-3 Time travel (backup motivation):** `git show <sha-from-last-week>:tasks/db.sql`
  loaded into a scratch sqlite → answer "what were my tasks last week". Manual flow,
  documented; a `memento db query --at <ref>` convenience is deferred until wanted.
- **E2E-4 Unattended week:** simulate launchd (sync every N sec in a loop) while a
  writer process makes periodic writes and the "remote" occasionally gets pushes from
  a second clone incl. one conflicting + one SQL-breaking merge → at the end: no
  corruption, every failure visible in `status`, all local commits retained.
- **E2E-5 rogue/xmen import:** import `xmen/scheduler/db/store.sqlite` (real ~17-column
  `tasks` table) + `xmen/work/files` into namespaces; distill `powers/finances.md`
  schema into `finances/files/SCHEMA.md` via migrations → `context finances` returns
  it; row counts match the source DBs.

## 3. Agent-level scenarios (manual / eval — exercise the subagent)

- **AG-1 log:** "log 45€ groceries yesterday" → subagent: `ns list` → `context finances`
  → single INSERT with correct relative date → summarized confirmation. Assert: no
  SQL in the main session's visible reply; exactly one data commit.
- **AG-2 recall:** "what are my open tasks?" → SELECT + natural-language summary, not
  a raw table dump.
- **AG-3 schema evolution:** "start tracking mood alongside my goals" → `migrate new`
  + `migrate up` + `fs write goals SCHEMA.md` (NOT raw DDL via exec — this is MG-07's
  convention, asserted here) → SCHEMA.md and DDL agree.
- **AG-4 wrong-namespace guard:** "log that I ran 5k" with no fitness ns → subagent
  asks before `ns create`, doesn't silently invent one.
- **AG-5 destructive guard:** "clear my finished tasks" → subagent relays a
  confirmation request before DELETE.
- **AG-6 unstructured routing:** "save this how-to under finances" → lands in
  `files/*.md` via `fs write`, not as DB rows.

## 4. Edge cases (documented behavior, one assertion each)

- `db exec` with an empty string / whitespace-only SQL → BAD_ARG, no commit.
- TEXT values containing the dump's own delimiters (`);`, `INSERT INTO`) — round-trip.
- A namespace whose db.sql is empty (never had exec) — `context` and `ns info` work.
- 50 MB TEXT value — works but slow; document as the accepted in-memory cap territory.
- `git gc`/repack running concurrently with sync — git's own locking; sync retries next tick.
- Config `store_path` pointing at a non-existent dir → STORE_NOT_INIT, not a crash.
- System sqlite vs modernc version drift is irrelevant (single embedded engine does
  everything) — but assert the dump loads under `/usr/bin/sqlite3` too, since manual
  recovery flows use it.
- Commit author: data commits authored as `memento <memento@localhost>` so `git log
  --author=memento` separates data from code history (resolves SPEC open question 2).

---

## 5. Stress-test findings → spec amendments

Walking sections 1–3 against the spec broke it in the following places. SPEC.md has
been amended accordingly (marked *(amended)*) or the item is flagged as a user
decision (marked *(decision)*).

**F1 — SCHEMA.md was unreachable through the CLI.** *(resolved — user decision)* The
subagent's rule 4 ("update SCHEMA.md in the same task") was impossible: `fs` is jailed
to `files/`, and SCHEMA.md lived at the namespace root. Resolution: **SCHEMA.md moved
into `files/`** — it's a file like any other, managed by plain `fs write/cat`, no
dedicated commands. `context` reads `files/SCHEMA.md`.

**F2 — Deadlock in the two-lock design.** *(amended)* Spec had per-namespace locks
plus a store-wide git lock. `db exec` acquires ns→git; sync's post-merge validation
acquires git→ns. Classic lock-order inversion. Replaced with **one exclusive
store-wide flock per command invocation**. Cost: writes to different namespaces
serialize — irrelevant at single-user, KB–MB scale; kills the whole deadlock class.

**F3 — `git add -A` would sweep code edits into data commits.** *(mostly dissolved by
the two-repo split, see F10)* The store repo is now data-only, so the safety risk is
gone. Narrow staging (**only the paths the command touched**) is kept as hygiene —
it's what keeps every commit surgically revertable (E2E-2).

**F4 — Crash between db.sql write and commit left an orphan forever.** *(amended)*
Since commits stage narrowly (F3), an interrupted write's change would never be
committed by later commands. `sync` now auto-commits stray store-repo changes as
`data(<ns>): recover uncommitted changes` before pulling — this also absorbs manual
edits made in the store bypassing the CLI (SY-09).

**F5 — `<repo>/store` config default was meaningless.** *(amended)* A PATH-installed
binary has no `<repo>`. `init` now resolves and writes an **absolute** `store_path`
into the config; all other commands require config or `MEMENTO_STORE`.

**F6 — Post-merge validation rollback was unspecified.** *(amended)* Now concrete:
validation failure → `git reset --hard ORIG_HEAD` (covers both merge and fast-forward
pulls), local commits kept, exit 9, recorded for `status`.

**F7 — Migration failure semantics were ambiguous.** *(amended)* "Transactional per
file" now explicit: one commit per applied file; `up` stops at first failure (earlier
files stay applied); checksum drift on an applied file → refuse with MIGRATION_ERROR.

**F8 — Dumper spec missed internal tables and crash-atomicity.** *(amended)* Dumper
must handle `sqlite_sequence` / `sqlite_stat*`; db.sql writes go through temp-file +
atomic rename (DBE-05, DBE-08).

**F9 — fs jail didn't address symlinks; mv overwrite undefined.** *(amended)* Jail
resolves symlinks before checking containment; `fs mv` refuses existing targets
without `--force`.

**F10 — One-repo model auto-publishes your code commits.** *(resolved — user decision)*
The launchd push would have shipped local code commits along with data. Resolution:
**two repos** — `memento` (tool, manual git workflow) and `memento-store` (data,
per-write commits + periodic push), the store created/cloned by `memento init` at the
configured path.

**F11 — Subagent frontmatter tool syntax.** *(amended)* Permission-scoped Bash in
agent frontmatter uses the `Bash(memento:*)` (colon) form.

Held up fine under stress, for the record: reject-and-resolve conflict flow (SY-04),
text-dump round-trip approach, SELECT-only query enforcement, per-write commits
enabling surgical revert (E2E-2), the generalist-subagent + `context` design (AG-1..6
need no per-namespace prompts), and the launchd/no-daemon posture (SY-07/08 + `status`
cover unattended failure without notifications).
