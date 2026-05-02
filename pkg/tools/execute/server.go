package execute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"ollie/internal/sandbox"
	"ollie/pkg/paths"
	"ollie/pkg/tools"
)

// Server runs code in a sandboxed environment.
type Server struct {
	// Confirm is an optional function called before executing sensitive operations.
	// Returns true to allow, false to deny. Trusted tools bypass this check.
	Confirm func(string) bool

	// trusted is the set of tool names that bypass Confirm.
	trustedMu sync.RWMutex
	trusted   map[string]bool

	// cwd is the working directory for sandboxed commands. If empty,
	// the process working directory is used.
	wdMu sync.RWMutex
	cwd  string

	// envExtra holds per-session environment variables injected via SetEnv.
	envMu    sync.RWMutex
	envExtra map[string]string

	// lockDir is the directory for advisory flock files used during parallel
	// step dispatch. Set once at session init via SetLockDir; empty disables locking.
	lockDir string

	// rate limiting state (per-Server)
	rateLimitMu        sync.Mutex
	validationFailures int
	lastFailure        time.Time
	blockedUntil       time.Time
}

// Decl returns a factory for an execute Server with the given working directory.
func Decl(cwd string) func() tools.Server {
	return func() tools.Server {
		return New(cwd)
	}
}

// ListTools implements tools.Server, returning the ToolInfo definitions for the execute_* built-in tools.
func (e *Server) ListTools() ([]tools.ToolInfo, error) {
	return []tools.ToolInfo{
		{
			Name: "execute_code",
			Description: `Run a pipeline of one or more stages in a sandboxed environment.

Stages run sequentially; each stage's stdout is fed as stdin to the next.
A single stage with inline code or a tool script is the degenerate (non-pipeline) case
and returns raw output. A parallel stage fans out concurrently and concatenates results
in submission order before passing them to the next stage.

Each stage is one of:
- {code, language}            — inline code (default language: bash)
- {tool, args}                — named script from OLLIE_TOOLS_PATH (language from shebang)
- {parallel: [{code/tool}...]}— concurrent fan-out; outputs concatenated in submission order

Supported inline languages: bash, python3, perl, lua, awk, sed, jq, ed, expect, bc.
Timeout is a top-level parameter (not per-step) applied to each stage (default: 30s). A failed stage aborts the pipeline.

Examples:
- Single step:        steps=[{code: "ls -la"}]
- Pipeline:           steps=[{code: "grep error app.log"}, {code: "wc -l"}]
- Parallel fan-out:   steps=[{parallel: [{code: "wc -l a.txt"}, {code: "wc -l b.txt"}]}]
- Fan-out then sort:  steps=[{parallel: [{code: "cat a.txt"}, {code: "cat b.txt"}]}, {code: "sort"}]
- Tool in pipeline:   steps=[{tool: "fetch.sh", args: ["--last=1h"]}, {code: "jq .result"}]`,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["steps"],
				"properties": {
					"steps": {
						"type": "array",
						"description": "Pipeline stages. Run sequentially, stdout piped to next stage's stdin. Each stage is inline code, a named tool script, or a parallel fan-out.",
						"items": {
							"type": "object",
							"properties": {
								"code":     {"type": "string", "description": "Inline code to execute."},
								"language": {"type": "string", "description": "Language interpreter (default: bash). Ignored when tool or parallel is set."},
								"tool":     {"type": "string", "description": "Named tool script from the tools directory. Discover available tools: grep -iA2 'keyword' $OLLIE/t/idx"},
								"args":     {"type": "array", "items": {"type": "string"}, "description": "Arguments for the tool script."},
								"elevated": {"type": "boolean", "description": "Run this step outside the sandbox via the elevation backend. Only bash code is supported. Omit or false for normal sandboxed execution."},
								"parallel": {
									"type": "array",
									"description": "Fan-out: steps run concurrently, outputs concatenated in submission order, result fed to next stage. All parallel steps should produce the same output schema so the concatenated result is coherent.",
									"items": {
										"type": "object",
										"properties": {
											"code":     {"type": "string"},
											"language": {"type": "string"},
											"tool":     {"type": "string"},
											"args":     {"type": "array", "items": {"type": "string"}},
											"elevated": {"type": "boolean"}
										}
									}
								}
							}
						}
					},
					"timeout": {"type": "integer", "description": "Timeout in seconds applied to each stage (default: 30). Top-level parameter, not per-step."},
					"sandbox": {"type": "string",  "description": "Sandbox name (default: default)."}
				}
			}`),
		},
	}, nil
}

// CallTool implements tools.Server.
func (e *Server) CallTool(ctx context.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
	result, err := e.Dispatch(ctx, tool, args)
	if err != nil {
		return json.Marshal(map[string]any{
			"isError": true,
			"content": []map[string]string{{"type": "text", "text": err.Error()}},
		})
	}
	return json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": result}},
	})
}

// New creates a new Server with the given working directory.
func New(cwd string) *Server { return &Server{cwd: paths.ExpandHome(cwd)} }

// SetCWD updates the working directory used for subsequent command executions.
func (e *Server) SetCWD(dir string) {
	e.wdMu.Lock()
	e.cwd = paths.ExpandHome(dir)
	e.wdMu.Unlock()
}

// SetTrustedTools marks the given tool names as trusted; they bypass Confirm.
func (e *Server) SetTrustedTools(tools []string) {
	e.trustedMu.Lock()
	e.trusted = make(map[string]bool, len(tools))
	for _, t := range tools {
		e.trusted[t] = true
	}
	e.trustedMu.Unlock()
}

// allowed returns true if the tool call should proceed.
// Trusted tools are always allowed. Untrusted tools are passed to Confirm;
// if Confirm is nil, untrusted tools are denied.
func (e *Server) allowed(tool, detail string) bool {
	e.trustedMu.RLock()
	trusted := e.trusted[tool]
	e.trustedMu.RUnlock()
	if trusted {
		return true
	}
	if e.Confirm == nil {
		return false // no confirm fn and not trusted: deny by default
	}
	return e.Confirm(detail)
}

// SetLockDir sets the directory used for advisory flock files during parallel
// step dispatch. Must be called before the Server handles concurrent requests.
func (e *Server) SetLockDir(dir string) { e.lockDir = dir }

// SetEnv adds a session-scoped environment variable injected into all
// subsequent subprocess invocations for this session.
func (e *Server) SetEnv(key, value string) {
	e.envMu.Lock()
	if e.envExtra == nil {
		e.envExtra = make(map[string]string)
	}
	e.envExtra[key] = value
	e.envMu.Unlock()
}

// executeElevated runs cmd outside the sandbox via x/elevate.
// Returns (output, error); a non-zero exit code is treated as an error.
func (e *Server) executeElevated(ctx context.Context, cmd, dir string, timeout int) (string, error) {
	script := filepath.Join(PluginsPath(), "elevate")
	if _, err := os.Stat(script); err != nil {
		return "", fmt.Errorf("elevation not available: x/elevate not found")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	c := exec.CommandContext(ctx, script, "--", cmd)
	c.Dir = dir
	var outBuf, errBuf bytes.Buffer
	lw := &limitedWriter{w: &outBuf, limit: 10 * 1024 * 1024}
	c.Stdout = lw
	c.Stderr = &errBuf
	err := c.Run()
	combined := outBuf.String()
	if errBuf.Len() > 0 {
		combined += errBuf.String()
	}
	if lw.truncated {
		combined += "\n[output truncated at 10MB]"
	}
	if ctx.Err() == context.DeadlineExceeded {
		return combined, fmt.Errorf("execution timeout after %d seconds", timeout)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return combined, fmt.Errorf("elevated execution failed (exit %d)\nOutput: %s", exitErr.ExitCode(), combined)
		}
		return combined, fmt.Errorf("elevated execution failed: %v", err)
	}
	return combined, nil
}

var whitespacePattern = regexp.MustCompile(`\s+`)

const (
	maxFailures   = 5
	blockDuration = 30 * time.Second
	failureWindow = 60 * time.Second
)

func (e *Server) checkRateLimit() error {
	e.rateLimitMu.Lock()
	defer e.rateLimitMu.Unlock()

	now := time.Now()
	if now.Before(e.blockedUntil) {
		remaining := e.blockedUntil.Sub(now).Round(time.Second)
		return fmt.Errorf("rate limited: too many validation failures, blocked for %v", remaining)
	}
	return nil
}

func (e *Server) recordValidationFailure() {
	e.rateLimitMu.Lock()
	defer e.rateLimitMu.Unlock()

	now := time.Now()
	if now.Sub(e.lastFailure) > failureWindow {
		e.validationFailures = 0
	}

	e.validationFailures++
	e.lastFailure = now

	if e.validationFailures >= maxFailures {
		e.blockedUntil = now.Add(blockDuration)
		e.validationFailures = 0
	}
}

// Execute runs code in a sandbox and returns combined stdout+stderr.
func (e *Server) Execute(ctx context.Context, code, language string, timeout int, sandboxName string, trusted bool) (string, error) {
	return e.executeWithStdin(ctx, code, language, timeout, sandboxName, trusted, "")
}

// executeWithStdin is like Execute but feeds stdinData to the command's stdin.
// For languages where code is itself passed via stdin (ed, expect, bc), stdinData is ignored.
func (e *Server) executeWithStdin(ctx context.Context, code, language string, timeout int, sandboxName string, trusted bool, stdinData string) (string, error) {
	if timeout <= 0 {
		timeout = 30
	}

	// Domain-specific tools (awk, sed, jq, ed, bc, expect) skip validation —
	// they are constrained by design and the sandbox handles OS-level restrictions.
	// General-purpose languages get universal + language-specific pattern checks.
	_, isDomainSpecific := map[string]struct{}{
		"awk": {}, "sed": {}, "jq": {}, "ed": {}, "bc": {}, "expect": {},
	}[language]
	if !trusted && !isDomainSpecific {
		if err := e.ValidateCode(code, language); err != nil {
			return "", err
		}
	}

	cfg, err := loadSandboxConfig(sandboxName)
	if err != nil {
		return "", err
	}

	e.wdMu.RLock()
	workDir := e.cwd
	e.wdMu.RUnlock()
	if workDir == "" {
		workDir, _ = os.Getwd()
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	var interpreter []string
	// codeStdin: non-empty means the code itself is fed via stdin (ed, expect, bc).
	// In these cases stdinData cannot be used simultaneously.
	var codeStdin string
	switch language {
	case "bash", "":
		interpreter = []string{"bash", "-c", code}
	case "python3", "python":
		interpreter = []string{"python3", "-c", code}
	case "perl":
		interpreter = []string{"perl", "-e", code}
	case "lua":
		interpreter = []string{"lua", "-e", code}
	case "awk":
		interpreter = []string{"gawk", code}
	case "sed":
		interpreter = []string{"sed", "-e", code}
	case "jq":
		interpreter = []string{"jq", code}
	case "ed":
		interpreter = []string{"ed", "-s"}
		codeStdin = code
	case "expect":
		interpreter = []string{"expect", "-"}
		codeStdin = code
	case "bc":
		interpreter = []string{"bc", "-l"}
		codeStdin = code
	default:
		return "", fmt.Errorf("unsupported language: %s (supported: bash, python3, perl, awk, sed, ed, jq, expect, bc, lua)", language)
	}
	wrapped := sandbox.WrapCommand(cfg, interpreter, workDir)
	cmd = exec.CommandContext(ctx, wrapped[0], wrapped[1:]...)
	cmd.Dir = workDir
	switch {
	case codeStdin != "":
		cmd.Stdin = strings.NewReader(codeStdin)
	case stdinData != "":
		cmd.Stdin = strings.NewReader(stdinData)
	}

	e.envMu.RLock()
	cmd.Env = prependOlliePath(os.Environ())
	cmd.Env = append(cmd.Env, "OLLIE_TOOLS_PATH="+ToolsPath())
	for k, v := range e.envExtra {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	e.envMu.RUnlock()

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}

	var outputBuf bytes.Buffer
	lw := &limitedWriter{w: &outputBuf, limit: 10 * 1024 * 1024}
	cmd.Stdout = lw
	cmd.Stderr = lw

	err = cmd.Run()
	output := outputBuf.Bytes()
	if lw.truncated {
		output = append(output, []byte("\n[output truncated at 10MB]")...)
	}

	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("execution timeout after %d seconds", timeout)
	}
	if err != nil {
		return string(output), fmt.Errorf("execution failed: %v\nOutput: %s", err, string(output))
	}
	return string(output), nil
}

// prependOlliePath returns env with $OLLIE/x/ prepended to PATH so wrapper
// scripts placed there (e.g. ~/.config/ollie/scripts/x/) shadow system binaries.
func prependOlliePath(env []string) []string {
	ollie := os.Getenv("OLLIE")
	if ollie == "" {
		return env
	}
	xdir := filepath.Join(ollie, "x")
	result := make([]string, 0, len(env))
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			e = "PATH=" + xdir + string(filepath.ListSeparator) + e[5:]
		}
		result = append(result, e)
	}
	return result
}

var _ tools.Server = (*Server)(nil) // compile-time interface check
