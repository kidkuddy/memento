package main

import (
	"os/exec"
	"strings"
)

type gitResult struct {
	Stdout string
	Stderr string
	Code   int
}

// gitRun executes system git in dir. Sync semantics must match what a human
// gets when they open the repo with real git — hence os/exec, not go-git.
func gitRun(dir string, args ...string) gitResult {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			die("GIT_ERROR", "cannot run git: %v", err)
		}
	}
	return gitResult{Stdout: out.String(), Stderr: errb.String(), Code: code}
}

func git(dir string, args ...string) gitResult {
	r := gitRun(dir, args...)
	if r.Code != 0 {
		die("GIT_ERROR", "git %s failed: %s", strings.Join(args, " "), strings.TrimSpace(r.Stderr))
	}
	return r
}

// dataCommit stages exactly the given paths and commits as the memento author.
// Narrow staging keeps every commit surgically revertable (SPEC / TESTS.md F3).
// No-op when nothing is staged.
// No paths → commit whatever is already staged (e.g. after git rm/mv).
func dataCommit(store, msg string, paths ...string) bool {
	if len(paths) == 1 && paths[0] == "-A" {
		git(store, "add", "-A") // recovery case: sweep the whole (data-only) tree
	} else if len(paths) > 0 {
		args := append([]string{"add", "--"}, paths...)
		git(store, args...)
	}
	if gitRun(store, "diff", "--cached", "--quiet").Code == 0 {
		return false // nothing staged
	}
	git(store, "-c", "user.name=memento", "-c", "user.email=memento@localhost",
		"commit", "-q", "-m", msg)
	return true
}

func currentBranch(store string) string {
	return strings.TrimSpace(git(store, "rev-parse", "--abbrev-ref", "HEAD").Stdout)
}

func hasOrigin(store string) bool {
	for _, r := range strings.Fields(git(store, "remote").Stdout) {
		if r == "origin" {
			return true
		}
	}
	return false
}
