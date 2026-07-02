package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ensureStore returns the store repo path or dies with STORE_NOT_INIT.
func ensureStore() string {
	d := storePath()
	if d == "" {
		die("STORE_NOT_INIT", "no store configured (run: memento init, or set MEMENTO_STORE)")
	}
	if st, err := os.Stat(filepath.Join(d, ".git")); err != nil || !st.IsDir() {
		die("STORE_NOT_INIT", "store not initialized at %s (run: memento init)", d)
	}
	return d
}

// lockStore takes the single exclusive store-wide flock (SPEC: one lock per
// command invocation — a two-lock design deadlocked on paper, see TESTS.md F2).
// Returns an unlock func. Lock auto-releases on process death (flock semantics).
func lockStore(store string) func() {
	path := filepath.Join(store, ".memento.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		die("FS_ERROR", "cannot open lock file: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			f.Close()
			die("LOCKED", "store is locked by another memento process (waited 30s): %s", path)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}

// --- namespaces ------------------------------------------------------------

func validNSName(name string) bool {
	if name == "" || strings.ContainsAny(name, "/\\") {
		return false
	}
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
		return false
	}
	return true
}

func nsDir(store, name string) string {
	if !validNSName(name) {
		die("BAD_ARG", "invalid namespace name: %q", name)
	}
	return filepath.Join(store, name)
}

func requireNS(store, name string) string {
	p := nsDir(store, name)
	if _, err := os.Stat(filepath.Join(p, "scope.yaml")); err != nil {
		die("NS_NOT_FOUND", "namespace not found: %s", name)
	}
	return p
}

func listNS(store string) []string {
	entries, err := os.ReadDir(store)
	if err != nil {
		die("FS_ERROR", "cannot read store: %v", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || !validNSName(e.Name()) {
			continue
		}
		if _, err := os.Stat(filepath.Join(store, e.Name(), "scope.yaml")); err == nil {
			names = append(names, e.Name())
		}
	}
	return names
}

func writeScope(nsPath, name, desc string) {
	desc = strings.ReplaceAll(desc, "\n", " ")
	body := fmt.Sprintf("name: %s\ndescription: %q\ncreated_at: %s\nmanifest_version: 1\n",
		name, desc, time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(nsPath, "scope.yaml"), []byte(body), 0o644); err != nil {
		die("FS_ERROR", "cannot write scope.yaml: %v", err)
	}
}

// --- sync state (lives inside .git/ so it is never tracked) -----------------

type syncState struct {
	LastPushAt string     `json:"last_push_at,omitempty"`
	LastOK     bool       `json:"last_ok"`
	LastError  *syncError `json:"last_error,omitempty"`
}

type syncError struct {
	Kind    string   `json:"kind"` // conflict | invalid_merge | offline | push_rejected
	Message string   `json:"message"`
	Files   []string `json:"files,omitempty"`
	At      string   `json:"at"`
}

func syncStatePath(store string) string {
	return filepath.Join(store, ".git", "memento-state.json")
}

func readSyncState(store string) syncState {
	var s syncState
	if b, err := os.ReadFile(syncStatePath(store)); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

func writeSyncState(store string, s syncState) {
	b, _ := json.MarshalIndent(s, "", "  ")
	os.WriteFile(syncStatePath(store), b, 0o644)
}

func recordSyncError(store, kind, message string, files []string) {
	s := readSyncState(store)
	s.LastOK = false
	s.LastError = &syncError{Kind: kind, Message: message, Files: files,
		At: time.Now().UTC().Format(time.RFC3339)}
	writeSyncState(store, s)
}

// atomicWrite writes via temp file + rename so a killed process leaves the old
// content intact, never a partial file.
func atomicWrite(path string, data []byte) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".memento-tmp-*")
	if err != nil {
		die("FS_ERROR", "cannot create temp file: %v", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		die("FS_ERROR", "cannot write temp file: %v", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		die("FS_ERROR", "cannot close temp file: %v", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		die("FS_ERROR", "cannot replace %s: %v", path, err)
	}
}
