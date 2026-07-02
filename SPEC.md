# memento — Spec v2 (Go)

> Supersedes `memory-workspace-service-spec.md` (historical: the GCS/changeset design,
> kept only as the "if we ever outgrow git" reference) and the Python POC in `memo/`
> (kept as a working reference implementation until the Go CLI reaches parity).

## Vision

A single-user CLI, `memento`, that is the memory backend for all AI agents and personal
tools. Data lives per **namespace** in two forms:

- **structured** — a SQLite database (budgets, tasks, goals, logs)
- **unstructured** — markdown/files (category docs, how-tos, notes, summaries)

Every consumer — a Claude Code subagent, a cron job, a third-party task loader —
talks to the same CLI. The store is a git repo, so every write is versioned, history
is the backup, and a periodic push mirrors it to GitHub.

**Core philosophy unchanged:** dumb storage, smart agent. No AI logic, no embeddings,
no auto-anything in the storage layer. The intelligence lives in the calling agent —
specifically a generalist Claude Code subagent that owns SQL authoring, schema
creation, and migrations, so the main session never writes SQL mid-conversation.

## Predecessor lessons (rogue / xmen)

The previous system (`~/Desktop/github/rogue`, store at `~/Desktop/agents/memory/xmen`)
already proved the namespace + sqlite + files model. What memento changes and why:

| rogue did | memento does | why |
|---|---|---|
| MCP server (`rogue-store`) | plain CLI | usable by cron, shell scripts, third-party tools — no MCP client needed |
| binary `store.sqlite` (WAL) on disk | text dump `db.sql` in git | diffable history; binary blobs give opaque, unmergeable history |
| schema embedded in prompt files (`powers/finances.md`) | `SCHEMA.md` + `migrations/` *inside* the namespace | schema lives with the data, versioned together, discoverable via `memento context` |
| per-domain "powers" prompts | one generalist subagent | new namespaces need zero prompt changes |

## Locked decisions

| # | Decision |
|---|---|
| Users | Single user, macOS only (for now). |
| Language | Go. Dependencies allowed if they work. No branches in the store repo — single branch, linear-ish history. |
| Repo layout | **Two repos.** `memento` = the tool (this folder, Go CLI). `memento-store` = the data repo at a separate path, created/initialized by `memento init`; only the store repo gets per-write commits and the periodic push. Code publishing stays manual. |
| CLI name | `memento` |
| Substrate | git + GitHub. History = backup. |
| DB persistence | Text dump (`db.sql`) is the tracked source of truth; binary SQLite is never committed. |
| Commit granularity | **Per write.** Every mutating command creates one commit. |
| Push cadence | Periodic, via **launchd** calling `memento sync`. |
| Conflict UX | Reject-and-resolve. Unattended sync failures surface through `memento status`. |
| Namespaces | One store, many namespaces. Namespace argument is **always required** — no default binding. |
| Config | Single global config: `~/.config/memento/config.toml`. |
| Consumers | Everything goes **through the CLI**. No direct file/DB access by tools. All namespaces visible to all consumers (single trusted user). |
| Schema ownership | The **subagent** owns schema creation and migrations, via the CLI's migration machinery. |
| Agent layer | One **generalist** Claude Code subagent, invoked with natural-language tasks. |
| Retrieval | `grep` + optional FTS5. No embeddings. |
| First namespaces | `tasks`, `finances`, `goals`. |
| Adoption path | Step 1: replace rogue/xmen. Later: overtake adhd, engram, hub, and the default Claude memory dir. |

## Repo & store layout

```
memento/                        # tool repo (this folder) — normal manual git workflow
├── SPEC.md / TESTS.md
├── go.mod / main.go / internal/...   # the CLI
└── agents/memento.md           # the Claude Code subagent definition (source of truth)

memento-store/                  # data repo — separate git repo at the path in config,
├── .gitignore                  #   created by `memento init` (or cloned if remote set);
└── <namespace>/                #   blocks **/db.sqlite*
    ├── scope.yaml              # name, description, created_at, manifest_version
    ├── db.sql                  # SQLite as deterministic TEXT dump — tracked source of truth
    ├── migrations/             # 0001_init.sql, 0002_add_x.sql ... (subagent-authored)
    └── files/                  # unstructured: md docs, how-tos, exports, blobs
        └── SCHEMA.md           # semantics: table meanings, enums, units, conventions
                                #   — a convention, not a special file: lives in files/
                                #   so normal `fs write/cat` manage it
```

- The binary DB is materialized **in memory** per operation from `db.sql` and dumped
  back. No `db.sqlite` on disk (`.gitignore` enforced).
- Reserved: `_meta_migrations` table (the only schema the service imposes),
  `scope.yaml`, `migrations/`.
- Big binaries go in `files/`, never the DB.

## CLI surface

Stateless; every db/fs command names its namespace explicitly.

| group | commands |
|---|---|
| store | `init`, `sync`, `status` |
| namespace | `ns create <name> [-d desc]`, `ns list`, `ns info`, `ns rename`, `ns delete` |
| db | `db query <ns> <sql>` (**SELECT-only, enforced**), `db exec <ns> <sql>`, `db schema <ns>` |
| migrations | `db migrate new <ns> <slug>` (scaffolds file), `db migrate up <ns>`, `db migrate status <ns>` |
| fs | `fs write <ns> <path> [content\|stdin]`, `fs cat`, `fs ls`, `fs rm`, `fs mv`, `fs mkdir`, `fs grep <ns> <pattern>` |
| agent | `context <ns>` — one-call orientation blob (see Agent integration). `SCHEMA.md` needs no dedicated commands — it lives in `files/`, managed by `fs write/cat` like any doc |

Behavior rules:

- **Every mutating command commits** its own change with a conventional message:
  `data(<ns>): <summary>` (auto) or `-m` override, authored as
  `memento <memento@localhost>`. Commits **stage only the paths the command touched**
  — the store repo is data-only so this is hygiene rather than safety, but it keeps
  each commit surgically revertable. `sync` never commits fresh work — it only pulls
  + pushes (plus the recovery case below).
- `db query` refuses non-SELECT statements (read-only authorizer). *POC defect fix:
  the Python version silently discarded writes issued through `query`.*
- `db exec` on failure leaves `db.sql` untouched (dump only written on success) —
  on-disk atomicity per command.
- `fs` paths are jailed under `<ns>/files/`; traversal rejected, **symlinks resolved
  before the containment check** (a link escaping the jail is rejected); `fs rm`
  refuses the `files/` root; `fs mv` refuses an existing target without `--force`.
  `fs write`/`cat` support binary via stdin/stdout (base64 in JSON mode).
- **Locking: one exclusive store-wide flock per command invocation** (mutations,
  sync, and reads that load a DB). Parallel sessions are safe; writes to different
  namespaces serialize, which is irrelevant at this scale. A single lock was chosen
  over per-namespace + git locks after stress-testing found a lock-order inversion
  (exec takes ns→git, sync validation takes git→ns) — see TESTS.md F2. Lock wait
  timeout → exit 10. *POC defect fix: last-writer-wins whole-DB overwrite.*
- **db.sql writes are atomic** (temp file + rename) — a killed process leaves the old
  dump intact, never a partial one.

## Output contract

- `--json` flag (agents/tools always pass it): `{ "ok", "data", "error": {code, message}, "meta" }`
- Human-readable tables/text on a TTY otherwise.
- Stable exit codes: 0 ok, 2 BAD_ARG, 3 NS_NOT_FOUND, 4 NS_EXISTS, 5 STORE_NOT_INIT,
  6 SQL_ERROR, 7 FS_ERROR, 8 GIT_ERROR, 9 DIVERGED, 10 LOCKED, 11 MIGRATION_ERROR.

## Migrations

- A migration is a plain SQL file in `<ns>/migrations/`, authored by the subagent
  (or the user), applied with `db migrate up`.
- Applied migrations tracked in the reserved `_meta_migrations` table
  (filename, applied_at, sha256).
- `db migrate up` applies pending files in order, **one commit per applied file**
  (`data(<ns>): migrate 0002_x`); a failing file applies nothing of itself and stops
  the run — earlier files stay applied, re-running `up` resumes after the fix.
- An applied migration file whose content no longer matches its recorded sha256 is
  **checksum drift**: `migrate status` flags it, `migrate up` refuses (exit 11).
- Recovery story: `migrations/` + git history means any past schema state is
  reconstructable; data recovery is `git revert`/`checkout` on `db.sql`.
- Convention for the subagent: **schema changes go through migrations, never through
  raw `db exec` DDL**, and every migration updates `SCHEMA.md` in the same change.

## Sync, status, divergence

- `sync`: recovery-commit → fetch → pull (merge, no rebase) → push. On merge
  conflict: abort the merge, keep local commits unpushed, leave the tree clean (a
  conflicted `db.sql` must never exist on disk), record the failure, exit 9.
- **Recovery commit:** if the store repo's tree has uncommitted changes (a crash
  between a db.sql write and its commit, or manual edits made directly in the store
  bypassing the CLI — narrow staging means no later command would ever pick them up),
  sync first commits them as `data(<ns>): recover uncommitted changes`.
- **Post-merge validation** (*POC defect fix*): after any pull that changed `db.sql`
  files, re-load each affected namespace; if a "clean" git merge produced SQL that
  fails to load (duplicate PKs, duplicate CREATE TABLE), roll back with
  `git reset --hard ORIG_HEAD` (covers merge and fast-forward pulls alike), keep
  local commits, record it, exit 9. Git-clean is not SQL-clean.
- `status`: working-tree state, ahead/behind counts vs origin, last successful push
  timestamp, and any recorded sync failure (with conflicting files). This is the
  single place to look when the unattended push has been failing.
- Periodic push: a launchd agent (`com.user.memento.sync`, e.g. every 30 min) runs
  `memento sync --json >> log`. Unattended conflicts don't page you; they show up in
  `status`, and every local write is still committed locally (nothing is ever lost).
- `memento install-launchd` scaffolds the plist (nice-to-have, can be manual at first).

## Agent integration (the product layer)

### `memento context <ns>`

One call returns everything an agent needs to operate on a namespace:

```json
{
  "namespace": "finances",
  "description": "monthly budget tracking",
  "tables": { "months": "CREATE TABLE months(...)", ... },
  "schema_md": "<contents of files/SCHEMA.md>",
  "migrations": ["0001_init.sql", "0002_add_loans.sql"],
  "files": ["categories.md", "howto-monthly-close.md"]
}
```

### The generalist subagent

Defined at `.claude/agents/memento.md` (source-of-truth copy in this repo at
`agents/memento.md`). Claude Code subagent format: markdown with YAML frontmatter.

```markdown
---
name: memento
description: >
  Memory agent. Use PROACTIVELY for anything the user wants remembered, recalled,
  logged, or queried from personal data: tasks, finances, goals, notes, learnings.
  Translates natural-language requests into memento CLI calls (SQL + files).
tools: Bash(memento:*)
---

You operate the `memento` CLI — the user's single memory store. You receive a
natural-language task (e.g. "log 45€ groceries yesterday", "what are my open
tasks?", "save this book summary under goals").

Rules:
1. Run `memento ns list --json`, pick the right namespace; then
   `memento context <ns> --json` to load its schema and conventions. Never guess
   a schema from memory.
2. Reads: `memento db query <ns> "<SELECT ...>" --json` and/or `memento fs cat/grep`.
3. Writes: `memento db exec` for rows, `memento fs write` for prose.
4. Schema changes (new table/column): create a migration with
   `memento db migrate new`, write the SQL, `memento db migrate up`, and update
   the conventions doc via `memento fs write <ns> SCHEMA.md` in the same task.
   Never issue DDL through raw exec.
5. If no namespace fits, ask the main session before creating one with `ns create`.
6. Return a short natural-language answer plus the essential data. Never return
   raw SQL, raw dumps, or large result sets — summarize.
7. Deletions require explicit user confirmation relayed through the main session.
```

The main session's contract: it never writes SQL. It delegates any memory task to
this subagent in natural language and relays the summarized answer.

### Third-party tools

Same door: invoke `memento ... --json`, parse the envelope, respect exit codes.
A tool that reads the `tasks` namespace relies on the migration discipline above —
the subagent evolves schemas additively (add columns/tables; renames and drops need
a deliberate migration you review). The envelope shape and exit codes are the
compatibility contract and only change with a major version.

## Go implementation notes

- **SQLite driver:** `modernc.org/sqlite` (pure Go, no cgo, FTS5 included) —
  recommended: trivial single-binary builds. Alternatives: `zombiezen.com/go/sqlite`
  (ergonomic wrapper over modernc), `mattn/go-sqlite3` (cgo; fine since macOS-only,
  but adds a toolchain dependency for zero benefit at this scale).
- **The dumper is the critical piece.** Go has no `iterdump()`. Implement a
  deterministic text dump: emit schema from `sqlite_master` (stable order: tables by
  name, then indexes/triggers/views), then rows ordered by rowid/PK, one INSERT per
  row, stable value formatting. Must round-trip: dump → load → dump is byte-identical.
  **FTS5 virtual tables must survive the round-trip** — emit `CREATE VIRTUAL TABLE`
  and rebuild content, don't dump shadow tables. Internal tables need explicit
  handling: `sqlite_sequence` (AUTOINCREMENT state) is re-populated, `sqlite_stat*`
  skipped. (Escape hatch if this drags:
  shell out to `/usr/bin/sqlite3 .dump` — acceptable on macOS-only, but pins output
  format to the OS's sqlite version.)
- CLI framework: `cobra` or stdlib `flag` — whichever, no strong opinion.
- git via `os/exec` (system git), not go-git — sync semantics must match what a human
  gets when they open the repo with real git.
- Config: `~/.config/memento/config.toml` → `store_path` (**absolute** — written by
  `init`; a PATH-installed binary has no meaningful default), `remote`,
  `sync.interval`. `init` creates the store repo at `store_path` (`git init`, or
  `git clone` when `remote` is set and non-empty). Env overrides (`MEMENTO_STORE`,
  `MEMENTO_REMOTE`) for tests. No config and no env → STORE_NOT_INIT.

## Testing

The full test plan lives in **`TESTS.md`**: scriptable CLI tests (store/ns/db/fs/
migrations/sync/locking), end-to-end scenarios (cold start, surgical undo, time
travel, unattended week, rogue/xmen import), agent-level scenarios exercising the
subagent, edge cases, and the stress-test findings (F1–F11) that shaped this spec.
The Python POC's `test.sh` scenarios are carried over as the base acceptance suite.

## Adoption plan

1. **v0:** Go CLI reaches Python-POC parity + the defect fixes + migrations + context.
2. **Migrate rogue/xmen:** import `xmen/*/db/store.sqlite` → dump into namespaces;
   move `xmen/*/files` → `files/`; distill each relevant `powers/*.md` schema section
   into that namespace's `SCHEMA.md`. Seed `tasks`, `finances` (schema already exists
   in `powers/finances.md`), `goals`.
3. **Wire the subagent** into Claude Code globally; add the "delegate memory tasks to
   memento" rule to `~/.claude/CLAUDE.md`.
4. **Launchd periodic sync.**
5. Later: migrate adhd namespaces, engram, hub, and the default memory dir; retire them.

## Non-goals (unchanged)

Vector DBs, embeddings, semantic search, auto-tagging, knowledge graphs, AI reasoning,
auto-categorization, auto-schema-gen, templates, conversation storage, multi-user,
servers/daemons (launchd invoking a short-lived CLI is not a daemon), cross-namespace
queries (agent runs multiple queries), branches in the store repo.

Deferred until actually hit: semantic SQL merge / custom merge driver, changeset-based
incremental sync, hundreds-of-MB namespaces (in-memory materialization caps practical
DB size at tens of MB — accepted).

## Open questions

None blocking. Resolved along the way:

- `ns delete` requires `--force` and tags `pre-delete-<ns>-<date>` first.
- Data commits authored `memento <memento@localhost>`.
- **Repo split (was TESTS.md F10):** two repos — `memento` (tool) and `memento-store`
  (data). Only the store repo gets per-write commits and the launchd push; code
  publishing stays under manual control. This also dissolves the code-sweeping risk
  that motivated F3.
- **`git gc`:** no scheduled job. git automatically packs/prunes loose objects
  (`gc --auto` fires during normal operations like commit/pull once thresholds are
  hit), and since we shell out to system git, that behavior comes for free.
