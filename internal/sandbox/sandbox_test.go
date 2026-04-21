package sandbox

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestMain creates a fake landrun binary so IsAvailable() returns true for all tests.
func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "sandbox-test-*")
	if err != nil {
		panic(err)
	}

	fakeLandrun := filepath.Join(tmpDir, "landrun")
	if err := os.WriteFile(fakeLandrun, []byte("#!/bin/sh\necho 'landrun 0.0.0'\n"), 0755); err != nil {
		panic(err)
	}
	os.Setenv("PATH", tmpDir+":"+os.Getenv("PATH"))

	os.Exit(m.Run())
}

// ---- expandPath ----

func TestExpandPath(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		cwd     string
		env     map[string]string
		want    string
	}{
		{
			name:    "literal",
			pattern: "/etc/passwd",
			cwd:     "/cwd",
			want:    "/etc/passwd",
		},
		{
			name:    "CWD",
			pattern: "{CWD}/foo",
			cwd:     "/my/cwd",
			want:    "/my/cwd/foo",
		},
		{
			name:    "TMPDIR from env",
			pattern: "{TMPDIR}",
			cwd:     "/cwd",
			env:     map[string]string{"TMPDIR": "/custom/tmp"},
			want:    "/custom/tmp",
		},
		{
			name:    "TMPDIR default",
			pattern: "{TMPDIR}",
			cwd:     "/cwd",
			env:     map[string]string{"TMPDIR": ""},
			want:    "/tmp",
		},
		{
			name:    "custom env var",
			pattern: "{MY_SANDBOX_TEST_VAR}/bar",
			cwd:     "/cwd",
			env:     map[string]string{"MY_SANDBOX_TEST_VAR": "/custom"},
			want:    "/custom/bar",
		},
		{
			name:    "unknown var unchanged",
			pattern: "{NONEXISTENT_SANDBOX_VAR_XYZ}",
			cwd:     "/cwd",
			want:    "{NONEXISTENT_SANDBOX_VAR_XYZ}",
		},
		{
			name:    "XDG_CONFIG_HOME from env",
			pattern: "{XDG_CONFIG_HOME}/app",
			cwd:     "/cwd",
			env:     map[string]string{"XDG_CONFIG_HOME": "/xdg/config"},
			want:    "/xdg/config/app",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got := expandPath(tc.pattern, tc.cwd)
			if got != tc.want {
				t.Errorf("expandPath(%q, %q) = %q, want %q", tc.pattern, tc.cwd, got, tc.want)
			}
		})
	}
}

// ---- WrapCommand ----

func TestWrapCommand(t *testing.T) {
	tmpDir := t.TempDir()
	rwDir := filepath.Join(tmpDir, "rw")
	roDir := filepath.Join(tmpDir, "ro")
	if err := os.MkdirAll(rwDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(roDir, 0755); err != nil {
		t.Fatal(err)
	}

	t.Run("starts with landrun", func(t *testing.T) {
		cfg := &Config{General: GeneralConfig{}, Advanced: AdvancedConfig{}, Network: NetworkConfig{}}
		got := WrapCommand(cfg, []string{"echo", "hi"}, tmpDir)
		if len(got) == 0 || got[0] != "landrun" {
			t.Errorf("expected landrun as first arg, got %v", got)
		}
	})

	t.Run("separator before original command", func(t *testing.T) {
		cfg := &Config{General: GeneralConfig{}, Advanced: AdvancedConfig{}, Network: NetworkConfig{}}
		got := WrapCommand(cfg, []string{"echo", "test"}, tmpDir)
		sepIdx := indexOf(got, "--")
		if sepIdx == -1 {
			t.Fatal("missing -- separator")
		}
		if got[sepIdx+1] != "echo" || got[sepIdx+2] != "test" {
			t.Errorf("expected [echo test] after --, got %v", got[sepIdx+1:])
		}
	})

	t.Run("log level flag", func(t *testing.T) {
		cfg := &Config{General: GeneralConfig{LogLevel: "debug"}, Advanced: AdvancedConfig{}, Network: NetworkConfig{}}
		got := WrapCommand(cfg, []string{"sh"}, tmpDir)
		assertFlagValue(t, got, "--log-level", "debug")
	})

	t.Run("no log level when empty", func(t *testing.T) {
		cfg := &Config{General: GeneralConfig{LogLevel: ""}, Advanced: AdvancedConfig{}, Network: NetworkConfig{}}
		got := WrapCommand(cfg, []string{"sh"}, tmpDir)
		assertDoesNotContain(t, got, "--log-level")
	})

	t.Run("best effort flag", func(t *testing.T) {
		cfg := &Config{General: GeneralConfig{BestEffort: true}, Advanced: AdvancedConfig{}, Network: NetworkConfig{}}
		got := WrapCommand(cfg, []string{"sh"}, tmpDir)
		assertContains(t, got, "--best-effort")
	})

	t.Run("no best effort when false", func(t *testing.T) {
		cfg := &Config{General: GeneralConfig{BestEffort: false}, Advanced: AdvancedConfig{}, Network: NetworkConfig{}}
		got := WrapCommand(cfg, []string{"sh"}, tmpDir)
		assertDoesNotContain(t, got, "--best-effort")
	})

	t.Run("ldd flag", func(t *testing.T) {
		cfg := &Config{General: GeneralConfig{}, Advanced: AdvancedConfig{LDD: true}, Network: NetworkConfig{}}
		got := WrapCommand(cfg, []string{"sh"}, tmpDir)
		assertContains(t, got, "--ldd")
	})

	t.Run("add-exec flag", func(t *testing.T) {
		cfg := &Config{General: GeneralConfig{}, Advanced: AdvancedConfig{AddExec: true}, Network: NetworkConfig{}}
		got := WrapCommand(cfg, []string{"sh"}, tmpDir)
		assertContains(t, got, "--add-exec")
	})

	t.Run("rw path included when exists", func(t *testing.T) {
		cfg := &Config{
			General:    GeneralConfig{},
			Advanced:   AdvancedConfig{},
			Filesystem: FilesystemConfig{RW: []string{rwDir}},
			Network:    NetworkConfig{},
		}
		got := WrapCommand(cfg, []string{"sh"}, tmpDir)
		assertFlagValue(t, got, "--rw", rwDir)
	})

	t.Run("ro path included when exists", func(t *testing.T) {
		cfg := &Config{
			General:    GeneralConfig{},
			Advanced:   AdvancedConfig{},
			Filesystem: FilesystemConfig{RO: []string{roDir}},
			Network:    NetworkConfig{},
		}
		got := WrapCommand(cfg, []string{"sh"}, tmpDir)
		assertFlagValue(t, got, "--ro", roDir)
	})

	t.Run("nonexistent path excluded", func(t *testing.T) {
		cfg := &Config{
			General:    GeneralConfig{},
			Advanced:   AdvancedConfig{},
			Filesystem: FilesystemConfig{RW: []string{"/nonexistent/path/that/does/not/exist/xyz"}},
			Network:    NetworkConfig{},
		}
		got := WrapCommand(cfg, []string{"sh"}, tmpDir)
		assertDoesNotContain(t, got, "--rw")
	})

	t.Run("CWD template expanded to real path", func(t *testing.T) {
		cfg := &Config{
			General:    GeneralConfig{},
			Advanced:   AdvancedConfig{},
			Filesystem: FilesystemConfig{RW: []string{"{CWD}"}},
			Network:    NetworkConfig{},
		}
		got := WrapCommand(cfg, []string{"sh"}, tmpDir)
		assertFlagValue(t, got, "--rw", tmpDir)
	})

	t.Run("unrestricted network", func(t *testing.T) {
		cfg := &Config{
			General:  GeneralConfig{},
			Advanced: AdvancedConfig{},
			Network:  NetworkConfig{Enabled: true, Unrestricted: true},
		}
		got := WrapCommand(cfg, []string{"sh"}, tmpDir)
		assertContains(t, got, "--unrestricted-network")
		assertDoesNotContain(t, got, "--connect-tcp")
		assertDoesNotContain(t, got, "--bind-tcp")
	})

	t.Run("restricted network ports", func(t *testing.T) {
		cfg := &Config{
			General:  GeneralConfig{},
			Advanced: AdvancedConfig{},
			Network: NetworkConfig{
				Enabled:    true,
				ConnectTCP: []string{"443", "80"},
				BindTCP:    []string{"8080"},
			},
		}
		got := WrapCommand(cfg, []string{"sh"}, tmpDir)
		assertFlagValue(t, got, "--connect-tcp", "443")
		assertFlagValue(t, got, "--connect-tcp", "80")
		assertFlagValue(t, got, "--bind-tcp", "8080")
		assertDoesNotContain(t, got, "--unrestricted-network")
	})

	t.Run("network disabled ignores ports", func(t *testing.T) {
		cfg := &Config{
			General:  GeneralConfig{},
			Advanced: AdvancedConfig{},
			Network:  NetworkConfig{Enabled: false, ConnectTCP: []string{"443"}},
		}
		got := WrapCommand(cfg, []string{"sh"}, tmpDir)
		assertDoesNotContain(t, got, "--connect-tcp")
		assertDoesNotContain(t, got, "--unrestricted-network")
	})

	t.Run("env vars", func(t *testing.T) {
		cfg := &Config{
			General:  GeneralConfig{},
			Advanced: AdvancedConfig{},
			Network:  NetworkConfig{},
			Env:      []string{"HOME", "PATH"},
		}
		got := WrapCommand(cfg, []string{"sh"}, tmpDir)
		assertFlagValue(t, got, "--env", "HOME")
		assertFlagValue(t, got, "--env", "PATH")
	})

	t.Run("parent path sorted before child", func(t *testing.T) {
		parent := filepath.Join(tmpDir, "parent")
		child := filepath.Join(tmpDir, "parent", "child")
		if err := os.MkdirAll(child, 0755); err != nil {
			t.Fatal(err)
		}
		cfg := &Config{
			General:    GeneralConfig{},
			Advanced:   AdvancedConfig{},
			Filesystem: FilesystemConfig{RW: []string{child, parent}}, // child listed first
			Network:    NetworkConfig{},
		}
		got := WrapCommand(cfg, []string{"sh"}, tmpDir)
		parentIdx := indexOf(got, parent)
		childIdx := indexOf(got, child)
		if parentIdx == -1 || childIdx == -1 {
			t.Fatal("parent or child path missing from args")
		}
		if parentIdx > childIdx {
			t.Errorf("parent (idx %d) should appear before child (idx %d) in args", parentIdx, childIdx)
		}
	})
}

// ---- checkPath ----

func TestCheckPath(t *testing.T) {
	tmpDir := t.TempDir()
	rwDir := filepath.Join(tmpDir, "rw")
	roDir := filepath.Join(tmpDir, "ro")
	if err := os.MkdirAll(rwDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(roDir, 0755); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		Filesystem: FilesystemConfig{
			RO: []string{roDir},
			RW: []string{rwDir},
		},
	}

	tests := []struct {
		name    string
		path    string
		write   bool
		wantErr bool
	}{
		{"rw path read allowed", filepath.Join(rwDir, "file.txt"), false, false},
		{"rw path write allowed", filepath.Join(rwDir, "file.txt"), true, false},
		{"ro path read allowed", filepath.Join(roDir, "file.txt"), false, false},
		{"ro path write denied", filepath.Join(roDir, "file.txt"), true, true},
		{"outside path read denied", "/outside/path/xyz", false, true},
		{"outside path write denied", "/outside/path/xyz", true, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPath(cfg, tc.path, tc.write, tmpDir)
			if (err != nil) != tc.wantErr {
				t.Errorf("checkPath(%q, write=%v) error = %v, wantErr %v", tc.path, tc.write, err, tc.wantErr)
			}
		})
	}

	t.Run("CWD template resolved", func(t *testing.T) {
		cfg2 := &Config{Filesystem: FilesystemConfig{RW: []string{"{CWD}"}}}
		err := checkPath(cfg2, filepath.Join(tmpDir, "newfile.txt"), true, tmpDir)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("RWX grants write access", func(t *testing.T) {
		rwxDir := filepath.Join(tmpDir, "rwx")
		if err := os.MkdirAll(rwxDir, 0755); err != nil {
			t.Fatal(err)
		}
		cfg3 := &Config{Filesystem: FilesystemConfig{RWX: []string{rwxDir}}}
		if err := checkPath(cfg3, filepath.Join(rwxDir, "bin"), true, tmpDir); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

// ---- LoadSandbox ----

func TestLoadSandbox(t *testing.T) {
	t.Run("full config", func(t *testing.T) {
		yaml := []byte(`
general:
  best_effort: true
  log_level: debug
filesystem:
  ro:
    - /etc/passwd
  rox:
    - /usr
  rw:
    - "{CWD}"
  rwx:
    - "{OLLIE_CFG_PATH}"
network:
  enabled: true
  unrestricted: false
  bind_tcp:
    - "8080"
  connect_tcp:
    - "443"
env:
  - HOME
  - PATH
advanced:
  ldd: true
  add_exec: false
`)
		cfg, err := LoadSandbox(bytes.NewReader(yaml))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !cfg.General.BestEffort {
			t.Error("BestEffort should be true")
		}
		if cfg.General.LogLevel != "debug" {
			t.Errorf("LogLevel = %q, want %q", cfg.General.LogLevel, "debug")
		}
		if !containsStr(cfg.Filesystem.RO, "/etc/passwd") {
			t.Error("RO missing /etc/passwd")
		}
		if !containsStr(cfg.Filesystem.ROX, "/usr") {
			t.Error("ROX missing /usr")
		}
		if !containsStr(cfg.Filesystem.RW, "{CWD}") {
			t.Error("RW missing {CWD}")
		}
		if !containsStr(cfg.Filesystem.RWX, "{OLLIE_CFG_PATH}") {
			t.Error("RWX missing {OLLIE_CFG_PATH}")
		}
		if !cfg.Network.Enabled {
			t.Error("Network.Enabled should be true")
		}
		if cfg.Network.Unrestricted {
			t.Error("Network.Unrestricted should be false")
		}
		if !containsStr(cfg.Network.BindTCP, "8080") {
			t.Error("BindTCP missing 8080")
		}
		if !containsStr(cfg.Network.ConnectTCP, "443") {
			t.Error("ConnectTCP missing 443")
		}
		if !containsStr(cfg.Env, "HOME") || !containsStr(cfg.Env, "PATH") {
			t.Error("Env missing HOME or PATH")
		}
		if !cfg.Advanced.LDD {
			t.Error("Advanced.LDD should be true")
		}
		if cfg.Advanced.AddExec {
			t.Error("Advanced.AddExec should be false")
		}
	})

	t.Run("empty yaml produces zero config", func(t *testing.T) {
		cfg, err := LoadSandbox(bytes.NewReader(nil))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.General.BestEffort || cfg.General.LogLevel != "" {
			t.Error("expected zero GeneralConfig")
		}
		if len(cfg.Filesystem.RW) != 0 || len(cfg.Env) != 0 {
			t.Error("expected empty filesystem and env")
		}
	})

	t.Run("invalid yaml returns error", func(t *testing.T) {
		_, err := LoadSandbox(bytes.NewReader([]byte("{ not: valid: yaml: [")))
		if err == nil {
			t.Error("expected error for invalid yaml")
		}
	})

	t.Run("partial config leaves other fields zero", func(t *testing.T) {
		yaml := []byte(`
env:
  - TERM
`)
		cfg, err := LoadSandbox(bytes.NewReader(yaml))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !containsStr(cfg.Env, "TERM") {
			t.Error("Env missing TERM")
		}
		if len(cfg.Filesystem.RW) != 0 {
			t.Error("expected empty RW")
		}
	})
}

// ---- helpers ----

func assertContains(t *testing.T, args []string, s string) {
	t.Helper()
	if indexOf(args, s) == -1 {
		t.Errorf("args %v does not contain %q", args, s)
	}
}

func assertDoesNotContain(t *testing.T, args []string, s string) {
	t.Helper()
	if indexOf(args, s) != -1 {
		t.Errorf("args %v should not contain %q", args, s)
	}
}

func assertFlagValue(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return
		}
	}
	t.Errorf("args %v missing %s %s", args, flag, value)
}

func indexOf(args []string, s string) int {
	for i, a := range args {
		if a == s {
			return i
		}
	}
	return -1
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
