package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

func cmdDB(args []string) {
	if len(args) == 0 {
		die("BAD_ARG", "db needs a subcommand: query|exec|schema|migrate")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "query":
		dbQuery(rest)
	case "exec":
		dbExec(rest)
	case "schema":
		dbSchema(rest)
	case "migrate":
		dbMigrate(rest)
	default:
		die("BAD_ARG", "unknown db subcommand: %s", sub)
	}
}

func tableNames(db *sql.DB) []string {
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' " +
		"AND name NOT LIKE 'sqlite_%' AND name NOT LIKE '\\_meta\\_%' ESCAPE '\\' ORDER BY name")
	if err != nil {
		die("SQL_ERROR", "%v", err)
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	return names
}

// --- query (read-only, enforced) ---------------------------------------------

func dbQuery(args []string) {
	if len(args) != 2 {
		die("BAD_ARG", "usage: db query <ns> <sql|->")
	}
	name, sqlText := args[0], sqlArg(args[1])
	if strings.TrimSpace(sqlText) == "" {
		die("BAD_ARG", "empty SQL")
	}
	if !isSingleStatement(sqlText) {
		die("SQL_ERROR", "db query takes a single statement (use db exec for scripts)")
	}
	store := ensureStore()
	unlock := lockStore(store)
	defer unlock()
	p := requireNS(store, name)
	db := loadDB(p)
	defer db.Close()
	// Root-cause enforcement, not statement sniffing: any write attempt fails
	// with SQLITE_READONLY. POC defect: writes via query were silently discarded.
	if _, err := db.Exec("PRAGMA query_only=ON"); err != nil {
		die("SQL_ERROR", "%v", err)
	}
	up := strings.ToUpper(firstToken(sqlText))
	if up == "PRAGMA" {
		die("SQL_ERROR", "PRAGMA is not available through db query — use db schema for introspection")
	}
	rows, err := db.Query(sqlText)
	if err != nil {
		die("SQL_ERROR", "%v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		die("SQL_ERROR", "%v", err)
	}
	out := []map[string]any{}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			die("SQL_ERROR", "%v", err)
		}
		m := map[string]any{}
		for i, c := range cols {
			m[c] = vals[i]
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		die("SQL_ERROR", "%v", err)
	}
	emit(out, map[string]any{"columns": cols, "rows": len(out), "namespace": name},
		renderTable(cols, out))
}

func firstToken(s string) string {
	s = strings.TrimSpace(s)
	for {
		switch {
		case strings.HasPrefix(s, "--"):
			if i := strings.Index(s, "\n"); i >= 0 {
				s = strings.TrimSpace(s[i+1:])
				continue
			}
			return ""
		case strings.HasPrefix(s, "/*"):
			if i := strings.Index(s, "*/"); i >= 0 {
				s = strings.TrimSpace(s[i+2:])
				continue
			}
			return ""
		}
		break
	}
	f := strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '(' || r == ';'
	})
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// sqlArg reads the SQL from stdin when the argument is "-", so callers can keep
// large or sensitive SQL out of argv (visible to ps / matched by pkill -f).
func sqlArg(a string) string {
	if a != "-" {
		return a
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		die("BAD_ARG", "cannot read SQL from stdin: %v", err)
	}
	return string(b)
}

// --- exec ---------------------------------------------------------------------

func dbExec(args []string) {
	var name, sqlText, msg string
	pos := []string{}
	for i := 0; i < len(args); i++ {
		if args[i] == "-m" || args[i] == "--message" {
			i++
			if i >= len(args) {
				die("BAD_ARG", "-m needs a value")
			}
			msg = args[i]
			continue
		}
		pos = append(pos, args[i])
	}
	if len(pos) != 2 {
		die("BAD_ARG", "usage: db exec <ns> <sql|-> [-m msg]")
	}
	name, sqlText = pos[0], sqlArg(pos[1])
	if strings.TrimSpace(sqlText) == "" {
		die("BAD_ARG", "empty SQL")
	}
	store := ensureStore()
	unlock := lockStore(store)
	defer unlock()
	p := requireNS(store, name)
	db := loadDB(p)
	defer db.Close()
	if _, err := db.Exec(sqlText); err != nil {
		// dump not saved → on-disk state untouched: exec is atomic per command
		die("SQL_ERROR", "%v", err)
	}
	saveDB(p, db)
	if msg == "" {
		msg = fmt.Sprintf("data(%s): exec", name)
	}
	dataCommit(store, msg, filepath.Join(name, "db.sql"))
	emit(map[string]any{"applied": true, "namespace": name}, nil, "ok")
}

// --- schema ---------------------------------------------------------------------

func dbSchema(args []string) {
	if len(args) != 1 {
		die("BAD_ARG", "usage: db schema <ns>")
	}
	store := ensureStore()
	unlock := lockStore(store)
	defer unlock()
	p := requireNS(store, args[0])
	db := loadDB(p)
	defer db.Close()
	rows, err := db.Query("SELECT type, name, sql FROM sqlite_master " +
		"WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' ORDER BY type='table' DESC, name")
	if err != nil {
		die("SQL_ERROR", "%v", err)
	}
	defer rows.Close()
	type obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
		SQL  string `json:"sql"`
	}
	var objs []obj
	var human []string
	for rows.Next() {
		var o obj
		rows.Scan(&o.Type, &o.Name, &o.SQL)
		objs = append(objs, o)
		human = append(human, strings.TrimSpace(o.SQL)+";")
	}
	if objs == nil {
		objs = []obj{}
	}
	emit(objs, map[string]any{"count": len(objs), "namespace": args[0]},
		strings.Join(human, "\n"))
}

// --- migrations -------------------------------------------------------------------

const metaMigrations = `CREATE TABLE IF NOT EXISTS _meta_migrations(
  filename TEXT PRIMARY KEY,
  applied_at TEXT NOT NULL,
  sha256 TEXT NOT NULL
)`

var migFileRe = regexp.MustCompile(`^(\d{4})_[a-z0-9][a-z0-9_-]*\.sql$`)

func dbMigrate(args []string) {
	if len(args) == 0 {
		die("BAD_ARG", "db migrate needs a subcommand: new|up|status")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "new":
		migNew(rest)
	case "up":
		migUp(rest)
	case "status":
		migStatus(rest)
	default:
		die("BAD_ARG", "unknown migrate subcommand: %s", sub)
	}
}

func migDir(nsPath string) string { return filepath.Join(nsPath, "migrations") }

func migFiles(nsPath string) []string {
	entries, err := os.ReadDir(migDir(nsPath))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && migFileRe.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func appliedMigrations(db *sql.DB) map[string]string { // filename -> sha256
	if _, err := db.Exec(metaMigrations); err != nil {
		die("SQL_ERROR", "%v", err)
	}
	rows, err := db.Query("SELECT filename, sha256 FROM _meta_migrations")
	if err != nil {
		die("SQL_ERROR", "%v", err)
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var f, h string
		rows.Scan(&f, &h)
		m[f] = h
	}
	return m
}

func fileSHA(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		die("FS_ERROR", "cannot read migration: %v", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func migNew(args []string) {
	if len(args) != 2 {
		die("BAD_ARG", "usage: db migrate new <ns> <slug>")
	}
	name, slug := args[0], args[1]
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`).MatchString(slug) {
		die("BAD_ARG", "slug must be kebab/snake lowercase: %q", slug)
	}
	store := ensureStore()
	unlock := lockStore(store)
	defer unlock()
	p := requireNS(store, name)
	next := 1
	for _, f := range migFiles(p) {
		fmt.Sscanf(f[:4], "%d", &next)
		next++
	}
	fname := fmt.Sprintf("%04d_%s.sql", next, slug)
	full := filepath.Join(migDir(p), fname)
	os.MkdirAll(migDir(p), 0o755)
	body := fmt.Sprintf("-- migration %s (namespace: %s)\n-- write DDL/DML below; applied atomically by `memento db migrate up %s`\n\n", fname, name, name)
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		die("FS_ERROR", "cannot write migration: %v", err)
	}
	dataCommit(store, fmt.Sprintf("data(%s): scaffold migration %s", name, fname),
		filepath.Join(name, "migrations", fname))
	emit(map[string]any{"created": fname, "path": full}, nil, "created "+full)
}

func migStatus(args []string) {
	if len(args) != 1 {
		die("BAD_ARG", "usage: db migrate status <ns>")
	}
	store := ensureStore()
	unlock := lockStore(store)
	defer unlock()
	p := requireNS(store, args[0])
	db := loadDB(p)
	defer db.Close()
	applied := appliedMigrations(db)
	type row struct {
		File  string `json:"file"`
		State string `json:"state"` // applied | pending | drift
	}
	var out []row
	var human []string
	for _, f := range migFiles(p) {
		st := "pending"
		if h, ok := applied[f]; ok {
			st = "applied"
			if h != fileSHA(filepath.Join(migDir(p), f)) {
				st = "drift" // applied file edited afterwards — up will refuse
			}
		}
		out = append(out, row{File: f, State: st})
		human = append(human, fmt.Sprintf("%-10s %s", st, f))
	}
	if out == nil {
		out = []row{}
	}
	emit(out, map[string]any{"count": len(out)}, strings.Join(human, "\n")+emptyMig(len(out)))
}

func emptyMig(n int) string {
	if n == 0 {
		return "(no migrations)"
	}
	return ""
}

func migUp(args []string) {
	if len(args) != 1 {
		die("BAD_ARG", "usage: db migrate up <ns>")
	}
	name := args[0]
	store := ensureStore()
	unlock := lockStore(store)
	defer unlock()
	p := requireNS(store, name)

	appliedCount := 0
	for {
		// fresh load per file: a failed file's partial in-memory effects must
		// never leak into the next one (SPEC: one commit per applied file)
		db := loadDB(p)
		applied := appliedMigrations(db)
		var next string
		for _, f := range migFiles(p) {
			full := filepath.Join(migDir(p), f)
			if h, ok := applied[f]; ok {
				if h != fileSHA(full) {
					db.Close()
					die("MIGRATION_ERROR",
						"checksum drift: applied migration %s was edited — refusing to continue", f)
				}
				continue
			}
			next = f
			break
		}
		if next == "" {
			db.Close()
			break
		}
		full := filepath.Join(migDir(p), next)
		body, err := os.ReadFile(full)
		if err != nil {
			db.Close()
			die("FS_ERROR", "cannot read migration: %v", err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			db.Close()
			die("MIGRATION_ERROR", "migration %s failed (nothing from it applied, "+
				"%d earlier file(s) remain applied): %v", next, appliedCount, err)
		}
		if _, err := db.Exec(
			"INSERT INTO _meta_migrations(filename, applied_at, sha256) VALUES(?,?,?)",
			next, time.Now().UTC().Format(time.RFC3339), fileSHA(full)); err != nil {
			db.Close()
			die("SQL_ERROR", "%v", err)
		}
		saveDB(p, db)
		db.Close()
		dataCommit(store, fmt.Sprintf("data(%s): migrate %s", name, strings.TrimSuffix(next, ".sql")),
			filepath.Join(name, "db.sql"))
		appliedCount++
	}
	emit(map[string]any{"applied": appliedCount}, nil,
		fmt.Sprintf("applied %d migration(s)", appliedCount))
}
