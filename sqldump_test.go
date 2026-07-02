package main

import (
	"strings"
	"testing"
)

// roundTrip dumps db, loads the dump into a fresh DB, dumps again, and
// requires byte-identical output — the core determinism guarantee (DBE-04..06).
func roundTrip(t *testing.T, setup string) string {
	t.Helper()
	db := openMem()
	defer db.Close()
	if _, err := db.Exec(setup); err != nil {
		t.Fatalf("setup: %v", err)
	}
	d1, err := dumpSQL(db)
	if err != nil {
		t.Fatalf("dump 1: %v", err)
	}
	db2 := openMem()
	defer db2.Close()
	if strings.TrimSpace(d1) != "" {
		if _, err := db2.Exec(d1); err != nil {
			t.Fatalf("reload of dump failed: %v\ndump:\n%s", err, d1)
		}
	}
	d2, err := dumpSQL(db2)
	if err != nil {
		t.Fatalf("dump 2: %v", err)
	}
	if d1 != d2 {
		t.Fatalf("round-trip not byte-identical\n--- d1:\n%s\n--- d2:\n%s", d1, d2)
	}
	return d1
}

func TestRoundTripBasic(t *testing.T) {
	roundTrip(t, `
		CREATE TABLE tasks(id INTEGER PRIMARY KEY, title TEXT, done INT DEFAULT 0);
		INSERT INTO tasks(title) VALUES ('alpha'),('beta');
		CREATE INDEX idx_done ON tasks(done);
	`)
}

func TestRoundTripValueTypes(t *testing.T) {
	d := roundTrip(t, `
		CREATE TABLE v(a, b, c, d, e, f);
		INSERT INTO v VALUES (NULL, -42, 3.14, 2.0, 'it''s "quoted"
multiline 🚀', X'00ff10');
	`)
	for _, want := range []string{"NULL", "-42", "3.14", "2.0", "🚀", "X'00ff10'", "it''s"} {
		if !strings.Contains(d, want) {
			t.Errorf("dump missing %q:\n%s", want, d)
		}
	}
}

func TestRoundTripAutoincrement(t *testing.T) {
	d := roundTrip(t, `
		CREATE TABLE t(id INTEGER PRIMARY KEY AUTOINCREMENT, x TEXT);
		INSERT INTO t(x) VALUES ('a'),('b'),('c');
		DELETE FROM t WHERE id = 3;
	`)
	if !strings.Contains(d, "sqlite_sequence") {
		t.Fatalf("dump missing sqlite_sequence state:\n%s", d)
	}
	// counter must survive: next insert after reload must get id=4, not 3
	db := openMem()
	defer db.Close()
	if _, err := db.Exec(d); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO t(x) VALUES ('d')"); err != nil {
		t.Fatal(err)
	}
	var id int64
	db.QueryRow("SELECT id FROM t WHERE x='d'").Scan(&id)
	if id != 4 {
		t.Fatalf("AUTOINCREMENT counter lost: new id = %d, want 4", id)
	}
}

func TestRoundTripFTS5(t *testing.T) {
	d := roundTrip(t, `
		CREATE TABLE notes(id INTEGER PRIMARY KEY, body TEXT);
		INSERT INTO notes(body) VALUES ('memento remembers'), ('the quick brown fox');
		CREATE VIRTUAL TABLE notes_fts USING fts5(body);
		INSERT INTO notes_fts(rowid, body) SELECT id, body FROM notes;
	`)
	if strings.Contains(d, "notes_fts_data") {
		t.Fatalf("dump contains FTS5 shadow tables:\n%s", d)
	}
	// MATCH must work on the rebuilt DB — the whole retrieval plan rests on it
	db := openMem()
	defer db.Close()
	if _, err := db.Exec(d); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(
		"SELECT count(*) FROM notes_fts WHERE notes_fts MATCH 'memento'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("MATCH on rebuilt FTS5 returned %d, want 1", n)
	}
}

func TestRoundTripWithoutRowid(t *testing.T) {
	roundTrip(t, `
		CREATE TABLE kv(k TEXT PRIMARY KEY, v TEXT) WITHOUT ROWID;
		INSERT INTO kv VALUES ('b','2'),('a','1');
	`)
}

func TestRoundTripViewsTriggers(t *testing.T) {
	roundTrip(t, `
		CREATE TABLE t(id INTEGER PRIMARY KEY, x TEXT, updated_at TEXT);
		CREATE VIEW open_t AS SELECT * FROM t WHERE x IS NOT NULL;
		CREATE TRIGGER touch AFTER UPDATE ON t BEGIN
			UPDATE t SET updated_at = 'now' WHERE id = NEW.id;
		END;
		INSERT INTO t(x) VALUES ('a');
	`)
}

func TestEmptyDBDumpsEmpty(t *testing.T) {
	db := openMem()
	defer db.Close()
	d, err := dumpSQL(db)
	if err != nil {
		t.Fatal(err)
	}
	if d != "" {
		t.Fatalf("empty DB should dump to empty string, got:\n%s", d)
	}
}

func TestSingleStatement(t *testing.T) {
	cases := map[string]bool{
		"SELECT 1":                              true,
		"SELECT 1;":                             true,
		"SELECT 1; SELECT 2":                    false,
		"SELECT ';' ; ":                         true,
		"SELECT 'a;b'":                          true,
		"SELECT 1 -- comment; with semi\n":      true,
		"SELECT 1; -- trailing comment":         false,
		"SELECT /* ; */ 1":                      true,
		"WITH x AS (SELECT 1) SELECT * FROM x;": true,
		"SELECT 1;\nDROP TABLE t;":              false,
		`SELECT "a;b" FROM t`:                   true,
		"SELECT 1; ;":                           false,
	}
	for sql, want := range cases {
		if got := isSingleStatement(sql); got != want {
			t.Errorf("isSingleStatement(%q) = %v, want %v", sql, got, want)
		}
	}
}
