package sandbox

import (
	"bytes"
	"fmt"
	"net"
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
	old := isSuperpowerdRunningFn
	isSuperpowerdRunningFn = func() bool { return false }
	defer func() { isSuperpowerdRunningFn = old }()

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
		got := mustWrapCommand(t, cfg, []string{"echo", "hi"}, tmpDir)
		if len(got) == 0 || got[0] != "landrun" {
			t.Errorf("expected landrun as first arg, got %v", got)
		}
	})

	t.Run("separator before original command", func(t *testing.T) {
		cfg := &Config{General: GeneralConfig{}, Advanced: AdvancedConfig{}, Network: NetworkConfig{}}
		got := mustWrapCommand(t, cfg, []string{"echo", "test"}, tmpDir)
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
		got := mustWrapCommand(t, cfg, []string{"sh"}, tmpDir)
		assertFlagValue(t, got, "--log-level", "debug")
	})

	t.Run("no log level when empty", func(t *testing.T) {
		cfg := &Config{General: GeneralConfig{LogLevel: ""}, Advanced: AdvancedConfig{}, Network: NetworkConfig{}}
		got := mustWrapCommand(t, cfg, []string{"sh"}, tmpDir)
		assertDoesNotContain(t, got, "--log-level")
	})

	t.Run("best effort flag", func(t *testing.T) {
		cfg := &Config{General: GeneralConfig{BestEffort: true}, Advanced: AdvancedConfig{}, Network: NetworkConfig{}}
		got := mustWrapCommand(t, cfg, []string{"sh"}, tmpDir)
		assertContains(t, got, "--best-effort")
	})

	t.Run("no best effort when false", func(t *testing.T) {
		cfg := &Config{General: GeneralConfig{BestEffort: false}, Advanced: AdvancedConfig{}, Network: NetworkConfig{}}
		got := mustWrapCommand(t, cfg, []string{"sh"}, tmpDir)
		assertDoesNotContain(t, got, "--best-effort")
	})

	t.Run("ldd flag", func(t *testing.T) {
		cfg := &Config{General: GeneralConfig{}, Advanced: AdvancedConfig{LDD: true}, Network: NetworkConfig{}}
		got := mustWrapCommand(t, cfg, []string{"sh"}, tmpDir)
		assertContains(t, got, "--ldd")
	})

	t.Run("add-exec flag", func(t *testing.T) {
		cfg := &Config{General: GeneralConfig{}, Advanced: AdvancedConfig{AddExec: true}, Network: NetworkConfig{}}
		got := mustWrapCommand(t, cfg, []string{"sh"}, tmpDir)
		assertContains(t, got, "--add-exec")
	})

	t.Run("rw path included when exists", func(t *testing.T) {
		cfg := &Config{
			General:    GeneralConfig{},
			Advanced:   AdvancedConfig{},
			Filesystem: FilesystemConfig{RW: []string{rwDir}},
			Network:    NetworkConfig{},
		}
		got := mustWrapCommand(t, cfg, []string{"sh"}, tmpDir)
		assertFlagValue(t, got, "--rw", rwDir)
	})

	t.Run("ro path included when exists", func(t *testing.T) {
		cfg := &Config{
			General:    GeneralConfig{},
			Advanced:   AdvancedConfig{},
			Filesystem: FilesystemConfig{RO: []string{roDir}},
			Network:    NetworkConfig{},
		}
		got := mustWrapCommand(t, cfg, []string{"sh"}, tmpDir)
		assertFlagValue(t, got, "--ro", roDir)
	})

	t.Run("nonexistent path excluded", func(t *testing.T) {
		cfg := &Config{
			General:    GeneralConfig{},
			Advanced:   AdvancedConfig{},
			Filesystem: FilesystemConfig{RW: []string{"/nonexistent/path/that/does/not/exist/xyz"}},
			Network:    NetworkConfig{},
		}
		got := mustWrapCommand(t, cfg, []string{"sh"}, tmpDir)
		assertDoesNotContain(t, got, "--rw")
	})

	t.Run("CWD template expanded to real path", func(t *testing.T) {
		cfg := &Config{
			General:    GeneralConfig{},
			Advanced:   AdvancedConfig{},
			Filesystem: FilesystemConfig{RW: []string{"{CWD}"}},
			Network:    NetworkConfig{},
		}
		got := mustWrapCommand(t, cfg, []string{"sh"}, tmpDir)
		assertFlagValue(t, got, "--rw", tmpDir)
	})

	t.Run("unrestricted network", func(t *testing.T) {
		cfg := &Config{
			General:  GeneralConfig{},
			Advanced: AdvancedConfig{},
			Network:  NetworkConfig{Enabled: true, Unrestricted: true},
		}
		got := mustWrapCommand(t, cfg, []string{"sh"}, tmpDir)
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
		got := mustWrapCommand(t, cfg, []string{"sh"}, tmpDir)
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
		got := mustWrapCommand(t, cfg, []string{"sh"}, tmpDir)
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
		got := mustWrapCommand(t, cfg, []string{"sh"}, tmpDir)
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
		got := mustWrapCommand(t, cfg, []string{"sh"}, tmpDir)
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

// ---- expandPath: remaining branches ----

func TestExpandPath_HOME(t *testing.T) {
	got := expandPath("{HOME}/test", "/cwd")
	home, _ := os.UserHomeDir()
	if got != home+"/test" {
		t.Errorf("expandPath({HOME}/test) = %q; want %q", got, home+"/test")
	}
}

func TestExpandPath_XDG_Defaults(t *testing.T) {
	home, _ := os.UserHomeDir()
	for _, tc := range []struct {
		varName, pattern, want string
	}{
		{"XDG_CONFIG_HOME", "{XDG_CONFIG_HOME}", filepath.Join(home, ".config")},
		{"XDG_DATA_HOME", "{XDG_DATA_HOME}", filepath.Join(home, ".local/share")},
		{"XDG_CACHE_HOME", "{XDG_CACHE_HOME}", filepath.Join(home, ".cache")},
		{"XDG_STATE_HOME", "{XDG_STATE_HOME}", filepath.Join(home, ".local/state")},
		{"XDG_RUNTIME_DIR", "{XDG_RUNTIME_DIR}", fmt.Sprintf("/run/user/%d", os.Getuid())},
	} {
		t.Run(tc.varName+"_default", func(t *testing.T) {
			t.Setenv(tc.varName, "")
			got := expandPath(tc.pattern, "/cwd")
			if got != tc.want {
				t.Errorf("expandPath(%q) = %q; want %q", tc.pattern, got, tc.want)
			}
		})
	}
}

func TestExpandPath_XDG_FromEnv(t *testing.T) {
	for _, tc := range []struct {
		varName, pattern string
	}{
		{"XDG_DATA_HOME", "{XDG_DATA_HOME}"},
		{"XDG_CACHE_HOME", "{XDG_CACHE_HOME}"},
		{"XDG_STATE_HOME", "{XDG_STATE_HOME}"},
		{"XDG_RUNTIME_DIR", "{XDG_RUNTIME_DIR}"},
	} {
		t.Run(tc.varName+"_env", func(t *testing.T) {
			t.Setenv(tc.varName, "/custom/"+tc.varName)
			got := expandPath(tc.pattern, "/cwd")
			if got != "/custom/"+tc.varName {
				t.Errorf("expandPath(%q) = %q; want /custom/%s", tc.pattern, got, tc.varName)
			}
		})
	}
}

func TestExpandPath_OlliePaths(t *testing.T) {
	got1 := expandPath("{OLLIE_CFG_PATH}", "/cwd")
	if got1 == "{OLLIE_CFG_PATH}" || got1 == "" {
		t.Errorf("OLLIE_CFG_PATH not expanded: %q", got1)
	}
	got2 := expandPath("{OLLIE_DATA_PATH}", "/cwd")
	if got2 == "{OLLIE_DATA_PATH}" || got2 == "" {
		t.Errorf("OLLIE_DATA_PATH not expanded: %q", got2)
	}
}

// ---- checkPath: remaining branches ----

func TestCheckPath_SymlinkResolved(t *testing.T) {
	tmpDir := t.TempDir()
	realDir := filepath.Join(tmpDir, "real")
	os.MkdirAll(realDir, 0755)
	// Create a real file so EvalSymlinks succeeds on the full path
	realFile := filepath.Join(realDir, "file")
	os.WriteFile(realFile, []byte("x"), 0644)
	link := filepath.Join(tmpDir, "link")
	os.Symlink(realDir, link)

	cfg := &Config{Filesystem: FilesystemConfig{RW: []string{realDir}}}
	// Access via symlink — EvalSymlinks resolves link/file to real/file
	if err := checkPath(cfg, filepath.Join(link, "file"), true, tmpDir); err != nil {
		t.Errorf("symlink path should be allowed: %v", err)
	}
}

func TestCheckPath_ROX_ReadAllowed(t *testing.T) {
	tmpDir := t.TempDir()
	roxDir := filepath.Join(tmpDir, "rox")
	os.MkdirAll(roxDir, 0755)

	cfg := &Config{Filesystem: FilesystemConfig{ROX: []string{roxDir}}}
	if err := checkPath(cfg, filepath.Join(roxDir, "bin"), false, tmpDir); err != nil {
		t.Errorf("ROX read should be allowed: %v", err)
	}
	if err := checkPath(cfg, filepath.Join(roxDir, "bin"), true, tmpDir); err == nil {
		t.Error("ROX write should be denied")
	}
}

// ---- LoadSandbox: reader error ----

func TestLoadSandbox_ReaderError(t *testing.T) {
	_, err := LoadSandbox(&errReader{})
	if err == nil {
		t.Error("expected error from bad reader")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, fmt.Errorf("read error") }

// ---- WrapCommand: remaining branches ----

func TestWrapCommand_LandrunUnavailable(t *testing.T) {
	old := isAvailableFn
	isAvailableFn = func() bool { return false }
	defer func() { isAvailableFn = old }()

	cfg := &Config{}
	_, err := WrapCommand(cfg, []string{"echo", "hi"}, "/tmp")
	if err == nil {
		t.Fatal("expected error when landrun unavailable")
	}
	if err != ErrNotAvailable {
		t.Errorf("expected ErrNotAvailable, got %v", err)
	}
}

func TestWrapCommand_ROX_RWX(t *testing.T) {
	tmpDir := t.TempDir()
	roxDir := filepath.Join(tmpDir, "rox")
	rwxDir := filepath.Join(tmpDir, "rwx")
	os.MkdirAll(roxDir, 0755)
	os.MkdirAll(rwxDir, 0755)

	cfg := &Config{
		Filesystem: FilesystemConfig{
			ROX: []string{roxDir},
			RWX: []string{rwxDir},
		},
	}
	got := mustWrapCommand(t, cfg, []string{"sh"}, tmpDir)
	assertFlagValue(t, got, "--rox", roxDir)
	assertFlagValue(t, got, "--rwx", rwxDir)
}

func TestWrapCommand_SortTiebreaker(t *testing.T) {
	tmpDir := t.TempDir()
	// Two paths of equal length
	dirA := filepath.Join(tmpDir, "aaa")
	dirB := filepath.Join(tmpDir, "bbb")
	os.MkdirAll(dirA, 0755)
	os.MkdirAll(dirB, 0755)

	cfg := &Config{
		Filesystem: FilesystemConfig{RW: []string{dirB, dirA}},
	}
	got := mustWrapCommand(t, cfg, []string{"sh"}, tmpDir)
	idxA := indexOf(got, dirA)
	idxB := indexOf(got, dirB)
	if idxA == -1 || idxB == -1 {
		t.Fatal("both dirs should be in args")
	}
	if idxA > idxB {
		t.Errorf("dirA (%q) should sort before dirB (%q)", dirA, dirB)
	}
}

func TestWrapCommand_Superpowerd(t *testing.T) {
	// Create a fake superpowers binary
	tmpBin := t.TempDir()
	fakeSP := filepath.Join(tmpBin, "superpowers")
	os.WriteFile(fakeSP, []byte("#!/bin/sh\n"), 0755)
	t.Setenv("PATH", tmpBin+":"+os.Getenv("PATH"))

	old := isSuperpowerdRunningFn
	isSuperpowerdRunningFn = func() bool { return true }
	defer func() { isSuperpowerdRunningFn = old }()

	cfg := &Config{}
	got := mustWrapCommand(t, cfg, []string{"echo"}, "/tmp")
	if got[0] != fakeSP {
		t.Errorf("expected superpowers as first arg; got %v", got)
	}
	assertContains(t, got, "run-session")
	assertContains(t, got, "SUPERPOWERD_SESSION_TOKEN")
}

// ---- isSuperpowerdRunning ----

func TestIsSuperpowerdRunning_NoSocket(t *testing.T) {
	t.Setenv("SUPERPOWERD_SOCKET_DIR", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	if isSuperpowerdRunning() {
		t.Error("should return false with no socket dirs")
	}
}

func TestIsSuperpowerdRunning_CustomSocketDir(t *testing.T) {
	t.Setenv("SUPERPOWERD_SOCKET_DIR", "/nonexistent/socket/dir")
	if isSuperpowerdRunning() {
		t.Error("should return false with nonexistent socket")
	}
}

func TestIsSuperpowerdRunning_XDGFallback(t *testing.T) {
	t.Setenv("SUPERPOWERD_SOCKET_DIR", "")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	if isSuperpowerdRunning() {
		t.Error("should return false with no actual socket")
	}
}

func TestIsSuperpowerdRunning_SocketExists(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "superpowerd.sock")

	// Start a unixpacket listener
	ln, err := net.ListenPacket("unixpacket", sockPath)
	if err != nil {
		t.Skipf("cannot create unixpacket socket: %v", err)
	}
	defer ln.Close()

	t.Setenv("SUPERPOWERD_SOCKET_DIR", tmpDir)
	if !isSuperpowerdRunning() {
		t.Error("should return true with active socket")
	}
}

// ---- helpers ----

func mustWrapCommand(t *testing.T, cfg *Config, cmd []string, cwd string) []string {
	t.Helper()
	got, err := WrapCommand(cfg, cmd, cwd)
	if err != nil {
		t.Fatalf("WrapCommand failed: %v", err)
	}
	return got
}

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
