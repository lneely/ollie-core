package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config represents the global sandbox configuration
type Config struct {
	General    GeneralConfig    `yaml:"general"`
	Filesystem FilesystemConfig `yaml:"filesystem"`
	Network    NetworkConfig    `yaml:"network"`
	Env        []string         `yaml:"env"`
	Advanced   AdvancedConfig   `yaml:"advanced"`
}

// LayeredConfig represents a single layer (system/backend/role/task)
type LayeredConfig struct {
	Filesystem FilesystemConfig `yaml:"filesystem"`
	Network    NetworkConfig    `yaml:"network"`
	Env        []string         `yaml:"env"`
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

// DefaultConfig returns the default locked-down configuration
func DefaultConfig() *Config {
	return &Config{
		General: GeneralConfig{
			BestEffort: false, // Fail-closed: refuse to run if sandboxing unavailable
			LogLevel:   "error",
		},
		Filesystem: FilesystemConfig{
			RO: []string{
				"/etc/passwd",       // UID→homedir lookup (getpwuid)
				"/dev/null",         // Null device
				"/proc/meminfo",     // Memory info
				"/proc/self/cgroup", // Cgroup info
				"/proc/self/maps",   // Process memory maps
				"/proc/version",     // Kernel version
			},
			ROX: []string{
				"/usr",
				"/lib",
				"/lib64",
				"/bin",
				"/sbin",
			},
			RW: []string{
				"{CWD}",
				"{TMPDIR}",
				"{HOME}/.claude",
				"{HOME}/.kiro",
				"{HOME}/.claude.json",
			},
			RWX: []string{
				"{HOME}/.config/ollie",
				"{OLLIE}",
				"{HOME}/mnt/ollie",
			},
		},
		Network: NetworkConfig{
			Enabled:      false,
			Unrestricted: true, // Unrestricted network (fine-grained restrictions require Landlock v5+)
			BindTCP:      []string{},
			ConnectTCP:   []string{"443"},
		},
		Env: []string{
			"HOME",
			"USER",
			"PATH",
			"LANG",
			"TERM",
			"OLLIE_SESSION_ID",
			"OLLIE",
			"OLLIE_MEMORY_PATH",
		},
		Advanced: AdvancedConfig{
			LDD:     false,
			AddExec: true,
		},
	}
}

// ConfigPath returns the path to the sandbox config file
func ConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "ollie", "sandbox.yaml")
}

// Load loads the config from disk, creating defaults if it doesn't exist
func Load() (*Config, error) {
	path := ConfigPath()

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		cfg := DefaultConfig()
		if saveErr := Save(cfg); saveErr != nil {
			return cfg, saveErr
		}
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Save saves the config to disk
func Save(cfg *Config) error {
	path := ConfigPath()

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}

	header := `# Ollie Sandbox Configuration
# Controls landrun sandboxing for execute_code
# Documentation: https://github.com/landlock-lsm/landrun
#
# Path templates:
#   {CWD}     - Session working directory
#   {HOME}    - User home directory
#   {TMPDIR}  - Temporary directory (/tmp or $TMPDIR)
#
# Changes apply to NEW sessions only.

`
	data = append([]byte(header), data...)

	return os.WriteFile(path, data, 0644)
}

// DefaultSandbox is the sandbox config used when none is specified
const DefaultSandbox = "default"

// validateName checks if a name is safe for use in file paths
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("name cannot be empty")
	}
	if len(name) > 64 {
		return fmt.Errorf("name too long (max 64 characters)")
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return fmt.Errorf("name contains invalid character: %c (only alphanumeric, hyphen, underscore allowed)", r)
		}
	}
	return nil
}

// ExpandPath replaces template variables with actual values
func ExpandPath(pattern, cwd string) string {
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
		}

		if val := os.Getenv(varName); val != "" {
			return val
		}
		return match
	})

	return s
}

// LoadMerged loads the base config merged with a named sandbox layer.
// If the named layer doesn't exist, returns the base config alone.
func LoadMerged(name string) (*Config, error) {
	baseCfg, err := Load()
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = "default"
	}
	baseLayer := LayeredConfig{
		Filesystem: baseCfg.Filesystem,
		Network:    baseCfg.Network,
		Env:        baseCfg.Env,
	}
	layers := []LayeredConfig{baseLayer}
	if sbxLayer, err := LoadSandbox(name); err == nil {
		layers = append(layers, sbxLayer)
	}
	return Merge(baseCfg.General, baseCfg.Advanced, layers...), nil
}

// CheckPath checks if the given absolute path is allowed by the sandbox config.
// If write is true, the path must fall under an RW or RWX entry.
// If write is false, any entry (RO, ROX, RW, RWX) grants access.
func CheckPath(cfg *Config, path string, write bool, cwd string) error {
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
		allowed = append(allowed, ExpandPath(p, cwd))
	}
	for _, p := range cfg.Filesystem.RWX {
		allowed = append(allowed, ExpandPath(p, cwd))
	}
	if !write {
		for _, p := range cfg.Filesystem.RO {
			allowed = append(allowed, ExpandPath(p, cwd))
		}
		for _, p := range cfg.Filesystem.ROX {
			allowed = append(allowed, ExpandPath(p, cwd))
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

// SystemDefaults returns the system-level defaults (restrictive baseline)
func SystemDefaults() LayeredConfig {
	return LayeredConfig{
		Filesystem: FilesystemConfig{
			RO: []string{
				"/etc/passwd",
				"/dev/null",
				"/proc/meminfo",
				"/proc/self/cgroup",
				"/proc/self/maps",
				"/proc/version",
			},
			ROX: []string{
				"/usr",
				"/lib",
				"/lib64",
				"/bin",
				"/sbin",
			},
			RW: []string{
				"{TMPDIR}",
			},
		},
		Network: NetworkConfig{
			Enabled:      false,
			Unrestricted: false,
		},
		Env: []string{
			"HOME",
			"USER",
			"PATH",
			"LANG",
			"TERM",
			"OLLIE_SESSION_ID",
			"OLLIE",
		},
	}
}

// LoadSandbox loads a named sandbox layer config from ~/.config/ollie/sandbox/<name>.yaml
func LoadSandbox(name string) (LayeredConfig, error) {
	if err := validateName(name); err != nil {
		return LayeredConfig{}, fmt.Errorf("invalid sandbox name: %w", err)
	}

	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".config", "ollie", "sandbox", name+".yaml")

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return LayeredConfig{}, fmt.Errorf("sandbox %q not found (looked in %s)", name, path)
	}
	if err != nil {
		return LayeredConfig{}, err
	}

	var cfg LayeredConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return LayeredConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}

	return cfg, nil
}

// Merge combines multiple layers into a final Config (most permissive wins)
func Merge(general GeneralConfig, advanced AdvancedConfig, layers ...LayeredConfig) *Config {
	cfg := &Config{
		General:  general,
		Advanced: advanced,
		Filesystem: FilesystemConfig{
			RO:  []string{},
			ROX: []string{},
			RW:  []string{},
			RWX: []string{},
		},
		Network: NetworkConfig{
			Enabled:      false,
			Unrestricted: false,
		},
		Env: []string{},
	}

	pathPerms := make(map[string]int)
	envSet := make(map[string]bool)

	for _, layer := range layers {
		for _, p := range layer.Filesystem.RO {
			if pathPerms[p] < 1 {
				pathPerms[p] = 1
			}
		}
		for _, p := range layer.Filesystem.ROX {
			if pathPerms[p] < 2 {
				pathPerms[p] = 2
			}
		}
		for _, p := range layer.Filesystem.RW {
			if pathPerms[p] < 3 {
				pathPerms[p] = 3
			}
		}
		for _, p := range layer.Filesystem.RWX {
			if pathPerms[p] < 4 {
				pathPerms[p] = 4
			}
		}

		if layer.Network.Enabled {
			cfg.Network.Enabled = true
		}
		if layer.Network.Unrestricted {
			cfg.Network.Unrestricted = true
		}
		cfg.Network.BindTCP = append(cfg.Network.BindTCP, layer.Network.BindTCP...)
		cfg.Network.ConnectTCP = append(cfg.Network.ConnectTCP, layer.Network.ConnectTCP...)

		for _, e := range layer.Env {
			envSet[e] = true
		}
	}

	for path, perm := range pathPerms {
		switch perm {
		case 1:
			cfg.Filesystem.RO = append(cfg.Filesystem.RO, path)
		case 2:
			cfg.Filesystem.ROX = append(cfg.Filesystem.ROX, path)
		case 3:
			cfg.Filesystem.RW = append(cfg.Filesystem.RW, path)
		case 4:
			cfg.Filesystem.RWX = append(cfg.Filesystem.RWX, path)
		}
	}

	for e := range envSet {
		cfg.Env = append(cfg.Env, e)
	}

	return cfg
}
