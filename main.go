// memento — single-user, git-backed memory workspace CLI.
//
// Dumb storage for AI agents and personal tools. Each namespace holds a SQLite
// database persisted as a deterministic text dump (db.sql) plus a files/ tree.
// The store is its own git repo: every write commits, sync pulls + pushes.
// No AI logic lives here; see SPEC.md.
package main

import (
	"fmt"
	"os"
)

var jsonMode bool

func usage() string {
	return `usage: memento [--json] <command> ...

store:
  init [--store <path>] [--remote <url>]   initialize config + store repo
  sync                                     recovery-commit, pull, validate, push
  status                                   tree state, ahead/behind, last sync result

namespace:
  ns create <ns> [-d <desc>]
  ns list
  ns info <ns>
  ns rename <old> <new>
  ns delete <ns> --force

database:
  db query <ns> <sql>                      SELECT-only (enforced)
  db exec <ns> <sql> [-m <msg>]
  db schema <ns>
  db migrate new <ns> <slug>
  db migrate up <ns>
  db migrate status <ns>

filesystem (jailed under <ns>/files/):
  fs write <ns> <path> [content] [-m <msg>]   (reads stdin when no content arg)
  fs cat <ns> <path>
  fs ls <ns> [path]
  fs rm <ns> <path>
  fs mv <ns> <src> <dst> [--force]
  fs mkdir <ns> <path>
  fs grep <ns> <pattern>

agent:
  context <ns>                             one-call orientation blob`
}

func main() {
	args := make([]string, 0, len(os.Args)-1)
	for _, a := range os.Args[1:] {
		if a == "--json" {
			jsonMode = true
			continue
		}
		args = append(args, a)
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage())
		os.Exit(2)
	}
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(*MErr); ok {
				failOut(e)
			}
			panic(r)
		}
	}()
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "init":
		if !helpFor("init", rest) {
			cmdInit(rest)
		}
	case "sync":
		if !helpFor("sync", rest) {
			cmdSync(rest)
		}
	case "status":
		if !helpFor("status", rest) {
			cmdStatus(rest)
		}
	case "ns", "namespace":
		if !helpFor("ns", rest) {
			cmdNS(rest)
		}
	case "db":
		if !helpFor("db", rest) {
			cmdDB(rest)
		}
	case "fs":
		if !helpFor("fs", rest) {
			cmdFS(rest)
		}
	case "context":
		if !helpFor("context", rest) {
			cmdContext(rest)
		}
	case "help", "--help", "-h":
		cmdHelp(rest)
	default:
		die("BAD_ARG", "unknown command: %s\n\n%s", cmd, usage())
	}
}
