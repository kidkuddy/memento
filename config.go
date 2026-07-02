package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is a flat TOML file at ~/.config/memento/config.toml. Only flat
// `key = "value"` pairs are supported (documented limitation — three keys).
type Config struct {
	StorePath    string // absolute path to the store repo
	Remote       string // git remote URL for the store, optional
	SyncInterval string // informational, consumed by the launchd plist
}

func configPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		die("BAD_ARG", "cannot resolve home dir: %v", err)
	}
	return filepath.Join(home, ".config", "memento", "config.toml")
}

func loadConfig() Config {
	var c Config
	b, err := os.ReadFile(configPath())
	if err != nil {
		return c
	}
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		switch k {
		case "store_path":
			c.StorePath = v
		case "remote":
			c.Remote = v
		case "sync_interval":
			c.SyncInterval = v
		}
	}
	return c
}

func writeConfig(c Config) {
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		die("FS_ERROR", "cannot create config dir: %v", err)
	}
	body := fmt.Sprintf("store_path = %q\nremote = %q\nsync_interval = %q\n",
		c.StorePath, c.Remote, c.SyncInterval)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		die("FS_ERROR", "cannot write config: %v", err)
	}
}

// storePath resolves the store location: MEMENTO_STORE env wins, then config.
func storePath() string {
	if p := strings.TrimSpace(os.Getenv("MEMENTO_STORE")); p != "" {
		abs, err := filepath.Abs(p)
		if err == nil {
			return abs
		}
		return p
	}
	if c := loadConfig(); c.StorePath != "" {
		return c.StorePath
	}
	return ""
}

func remoteURL() string {
	if r := strings.TrimSpace(os.Getenv("MEMENTO_REMOTE")); r != "" {
		return r
	}
	return loadConfig().Remote
}
