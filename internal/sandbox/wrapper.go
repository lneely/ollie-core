package sandbox

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

// pathEntry holds a path with its permission type
type pathEntry struct {
	path string
	flag string // --ro, --rox, --rw, --rwx
}

// isAvailableFn and isSuperpowerdRunningFn are overridable for testing.
var (
	isAvailableFn          = isAvailable
	isSuperpowerdRunningFn = isSuperpowerdRunning
)

// WrapCommand wraps a command with landrun based on the configuration.
// Returns the wrapped command args, or the original command if landrun is unavailable.
func WrapCommand(cfg *Config, originalCmd []string, cwd string) []string {
	if !isAvailableFn() {
		return originalCmd
	}

	args := []string{"landrun"}

	if cfg.General.LogLevel != "" {
		args = append(args, "--log-level", cfg.General.LogLevel)
	}

	if cfg.General.BestEffort {
		args = append(args, "--best-effort")
	}

	if cfg.Advanced.LDD {
		args = append(args, "--ldd")
	}
	if cfg.Advanced.AddExec {
		args = append(args, "--add-exec")
	}

	var entries []pathEntry
	for _, path := range cfg.Filesystem.RO {
		expanded := expandPath(path, cwd)
		if pathExists(expanded) {
			entries = append(entries, pathEntry{expanded, "--ro"})
		}
	}
	for _, path := range cfg.Filesystem.ROX {
		expanded := expandPath(path, cwd)
		if pathExists(expanded) {
			entries = append(entries, pathEntry{expanded, "--rox"})
		}
	}
	for _, path := range cfg.Filesystem.RW {
		expanded := expandPath(path, cwd)
		if pathExists(expanded) {
			entries = append(entries, pathEntry{expanded, "--rw"})
		}
	}
	for _, path := range cfg.Filesystem.RWX {
		expanded := expandPath(path, cwd)
		if pathExists(expanded) {
			entries = append(entries, pathEntry{expanded, "--rwx"})
		}
	}

	// Sort so parents come before children
	sort.Slice(entries, func(i, j int) bool {
		if len(entries[i].path) != len(entries[j].path) {
			return len(entries[i].path) < len(entries[j].path)
		}
		return entries[i].path < entries[j].path
	})

	for _, e := range entries {
		args = append(args, e.flag, e.path)
	}

	if cfg.Network.Unrestricted {
		args = append(args, "--unrestricted-network")
	} else if cfg.Network.Enabled {
		for _, port := range cfg.Network.BindTCP {
			args = append(args, "--bind-tcp", port)
		}
		for _, port := range cfg.Network.ConnectTCP {
			args = append(args, "--connect-tcp", port)
		}
	}

	for _, name := range cfg.Env {
		args = append(args, "--env", name)
	}

	args = append(args, "--")
	args = append(args, originalCmd...)

	// Wrap with superpowers run-session if superpowerd is running
	if isSuperpowerdRunningFn() {
		if superpowersPath, err := exec.LookPath("superpowers"); err == nil {
			args = append([]string{args[0], "--env", "SUPERPOWERD_SESSION_TOKEN"}, args[1:]...)
			args = append([]string{superpowersPath, "run-session", "--"}, args...)
		}
	}

	return args
}

// isSuperpowerdRunning checks if superpowerd is running by testing socket connectivity
func isSuperpowerdRunning() bool {
	socketDir := os.Getenv("SUPERPOWERD_SOCKET_DIR")
	if socketDir == "" {
		socketDir = os.Getenv("XDG_RUNTIME_DIR")
		if socketDir == "" {
			return false
		}
		socketDir = filepath.Join(socketDir, "superpowerd")
	}
	conn, err := net.Dial("unixpacket", filepath.Join(socketDir, "superpowerd.sock"))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// pathExists checks if a file or directory exists
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
