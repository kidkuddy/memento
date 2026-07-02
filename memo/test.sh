#!/usr/bin/env bash
# End-to-end test for memo. Uses a temp store + a local bare repo as the "GitHub"
# remote. No network required. Proves: basic ops, db text round-trip across a fresh
# clone (device B), and reject-and-resolve on divergence.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
MEMO="$HERE/memo.py"
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

echo "workdir: $WORK"

# bare remote = stand-in for GitHub
git init -q --bare "$REMOTE"

echo "== device A: init + populate =="
export MEMO_DIR="$A"; export MEMO_REMOTE="file://$REMOTE"
"$MEMO" init >/dev/null
"$MEMO" ws create tasks -d "test tasks" >/dev/null
check "workspace listed" '"$MEMO" ws list | grep -q "^tasks$"'

"$MEMO" db exec tasks "CREATE TABLE tasks(id INTEGER PRIMARY KEY, title TEXT, done INT DEFAULT 0);" >/dev/null
"$MEMO" db exec tasks "INSERT INTO tasks(title) VALUES ('alpha'),('beta');" >/dev/null
ROWS=$("$MEMO" --json db query tasks "SELECT count(*) c FROM tasks;" | python3 -c "import sys,json;print(json.load(sys.stdin)['data'][0]['c'])")
check "query returns 2 rows" '[ "$ROWS" = "2" ]'

check "db.sql is text (no binary db)" 'grep -q "CREATE TABLE tasks" "$A/tasks/db.sql" && [ ! -f "$A/tasks/db.sqlite" ]'

"$MEMO" fs write tasks notes/idea.md "# first idea" >/dev/null
check "fs cat round-trips" '[ "$("$MEMO" fs cat tasks notes/idea.md)" = "# first idea" ]'
check "path traversal blocked" '! "$MEMO" fs write tasks ../escape.md "x" 2>/dev/null'

"$MEMO" sync -m "seed tasks" >/dev/null
check "pushed to remote" '[ $? -eq 0 ]'

echo "== device B: fresh clone sees the same data =="
git clone -q "file://$REMOTE" "$B"
export MEMO_DIR="$B"
# point B's env at its own store; remote already wired by clone, but memo needs origin name
( cd "$B" && git config user.email memo@localhost && git config user.name memo )
BROWS=$("$MEMO" --json db query tasks "SELECT title FROM tasks ORDER BY id;" | python3 -c "import sys,json;print(','.join(r['title'] for r in json.load(sys.stdin)['data']))")
check "device B rebuilt db from text dump" '[ "$BROWS" = "alpha,beta" ]'
check "device B sees the note" '[ "$("$MEMO" fs cat tasks notes/idea.md)" = "# first idea" ]'

echo "== divergence case 1: different files merge cleanly =="
# B adds a DB row and pushes first
export MEMO_DIR="$B"; export MEMO_REMOTE="file://$REMOTE"
"$MEMO" db exec tasks "INSERT INTO tasks(title) VALUES ('gamma-from-B');" >/dev/null
"$MEMO" sync -m "B adds gamma" >/dev/null

# A (stale) edits a DIFFERENT file, then syncs -> clean auto-merge + push
export MEMO_DIR="$A"
"$MEMO" fs write tasks notes/from-a.md "A's note" >/dev/null
"$MEMO" sync -m "A adds note" >/dev/null
check "A sync auto-merges non-overlapping change" '[ $? -eq 0 ]'
GOT=$("$MEMO" --json db query tasks "SELECT title FROM tasks ORDER BY title;" | python3 -c "import sys,json;print(','.join(r['title'] for r in json.load(sys.stdin)['data']))")
check "A pulled B's row via clean merge" 'echo "$GOT" | grep -q "gamma-from-B"'

echo "== divergence case 2: same-table conflict is surfaced, not lost/corrupted =="
# B (must pull A's merge first) adds a row, pushes
export MEMO_DIR="$B"
"$MEMO" db exec tasks "INSERT INTO tasks(title) VALUES ('epsilon-from-B');" >/dev/null
"$MEMO" sync -m "B adds epsilon" >/dev/null

# A (now stale) appends to the SAME table -> collides with epsilon on the dump
export MEMO_DIR="$A"
"$MEMO" db exec tasks "INSERT INTO tasks(title) VALUES ('zeta-from-A');" >/dev/null
"$MEMO" sync -m "A adds zeta" >/dev/null 2>&1
SYNC_RC=$?
check "conflicting sync fails loudly (reject-and-resolve)" '[ $SYNC_RC -ne 0 ]'
check "db.sql left clean (no conflict markers)" '! grep -q "<<<<<<<" "$A/tasks/db.sql"'
check "local change not lost after refused sync" '"$MEMO" db query tasks "SELECT 1 FROM tasks WHERE title='"'"'zeta-from-A'"'"'" | grep -q 1'

echo
echo "==================================="
echo "PASS: $pass   FAIL: $fail"
[ "$fail" -eq 0 ] && echo "ALL GREEN" || echo "SOME FAILED"
exit "$fail"
