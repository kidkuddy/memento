package main

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

// openMem opens an in-memory SQLite. MaxOpenConns(1) is load-bearing: with the
// default pool, each new connection would be a fresh empty :memory: database.
func openMem() *sql.DB {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		die("SQL_ERROR", "cannot open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db
}

func dumpFile(nsPath string) string { return filepath.Join(nsPath, "db.sql") }

// loadDB materializes a namespace DB from its text dump into memory.
func loadDB(nsPath string) *sql.DB {
	db := openMem()
	b, err := os.ReadFile(dumpFile(nsPath))
	if err != nil {
		if os.IsNotExist(err) {
			return db
		}
		die("FS_ERROR", "cannot read db.sql: %v", err)
	}
	script := strings.TrimSpace(string(b))
	if script == "" {
		return db
	}
	if _, err := db.Exec(script); err != nil {
		db.Close()
		die("SQL_ERROR", "failed to load db.sql: %v", err)
	}
	return db
}

// validateDump loads a dump file and reports an error instead of dying —
// used by sync's post-merge validation (git-clean is not SQL-clean).
func validateDump(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	script := strings.TrimSpace(string(b))
	if script == "" {
		return nil
	}
	db := openMem()
	defer db.Close()
	_, err = db.Exec(script)
	return err
}

// saveDB dumps the in-memory DB back to db.sql atomically (temp + rename).
func saveDB(nsPath string, db *sql.DB) {
	dump, err := dumpSQL(db)
	if err != nil {
		die("SQL_ERROR", "dump failed: %v", err)
	}
	atomicWrite(dumpFile(nsPath), []byte(dump))
}

type masterRow struct{ name, typ, sqlText string }

// dumpSQL produces a deterministic text dump: tables sorted by name (schema
// then rows ordered by rowid), then sqlite_sequence state, then indexes,
// triggers, and views sorted by name. Round-trip requirement: dump → load →
// dump is byte-identical (TESTS.md DBE-04..06).
func dumpSQL(db *sql.DB) (string, error) {
	rows, err := db.Query(
		"SELECT name, type, sql FROM sqlite_master WHERE sql IS NOT NULL")
	if err != nil {
		return "", err
	}
	var all []masterRow
	for rows.Next() {
		var m masterRow
		if err := rows.Scan(&m.name, &m.typ, &m.sqlText); err != nil {
			rows.Close()
			return "", err
		}
		all = append(all, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}

	// virtual tables own shadow tables named <vtab>_<suffix> — those are
	// rebuilt by re-inserting into the vtab, never dumped directly.
	virtual := map[string]bool{}
	for _, m := range all {
		if m.typ == "table" && strings.HasPrefix(strings.ToUpper(strings.TrimSpace(m.sqlText)), "CREATE VIRTUAL TABLE") {
			virtual[m.name] = true
		}
	}
	isShadow := func(name string) bool {
		for v := range virtual {
			if strings.HasPrefix(name, v+"_") {
				return true
			}
		}
		return false
	}

	var tables, indexes, triggers, views []masterRow
	for _, m := range all {
		if strings.HasPrefix(m.name, "sqlite_") || isShadow(m.name) {
			continue
		}
		switch m.typ {
		case "table":
			tables = append(tables, m)
		case "index":
			indexes = append(indexes, m)
		case "trigger":
			triggers = append(triggers, m)
		case "view":
			views = append(views, m)
		}
	}
	byName := func(s []masterRow) {
		sort.Slice(s, func(i, j int) bool { return s[i].name < s[j].name })
	}
	byName(tables)
	byName(indexes)
	byName(triggers)
	byName(views)

	if len(tables)+len(indexes)+len(triggers)+len(views) == 0 {
		return "", nil // empty DB → empty file (stable for fresh namespaces)
	}

	var b strings.Builder
	b.WriteString("BEGIN TRANSACTION;\n")
	for _, t := range tables {
		b.WriteString(strings.TrimSpace(t.sqlText))
		b.WriteString(";\n")
		if err := dumpTableRows(db, &b, t.name, virtual[t.name]); err != nil {
			return "", err
		}
	}
	if err := dumpSequence(db, &b); err != nil {
		return "", err
	}
	for _, group := range [][]masterRow{indexes, triggers, views} {
		for _, m := range group {
			b.WriteString(strings.TrimSpace(m.sqlText))
			b.WriteString(";\n")
		}
	}
	b.WriteString("COMMIT;\n")
	return b.String(), nil
}

func dumpTableRows(db *sql.DB, b *strings.Builder, table string, isVirtual bool) error {
	q := fmt.Sprintf(`SELECT rowid, * FROM %s ORDER BY rowid`, quoteIdent(table))
	withRowid := true
	rows, err := db.Query(q)
	if err != nil {
		// WITHOUT ROWID tables (and some vtabs) have no rowid; b-tree order
		// is PK order, which is deterministic.
		withRowid = false
		rows, err = db.Query(fmt.Sprintf(`SELECT * FROM %s`, quoteIdent(table)))
		if err != nil {
			if isVirtual {
				return nil // e.g. contentless FTS5 — schema round-trips, content doesn't
			}
			return err
		}
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	// Virtual tables need explicit column names (and rowid, to preserve
	// docids); plain tables use bare VALUES in declared column order.
	var prefix string
	if isVirtual {
		names := make([]string, 0, len(cols))
		if withRowid {
			names = append(names, "rowid")
		}
		start := 0
		if withRowid {
			start = 1
		}
		for _, c := range cols[start:] {
			names = append(names, quoteIdent(c))
		}
		prefix = fmt.Sprintf("INSERT INTO %s(%s) VALUES(", quoteIdent(table), strings.Join(names, ","))
	} else {
		prefix = fmt.Sprintf("INSERT INTO %s VALUES(", quoteIdent(table))
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		start := 0
		if withRowid && !isVirtual {
			start = 1 // rowid was only fetched for ordering
		}
		lits := make([]string, 0, len(cols)-start)
		for _, v := range vals[start:] {
			lits = append(lits, sqlLiteral(v))
		}
		b.WriteString(prefix)
		b.WriteString(strings.Join(lits, ","))
		b.WriteString(");\n")
	}
	return rows.Err()
}

// dumpSequence emits AUTOINCREMENT state. It must come after all table data:
// the data inserts above regenerate sqlite_sequence rows, and this block then
// pins the exact counters (same order as sqlite3 .dump).
func dumpSequence(db *sql.DB, b *strings.Builder) error {
	var n int
	err := db.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE name='sqlite_sequence'").Scan(&n)
	if err != nil || n == 0 {
		return err
	}
	rows, err := db.Query("SELECT name, seq FROM sqlite_sequence ORDER BY name")
	if err != nil {
		return err
	}
	defer rows.Close()
	wrote := false
	for rows.Next() {
		var name string
		var seq any
		if err := rows.Scan(&name, &seq); err != nil {
			return err
		}
		if !wrote {
			b.WriteString("DELETE FROM sqlite_sequence;\n")
			wrote = true
		}
		fmt.Fprintf(b, "INSERT INTO sqlite_sequence VALUES(%s,%s);\n",
			sqlLiteral(name), sqlLiteral(seq))
	}
	return rows.Err()
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func sqlLiteral(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		if math.IsInf(x, 1) {
			return "9.0e999"
		}
		if math.IsInf(x, -1) {
			return "-9.0e999"
		}
		s := strconv.FormatFloat(x, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eE") {
			s += ".0" // keep REAL-typed on typeless columns
		}
		return s
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	case []byte:
		return "X'" + hex.EncodeToString(x) + "'"
	case bool:
		if x {
			return "1"
		}
		return "0"
	default:
		return "'" + strings.ReplaceAll(fmt.Sprintf("%v", x), "'", "''") + "'"
	}
}

// isSingleStatement reports whether sql contains exactly one statement —
// a ';' followed by anything non-whitespace outside strings/comments means
// multiple. db query enforces this (TESTS.md DBQ-05).
func isSingleStatement(sqlText string) bool {
	s := sqlText
	i := 0
	n := len(s)
	sawSemi := false
	for i < n {
		c := s[i]
		if sawSemi && !isSpace(c) {
			return false
		}
		switch {
		case c == ';':
			sawSemi = true
			i++
		case c == '\'' || c == '"' || c == '`':
			q := c
			i++
			for i < n {
				if s[i] == q {
					if i+1 < n && s[i+1] == q { // doubled quote escape
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
		case c == '-' && i+1 < n && s[i+1] == '-':
			for i < n && s[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && s[i+1] == '*':
			i += 2
			for i+1 < n && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			i += 2
		case c == '[':
			for i < n && s[i] != ']' {
				i++
			}
			i++
		default:
			i++
		}
	}
	return true
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
