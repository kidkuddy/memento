package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cmdContext emits everything an agent needs to operate on a namespace in one
// call: manifest, table DDL, files/SCHEMA.md conventions, migrations, file tree.
// This is the highest-leverage command for the subagent (SPEC: Agent integration).
func cmdContext(args []string) {
	if len(args) != 1 {
		die("BAD_ARG", "usage: context <ns>")
	}
	name := args[0]
	store := ensureStore()
	unlock := lockStore(store)
	defer unlock()
	p := requireNS(store, name)

	manifest, _ := os.ReadFile(filepath.Join(p, "scope.yaml"))
	desc := ""
	for _, l := range strings.Split(string(manifest), "\n") {
		if v, ok := strings.CutPrefix(l, "description: "); ok {
			desc = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}

	db := loadDB(p)
	defer db.Close()
	tables := map[string]string{}
	rows, err := db.Query("SELECT name, sql FROM sqlite_master WHERE type='table' " +
		"AND sql IS NOT NULL AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		die("SQL_ERROR", "%v", err)
	}
	for rows.Next() {
		var n, s string
		rows.Scan(&n, &s)
		tables[n] = strings.TrimSpace(s)
	}
	rows.Close()

	schemaMD := ""
	if b, err := os.ReadFile(filepath.Join(p, "files", "SCHEMA.md")); err == nil {
		schemaMD = string(b)
	}

	migrations := migFiles(p)
	if migrations == nil {
		migrations = []string{}
	}

	files := []string{}
	root := filepath.Join(p, "files")
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == ".gitkeep" {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		files = append(files, rel)
		return nil
	})

	data := map[string]any{
		"namespace":   name,
		"description": desc,
		"tables":      tables,
		"schema_md":   schemaMD,
		"migrations":  migrations,
		"files":       files,
	}

	var h strings.Builder
	fmt.Fprintf(&h, "namespace: %s\ndescription: %s\n", name, desc)
	if len(tables) > 0 {
		h.WriteString("\ntables:\n")
		for n, s := range tables {
			fmt.Fprintf(&h, "-- %s\n%s;\n", n, s)
		}
	} else {
		h.WriteString("\ntables: (none)\n")
	}
	if len(migrations) > 0 {
		fmt.Fprintf(&h, "\nmigrations: %s\n", strings.Join(migrations, ", "))
	}
	if len(files) > 0 {
		fmt.Fprintf(&h, "\nfiles:\n  %s\n", strings.Join(files, "\n  "))
	}
	if schemaMD != "" {
		fmt.Fprintf(&h, "\n--- SCHEMA.md ---\n%s", schemaMD)
	} else {
		h.WriteString("\n(no SCHEMA.md — conventions undocumented; consider fs write " +
			name + " SCHEMA.md)")
	}
	emit(data, nil, h.String())
}
