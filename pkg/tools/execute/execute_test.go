package execute

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain points OLLIE_CFG_PATH at testdata so loadSandboxConfig uses the
// minimal test sandbox config rather than the user's ~/.config/ollie/sandbox/.
func TestMain(m *testing.M) {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	os.Setenv("OLLIE_CFG_PATH", filepath.Join(wd, "testdata"))
	os.Exit(m.Run())
}

// newServer returns a Server with Confirm always returning true.
func newServer(t *testing.T) *Server {
	t.Helper()
	s := New(t.TempDir())
	s.Confirm = func(string) bool { return true }
	return s
}

// callCode is a helper that invokes execute_code via Dispatch.
func callCode(t *testing.T, s *Server, steps []map[string]any, extra ...map[string]any) (string, error) {
	t.Helper()
	payload := map[string]any{"steps": steps}
	if len(extra) > 0 {
		for k, v := range extra[0] {
			payload[k] = v
		}
	}
	raw, _ := json.Marshal(payload)
	return s.Dispatch(context.Background(), "execute_code", raw)
}

// ---- detectLanguage ----

func TestDetectLanguage(t *testing.T) {
	cases := []struct{ code, want string }{
		{"echo hi", "bash"},
		{"#!/bin/bash\necho hi", "bash"},
		{"#!/usr/bin/env python3\nprint(1)", "python3"},
		{"#!/usr/bin/python\nprint(1)", "python3"},
		{"#!/usr/bin/perl\nprint 1", "perl"},
		{"#!/usr/bin/awk -f\n{print}", "awk"},
		{"#!/usr/bin/env gawk\n{print}", "awk"},
		{"#!/usr/bin/sed -f\ns/a/b/", "sed"},
		{"#!/usr/bin/ed\n,p", "ed"},
		{"#!/usr/bin/env jq\n.", "jq"},
		{"#!/usr/bin/env lua\nprint(1)", "lua"},
	}
	for _, c := range cases {
		if got := detectLanguage(c.code); got != c.want {
			t.Errorf("detectLanguage(%q) = %q, want %q", c.code[:min(20, len(c.code))], got, c.want)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---- ansiCEscape ----

func TestAnsiCEscape(t *testing.T) {
	got := ansiCEscape("a\\b'c\nd\te")
	want := `a\\b\'c\nd\te`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// ---- injectArgs ----

func TestInjectArgsBash(t *testing.T) {
	out := injectArgs("bash", "myscript", []string{"hello", "world"}, "echo $1 $2")
	if !strings.HasPrefix(out, "set -- ") {
		t.Errorf("bash inject should start with 'set --', got: %q", out)
	}
	if !strings.Contains(out, "echo $1 $2") {
		t.Errorf("bash inject should contain original code")
	}
}

func TestInjectArgsPython(t *testing.T) {
	out := injectArgs("python3", "s", []string{"a"}, "print(sys.argv)")
	if !strings.HasPrefix(out, "import sys\n") {
		t.Errorf("python inject should start with 'import sys', got: %q", out)
	}
}

// ---- ToolsPath / PluginsPath ----

func TestToolsPathEnv(t *testing.T) {
	t.Setenv("OLLIE_TOOLS_PATH", "/custom/tools:/other")
	if got := ToolsPath(); got != "/custom/tools" {
		t.Errorf("got %q, want /custom/tools", got)
	}
}

func TestPluginsPathEnv(t *testing.T) {
	t.Setenv("OLLIE_PLUGINS_PATH", "/custom/plugins:/other")
	if got := PluginsPath(); got != "/custom/plugins" {
		t.Errorf("got %q, want /custom/plugins", got)
	}
}

// ---- ReadTool ----

func TestReadTool(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OLLIE_TOOLS_PATH", dir)
	if err := os.WriteFile(filepath.Join(dir, "mytool"), []byte("#!/bin/bash\necho ok"), 0644); err != nil {
		t.Fatal(err)
	}
	code, err := ReadTool("mytool")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(code, "echo ok") {
		t.Errorf("unexpected content: %q", code)
	}
}

func TestReadToolNotFound(t *testing.T) {
	t.Setenv("OLLIE_TOOLS_PATH", t.TempDir())
	_, err := ReadTool("nonexistent")
	if err == nil || !strings.Contains(err.Error(), "tool not found") {
		t.Errorf("expected 'tool not found' error, got %v", err)
	}
}

func TestReadToolInvalidName(t *testing.T) {
	for _, name := range []string{"../etc/passwd", "foo/bar"} {
		_, err := ReadTool(name)
		if err == nil {
			t.Errorf("expected error for name %q", name)
		}
	}
}

// ---- ValidateCode ----

func TestValidateCodeDangerous(t *testing.T) {
	s := newServer(t)
	cases := []struct{ code, lang string }{
		{"sudo rm -rf /", "bash"},
		{"rm -rf /home", "bash"},
		{"mkfs /dev/sda", "bash"},
		{"dd if=/dev/zero of=/dev/sda", "bash"},
		{"shutil.rmtree('/')", "python3"},
	}
	for _, c := range cases {
		if err := s.ValidateCode(c.code, c.lang); err == nil {
			t.Errorf("expected dangerous pattern error for %q (%s)", c.code, c.lang)
		}
	}
}

func TestValidateCodeSafe(t *testing.T) {
	s := newServer(t)
	if err := s.ValidateCode("echo hello", "bash"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// ---- allowed ----

func TestAllowedWithConfirm(t *testing.T) {
	s := New(t.TempDir())
	s.Confirm = func(string) bool { return true }
	if !s.allowed("execute_code", "echo hi") {
		t.Error("expected allowed=true when Confirm returns true")
	}
}

func TestAllowedDeniedNoConfirm(t *testing.T) {
	s := New(t.TempDir())
	if s.allowed("execute_code", "echo hi") {
		t.Error("expected allowed=false when Confirm is nil")
	}
}

func TestAllowedTrustedTool(t *testing.T) {
	s := New(t.TempDir())
	s.SetTrustedTools([]string{"execute_code"})
	if !s.allowed("execute_code", "anything") {
		t.Error("expected trusted tool to be allowed without Confirm")
	}
}

// ---- Execute (integration, requires bash) ----

func TestExecuteSimpleBash(t *testing.T) {
	s := newServer(t)
	out, err := s.Execute(context.Background(), "echo hello", "bash", 10, "default", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(out) != "hello" {
		t.Errorf("got %q, want %q", out, "hello")
	}
}

func TestExecuteStdin(t *testing.T) {
	s := newServer(t)
	out, err := s.executeWithStdin(context.Background(), "cat", "bash", 10, "default", true, "piped input")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(out) != "piped input" {
		t.Errorf("got %q, want %q", out, "piped input")
	}
}

func TestExecuteTimeout(t *testing.T) {
	s := newServer(t)
	_, err := s.Execute(context.Background(), "sleep 10", "bash", 1, "default", true)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Errorf("expected timeout error, got %v", err)
	}
}

func TestExecuteNonZeroExit(t *testing.T) {
	s := newServer(t)
	_, err := s.Execute(context.Background(), "exit 1", "bash", 10, "default", true)
	if err == nil {
		t.Error("expected error for non-zero exit")
	}
}

func TestExecuteUnsupportedLanguage(t *testing.T) {
	s := newServer(t)
	_, err := s.Execute(context.Background(), "code", "cobol", 10, "default", true)
	if err == nil || !strings.Contains(err.Error(), "unsupported language") {
		t.Errorf("expected unsupported language error, got %v", err)
	}
}

// ---- Dispatch / execute_code ----

func TestDispatchUnknownTool(t *testing.T) {
	s := newServer(t)
	_, err := s.Dispatch(context.Background(), "unknown_tool", json.RawMessage(`{}`))
	if err == nil {
		t.Error("expected error for unknown tool")
	}
}

func TestDispatchNoSteps(t *testing.T) {
	s := newServer(t)
	_, err := callCode(t, s, []map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "at least one step") {
		t.Errorf("expected 'at least one step' error, got %v", err)
	}
}

func TestDispatchDenied(t *testing.T) {
	s := New(t.TempDir()) // no Confirm, not trusted → denied
	_, err := callCode(t, s, []map[string]any{{"code": "echo hi"}})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("expected denied error, got %v", err)
	}
}

func TestDispatchSingleStep(t *testing.T) {
	s := newServer(t)
	out, err := callCode(t, s, []map[string]any{{"code": "echo single"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "single") {
		t.Errorf("got %q, want output containing 'single'", out)
	}
}

func TestDispatchPipeline(t *testing.T) {
	s := newServer(t)
	out, err := callCode(t, s, []map[string]any{
		{"code": "printf 'a\\nb\\nc'"},
		{"code": "grep b"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(out) != "b" {
		t.Errorf("got %q, want %q", out, "b")
	}
}

func TestDispatchParallel(t *testing.T) {
	s := newServer(t)
	out, err := callCode(t, s, []map[string]any{
		{"parallel": []map[string]any{
			{"code": "echo A"},
			{"code": "echo B"},
		}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "A") || !strings.Contains(out, "B") {
		t.Errorf("parallel output missing A or B: %q", out)
	}
}

func TestDispatchToolStep(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OLLIE_TOOLS_PATH", dir)
	if err := os.WriteFile(filepath.Join(dir, "greet"), []byte("#!/bin/bash\necho hello-from-tool"), 0755); err != nil {
		t.Fatal(err)
	}
	s := newServer(t)
	out, err := callCode(t, s, []map[string]any{{"tool": "greet"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "hello-from-tool") {
		t.Errorf("got %q, want output containing 'hello-from-tool'", out)
	}
}

// ---- limitedWriter ----

func TestLimitedWriter(t *testing.T) {
	var buf strings.Builder
	lw := &limitedWriter{w: &buf, limit: 5}
	lw.Write([]byte("hello world"))
	if buf.String() != "hello" {
		t.Errorf("got %q, want %q", buf.String(), "hello")
	}
	if !lw.truncated {
		t.Error("expected truncated=true")
	}
}

// ---- rate limiting ----

func TestRateLimitBlocks(t *testing.T) {
	s := newServer(t)
	// Trigger maxFailures validation failures.
	for i := 0; i < maxFailures; i++ {
		s.recordValidationFailure()
	}
	if err := s.checkRateLimit(); err == nil {
		t.Error("expected rate limit error after max failures")
	}
}

// ---- SetEnv / SetCWD ----

func TestSetEnvInjected(t *testing.T) {
	s := newServer(t)
	s.SetEnv("MY_TEST_VAR", "injected_value")
	out, err := s.Execute(context.Background(), "echo $MY_TEST_VAR", "bash", 10, "default", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(out) != "injected_value" {
		t.Errorf("got %q, want %q", out, "injected_value")
	}
}

func TestSetCWD(t *testing.T) {
	dir := t.TempDir()
	s := newServer(t)
	s.SetCWD(dir)
	out, err := s.Execute(context.Background(), "pwd", "bash", 10, "default", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// TempDir may use symlinks; compare base names.
	if !strings.Contains(out, filepath.Base(dir)) {
		t.Errorf("pwd output %q doesn't contain expected dir %q", out, dir)
	}
}

// ---- Server contract (ListTools / CallTool / Close) ----

func TestServerContract(t *testing.T) {
	s := newServer(t)
	tools, err := s.ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("ListTools returned no tools")
	}
	for _, ti := range tools {
		if ti.Name == "" {
			t.Error("tool has empty Name")
		}
		if ti.Description == "" {
			t.Errorf("tool %q has empty Description", ti.Name)
		}
		if len(ti.InputSchema) == 0 {
			t.Errorf("tool %q has empty InputSchema", ti.Name)
		}
	}
	// CallTool success path
	args, _ := json.Marshal(map[string]any{"steps": []map[string]any{{"code": "echo contract"}}})
	res, err := s.CallTool(context.Background(), "execute_code", args)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !strings.Contains(string(res), "contract") {
		t.Errorf("CallTool result missing expected output: %s", res)
	}
	// CallTool error path (unknown tool)
	res, err = s.CallTool(context.Background(), "bogus", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool error path should marshal, not return error: %v", err)
	}
	if !strings.Contains(string(res), "isError") {
		t.Errorf("CallTool error path should contain isError: %s", res)
	}
}

// ---- Decl factory ----

func TestDecl(t *testing.T) {
	factory := Decl(t.TempDir())
	srv := factory()
	if srv == nil {
		t.Fatal("Decl factory returned nil")
	}
}

// ---- executeElevated (plugin not found) ----

func TestExecuteElevatedNoPlugin(t *testing.T) {
	s := newServer(t)
	t.Setenv("OLLIE_PLUGINS_PATH", t.TempDir()) // empty dir, no elevate script
	_, err := s.executeElevated(context.Background(), "echo hi", t.TempDir(), 10)
	if err == nil || !strings.Contains(err.Error(), "elevation not available") {
		t.Errorf("expected 'elevation not available' error, got %v", err)
	}
}
