package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// MErr is a structured, machine-readable failure. Panicking with one unwinds
// to main's recover, which prints the envelope and exits with the mapped code.
type MErr struct {
	Code string
	Msg  string
}

func (e *MErr) Error() string { return e.Code + ": " + e.Msg }

var exitCodes = map[string]int{
	"BAD_ARG":         2,
	"NS_NOT_FOUND":    3,
	"NS_EXISTS":       4,
	"STORE_NOT_INIT":  5,
	"SQL_ERROR":       6,
	"FS_ERROR":        7,
	"GIT_ERROR":       8,
	"DIVERGED":        9,
	"LOCKED":          10,
	"MIGRATION_ERROR": 11,
}

func die(code, format string, a ...any) {
	panic(&MErr{Code: code, Msg: fmt.Sprintf(format, a...)})
}

func failOut(e *MErr) {
	if jsonMode {
		env := map[string]any{
			"ok":   false,
			"data": nil,
			"error": map[string]string{
				"code":    e.Code,
				"message": e.Msg,
			},
			"meta": map[string]any{},
		}
		b, _ := json.MarshalIndent(env, "", "  ")
		fmt.Println(string(b))
	} else {
		fmt.Fprintf(os.Stderr, "error [%s]: %s\n", e.Code, e.Msg)
	}
	code, ok := exitCodes[e.Code]
	if !ok {
		code = 1
	}
	os.Exit(code)
}

// emit prints a success result and exits 0. human is the TTY-mode rendering;
// when empty, data is JSON-rendered even in human mode.
func emit(data any, meta map[string]any, human string) {
	if jsonMode {
		if meta == nil {
			meta = map[string]any{}
		}
		env := map[string]any{"ok": true, "data": data, "error": nil, "meta": meta}
		b, err := json.MarshalIndent(env, "", "  ")
		if err != nil {
			die("BAD_ARG", "cannot encode result: %v", err)
		}
		fmt.Println(string(b))
	} else if human != "" {
		fmt.Println(human)
	} else if s, ok := data.(string); ok {
		fmt.Println(s)
	} else {
		b, _ := json.MarshalIndent(data, "", "  ")
		fmt.Println(string(b))
	}
	os.Exit(0)
}

// renderTable renders query results the way the POC did: aligned columns.
func renderTable(cols []string, rows []map[string]any) string {
	if len(rows) == 0 {
		return "(no rows)"
	}
	cell := func(v any) string {
		if v == nil {
			return ""
		}
		if b, ok := v.([]byte); ok {
			return fmt.Sprintf("<%d bytes>", len(b))
		}
		return fmt.Sprintf("%v", v)
	}
	w := make([]int, len(cols))
	for i, c := range cols {
		w[i] = len(c)
	}
	for _, r := range rows {
		for i, c := range cols {
			if l := len(cell(r[c])); l > w[i] {
				w[i] = l
			}
		}
	}
	var b strings.Builder
	for i, c := range cols {
		if i > 0 {
			b.WriteString("  ")
		}
		b.WriteString(pad(c, w[i]))
	}
	b.WriteString("\n")
	for i := range cols {
		if i > 0 {
			b.WriteString("  ")
		}
		b.WriteString(strings.Repeat("-", w[i]))
	}
	for _, r := range rows {
		b.WriteString("\n")
		for i, c := range cols {
			if i > 0 {
				b.WriteString("  ")
			}
			b.WriteString(pad(cell(r[c]), w[i]))
		}
	}
	return b.String()
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}
