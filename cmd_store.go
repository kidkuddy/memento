package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const storeGitignore = `# binary SQLite is never committed; db.sql (text) is the source of truth
**/db.sqlite
**/db.sqlite-*
.memento.lock
.memento-tmp-*
`

func cmdInit(args []string) {
	var storeArg, remoteArg string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--store":
			i++
			if i >= len(args) {
				die("BAD_ARG", "--store needs a path")
			}
			storeArg = args[i]
		case "--remote":
			i++
			if i >= len(args) {
				die("BAD_ARG", "--remote needs a url")
			}
			remoteArg = args[i]
		default:
			die("BAD_ARG", "unknown init flag: %s", args[i])
		}
	}

	cfg := loadConfig()
	store := storeArg
	if store == "" {
		store = storePath()
	}
	if store == "" {
		home, _ := os.UserHomeDir()
		store = filepath.Join(home, "memento-store")
	}
	abs, err := filepath.Abs(store)
	if err != nil {
		die("BAD_ARG", "cannot resolve store path: %v", err)
	}
	store = abs
	remote := remoteArg
	if remote == "" {
		remote = remoteURL()
	}

	created := false
	if _, err := os.Stat(filepath.Join(store, ".git")); err != nil {
		if remote != "" {
			// fresh machine, existing store on the remote → clone
			if err := os.MkdirAll(filepath.Dir(store), 0o755); err != nil {
				die("FS_ERROR", "cannot create parent dir: %v", err)
			}
			r := gitRun(filepath.Dir(store), "clone", "-q", remote, store)
			if r.Code != 0 {
				// remote may be empty/unreachable-as-repo → fall back to init
				if err := initFreshStore(store); err != nil {
					die("GIT_ERROR", "clone failed (%s) and init failed: %v",
						strings.TrimSpace(r.Stderr), err)
				}
			}
			created = true
		} else {
			if err := initFreshStore(store); err != nil {
				die("GIT_ERROR", "init failed: %v", err)
			}
			created = true
		}
	}
	if remote != "" && !hasOrigin(store) {
		git(store, "remote", "add", "origin", remote)
	}
	scaffoldStore(store) // clone of an empty remote yields a bare tree — idempotent

	cfg.StorePath = store
	if remote != "" {
		cfg.Remote = remote
	}
	if cfg.SyncInterval == "" {
		cfg.SyncInterval = "30m"
	}
	writeConfig(cfg)

	emit(map[string]any{
		"store":   store,
		"remote":  orNil(remote),
		"created": created,
		"config":  configPath(),
	}, nil, fmt.Sprintf("store at %s (config: %s)", store, configPath()))
}

func initFreshStore(store string) error {
	if err := os.MkdirAll(store, 0o755); err != nil {
		return err
	}
	if r := gitRun(store, "init", "-q"); r.Code != 0 {
		return fmt.Errorf("git init: %s", strings.TrimSpace(r.Stderr))
	}
	return nil
}

func scaffoldStore(store string) {
	if _, err := os.Stat(filepath.Join(store, ".gitignore")); err == nil {
		return
	}
	if err := os.WriteFile(filepath.Join(store, ".gitignore"), []byte(storeGitignore), 0o644); err != nil {
		die("FS_ERROR", "cannot write .gitignore: %v", err)
	}
	readme := "# memento store\n\nGit-backed memory namespaces. Each `<name>/` holds " +
		"`scope.yaml`, `db.sql`, `migrations/`, and `files/`.\nManaged by the " +
		"`memento` CLI — do not edit by hand unless you know why.\n"
	if err := os.WriteFile(filepath.Join(store, "README.md"), []byte(readme), 0o644); err != nil {
		die("FS_ERROR", "cannot write README.md: %v", err)
	}
	dataCommit(store, "chore: initialize memento store", ".gitignore", "README.md")
}

func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// --- sync -------------------------------------------------------------------

func cmdSync(args []string) {
	if len(args) > 0 {
		die("BAD_ARG", "sync takes no arguments")
	}
	store := ensureStore()
	unlock := lockStore(store)
	defer unlock()

	// Recovery commit: uncommitted store changes can only be a crash between a
	// write and its commit, or manual edits bypassing the CLI. Narrow staging
	// means no later command would ever pick them up — sync does (TESTS.md F4).
	recovered := false
	if dirty := strings.TrimSpace(git(store, "status", "--porcelain").Stdout); dirty != "" {
		ns := recoveryScope(dirty)
		dataCommit(store, fmt.Sprintf("data(%s): recover uncommitted changes", ns), "-A")
		recovered = true
	}

	if !hasOrigin(store) {
		s := readSyncState(store)
		s.LastOK = true
		s.LastError = nil
		writeSyncState(store, s)
		emit(map[string]any{"recovered": recovered, "pushed": false, "note": "no remote configured"},
			nil, "no remote; local only"+recoveredNote(recovered))
	}

	branch := currentBranch(store)
	ls := gitRun(store, "ls-remote", "--heads", "origin", branch)
	if ls.Code != 0 {
		msg := strings.TrimSpace(ls.Stderr)
		recordSyncError(store, "offline", msg, nil)
		die("GIT_ERROR", "remote unreachable (offline?): %s", msg)
	}

	pullNote := "remote empty"
	if strings.TrimSpace(ls.Stdout) != "" {
		pull := gitRun(store, "pull", "--no-edit", "--no-rebase", "origin", branch)
		if pull.Code != 0 {
			conflicts := strings.Fields(gitRun(store, "diff", "--name-only", "--diff-filter=U").Stdout)
			gitRun(store, "merge", "--abort")
			msg := fmt.Sprintf("diverged: remote changed the same data. Local commits kept "+
				"but NOT pushed (nothing lost). Resolve manually, then sync. conflicting: %v", conflicts)
			recordSyncError(store, "conflict", msg, conflicts)
			die("DIVERGED", "%s", msg)
		}
		pullNote = "pulled"
		// Post-merge validation: git-clean is not SQL-clean. A merge that
		// produced an unloadable db.sql is rolled back like a conflict.
		if bad := validateChangedDumps(store); len(bad) > 0 {
			git(store, "reset", "--hard", "ORIG_HEAD")
			msg := fmt.Sprintf("pull produced db.sql that fails to load (rolled back, "+
				"local commits kept): %v", bad)
			recordSyncError(store, "invalid_merge", msg, bad)
			die("DIVERGED", "%s", msg)
		}
	}

	push := gitRun(store, "push", "-u", "origin", branch)
	if push.Code != 0 {
		msg := fmt.Sprintf("push rejected (remote moved) — re-run sync to pull and retry. "+
			"details: %s", strings.TrimSpace(push.Stderr))
		recordSyncError(store, "push_rejected", msg, nil)
		die("DIVERGED", "%s", msg)
	}

	writeSyncState(store, syncState{
		LastPushAt: time.Now().UTC().Format(time.RFC3339), LastOK: true})
	emit(map[string]any{"recovered": recovered, "pull": pullNote, "pushed": true},
		nil, "synced: "+pullNote+", pushed"+recoveredNote(recovered))
}

func recoveredNote(r bool) string {
	if r {
		return " (recovered uncommitted changes)"
	}
	return ""
}

// recoveryScope names the namespace for the recovery commit message, or
// "store" when changes span several.
func recoveryScope(porcelain string) string {
	nsSet := map[string]bool{}
	for _, line := range strings.Split(porcelain, "\n") {
		if len(line) < 4 {
			continue
		}
		p := strings.TrimSpace(line[3:])
		if i := strings.Index(p, " -> "); i >= 0 {
			p = p[i+4:]
		}
		p = strings.Trim(p, `"`)
		if i := strings.Index(p, "/"); i > 0 {
			nsSet[p[:i]] = true
		} else {
			nsSet["store"] = true
		}
	}
	if len(nsSet) == 1 {
		for k := range nsSet {
			return k
		}
	}
	return "store"
}

// validateChangedDumps loads every db.sql the pull touched; returns the paths
// that fail to load.
func validateChangedDumps(store string) []string {
	diff := gitRun(store, "diff", "--name-only", "ORIG_HEAD", "HEAD")
	if diff.Code != 0 {
		return nil // no ORIG_HEAD → nothing merged
	}
	var bad []string
	for _, f := range strings.Fields(diff.Stdout) {
		if filepath.Base(f) != "db.sql" {
			continue
		}
		if err := validateDump(filepath.Join(store, f)); err != nil {
			bad = append(bad, f)
		}
	}
	return bad
}

// --- status -----------------------------------------------------------------

func cmdStatus(args []string) {
	if len(args) > 0 {
		die("BAD_ARG", "status takes no arguments")
	}
	store := ensureStore()
	branch := currentBranch(store)
	dirtyRaw := strings.TrimSpace(git(store, "status", "--porcelain").Stdout)
	var dirty []string
	if dirtyRaw != "" {
		for _, l := range strings.Split(dirtyRaw, "\n") {
			dirty = append(dirty, strings.TrimSpace(l))
		}
	}
	ahead, behind := -1, -1
	if hasOrigin(store) {
		if r := gitRun(store, "rev-list", "--left-right", "--count",
			"HEAD..."+"origin/"+branch); r.Code == 0 {
			fmt.Sscanf(strings.TrimSpace(r.Stdout), "%d\t%d", &ahead, &behind)
		}
	}
	st := readSyncState(store)

	human := fmt.Sprintf("store: %s\nbranch: %s\ndirty: %d file(s)", store, branch, len(dirty))
	if ahead >= 0 {
		human += fmt.Sprintf("\nahead: %d  behind: %d (as of last fetch)", ahead, behind)
	} else {
		human += "\nremote: none"
	}
	if st.LastPushAt != "" {
		human += "\nlast push: " + st.LastPushAt
	}
	if st.LastError != nil {
		human += fmt.Sprintf("\nLAST SYNC FAILED [%s] at %s: %s",
			st.LastError.Kind, st.LastError.At, st.LastError.Message)
	}
	emit(map[string]any{
		"store":      store,
		"branch":     branch,
		"dirty":      dirty,
		"ahead":      ahead,
		"behind":     behind,
		"last_push":  orNil(st.LastPushAt),
		"last_ok":    st.LastOK,
		"last_error": st.LastError,
	}, nil, human)
}
