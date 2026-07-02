# Memory Workspace Service — Corrected Spec (MVP)

> Supersedes the original vision doc. Same core idea ("dumb storage, smart agent"),
> but the network/offline/sync problems that the original waved away are made explicit,
> and the design is phased so v1 is actually buildable.

## Vision

A single-user CLI that is the **one place all your structured data and prose lives** —
tasks, finances, book summaries, things learned, work tickets, refinement docs — instead
of scattered across tools. Any AI tool or script you build talks to the same CLI, so
"add a task", "log a transaction", "save this summary" all hit one store you own.

The service is **dumb storage**: a filesystem + a SQLite database per namespace, exposed
through primitive CLI commands. All intelligence (what to store, schema, retrieval) lives
in the calling agent.

**Honest scope note:** the combination of *no server + object storage + offline edits +
multiple devices + hundreds-of-MB DBs* means this is a small sync engine, not a thin
wrapper. It is phased accordingly (see Phasing).

## Locked decisions

| # | Decision |
|---|----------|
| Users | Single user, many devices, parallel sessions per device. One key set, all workspaces open. No per-workspace authz. |
| Server | **None.** Strictly CLI + a blob store (GCS bucket or equivalent). No VM, no daemon. |
| Topology | **Local-first.** Bucket is the sync/backup substrate. Optional remote-direct mode for *small* workspaces only. |
| Cross-workspace | Not supported. Agent runs multiple queries against multiple workspaces. |
| Divergence | **Reject-and-resolve.** A stale write is rejected and surfaced, never silently overwritten. |
| DB size | Plan for **hundreds of MB** per workspace. Sync must be incremental; whole-file push only for initial seed. |
| Output | Two modes: `human` (TTY default) and `json` (non-TTY default). |
| Isolation | Footgun-guard, not a security boundary (single trusted user). Path-jail + `ATTACH`/extensions off by default. |

## Architecture

Local-first. Each device holds a local copy of every workspace it uses. The bucket is a
passive blob store: whole objects, no locking, no transactions. All coordination is done
by the CLI using object generation preconditions.

```
device A                          bucket (blobs only)                 device B
--------                          -------------------                 --------
local workspaces/  <--- sync --->  <ws>/base.sqlite (seed snapshot)  <--- sync ---> local workspaces/
                                   <ws>/changelog/000123.chg  (deltas)
                                   <ws>/files/...             (per-file objects)
                                   <ws>/lease.json            (active-device lease)
                                   <ws>/scope.yaml
```

### Sync model

- **DB writes**: commit locally (WAL mode), then append a **SQLite changeset** (session
  extension, row-level delta, KB-sized) to `<ws>/changelog/<seq>.chg`. Never ship the
  whole DB except the first `base.sqlite` seed.
- **Pull**: list changelog objects since last-seen `<seq>`, apply in order. Apply uses
  the changeset **conflict callback** → surfaces conflicts (reject-and-resolve).
- **files/ sync**: per-file by content hash. Upload only changed files. (git optional,
  text-only, for prose history — never for large binaries.)
- **Large blobs (PDFs/images) live in `files/`, not the DB**, to keep changesets small.

### Concurrency

- **Same device, parallel sessions**: SQLite WAL + `busy_timeout`. SQLite serializes writers.
- **Cross device (v1)**: a **lease** object per workspace marks the active device. A write
  from a device without a current lease (or with a stale generation) is **rejected**; the
  CLI tells you to pull/replay/retry. Effectively single-active-device-at-a-time.
- **Cross device (v2)**: concurrent offline editing via changeset merge with conflict
  callbacks. Scoped separately.

### Modes

- **Local mode** (required for large workspaces): edit local, sync deltas to bucket.
- **Remote-direct mode** (small workspaces only): download → SQL → upload with generation
  precondition. Not usable at hundreds of MB — the CLI refuses remote-direct above a size
  threshold and tells you to use local mode.

## Storage layout

```
workspaces/<scope>/
  scope.yaml        # manifest: name, description, created_at, manifest_version
  db.sqlite         # WAL-mode SQLite, user-defined schema
  files/            # user-controlled tree; large binaries here
  SCHEMA.md         # optional: human conventions/semantics for interop between tools
```

Reserved by the service (agents must not touch): `_meta_*` tables, `<ws>/changelog/`,
`<ws>/base.sqlite`, `<ws>/lease.json`.

## CLI surface

Every command names its workspace explicitly. Stateless.

**Workspace**: `create`, `list`, `info`, `rename`, `delete`, `snapshot`, `restore`
**Database**: `query` (SELECT), `exec` (arbitrary SQL, multi-statement = one transaction),
`migrate`, `schema` (introspection convenience over `sqlite_master`/`PRAGMA`)
**Filesystem**: `ls`, `cat`, `write`, `mkdir`, `mv`, `rm`, `grep` (content search)
**Sync**: `sync` (push/pull deltas), `status` (local vs remote divergence)

Notes:
- `exec` with multiple statements runs in a single SQLite transaction → read-modify-write
  is atomic *within the DB*. Anything spanning DB **and** files is **best-effort**; the
  agent handles partial failure. (No cross-resource atomicity — stated, not hidden.)
- `grep` + optional user-created **FTS5** virtual tables cover text retrieval. No embeddings.

## Output contract

Pin this before writing any consumer.

- `--format human|json`. Default `json` when stdout is not a TTY, `human` when it is.
- JSON envelope:
  ```json
  { "ok": true,  "data": {...}, "error": null, "meta": {"workspace":"tasks","rows":3} }
  { "ok": false, "data": null, "error": {"code":"WS_DIVERGED","message":"..."} }
  ```
- Stable exit codes (0 ok; distinct non-zero for not-found, diverged, conflict, auth, io).

## Migrations

- User-defined SQL. Service tracks applied migrations in a **reserved `_meta_migrations`
  table** (this is the one schema the service imposes — stated explicitly).
- Responsibilities: track applied, apply new, prevent duplicates. Never generate.
- Agents use `CREATE TABLE IF NOT EXISTS` discipline for idempotent cold-start schema.

## Backup / recovery / undo

- **Backup** = the changelog + base snapshot in the bucket *is* the durable history.
  Point-in-time restore = replay changelog to a target `<seq>`.
- **Pre-op snapshot**: before a destructive op (`DELETE`/`DROP`/`UPDATE`-without-`WHERE`,
  overwriting `write`), snapshot first → cheap undo for the "agent broke it" case.
- `files/` history via optional git (text) or object versioning on the bucket.

## Schema conventions (multi-tool interop)

- Structure: agents introspect via `schema` / `sqlite_master` / `PRAGMA table_info`.
- Semantics (enum values, units, meaning): kept in `SCHEMA.md` inside the workspace.
  Storage stays schema-agnostic; the convention doc is just data the agent reads.

## Error handling

Fail loudly. Structured, machine-readable errors (see envelope). Never silently modify.
Destructive ops snapshot first.

## Non-goals (MVP)

Vector DBs, embeddings, semantic search, auto-tagging, knowledge graphs, AI reasoning,
auto-categorization, auto-schema-gen, templates, conversation storage, document indexing.

**Deferred, not banned** (needed by the vision, scoped to v2): concurrent offline
multi-device merge; remote-direct mode for large workspaces.

## Phasing

- **v1 (buildable now):** local WAL SQLite; single-active-device lease + generation
  precondition (reject-and-resolve); changeset-log + base snapshot backup; per-file
  `files/` hash sync; pre-op snapshot undo; `grep`; two-mode output; migrations via
  `_meta_migrations`. Covers ~95% of real single-machine usage.
- **v2:** concurrent offline multi-device via changeset merge with conflict resolution;
  remote-direct for large workspaces if still wanted.

## Open risks

1. **Changeset-based sync is the crux.** SQLite session extension must be in the build,
   and conflict-callback UX (how *you* resolve a rejected write) needs a concrete flow.
2. **Base-snapshot seed of a hundreds-of-MB workspace is a slow first sync per device.**
   One-time cost, but real.
3. **Lease liveness**: if a device dies holding the lease, need a manual/forced lease-break.
4. **files/ + DB best-effort split** means a crash between a DB write and a file write
   leaves logical inconsistency the agent must tolerate.
