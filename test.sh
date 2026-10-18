#!/usr/bin/env bash
# Acceptance suite for memento (TESTS.md sections 1–2, scriptable subset).
# Temp store + local bare repo as the GitHub stand-in. No network.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
BIN="$HERE/memento"
WORK="$(mktemp -d)"
REMOTE="$WORK/remote.git"
A="$WORK/deviceA"
B="$WORK/deviceB"

pass=0; fail=0
ok()   { echo "  PASS: $1"; pass=$((pass+1)); }
bad()  { echo "  FAIL: $1"; fail=$((fail+1)); }
check(){ if eval "$2"; then ok "$1"; else bad "$1 -- [$2]"; fi; }

cleanup(){ rm -rf "$WORK"; }
trap cleanup EXIT

go build -o "$BIN" "$HERE" || { echo "BUILD FAILED"; exit 1; }
git init -q --bare "$REMOTE"
export HOME="$WORK/home"; mkdir -p "$HOME"   # isolate config from the real one

M="$BIN --json"
jget(){ python3 -c "import sys,json;d=json.load(sys.stdin);print($1)"; }

echo "== ST: init / config =="
export MEMENTO_STORE="$A" MEMENTO_REMOTE="file://$REMOTE"
"$BIN" init --store "$A" --remote "file://$REMOTE" >/dev/null
check "ST-01 store repo created" '[ -d "$A/.git" ] && grep -q db.sqlite "$A/.gitignore"'
"$BIN" init --store "$A" >/dev/null 2>&1
check "ST-02 init idempotent" '[ $("$BIN" init --store "$A" >/dev/null 2>&1; echo $?) -eq 0 ]'
OUT=$(MEMENTO_STORE="$WORK/nowhere" "$BIN" status 2>&1); RC=$?
check "ST-03 uninitialized store -> exit 5" '[ '"$RC"' -eq 5 ]'

echo "== NS: namespaces =="
"$BIN" ns create tasks -d "test tasks" >/dev/null
check "NS-01 create + listed" '"$BIN" ns list | grep -q "^tasks$"'
check "NS-01 scaffold" '[ -f "$A/tasks/scope.yaml" ] && [ -f "$A/tasks/db.sql" ] && [ -d "$A/tasks/files" ] && [ -d "$A/tasks/migrations" ]'
"$BIN" ns create tasks >/dev/null 2>&1; RC=$?
check "NS-02 duplicate -> exit 4" '[ '"$RC"' -eq 4 ]'
"$BIN" ns create "a/b" >/dev/null 2>&1; RC=$?
check "NS-03 invalid name slash -> exit 2" '[ '"$RC"' -eq 2 ]'
"$BIN" ns create ".hidden" >/dev/null 2>&1; RC=$?
check "NS-03 leading dot -> exit 2" '[ '"$RC"' -eq 2 ]'
"$BIN" ns create "_meta" >/dev/null 2>&1; RC=$?
check "NS-03 leading underscore -> exit 2" '[ '"$RC"' -eq 2 ]'
"$BIN" ns delete tasksghost >/dev/null 2>&1; RC=$?
check "NS ghost delete -> exit 3" '[ '"$RC"' -eq 3 ]'

echo "== DBE: exec + dump =="
"$BIN" db exec tasks "CREATE TABLE tasks(id INTEGER PRIMARY KEY, title TEXT, done INT DEFAULT 0);" >/dev/null
"$BIN" db exec tasks "INSERT INTO tasks(title) VALUES ('alpha'),('beta');" >/dev/null
ROWS=$($M db query tasks "SELECT count(*) c FROM tasks;" | jget "d['data'][0]['c']")
check "DBE-01 query returns 2" '[ "$ROWS" = "2" ]'
check "DBE db.sql is text, no binary" 'grep -q "CREATE TABLE tasks" "$A/tasks/db.sql" && [ ! -f "$A/tasks/db.sqlite" ]'
BEFORE=$(shasum "$A/tasks/db.sql")
"$BIN" db exec tasks "INSERT INTO tasks(title) VALUES ('ok'); INSERT INTO nonexistent VALUES (1);" >/dev/null 2>&1; RC=$?
check "DBE-02 failing script -> exit 6, dump untouched" '[ '"$RC"' -eq 6 ] && [ "$BEFORE" = "$(shasum "$A/tasks/db.sql")" ]'
NCOMMIT=$(git -C "$A" show --stat --format= HEAD | grep -c "|")
check "DBE-03 commit touches only db.sql" '[ "$NCOMMIT" = "1" ] && git -C "$A" show --stat HEAD | grep -q "tasks/db.sql"'
check "DBE-03 author is memento" 'git -C "$A" log -1 --format=%ae | grep -q memento@localhost'
check "DBE dump loads under system sqlite3" 'sqlite3 :memory: ".read $A/tasks/db.sql" "SELECT count(*) FROM tasks;" | grep -q 2'

echo "== DBS: SQL from stdin =="
echo "INSERT INTO tasks(title) VALUES ('from-stdin');" | "$BIN" db exec tasks - >/dev/null; RC=$?
GOT=$(echo "SELECT count(*) c FROM tasks WHERE title='from-stdin';" | $M db query tasks - | jget "d['data'][0]['c']")
check "DBS-01 exec/query read SQL from stdin with -" '[ '"$RC"' -eq 0 ] && [ "$GOT" = "1" ]'
"$BIN" db exec tasks - </dev/null >/dev/null 2>&1; RC=$?
check "DBS-02 empty stdin -> exit 2" '[ '"$RC"' -eq 2 ]'
"$BIN" db exec tasks "DELETE FROM tasks WHERE title='from-stdin';" >/dev/null

echo "== DBQ: read-only query =="
"$BIN" db query tasks "UPDATE tasks SET done=1" >/dev/null 2>&1; RC=$?
DONE=$($M db query tasks "SELECT count(*) c FROM tasks WHERE done=1" | jget "d['data'][0]['c']")
check "DBQ-03 UPDATE via query rejected + nothing persisted" '[ '"$RC"' -eq 6 ] && [ "$DONE" = "0" ]'
"$BIN" db query tasks "SELECT 1; SELECT 2" >/dev/null 2>&1; RC=$?
check "DBQ-05 multi-statement rejected" '[ '"$RC"' -eq 6 ]'
"$BIN" db query tasks "PRAGMA table_info(tasks)" >/dev/null 2>&1; RC=$?
check "DBQ-04 PRAGMA rejected" '[ '"$RC"' -eq 6 ]'
W=$($M db query tasks "WITH x AS (SELECT count(*) c FROM tasks) SELECT c FROM x" | jget "d['data'][0]['c']")
check "DBQ-02 CTE SELECT allowed" '[ "$W" = "2" ]'

echo "== FS: files =="
"$BIN" fs write tasks notes/idea.md "# first idea" >/dev/null
check "FS-01 cat round-trips" '[ "$("$BIN" fs cat tasks notes/idea.md)" = "# first idea" ]'
"$BIN" fs write tasks ../escape.md "x" >/dev/null 2>&1; RC=$?
check "FS-02 traversal blocked" '[ '"$RC"' -eq 7 ]'
ln -s /etc "$A/tasks/files/evil"
"$BIN" fs cat tasks evil/hosts >/dev/null 2>&1; RC=$?
check "FS-03 symlink escape blocked" '[ '"$RC"' -eq 7 ]'
rm "$A/tasks/files/evil"; git -C "$A" checkout -q -- . 2>/dev/null || true
printf '\x89PNG\x0d\x0a\x1a\x0a\x00binary' > "$WORK/blob.bin"
"$BIN" fs write tasks img/blob.bin < "$WORK/blob.bin" >/dev/null
"$BIN" fs cat tasks img/blob.bin > "$WORK/blob.out"
check "FS-04 binary round-trip" 'cmp -s "$WORK/blob.bin" "$WORK/blob.out"'
ENC=$($M fs cat tasks img/blob.bin | jget "d['data']['encoding']")
check "FS-04 json mode base64" '[ "$ENC" = "base64" ]'
"$BIN" fs rm tasks . >/dev/null 2>&1; RC=$?
check "FS-05 rm root refused" '[ '"$RC"' -eq 7 ]'
"$BIN" fs write tasks a.md "A" >/dev/null; "$BIN" fs write tasks b.md "B" >/dev/null
"$BIN" fs mv tasks a.md b.md >/dev/null 2>&1; RC=$?
check "FS-06 mv onto existing refused" '[ '"$RC"' -eq 7 ]'
"$BIN" fs mv tasks a.md c.md >/dev/null
check "FS-06 mv works" '[ "$("$BIN" fs cat tasks c.md)" = "A" ]'
HITS=$($M fs grep tasks "first idea" | jget "d['meta']['count']")
check "FS-07 grep finds text, skips binary" '[ "$HITS" = "1" ]'

echo "== MG: migrations =="
"$BIN" db migrate new tasks add-tags >/dev/null
check "MG-01 scaffolded" 'ls "$A/tasks/migrations" | grep -q "0001_add-tags.sql"'
echo "ALTER TABLE tasks ADD COLUMN tags TEXT DEFAULT '[]';" > "$A/tasks/migrations/0001_add-tags.sql"
"$BIN" db migrate new tasks broken >/dev/null
echo "ALTER TABLE nonexistent ADD COLUMN x;" > "$A/tasks/migrations/0002_broken.sql"
"$BIN" db migrate up tasks >/dev/null 2>&1; RC=$?
ST=$($M db migrate status tasks | jget "','.join(r['state'] for r in d['data'])")
check "MG-03 partial up: 1 applied, failure stops" '[ '"$RC"' -eq 11 ] && [ "$ST" = "applied,pending" ]'
echo "SELECT 1;" > "$A/tasks/migrations/0002_broken.sql"
"$BIN" db migrate up tasks >/dev/null
check "MG-03 resume after fix" '[ $? -eq 0 ]'
"$BIN" db migrate up tasks >/dev/null
N=$($M db migrate up tasks | jget "d['data']['applied']")
check "MG-04 re-run is no-op" '[ "$N" = "0" ]'
echo "SELECT 2;" > "$A/tasks/migrations/0002_broken.sql"
"$BIN" db migrate up tasks >/dev/null 2>&1; RC=$?
check "MG-05 checksum drift refused" '[ '"$RC"' -eq 11 ]'
git -C "$A" checkout -q -- tasks/migrations/0002_broken.sql

echo "== LK: concurrency =="
"$BIN" db exec tasks "CREATE TABLE race(n INT);" >/dev/null
( "$BIN" db exec tasks "INSERT INTO race VALUES (1);" >/dev/null 2>&1 ) &
( "$BIN" db exec tasks "INSERT INTO race VALUES (2);" >/dev/null 2>&1 ) &
wait
NRACE=$($M db query tasks "SELECT count(*) c FROM race" | jget "d['data'][0]['c']")
check "LK-01 concurrent writes both survive" '[ "$NRACE" = "2" ]'

echo "== SY: sync / device B / divergence =="
"$BIN" sync >/dev/null; RC=$?
check "SY-01 sync pushes" '[ '"$RC"' -eq 0 ]'
git clone -q "file://$REMOTE" "$B"
export MEMENTO_STORE="$B"
BROWS=$($M db query tasks "SELECT title FROM tasks ORDER BY id" | jget "','.join(r['title'] for r in d['data'])")
check "SY-02 device B rebuilt db from dump" '[ "$BROWS" = "alpha,beta" ]'
check "SY-02 device B sees files" '[ "$("$BIN" fs cat tasks notes/idea.md)" = "# first idea" ]'

# non-overlapping changes merge cleanly
"$BIN" db exec tasks "INSERT INTO tasks(title) VALUES ('gamma-from-B');" >/dev/null
"$BIN" sync >/dev/null
export MEMENTO_STORE="$A"
"$BIN" fs write tasks notes/from-a.md "A's note" >/dev/null
"$BIN" sync >/dev/null; RC=$?
GOT=$($M db query tasks "SELECT count(*) c FROM tasks WHERE title='gamma-from-B'" | jget "d['data'][0]['c']")
check "SY-03 non-overlapping merge clean, A sees B's row" '[ '"$RC"' -eq 0 ] && [ "$GOT" = "1" ]'

# same-table concurrent inserts conflict -> reject-and-resolve
export MEMENTO_STORE="$B"
"$BIN" sync >/dev/null
"$BIN" db exec tasks "INSERT INTO tasks(title) VALUES ('epsilon-from-B');" >/dev/null
"$BIN" sync >/dev/null
export MEMENTO_STORE="$A"
"$BIN" db exec tasks "INSERT INTO tasks(title) VALUES ('zeta-from-A');" >/dev/null
"$BIN" sync >/dev/null 2>&1; RC=$?
check "SY-04 conflict -> exit 9" '[ '"$RC"' -eq 9 ]'
check "SY-04 db.sql left clean" '! grep -q "<<<<<<<" "$A/tasks/db.sql"'
Z=$($M db query tasks "SELECT count(*) c FROM tasks WHERE title='zeta-from-A'" | jget "d['data'][0]['c']")
check "SY-04 local change kept" '[ "$Z" = "1" ]'
check "SY-10 status shows failure" '"$BIN" status | grep -q "LAST SYNC FAILED \[conflict\]"'
# resolve: take remote's db.sql, keep local commit history simple (reset to remote)
git -C "$A" fetch -q origin && git -C "$A" reset -q --hard origin/"$(git -C "$A" rev-parse --abbrev-ref HEAD)"

# SY-05: fast-forward pull of a git-clean but SQL-broken db.sql -> rollback
export MEMENTO_STORE="$B"
"$BIN" sync >/dev/null 2>&1 || true
echo "CREATE TABLE broken(; oops" > "$B/tasks/db.sql"
git -C "$B" add tasks/db.sql && git -C "$B" -c user.name=t -c user.email=t@t commit -q -m "corrupt" && git -C "$B" push -q origin HEAD
export MEMENTO_STORE="$A"
"$BIN" sync >/dev/null 2>&1; RC=$?
check "SY-05 invalid merge rolled back -> exit 9" '[ '"$RC"' -eq 9 ]'
check "SY-05 local db.sql still loads" '"$BIN" db query tasks "SELECT 1" >/dev/null 2>&1'
check "SY-05 status shows invalid_merge" '"$BIN" status | grep -q "invalid_merge"'
# repair remote for the next tests
git -C "$B" revert --no-edit HEAD >/dev/null && git -C "$B" push -q origin HEAD

# SY-08: crash simulation — stray uncommitted change is recovered by sync
"$BIN" sync >/dev/null 2>&1 || true
export MEMENTO_STORE="$A"
"$BIN" sync >/dev/null 2>&1 || true
echo "stray" > "$A/tasks/files/stray.md"
"$BIN" sync >/dev/null; RC=$?
check "SY-08 recovery commit made" '[ '"$RC"' -eq 0 ] && git -C "$A" log --oneline | grep -q "recover uncommitted"'

echo "== E2E-2: surgical undo =="
"$BIN" db exec tasks "DELETE FROM tasks WHERE title='alpha';" -m "data(tasks): oops delete" >/dev/null
SHA=$(git -C "$A" log --format="%h %s" | grep "oops delete" | head -1 | cut -d' ' -f1)
git -C "$A" -c user.name=memento -c user.email=memento@localhost revert --no-edit "$SHA" >/dev/null
BACK=$($M db query tasks "SELECT count(*) c FROM tasks WHERE title='alpha'" | jget "d['data'][0]['c']")
check "E2E-2 git revert of one data commit restores the row" '[ "$BACK" = "1" ]'

echo "== CTX: context =="
"$BIN" fs write tasks SCHEMA.md "# tasks schema — done: 0|1" >/dev/null
CTX=$($M context tasks)
check "CTX tables present" 'echo "$CTX" | jget "\"tasks\" in d[\"data\"][\"tables\"]" | grep -q True'
check "CTX schema_md present" 'echo "$CTX" | jget "d[\"data\"][\"schema_md\"]" | grep -q "done: 0|1"'
check "CTX migrations listed" 'echo "$CTX" | jget "len(d[\"data\"][\"migrations\"])" | grep -q 2'

echo "== NS delete =="
"$BIN" ns create scratch >/dev/null
"$BIN" ns delete scratch >/dev/null 2>&1; RC=$?
check "NS-06 delete without --force refused" '[ '"$RC"' -eq 2 ]'
"$BIN" ns delete scratch --force >/dev/null
check "NS-06 deleted with undo tag" '! "$BIN" ns list | grep -q scratch && git -C "$A" tag | grep -q "pre-delete-scratch"'

echo
echo "==================================="
echo "PASS: $pass   FAIL: $fail"
[ "$fail" -eq 0 ] && echo "ALL GREEN" || echo "SOME FAILED"
exit "$fail"
