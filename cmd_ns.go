package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func cmdNS(args []string) {
	if len(args) == 0 {
		die("BAD_ARG", "ns needs a subcommand: create|list|info|rename|delete")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		nsCreate(rest)
	case "list":
		nsList(rest)
	case "info":
		nsInfo(rest)
	case "rename":
		nsRename(rest)
	case "delete":
		nsDelete(rest)
	default:
		die("BAD_ARG", "unknown ns subcommand: %s", sub)
	}
}

func nsCreate(args []string) {
	var name, desc string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-d", "--description":
			i++
			if i >= len(args) {
				die("BAD_ARG", "-d needs a value")
			}
			desc = args[i]
		default:
			if name != "" {
				die("BAD_ARG", "unexpected argument: %s", args[i])
			}
			name = args[i]
		}
	}
	if name == "" {
		die("BAD_ARG", "usage: ns create <name> [-d desc]")
	}
	store := ensureStore()
	unlock := lockStore(store)
	defer unlock()

	p := nsDir(store, name)
	if _, err := os.Stat(filepath.Join(p, "scope.yaml")); err == nil {
		die("NS_EXISTS", "namespace already exists: %s", name)
	}
	if err := os.MkdirAll(filepath.Join(p, "files"), 0o755); err != nil {
		die("FS_ERROR", "cannot create namespace: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(p, "migrations"), 0o755); err != nil {
		die("FS_ERROR", "cannot create namespace: %v", err)
	}
	writeScope(p, name, desc)
	if err := os.WriteFile(filepath.Join(p, "db.sql"), nil, 0o644); err != nil {
		die("FS_ERROR", "cannot write db.sql: %v", err)
	}
	os.WriteFile(filepath.Join(p, "files", ".gitkeep"), nil, 0o644)
	os.WriteFile(filepath.Join(p, "migrations", ".gitkeep"), nil, 0o644)
	dataCommit(store, fmt.Sprintf("data(%s): create namespace", name), name)
	emit(map[string]any{"namespace": name, "path": p}, nil, "created namespace: "+name)
}

func nsList(args []string) {
	store := ensureStore()
	names := listNS(store)
	sort.Strings(names)
	if names == nil {
		names = []string{}
	}
	emit(names, map[string]any{"count": len(names)},
		strings.Join(names, "\n")+emptyNote(len(names)))
}

func emptyNote(n int) string {
	if n == 0 {
		return "(no namespaces)"
	}
	return ""
}

func nsInfo(args []string) {
	if len(args) != 1 {
		die("BAD_ARG", "usage: ns info <name>")
	}
	store := ensureStore()
	p := requireNS(store, args[0])
	manifest, err := os.ReadFile(filepath.Join(p, "scope.yaml"))
	if err != nil {
		die("FS_ERROR", "cannot read scope.yaml: %v", err)
	}
	db := loadDB(p)
	defer db.Close()
	tables := tableNames(db)
	nfiles := 0
	filepath.WalkDir(filepath.Join(p, "files"), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() != ".gitkeep" {
			nfiles++
		}
		return nil
	})
	emit(map[string]any{
		"manifest":   string(manifest),
		"tables":     tables,
		"file_count": nfiles,
	}, nil, fmt.Sprintf("%s\ntables: %v\nfiles: %d", strings.TrimSpace(string(manifest)), tables, nfiles))
}

func nsRename(args []string) {
	if len(args) != 2 {
		die("BAD_ARG", "usage: ns rename <old> <new>")
	}
	store := ensureStore()
	unlock := lockStore(store)
	defer unlock()
	old, nu := args[0], args[1]
	requireNS(store, old)
	np := nsDir(store, nu)
	if _, err := os.Stat(np); err == nil {
		die("NS_EXISTS", "target already exists: %s", nu)
	}
	git(store, "mv", old, nu)
	// keep the manifest's name field truthful
	sp := filepath.Join(np, "scope.yaml")
	if b, err := os.ReadFile(sp); err == nil {
		lines := strings.Split(string(b), "\n")
		for i, l := range lines {
			if strings.HasPrefix(l, "name: ") {
				lines[i] = "name: " + nu
			}
		}
		os.WriteFile(sp, []byte(strings.Join(lines, "\n")), 0o644)
	}
	dataCommit(store, fmt.Sprintf("data(%s): rename from %s", nu, old), old, nu)
	emit(map[string]any{"renamed": old, "to": nu}, nil, fmt.Sprintf("renamed %s -> %s", old, nu))
}

func nsDelete(args []string) {
	var name string
	force := false
	for _, a := range args {
		if a == "--force" {
			force = true
		} else if name == "" {
			name = a
		} else {
			die("BAD_ARG", "unexpected argument: %s", a)
		}
	}
	if name == "" {
		die("BAD_ARG", "usage: ns delete <name> --force")
	}
	store := ensureStore()
	unlock := lockStore(store)
	defer unlock()
	requireNS(store, name)
	if !force {
		die("BAD_ARG", "ns delete is destructive — re-run with --force "+
			"(a pre-delete tag will be created for undo)")
	}
	tag := fmt.Sprintf("pre-delete-%s-%s", name, time.Now().UTC().Format("20060102-150405"))
	git(store, "tag", tag)
	git(store, "rm", "-r", "-q", "--", name)
	dataCommit(store, fmt.Sprintf("data(%s): delete namespace (undo tag: %s)", name, tag))
	emit(map[string]any{"deleted": name, "undo_tag": tag}, nil,
		fmt.Sprintf("deleted %s (undo: git checkout %s -- %s)", name, tag, name))
}
