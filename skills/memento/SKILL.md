---
name: memento
description: >
  Personal memory store backed by the memento CLI (git-backed namespaces of
  SQLite + files). Use when the user wants to remember, recall, log, or query
  personal data — tasks, finances, goals, notes, learnings — e.g. "log this
  expense", "what are my open tasks", "save this summary", "add a goal".
  Prefer delegating to the memento subagent; this skill is the direct-CLI
  fallback and reference.
---

# memento — memory store

The user's single memory backend. One store, many namespaces; each namespace
holds a SQLite DB (persisted as a text dump, versioned per-write in git) and a
`files/` tree for prose. A launchd job pushes to GitHub periodically.

## Preferred path: delegate

If the `memento` subagent is available, delegate the whole natural-language
task to it ("log 45€ groceries yesterday") and relay its summary. It owns SQL
authoring, schema creation, and migrations. Only work the CLI directly when
the subagent is unavailable or you were explicitly asked to.

## Direct CLI use

Always `--json`; parse the envelope `{ok, data, error:{code,message}, meta}`.
Exit codes are stable (`memento help exit-codes`).

1. **Orient — never guess schemas:**
   ```
   memento ns list --json
   memento context <ns> --json     # DDL + files/SCHEMA.md + migrations + file tree
   ```
2. **Read:** `memento db query <ns> "<SELECT …>" --json` (SELECT-only, single
   statement — writes are rejected) · `memento fs cat/grep <ns> …`
3. **Write:** `memento db exec <ns> "<sql>" -m "data(<ns>): <what>"` ·
   `memento fs write <ns> <path> [content]` (stdin for binary). Every write
   auto-commits; do NOT run git in the store yourself.
4. **Schema changes** go through migrations, never raw DDL:
   ```
   memento db migrate new <ns> <slug>    # scaffolds migrations/NNNN_<slug>.sql
   # write the SQL into that file, then:
   memento db migrate up <ns>
   memento fs write <ns> SCHEMA.md "<updated conventions>"   # same task
   ```
   Evolve additively (add tables/columns); renames/drops need explicit user
   sign-off.

## Rules

- Namespace is always explicit; if none fits, ask before `ns create`.
- Resolve relative dates ("yesterday") to absolute ISO before writing; store
  timestamps UTC ISO-8601.
- Deletions need explicit user confirmation. `ns delete --force` is
  radioactive — it tags `pre-delete-<ns>-<date>` for undo, but still confirm.
- Don't run `memento sync` unless asked (launchd handles it). On DIVERGED or
  LOCKED, report — `memento status` explains — and don't improvise git surgery.
- Summarize results in natural language; never dump raw SQL or large result
  sets at the user.
