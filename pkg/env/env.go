// Package env manages the daemon-global environment for ollie.
// It owns the set of known OLLIE_* and SUPERPOWERD_* variables, provides
// defaults, and formats them for export to frontends via ollie/env.
package env

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// managed is the ordered list of env vars that ollie exposes to frontends.
var managed = []string{
	"OLLIE",
	"OLLIE_TOOLS_PATH",
	"OLLIE_SKILLS_PATH",
	"OLLIE_PROMPTS_PATH",
	"OLLIE_MEMORY_PATH",
	"OLLIE_TMP_PATH",
	"OLLIE_TRANSCRIPT_PATH",
	"OLLIE_ELEVATE_SOCKET",
	"SUPERPOWERD_SESSION_TOKEN",
	"SUPERPOWERD_SOCKET_DIR",
}

// EnsureDefaults sets default values for any OLLIE_* vars not already present
// in the process environment.
func EnsureDefaults() {
	home, _ := os.UserHomeDir()
	xdgRuntime := os.Getenv("XDG_RUNTIME_DIR")
	if xdgRuntime == "" {
		xdgRuntime = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	defaults := map[string]string{
		"OLLIE":                  filepath.Join(home, "mnt", "ollie"),
		"OLLIE_TOOLS_PATH":       filepath.Join(home, ".config", "ollie", "tools"),
		"OLLIE_SKILLS_PATH":      filepath.Join(home, ".config", "ollie", "skills"),
		"OLLIE_PROMPTS_PATH":     filepath.Join(home, ".config", "ollie", "prompts"),
		"OLLIE_MEMORY_PATH":      filepath.Join(home, ".config", "ollie", "memory"),
		"OLLIE_TMP_PATH":         filepath.Join(home, ".local", "share", "ollie", "tmp"),
		"OLLIE_TRANSCRIPT_PATH":  filepath.Join(home, ".config", "ollie", "transcript"),
		"OLLIE_ELEVATE_SOCKET":   filepath.Join(xdgRuntime, "ollie", "elevate.sock"),
	}
	for k, v := range defaults {
		if os.Getenv(k) == "" {
			os.Setenv(k, v) //nolint:errcheck
		}
	}
}

// Set sets a variable in the process environment.
func Set(k, v string) { os.Setenv(k, v) } //nolint:errcheck

// Get returns a variable from the process environment.
func Get(k string) string { return os.Getenv(k) }

// All returns the full process environment as a map.
func All() map[string]string {
	pairs := os.Environ()
	m := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		k, v, _ := strings.Cut(pair, "=")
		m[k] = v
	}
	return m
}

// Format returns the full process environment as NAME=VALUE lines, suitable
// for serving as ollie/env.
func Format() []byte {
	var sb strings.Builder
	for _, pair := range os.Environ() {
		fmt.Fprintf(&sb, "%s\n", pair)
	}
	return []byte(sb.String())
}
