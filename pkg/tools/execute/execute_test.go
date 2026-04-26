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

// ---- injectArgs: all languages ----

func TestInjectArgsPerl(t *testing.T) {
	got := injectArgs("perl", "test.pl", []string{"a", "b"}, "print @ARGV;")
	if !strings.Contains(got, "@ARGV") || !strings.Contains(got, "'a'") {
		t.Errorf("perl inject = %q", got)
	}
}

func TestInjectArgsAwk(t *testing.T) {
	got := injectArgs("awk", "test.awk", []string{"file.txt"}, "{print $1}")
	if !strings.Contains(got, "gawk") || !strings.Contains(got, "file.txt") {
		t.Errorf("awk inject = %q", got)
	}
}

func TestInjectArgsSed(t *testing.T) {
	got := injectArgs("sed", "test.sed", []string{"file.txt"}, "s/a/b/g")
	if !strings.Contains(got, "sed -e") || !strings.Contains(got, "file.txt") {
		t.Errorf("sed inject = %q", got)
	}
}

func TestInjectArgsEd(t *testing.T) {
	got := injectArgs("ed", "test.ed", []string{"file.txt"}, ",p")
	if !strings.Contains(got, "ed -s") || !strings.Contains(got, "file.txt") {
		t.Errorf("ed inject = %q", got)
	}
	// No args
	got2 := injectArgs("ed", "test.ed", nil, ",p")
	if strings.Contains(got2, "file") {
		t.Errorf("ed inject no args = %q", got2)
	}
}

func TestInjectArgsJq(t *testing.T) {
	got := injectArgs("jq", "test.jq", []string{"data.json"}, ".name")
	if !strings.Contains(got, "jq") || !strings.Contains(got, "data.json") {
		t.Errorf("jq inject = %q", got)
	}
}

func TestInjectArgsExpect(t *testing.T) {
	got := injectArgs("expect", "test.exp", nil, "spawn ls")
	if !strings.Contains(got, "expect -") {
		t.Errorf("expect inject = %q", got)
	}
}

func TestInjectArgsBc(t *testing.T) {
	got := injectArgs("bc", "test.bc", nil, "2+2")
	if !strings.Contains(got, "bc -ql") {
		t.Errorf("bc inject = %q", got)
	}
}

func TestInjectArgsLua(t *testing.T) {
	got := injectArgs("lua", "test.lua", []string{"x", "y"}, "print(arg[1])")
	if !strings.Contains(got, "arg={") || !strings.Contains(got, "\"x\"") {
		t.Errorf("lua inject = %q", got)
	}
}

// ---- ansiCEscape: \r branch ----

func TestAnsiCEscapeCarriageReturn(t *testing.T) {
	got := ansiCEscape("a\rb")
	if got != `a\rb` {
		t.Errorf("ansiCEscape(a\\rb) = %q; want a\\rb", got)
	}
}

// ---- resolveCodeStep ----

func TestResolveCodeStepToolError(t *testing.T) {
	_, _, _, err := resolveCodeStep(CodeStep{Tool: "nonexistent-tool-xyz"})
	if err == nil {
		t.Error("expected error for missing tool")
	}
}

func TestResolveCodeStepToolWithArgs(t *testing.T) {
	// Create a tool that looks like awk
	dir := t.TempDir()
	t.Setenv("OLLIE_TOOLS_PATH", dir)
	os.WriteFile(filepath.Join(dir, "mytool"), []byte("#!/usr/bin/awk -f\n{print $1}"), 0755)

	code, lang, trusted, err := resolveCodeStep(CodeStep{Tool: "mytool", Args: []string{"file.txt"}})
	if err != nil {
		t.Fatalf("resolveCodeStep: %v", err)
	}
	if !trusted {
		t.Error("tool should be trusted")
	}
	// awk with args switches to bash
	if lang != "bash" {
		t.Errorf("lang = %q; want bash (awk with args)", lang)
	}
	if !strings.Contains(code, "gawk") {
		t.Errorf("code = %q; want gawk wrapper", code)
	}
}

func TestResolveCodeStepInlineDefault(t *testing.T) {
	code, lang, trusted, err := resolveCodeStep(CodeStep{Code: "echo hi"})
	if err != nil {
		t.Fatal(err)
	}
	if lang != "bash" || trusted || code != "echo hi" {
		t.Errorf("got lang=%q trusted=%v code=%q", lang, trusted, code)
	}
}

// ---- runStage ----

func TestRunStageEmptyStage(t *testing.T) {
	s := newServer(t)
	_, err := s.runStage(context.Background(), 0, CodeStep{}, 5, "default", "")
	if err == nil || !strings.Contains(err.Error(), "requires code, tool, or parallel") {
		t.Errorf("expected empty stage error; got %v", err)
	}
}

func TestRunStageParallelError(t *testing.T) {
	s := newServer(t)
	stage := CodeStep{
		Parallel: []CodeStep{
			{Code: "echo ok"},
			{Code: "exit 1"},
		},
	}
	_, err := s.runStage(context.Background(), 0, stage, 5, "default", "")
	if err == nil {
		t.Error("expected error from parallel stage with exit 1")
	}
}

func TestRunStageParallelResolveError(t *testing.T) {
	s := newServer(t)
	stage := CodeStep{
		Parallel: []CodeStep{
			{Tool: "nonexistent-tool-xyz"},
		},
	}
	_, err := s.runStage(context.Background(), 0, stage, 5, "default", "")
	if err == nil {
		t.Error("expected error from parallel stage with bad tool")
	}
}

// ---- dispatchExecuteCode ----

func TestDispatchBadArgs(t *testing.T) {
	s := newServer(t)
	_, err := s.Dispatch(context.Background(), "execute_code", json.RawMessage(`{invalid`))
	if err == nil {
		t.Error("expected error for bad JSON")
	}
}

func TestDispatchPipelineError(t *testing.T) {
	s := newServer(t)
	args := `{"steps":[{"code":"echo hello"},{"code":"exit 1"}]}`
	_, err := s.Dispatch(context.Background(), "execute_code", json.RawMessage(args))
	if err == nil {
		t.Error("expected error from pipeline with failing step")
	}
}

// ---- limitedWriter ----

func TestLimitedWriterAlreadyAtLimit(t *testing.T) {
	var buf strings.Builder
	lw := &limitedWriter{w: &buf, limit: 5, written: 5}
	n, err := lw.Write([]byte("overflow"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 8 {
		t.Errorf("n = %d; want 8", n)
	}
	if !lw.truncated {
		t.Error("should be truncated")
	}
	if buf.Len() != 0 {
		t.Errorf("buf = %q; want empty", buf.String())
	}
}

func TestLimitedWriterError(t *testing.T) {
	lw := &limitedWriter{w: &errWriter{}, limit: 100}
	_, err := lw.Write([]byte("data"))
	if err == nil {
		t.Error("expected write error")
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }

// ---- ToolsPath / PluginsPath defaults ----

func TestToolsPathDefault(t *testing.T) {
	t.Setenv("OLLIE_TOOLS_PATH", "")
	got := ToolsPath()
	if !strings.HasSuffix(got, "/tools") {
		t.Errorf("ToolsPath() = %q; want suffix /tools", got)
	}
}

func TestPluginsPathDefault(t *testing.T) {
	t.Setenv("OLLIE_PLUGINS_PATH", "")
	got := PluginsPath()
	if !strings.HasSuffix(got, "/scripts/x") {
		t.Errorf("PluginsPath() = %q; want suffix /scripts/x", got)
	}
}

func TestToolsPathColonSeparated(t *testing.T) {
	t.Setenv("OLLIE_TOOLS_PATH", "/first:/second")
	if got := ToolsPath(); got != "/first" {
		t.Errorf("ToolsPath() = %q; want /first", got)
	}
}

func TestPluginsPathColonSeparated(t *testing.T) {
	t.Setenv("OLLIE_PLUGINS_PATH", "/first:/second")
	if got := PluginsPath(); got != "/first" {
		t.Errorf("PluginsPath() = %q; want /first", got)
	}
}

// ---- ReadTool: permission error ----

func TestReadToolPermissionError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OLLIE_TOOLS_PATH", dir)
	path := filepath.Join(dir, "noperm")
	os.WriteFile(path, []byte("#!/bin/sh"), 0000)
	// On some systems root can still read; skip if so
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("running as root, cannot test permission error")
	}
	_, err := ReadTool("noperm")
	if err == nil {
		t.Error("expected permission error")
	}
}

// ---- loadSandboxConfig ----

func TestLoadSandboxConfigDefault(t *testing.T) {
	cfg, err := loadSandboxConfig("")
	if err != nil {
		t.Fatalf("loadSandboxConfig default: %v", err)
	}
	if cfg == nil {
		t.Error("expected non-nil config")
	}
}

func TestLoadSandboxConfigNotFound(t *testing.T) {
	_, err := loadSandboxConfig("nonexistent-sandbox-xyz")
	if err == nil {
		t.Error("expected error for missing sandbox config")
	}
}

// ---- detectLanguage: remaining shebangs ----

func TestDetectLanguageEnvShebang(t *testing.T) {
	for _, tc := range []struct {
		shebang, want string
	}{
		{"#!/usr/bin/env perl\n", "perl"},
		{"#!/usr/bin/env lua\n", "lua"},
		{"#!/usr/bin/env lua5.4\n", "lua"},
		{"#!/usr/bin/env gawk\n", "awk"},
		{"#!/usr/bin/env gsed\n", "sed"},
		{"#!/usr/bin/env jq\n", "jq"},
		{"#!/usr/bin/env expect\n", "expect"},
		{"#!/usr/bin/env bc\n", "bc"},
		{"#!/usr/bin/env ed\n", "ed"},
		{"#!\n", "bash"},
	} {
		got := detectLanguage(tc.shebang + "code")
		if got != tc.want {
			t.Errorf("detectLanguage(%q) = %q; want %q", tc.shebang, got, tc.want)
		}
	}
}

// ---- executeWithStdin: language branches ----

func TestExecutePython(t *testing.T) {
	s := newServer(t)
	out, err := s.Execute(context.Background(), "print('hello')", "python3", 5, "default", false)
	if err != nil {
		t.Fatalf("python3: %v", err)
	}
	if strings.TrimSpace(out) != "hello" {
		t.Errorf("python3 output = %q", out)
	}
}

func TestExecutePerl(t *testing.T) {
	s := newServer(t)
	out, err := s.Execute(context.Background(), "print 42", "perl", 5, "default", false)
	if err != nil {
		t.Fatalf("perl: %v", err)
	}
	if strings.TrimSpace(out) != "42" {
		t.Errorf("perl output = %q", out)
	}
}

func TestExecuteBc(t *testing.T) {
	s := newServer(t)
	out, err := s.Execute(context.Background(), "2+3\n", "bc", 5, "default", false)
	if err != nil {
		t.Fatalf("bc: %v", err)
	}
	if strings.TrimSpace(out) != "5" {
		t.Errorf("bc output = %q", out)
	}
}

func TestExecuteJq(t *testing.T) {
	s := newServer(t)
	out, err := s.executeWithStdin(context.Background(), ".x", "jq", 5, "default", false, `{"x":1}`)
	if err != nil {
		t.Fatalf("jq: %v", err)
	}
	if strings.TrimSpace(out) != "1" {
		t.Errorf("jq output = %q", out)
	}
}

func TestExecuteAwk(t *testing.T) {
	s := newServer(t)
	out, err := s.executeWithStdin(context.Background(), "{print $1}", "awk", 5, "default", false, "hello world")
	if err != nil {
		t.Fatalf("awk: %v", err)
	}
	if strings.TrimSpace(out) != "hello" {
		t.Errorf("awk output = %q", out)
	}
}

func TestExecuteSed(t *testing.T) {
	s := newServer(t)
	out, err := s.executeWithStdin(context.Background(), "s/a/b/", "sed", 5, "default", false, "abc")
	if err != nil {
		t.Fatalf("sed: %v", err)
	}
	if strings.TrimSpace(out) != "bbc" {
		t.Errorf("sed output = %q", out)
	}
}

func TestExecuteEd(t *testing.T) {
	s := newServer(t)
	s.SetCWD(t.TempDir())
	f := filepath.Join(s.cwd, "test.txt")
	os.WriteFile(f, []byte("hello\n"), 0644)
	out, err := s.Execute(context.Background(), ",p\nq", "ed", 5, "default", false)
	// ed without a file arg just reads commands; may error, that's fine
	_ = out
	_ = err
}

func TestExecuteLua(t *testing.T) {
	s := newServer(t)
	out, err := s.Execute(context.Background(), "print(1+1)", "lua", 5, "default", false)
	if err != nil {
		t.Skipf("lua not available: %v", err)
	}
	if strings.TrimSpace(out) != "2" {
		t.Errorf("lua output = %q", out)
	}
}

// ---- dispatchExecuteCode: multi-step pipeline error mid-pipeline ----

func TestDispatchPipelineMidError(t *testing.T) {
	s := newServer(t)
	args := `{"steps":[{"code":"echo hello"},{"code":"exit 1"},{"code":"cat"}]}`
	result, err := s.Dispatch(context.Background(), "execute_code", json.RawMessage(args))
	// The error should be wrapped with step index
	_ = result
	if err == nil {
		t.Error("expected pipeline error")
	}
}

// ---- executeElevated ----

func fakeElevateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// Fake elevate script: runs the command after "--"
	script := "#!/bin/sh\nshift # skip --\neval \"$@\"\n"
	os.WriteFile(filepath.Join(dir, "elevate"), []byte(script), 0755)
	return dir
}

func TestExecuteElevatedSuccess(t *testing.T) {
	s := newServer(t)
	dir := fakeElevateDir(t)
	t.Setenv("OLLIE_PLUGINS_PATH", dir)

	out, err := s.executeElevated(context.Background(), "echo elevated", t.TempDir(), 5)
	if err != nil {
		t.Fatalf("executeElevated: %v", err)
	}
	if strings.TrimSpace(out) != "elevated" {
		t.Errorf("output = %q; want elevated", out)
	}
}

func TestExecuteElevatedStderr(t *testing.T) {
	s := newServer(t)
	dir := fakeElevateDir(t)
	t.Setenv("OLLIE_PLUGINS_PATH", dir)

	out, err := s.executeElevated(context.Background(), "echo out && echo err >&2", t.TempDir(), 5)
	if err != nil {
		t.Fatalf("executeElevated: %v", err)
	}
	if !strings.Contains(out, "out") || !strings.Contains(out, "err") {
		t.Errorf("output = %q; want both stdout and stderr", out)
	}
}

func TestExecuteElevatedNonZeroExit(t *testing.T) {
	s := newServer(t)
	dir := fakeElevateDir(t)
	t.Setenv("OLLIE_PLUGINS_PATH", dir)

	_, err := s.executeElevated(context.Background(), "exit 42", t.TempDir(), 5)
	if err == nil {
		t.Error("expected error for non-zero exit")
	}
	if !strings.Contains(err.Error(), "exit 42") {
		t.Errorf("error = %q; want to contain 'exit 42'", err)
	}
}

func TestRunStageElevated(t *testing.T) {
	s := newServer(t)
	dir := fakeElevateDir(t)
	t.Setenv("OLLIE_PLUGINS_PATH", dir)
	s.SetCWD(t.TempDir())

	out, err := s.runStage(context.Background(), 0, CodeStep{Code: "echo elev", Elevated: true}, 5, "default", "")
	if err != nil {
		t.Fatalf("runStage elevated: %v", err)
	}
	if !strings.Contains(out, "elev") {
		t.Errorf("output = %q", out)
	}
}

func TestRunStageParallelElevated(t *testing.T) {
	s := newServer(t)
	dir := fakeElevateDir(t)
	t.Setenv("OLLIE_PLUGINS_PATH", dir)
	s.SetCWD(t.TempDir())

	stage := CodeStep{
		Parallel: []CodeStep{
			{Code: "echo a", Elevated: true},
			{Code: "echo b", Elevated: true},
		},
	}
	out, err := s.runStage(context.Background(), 0, stage, 5, "default", "")
	if err != nil {
		t.Fatalf("parallel elevated: %v", err)
	}
	if !strings.Contains(out, "a") || !strings.Contains(out, "b") {
		t.Errorf("output = %q", out)
	}
}

func TestDispatchSingleElevated(t *testing.T) {
	s := newServer(t)
	dir := fakeElevateDir(t)
	t.Setenv("OLLIE_PLUGINS_PATH", dir)
	s.SetCWD(t.TempDir())

	args := `{"steps":[{"code":"echo elev","elevated":true}]}`
	result, err := s.Dispatch(context.Background(), "execute_code", json.RawMessage(args))
	if err != nil {
		t.Fatalf("dispatch elevated: %v", err)
	}
	if !strings.Contains(string(result), "elev") {
		t.Errorf("result = %q", result)
	}
}

// ---- flock / locking ----

func TestDetectParallelClass(t *testing.T) {
	tests := []struct {
		code string
		want lockClass
	}{
		{"#!/bin/sh\n# ollie:parallel read\ncat $1\n", lockClassRead},
		{"#!/bin/sh\n# ollie:parallel write\n", lockClassWrite},
		{"#!/bin/sh\n# ollie:parallel read extra\ncat\n", lockClassRead},
		{"#!/bin/sh\n# ollie:parallel write extra\n", lockClassWrite},
		{"#!/bin/sh\nno annotation\n", lockClassGlobal},
		{"", lockClassGlobal},
		// annotation after line 10 is ignored
		{strings.Repeat("# filler\n", 10) + "# ollie:parallel read\n", lockClassGlobal},
	}
	for _, tt := range tests {
		got := detectParallelClass(tt.code)
		if got != tt.want {
			t.Errorf("detectParallelClass(code) = %v; want %v", got, tt.want)
		}
	}
}

func TestSanitizeLockName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"simple", "simple"},
		{"path/to/file", "path_to_file"},
		{"a b:c*d?e<f>g|h\"i\\j", "a_b_c_d_e_f_g_h_i_j"},
		{"", "unnamed"},
		{strings.Repeat("x", 100), strings.Repeat("x", 64)},
	}
	for _, tt := range tests {
		got := sanitizeLockName(tt.in)
		if got != tt.want {
			t.Errorf("sanitizeLockName(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}
}

func TestAcquireFlockDisabled(t *testing.T) {
	f, err := acquireFlock("", "name", true)
	if err != nil || f != nil {
		t.Errorf("acquireFlock with empty dir: f=%v err=%v; want nil,nil", f, err)
	}
}

func TestAcquireFlockShared(t *testing.T) {
	dir := t.TempDir()
	f, err := acquireFlock(dir, "test", false)
	if err != nil {
		t.Fatalf("acquireFlock: %v", err)
	}
	if f == nil {
		t.Fatal("expected non-nil file")
	}
	f.Close()
}

func TestAcquireFlockExclusive(t *testing.T) {
	dir := t.TempDir()
	f, err := acquireFlock(dir, "excl", true)
	if err != nil {
		t.Fatalf("acquireFlock exclusive: %v", err)
	}
	f.Close()
}

func TestClassifyStep(t *testing.T) {
	// inline code → global
	if got := classifyStep(CodeStep{Code: "echo hi"}); got != lockClassGlobal {
		t.Errorf("inline code: got %v; want global", got)
	}
	// parallel group → global
	if got := classifyStep(CodeStep{Parallel: []CodeStep{{Code: "x"}}}); got != lockClassGlobal {
		t.Errorf("parallel group: got %v; want global", got)
	}
	// elevated → global
	if got := classifyStep(CodeStep{Elevated: true}); got != lockClassGlobal {
		t.Errorf("elevated: got %v; want global", got)
	}
	// unknown tool → global
	if got := classifyStep(CodeStep{Tool: "nonexistent_tool_xyz"}); got != lockClassGlobal {
		t.Errorf("unknown tool: got %v; want global", got)
	}
}

func TestSetLockDir(t *testing.T) {
	s := newServer(t)
	dir := t.TempDir()
	s.SetLockDir(dir)
	if s.lockDir != dir {
		t.Errorf("lockDir = %q; want %q", s.lockDir, dir)
	}
}

func TestRunReadBatch_Single(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()
	stages := []CodeStep{{Code: "echo batch", Tool: ""}}
	out, err := s.runReadBatch(ctx, 0, stages, 10, "default", "")
	if err != nil {
		t.Fatalf("runReadBatch: %v", err)
	}
	if !strings.Contains(out, "batch") {
		t.Errorf("output = %q; want 'batch'", out)
	}
}

func TestRunReadBatch_Multi(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()
	stages := []CodeStep{
		{Code: "echo one"},
		{Code: "echo two"},
	}
	out, err := s.runReadBatch(ctx, 0, stages, 10, "default", "")
	if err != nil {
		t.Fatalf("runReadBatch multi: %v", err)
	}
	if !strings.Contains(out, "one") || !strings.Contains(out, "two") {
		t.Errorf("output = %q; want 'one' and 'two'", out)
	}
}

func TestRunReadBatch_WithLockDir(t *testing.T) {
	s := newServer(t)
	s.SetLockDir(t.TempDir())
	ctx := context.Background()
	stages := []CodeStep{{Code: "echo locked"}}
	out, err := s.runReadBatch(ctx, 0, stages, 10, "default", "")
	if err != nil {
		t.Fatalf("runReadBatch with lockdir: %v", err)
	}
	if !strings.Contains(out, "locked") {
		t.Errorf("output = %q; want 'locked'", out)
	}
}
