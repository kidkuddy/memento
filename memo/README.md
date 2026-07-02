# memo — memory workspace CLI (POC)

Single-user, git-backed dumb storage for AI agents. See
[`../memory-workspace-service-spec.md`](../memory-workspace-service-spec.md) for the design.

## Model

- The store is a **git repo**. Sync = `git commit` + `push`. History, undo, offline
  edits, and reject-and-resolve-on-divergence all come from git.
- Each workspace is a namespace: `<name>/scope.yaml`, `<name>/db.sql`, `<name>/files/`.
- SQLite is stored as a **text dump** (`db.sql`), never as a binary — so git can diff
  and merge it. The binary is materialized in memory per-op and dumped back.
- No AI logic. The agent decides what to store, the schema, and how to retrieve.

## Setup

```bash
cp .env.example .env      # edit MEMO_DIR / MEMO_REMOTE as needed
./memo.py init
```

## Usage

```bash
./memo.py init
./memo.py ws create tasks -d "personal + work tasks"
./memo.py db exec tasks "CREATE TABLE tasks(id INTEGER PRIMARY KEY, title TEXT, done INT DEFAULT 0);"
./memo.py db exec tasks "INSERT INTO tasks(title) VALUES ('ship memo poc');"
./memo.py db query tasks "SELECT * FROM tasks WHERE done=0;"
./memo.py fs write tasks notes/idea.md "# rough idea"
./memo.py fs cat tasks notes/idea.md
./memo.py ws info tasks
./memo.py sync -m "add first task"        # commits; pushes if MEMO_REMOTE is set
```

Add `--json` to any command for a machine-readable envelope
(`{ok, data, error, meta}`) — the default when stdout is not a TTY-friendly agent.

## Commands

| group | commands |
|-------|----------|
| store | `init`, `sync` |
| workspace | `ws create`, `ws list`, `ws info` |
| db | `db query`, `db exec` |
| fs | `fs write`, `fs cat`, `fs ls`, `fs rm` |

## Test

```bash
./test.sh
```

Runs end-to-end against a temporary store + local bare "GitHub" remote, and proves:
db text round-trip across a fresh clone (device B), and reject-and-resolve when two
devices diverge.

## Known POC limits

- DB is materialized fully in memory per op — fine for KB–MB workspaces, not hundreds
  of MB (that's the deferred changeset-engine case in the spec).
- `db.sql` merges are text merges: independent rows merge cleanly; edits to the *same*
  row on two devices conflict and need manual resolution (surfaced by `sync`).
- Auth for a real remote is whatever git already uses (SSH key / token). No separate
  API-key layer in the POC.
