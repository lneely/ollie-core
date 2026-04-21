package sandbox

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"ollie/pkg/paths"

	"gopkg.in/yaml.v3"
)

// Config represents the sandbox configuration
type Config struct {
	General    GeneralConfig    `yaml:"general"`
	Filesystem FilesystemConfig `yaml:"filesystem"`
	Network    NetworkConfig    `yaml:"network"`
	Env        []string         `yaml:"env"`
	Advanced   AdvancedConfig   `yaml:"advanced"`
}

// GeneralConfig contains general sandbox settings
type GeneralConfig struct {
	BestEffort bool   `yaml:"best_effort"`
	LogLevel   string `yaml:"log_level"`
}

// FilesystemConfig contains filesystem permission settings
type FilesystemConfig struct {
	RO  []string `yaml:"ro"`  // Read-only
	ROX []string `yaml:"rox"` // Read-only with execute
	RW  []string `yaml:"rw"`  // Read-write
	RWX []string `yaml:"rwx"` // Read-write with execute
}

// NetworkConfig contains network permission settings
type NetworkConfig struct {
	Enabled      bool     `yaml:"enabled"`
	Unrestricted bool     `yaml:"unrestricted"`
	BindTCP      []string `yaml:"bind_tcp"`
	ConnectTCP   []string `yaml:"connect_tcp"`
}

// AdvancedConfig contains advanced landrun settings
type AdvancedConfig struct {
	LDD     bool `yaml:"ldd"`
	AddExec bool `yaml:"add_exec"`
}

// defaultSandbox is the sandbox config used when none is specified
const defaultSandbox = "default"

// expandPath replaces template variables with actual values
func expandPath(pattern, cwd string) string {
	s := pattern
	s = strings.ReplaceAll(s, "{CWD}", cwd)

	re := regexp.MustCompile(`\{([A-Z_][A-Z0-9_]*)\}`)
	s = re.ReplaceAllStringFunc(s, func(match string) string {
		varName := match[1 : len(match)-1]

		home, _ := os.UserHomeDir()
		switch varName {
		case "HOME":
			if home != "" {
				return home
			}
		case "TMPDIR":
			if tmpdir := os.Getenv("TMPDIR"); tmpdir != "" {
				return tmpdir
			}
			return "/tmp"
		case "XDG_CONFIG_HOME":
			if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
				return xdg
			}
			return filepath.Join(home, ".config")
		case "XDG_DATA_HOME":
			if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
				return xdg
			}
			return filepath.Join(home, ".local/share")
		case "XDG_CACHE_HOME":
			if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
				return xdg
			}
			return filepath.Join(home, ".cache")
		case "XDG_STATE_HOME":
			if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
				return xdg
			}
			return filepath.Join(home, ".local/state")
		case "XDG_RUNTIME_DIR":
			if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
				return xdg
			}
			return fmt.Sprintf("/run/user/%d", os.Getuid())
		case "OLLIE_CFG_PATH":
			return paths.CfgDir()
		case "OLLIE_DATA_PATH":
			return paths.DataDir()
		}

		if val := os.Getenv(varName); val != "" {
			return val
		}
		return match
	})

	return s
}

// checkPath checks if the given absolute path is allowed by the sandbox config.
// If write is true, the path must fall under an RW or RWX entry.
// If write is false, any entry (RO, ROX, RW, RWX) grants access.
func checkPath(cfg *Config, path string, write bool, cwd string) error {
	cleaned := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		cleaned = resolved
	} else {
		// File may not exist yet (new file creation). Resolve parent.
		if rp, err2 := filepath.EvalSymlinks(filepath.Dir(cleaned)); err2 == nil {
			cleaned = filepath.Join(rp, filepath.Base(cleaned))
		}
	}

	var allowed []string
	for _, p := range cfg.Filesystem.RW {
		allowed = append(allowed, expandPath(p, cwd))
	}
	for _, p := range cfg.Filesystem.RWX {
		allowed = append(allowed, expandPath(p, cwd))
	}
	if !write {
		for _, p := range cfg.Filesystem.RO {
			allowed = append(allowed, expandPath(p, cwd))
		}
		for _, p := range cfg.Filesystem.ROX {
			allowed = append(allowed, expandPath(p, cwd))
		}
	}

	for _, root := range allowed {
		if pathUnder(cleaned, root) {
			return nil
		}
	}

	if write {
		return fmt.Errorf("path outside sandbox (no write access): %s", path)
	}
	return fmt.Errorf("path outside sandbox (no read access): %s", path)
}

func pathUnder(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// LoadSandbox parses a sandbox config from r.
func LoadSandbox(r io.Reader) (*Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
