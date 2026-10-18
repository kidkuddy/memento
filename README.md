# memento

Single-user, git-backed memory workspace CLI. The one store all your AI agents and
personal tools read from and write to. Design: [`SPEC.md`](SPEC.md) · test plan:
[`TESTS.md`](TESTS.md) · Python POC (superseded, kept as reference): [`memo/`](memo/).

Each **namespace** holds structured data (SQLite, persisted as a deterministic text
dump `db.sql`) and unstructured files (`files/`, markdown mostly). The store is its
own git repo: **every write is one commit**, history is the backup, a periodic
launchd job pushes to GitHub.

## Build & setup

```bash
go build -o memento .
./memento init --store ~/memento-store --remote git@github.com:USER/memento-store.git
# config lands in ~/.config/memento/config.toml; --remote optional
```

## Usage

```bash
memento ns create tasks -d "personal + work tasks"
memento db exec tasks "CREATE TABLE tasks(id INTEGER PRIMARY KEY, title TEXT, done INT DEFAULT 0);"
memento db exec tasks "INSERT INTO tasks(title) VALUES ('ship memento');"
memento db query tasks "SELECT * FROM tasks WHERE done=0;"
echo "UPDATE tasks SET done=1 WHERE id=1;" | memento db exec tasks -   # "-" = SQL from stdin
memento fs write tasks notes/idea.md "# rough idea"
memento fs grep tasks "idea"
memento context tasks          # one-call orientation blob for agents
memento sync                   # pull + validate + push (launchd runs this periodically)
memento status                 # tree state, ahead/behind, last sync failure if any
```

`--json` on any command returns the envelope `{ok, data, error, meta}` — the
contract for agents and third-party tools. Stable exit codes per error class
(see SPEC.md, Output contract).

Schema changes go through migrations, not raw DDL:

```bash
memento db migrate new tasks add-tags     # scaffolds tasks/migrations/0001_add-tags.sql
memento db migrate up tasks               # applies, one commit per file
memento db migrate status tasks
```

## Claude Code integration

Copy [`agents/memento.md`](agents/memento.md) to `~/.claude/agents/memento.md`.
The main session then delegates memory tasks ("log 45€ groceries yesterday") to the
subagent, which orients via `memento context` and owns all SQL — the session never
writes SQL mid-conversation.

## Periodic push (launchd)

```bash
cat > ~/Library/LaunchAgents/com.user.memento.sync.plist <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.user.memento.sync</string>
  <key>ProgramArguments</key><array>
    <string>/opt/homebrew/bin/memento</string><string>--json</string><string>sync</string>
  </array>
  <key>StartInterval</key><integer>1800</integer>
  <key>StandardOutPath</key><string>/tmp/memento-sync.log</string>
  <key>StandardErrorPath</key><string>/tmp/memento-sync.log</string>
</dict></plist>
EOF
launchctl load ~/Library/LaunchAgents/com.user.memento.sync.plist
```

Unattended conflicts never lose data: local commits are kept, the failure is
recorded, and `memento status` is the single place to look.

## Test

```bash
go test ./...   # dumper round-trip guarantees (FTS5, AUTOINCREMENT, blobs, ...)
./test.sh       # 53-assertion acceptance suite: two devices, conflicts, recovery
```
