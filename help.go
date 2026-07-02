package main

import (
	"fmt"
	"os"
)

var helpTopics = map[string]string{
	"init": `memento init [--store <path>] [--remote <url>]

Initialize the config (~/.config/memento/config.toml) and the store repo.
Idempotent — safe to re-run.

  --store <path>   where the data repo lives (default: ~/memento-store,
                   or the current configured path)
  --remote <url>   git remote for the store; cloned from if the store
                   doesn't exist yet, otherwise wired as origin

Examples:
  memento init --store ~/memento-store --remote git@github.com:USER/memento-store.git
  memento init                                # local-only store, add remote later

Environment overrides (mainly for tests): MEMENTO_STORE, MEMENTO_REMOTE.`,

	"sync": `memento sync

Pull from origin, validate, push. Never creates fresh work — every write
already committed itself. What it does, in order:

  1. recovery commit: any uncommitted store changes (crash between a write
     and its commit, or manual edits) are committed as data(<ns>): recover
  2. pull (merge). Conflict -> merge aborted, tree left clean, local commits
     kept unpushed, exit 9. Nothing is ever lost.
  3. post-merge validation: every db.sql the pull touched must still load;
     a git-clean but SQL-broken merge is rolled back, exit 9
  4. push

Failures are recorded — 'memento status' is the single place to look.
A launchd job normally runs this periodically; run it by hand to sync now.`,

	"status": `memento status

Shows: store path, branch, dirty files, ahead/behind origin (as of last
fetch), last successful push time, and the last sync failure if any
(kind: conflict | invalid_merge | offline | push_rejected, plus the files).`,

	"ns": `memento ns <subcommand>

  ns create <ns> [-d <desc>]   scaffold namespace (scope.yaml, db.sql,
                               migrations/, files/) — one commit
  ns list                      names only
  ns info <ns>                 manifest + tables + file count
  ns rename <old> <new>        git mv + manifest update
  ns delete <ns> --force       tags pre-delete-<ns>-<date> first for undo

Names: no '/', no leading '.' or '_'. Namespace is always explicit — there
is no default namespace.

Undo a delete:  git -C <store> checkout <tag> -- <ns>`,

	"db": `memento db <subcommand>

  db query <ns> <sql>          SELECT-only, single statement, enforced
                               (writes fail with SQLITE_READONLY; PRAGMA ->
                               use db schema). Returns rows.
  db exec <ns> <sql> [-m msg]  any SQL, multi-statement. Atomic per command:
                               a failing script leaves db.sql untouched.
                               Commits as 'data(<ns>): exec' or your -m.
  db schema <ns>               all DDL (tables, indexes, triggers, views)

Migrations (schema changes should go through these, not raw exec DDL):
  db migrate new <ns> <slug>   scaffold migrations/NNNN_<slug>.sql
  db migrate up <ns>           apply pending in order, one commit per file;
                               first failure stops; drift (edited applied
                               file) refuses with exit 11
  db migrate status <ns>       applied | pending | drift per file

Examples:
  memento db exec tasks "INSERT INTO tasks(title) VALUES ('ship it');" -m "data(tasks): add task"
  memento db query tasks "SELECT * FROM tasks WHERE done=0;"
  memento --json db query tasks "SELECT count(*) c FROM tasks;"`,

	"fs": `memento fs <subcommand>       (paths jailed under <ns>/files/)

  fs write <ns> <path> [content] [-m msg]   stdin when no content (binary ok)
  fs cat <ns> <path>                        raw bytes; --json wraps utf8/base64
  fs ls <ns> [path]
  fs rm <ns> <path>                         refuses the files/ root
  fs mv <ns> <src> <dst> [--force]          refuses existing target without --force
  fs mkdir <ns> <path>
  fs grep <ns> <pattern>                    regex, skips binaries, capped at 200

Convention: <ns>/files/SCHEMA.md documents the namespace's table semantics —
'memento context <ns>' returns it to agents.

Examples:
  memento fs write finances SCHEMA.md "# finances — amounts in TND"
  memento fs write tasks img/chart.png < chart.png
  memento fs grep tasks "quarterly"`,

	"context": `memento context <ns>

One-call orientation blob for agents: description, table DDL, files/SCHEMA.md
content, migration list, file tree. An agent should run this (with --json)
before touching a namespace instead of guessing the schema.`,

	"json": `--json (global flag, any position)

Machine-readable envelope — the contract for agents and tools:

  { "ok": true,  "data": ..., "error": null, "meta": {...} }
  { "ok": false, "data": null, "error": {"code": "...", "message": "..."}, "meta": {} }

fs cat in JSON mode returns {"content": ..., "encoding": "utf8"|"base64"}.`,

	"exit-codes": `Exit codes (stable — part of the tool contract):

  0   ok
  2   BAD_ARG           bad usage / invalid name
  3   NS_NOT_FOUND
  4   NS_EXISTS
  5   STORE_NOT_INIT    run 'memento init' or set MEMENTO_STORE
  6   SQL_ERROR         bad SQL, rejected write via query, unloadable db.sql
  7   FS_ERROR          missing path, jail escape, mv onto existing target
  8   GIT_ERROR         git failed (incl. offline remote)
  9   DIVERGED          conflict / invalid merge / push rejected — see status
  10  LOCKED            another memento holds the store lock (30s wait)
  11  MIGRATION_ERROR   failed migration or checksum drift`,
}

var helpAliases = map[string]string{
	"namespace": "ns", "workspace": "ns", "query": "db", "exec": "db",
	"migrate": "db", "schema": "db", "write": "fs", "cat": "fs", "grep": "fs",
	"errors": "exit-codes", "codes": "exit-codes", "envelope": "json",
}

func cmdHelp(args []string) {
	if len(args) == 0 {
		fmt.Println(usage())
		fmt.Println("\nDetailed help: memento help <topic>   " +
			"(topics: init, sync, status, ns, db, fs, context, json, exit-codes)")
		os.Exit(0)
	}
	topic := args[0]
	if a, ok := helpAliases[topic]; ok {
		topic = a
	}
	if t, ok := helpTopics[topic]; ok {
		fmt.Println(t)
		os.Exit(0)
	}
	fmt.Fprintf(os.Stderr, "no help for %q — topics: init, sync, status, ns, db, fs, context, json, exit-codes\n", topic)
	os.Exit(2)
}

// wantsHelp reports whether any arg asks for help, so 'memento db --help'
// and 'memento ns create -h' work at every level.
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

func helpFor(group string, args []string) bool {
	if wantsHelp(args) {
		cmdHelp([]string{group})
		return true
	}
	return false
}
