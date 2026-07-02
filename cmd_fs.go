package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

func cmdFS(args []string) {
	if len(args) == 0 {
		die("BAD_ARG", "fs needs a subcommand: write|cat|ls|rm|mv|mkdir|grep")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "write":
		fsWrite(rest)
	case "cat":
		fsCat(rest)
	case "ls":
		fsLs(rest)
	case "rm":
		fsRm(rest)
	case "mv":
		fsMv(rest)
	case "mkdir":
		fsMkdir(rest)
	case "grep":
		fsGrep(rest)
	default:
		die("BAD_ARG", "unknown fs subcommand: %s", sub)
	}
}

// safePath jails rel under <ns>/files/. Traversal components are rejected
// outright, and symlinks are resolved on the deepest existing ancestor so a
// link pointing outside the jail cannot smuggle access (TESTS.md FS-03).
func safePath(nsPath, rel string) string {
	root := filepath.Join(nsPath, "files")
	if filepath.IsAbs(rel) {
		die("FS_ERROR", "path must be relative: %s", rel)
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == "" {
		return root
	}
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if part == ".." {
			die("FS_ERROR", "path escapes namespace: %s", rel)
		}
	}
	target := filepath.Join(root, clean)
	// resolve symlinks on the deepest existing ancestor
	probe := target
	for {
		if _, err := os.Lstat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	resolvedProbe, err := filepath.EvalSymlinks(probe)
	if err != nil {
		die("FS_ERROR", "cannot resolve path: %v", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		die("FS_ERROR", "cannot resolve files root: %v", err)
	}
	if resolvedProbe != resolvedRoot &&
		!strings.HasPrefix(resolvedProbe+string(filepath.Separator), resolvedRoot+string(filepath.Separator)) {
		die("FS_ERROR", "path escapes namespace: %s", rel)
	}
	return target
}

func fsWrite(args []string) {
	var msg string
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
	if len(pos) < 2 || len(pos) > 3 {
		die("BAD_ARG", "usage: fs write <ns> <path> [content] [-m msg]  (stdin when no content)")
	}
	name, rel := pos[0], pos[1]
	var content []byte
	if len(pos) == 3 {
		content = []byte(pos[2])
	} else {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			die("FS_ERROR", "cannot read stdin: %v", err)
		}
		content = b
	}
	store := ensureStore()
	unlock := lockStore(store)
	defer unlock()
	p := requireNS(store, name)
	dst := safePath(p, rel)
	if st, err := os.Stat(dst); err == nil && st.IsDir() {
		die("FS_ERROR", "target is a directory: %s", rel)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		die("FS_ERROR", "cannot create parent dirs: %v", err)
	}
	atomicWrite(dst, content)
	if msg == "" {
		msg = fmt.Sprintf("data(%s): write %s", name, rel)
	}
	gitRel, _ := filepath.Rel(store, dst)
	dataCommit(store, msg, gitRel)
	emit(map[string]any{"path": rel, "bytes": len(content)}, nil,
		fmt.Sprintf("wrote %s (%d bytes)", rel, len(content)))
}

func fsCat(args []string) {
	if len(args) != 2 {
		die("BAD_ARG", "usage: fs cat <ns> <path>")
	}
	store := ensureStore()
	p := requireNS(store, args[0])
	src := safePath(p, args[1])
	st, err := os.Stat(src)
	if err != nil || st.IsDir() {
		die("FS_ERROR", "no such file: %s", args[1])
	}
	b, err := os.ReadFile(src)
	if err != nil {
		die("FS_ERROR", "cannot read: %v", err)
	}
	if jsonMode {
		if utf8.Valid(b) {
			emit(map[string]any{"content": string(b), "encoding": "utf8"},
				map[string]any{"bytes": len(b)}, "")
		}
		emit(map[string]any{"content": base64.StdEncoding.EncodeToString(b), "encoding": "base64"},
			map[string]any{"bytes": len(b)}, "")
	}
	// human mode: raw bytes to stdout (binary-safe, pipe-friendly)
	os.Stdout.Write(b)
	os.Exit(0)
}

func fsLs(args []string) {
	if len(args) < 1 || len(args) > 2 {
		die("BAD_ARG", "usage: fs ls <ns> [path]")
	}
	rel := "."
	if len(args) == 2 {
		rel = args[1]
	}
	store := ensureStore()
	p := requireNS(store, args[0])
	root := safePath(p, rel)
	st, err := os.Stat(root)
	if err != nil {
		die("FS_ERROR", "no such path: %s", rel)
	}
	entries := []string{}
	if !st.IsDir() {
		entries = append(entries, filepath.Base(root))
	} else {
		des, err := os.ReadDir(root)
		if err != nil {
			die("FS_ERROR", "cannot list: %v", err)
		}
		for _, d := range des {
			if d.Name() == ".gitkeep" {
				continue
			}
			n := d.Name()
			if d.IsDir() {
				n += "/"
			}
			entries = append(entries, n)
		}
		sort.Strings(entries)
	}
	human := strings.Join(entries, "\n")
	if len(entries) == 0 {
		human = "(empty)"
	}
	emit(entries, map[string]any{"count": len(entries)}, human)
}

func fsRm(args []string) {
	if len(args) != 2 {
		die("BAD_ARG", "usage: fs rm <ns> <path>")
	}
	name, rel := args[0], args[1]
	store := ensureStore()
	unlock := lockStore(store)
	defer unlock()
	p := requireNS(store, name)
	tgt := safePath(p, rel)
	if tgt == filepath.Join(p, "files") {
		die("FS_ERROR", "refusing to remove the files/ root")
	}
	if _, err := os.Lstat(tgt); err != nil {
		die("FS_ERROR", "no such path: %s", rel)
	}
	if err := os.RemoveAll(tgt); err != nil {
		die("FS_ERROR", "cannot remove: %v", err)
	}
	gitRel, _ := filepath.Rel(store, tgt)
	dataCommit(store, fmt.Sprintf("data(%s): rm %s", name, rel), gitRel)
	emit(map[string]any{"removed": rel}, nil, "removed "+rel)
}

func fsMv(args []string) {
	force := false
	pos := []string{}
	for _, a := range args {
		if a == "--force" {
			force = true
		} else {
			pos = append(pos, a)
		}
	}
	if len(pos) != 3 {
		die("BAD_ARG", "usage: fs mv <ns> <src> <dst> [--force]")
	}
	name, srcRel, dstRel := pos[0], pos[1], pos[2]
	store := ensureStore()
	unlock := lockStore(store)
	defer unlock()
	p := requireNS(store, name)
	src := safePath(p, srcRel)
	dst := safePath(p, dstRel)
	if _, err := os.Lstat(src); err != nil {
		die("FS_ERROR", "no such path: %s", srcRel)
	}
	if _, err := os.Lstat(dst); err == nil && !force {
		die("FS_ERROR", "target exists: %s (use --force to overwrite)", dstRel)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		die("FS_ERROR", "cannot create parent dirs: %v", err)
	}
	if err := os.Rename(src, dst); err != nil {
		die("FS_ERROR", "cannot move: %v", err)
	}
	srcGit, _ := filepath.Rel(store, src)
	dstGit, _ := filepath.Rel(store, dst)
	dataCommit(store, fmt.Sprintf("data(%s): mv %s -> %s", name, srcRel, dstRel), srcGit, dstGit)
	emit(map[string]any{"moved": srcRel, "to": dstRel}, nil,
		fmt.Sprintf("moved %s -> %s", srcRel, dstRel))
}

func fsMkdir(args []string) {
	if len(args) != 2 {
		die("BAD_ARG", "usage: fs mkdir <ns> <path>")
	}
	name, rel := args[0], args[1]
	store := ensureStore()
	unlock := lockStore(store)
	defer unlock()
	p := requireNS(store, name)
	dst := safePath(p, rel)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		die("FS_ERROR", "cannot mkdir: %v", err)
	}
	// git tracks files, not dirs — drop a .gitkeep so the dir survives sync
	keep := filepath.Join(dst, ".gitkeep")
	os.WriteFile(keep, nil, 0o644)
	gitRel, _ := filepath.Rel(store, keep)
	dataCommit(store, fmt.Sprintf("data(%s): mkdir %s", name, rel), gitRel)
	emit(map[string]any{"created": rel}, nil, "created "+rel)
}

const grepCap = 200

func fsGrep(args []string) {
	if len(args) != 2 {
		die("BAD_ARG", "usage: fs grep <ns> <pattern>")
	}
	name, pattern := args[0], args[1]
	re, err := regexp.Compile(pattern)
	if err != nil {
		die("BAD_ARG", "invalid pattern: %v", err)
	}
	store := ensureStore()
	p := requireNS(store, name)
	root := filepath.Join(p, "files")
	type hit struct {
		File string `json:"file"`
		Line int    `json:"line"`
		Text string `json:"text"`
	}
	hits := []hit{}
	truncated := false
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == ".gitkeep" || truncated {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil || bytes.IndexByte(b, 0) >= 0 { // skip binaries
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(b), "\n") {
			if re.MatchString(line) {
				hits = append(hits, hit{File: rel, Line: i + 1, Text: line})
				if len(hits) >= grepCap {
					truncated = true
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	var human []string
	for _, h := range hits {
		human = append(human, fmt.Sprintf("%s:%d: %s", h.File, h.Line, h.Text))
	}
	hs := strings.Join(human, "\n")
	if len(hits) == 0 {
		hs = "(no matches)"
	} else if truncated {
		hs += fmt.Sprintf("\n(truncated at %d matches)", grepCap)
	}
	emit(hits, map[string]any{"count": len(hits), "truncated": truncated}, hs)
}
