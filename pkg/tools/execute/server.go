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

const (
	failureWindow = 1 * time.Minute
	maxFailures   = 5
	blockDuration = 5 * time.Minute
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

	// Strict rejects inline {code} steps; only {tool} steps are allowed.
	Strict bool

	// Yolo skips the landrun sandbox for all execution.
	Yolo bool

	// allowExecutors restricts which executors (execute_code, call_tool, pipe)
	// are available. Empty means all are allowed.
	allowExecutors map[string]bool

	// allowTools restricts which named tool scripts can be invoked via call_tool/pipe.
	// Empty means all are allowed.
	allowTools map[string]bool

	// rate limiting state (per-Server)
	rateLimitMu        sync.Mutex
	validationFailures int
	lastFailure        time.Time
	blockedUntil       time.Time
}

// Option configures a Server.
type Option func(*Server)

// WithStrict rejects inline {code} steps; only {tool} steps are allowed.
func WithStrict() Option { return func(s *Server) { s.Strict = true } }

// WithYolo skips the landrun sandbox.
func WithYolo() Option { return func(s *Server) { s.Yolo = true } }

// WithAllowExecutors restricts which executors are available.
func WithAllowExecutors(names []string) Option {
	return func(s *Server) {
		if len(names) > 0 {
			s.allowExecutors = make(map[string]bool, len(names))
			for _, n := range names {
				s.allowExecutors[n] = true
			}
		}
	}
}

// WithAllowTools restricts which tool scripts can be invoked.
func WithAllowTools(names []string) Option {
	return func(s *Server) {
		if len(names) > 0 {
			s.allowTools = make(map[string]bool, len(names))
			for _, n := range names {
				s.allowTools[n] = true
			}
		}
	}
}

// AllowTools returns the set of allowed tool names, or nil if unrestricted.
func (e *Server) AllowTools() []string {
	if len(e.allowTools) == 0 {
		return nil
	}
	out := make([]string, 0, len(e.allowTools))
	for k := range e.allowTools {
		out = append(out, k)
	}
	return out
}

// Decl returns a factory for an execute Server with the given working directory.
func Decl(cwd string, opts ...Option) func() tools.Server {
	return func() tools.Server {
		s := New(cwd)
		for _, o := range opts {
			o(s)
		}
		return s
	}
}

// ListTools implements tools.Server, returning ToolInfo for execute_code,
// call_tool, and pipe.
func (e *Server) ListTools() ([]tools.ToolInfo, error) {
	all := []tools.ToolInfo{
		{
			Name: "execute_code",
			Description: `Run one or more inline code steps in a sandboxed environment.

Steps run in parallel when consecutive steps carry an ollie:parallel annotation;
otherwise they run serially. Outputs are concatenated in submission order.
No stdout chaining between steps — use pipe for that.

Each step is one of:
- {code, language}              — inline code (default language: bash)
- {elevated: true, code}        — run outside sandbox via elevation backend (bash only)
- {parallel: [{code/language}...]} — concurrent fan-out; outputs concatenated in submission order

Supported languages: bash, python3, perl, lua, awk, sed, jq, ed, expect, bc.
timeout applies to each step independently (default: 30s). A failed step aborts.

Examples:
- Single step:   steps=[{code: "date"}]
- Two steps:     steps=[{code: "echo hello"}, {code: "echo world"}]
- Fan-out:       steps=[{parallel: [{code: "cat a.txt"}, {code: "cat b.txt"}]}]`,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["steps"],
				"properties": {
					"steps": {
						"type": "array",
						"description": "Inline code steps. Run in parallel when safe (ollie:parallel annotation), serially otherwise.",
						"items": {
							"type": "object",
							"properties": {
								"code":     {"type": "string", "description": "Inline code to execute."},
								"language": {"type": "string", "description": "Language interpreter (default: bash)."},
								"elevated": {"type": "boolean", "description": "Run outside the sandbox via the elevation backend. Only bash is supported."},
								"parallel": {
									"type": "array",
									"description": "Fan-out: steps run concurrently, outputs concatenated in submission order.",
									"items": {
										"type": "object",
										"properties": {
											"code":     {"type": "string"},
											"language": {"type": "string"},
											"elevated": {"type": "boolean"}
										}
									}
								}
							}
						}
					},
					"timeout": {"type": "integer", "description": "Timeout in seconds per step (default: 30)."},
					"sandbox": {"type": "string",  "description": "Sandbox name (default: default)."}
				}
			}`),
		},
		{
			Name: "call_tool",
			Description: `Run one or more named scripts from $OLLIE/s/$SID/t (the tools directory).

Tool names and their arguments are discoverable via:
  grep -iA2 'keyword' $OLLIE/s/$SID/t/idx

call_tool fans out in parallel when consecutive calls carry an ollie:parallel read
annotation in their script header; write-annotated or unannotated tools run serially.
Outputs are concatenated in submission order.
No stdout chaining between calls — use pipe for that.

Each call is one of:
- {tool, args}                   — named script with arguments
- {tool, args, elevated: true}   — run outside sandbox (bash tools only)
- {parallel: [{tool, args}...]}  — explicit concurrent fan-out

Examples:
- Single call:    calls=[{tool: "file_read", args: ["README.md"]}]
- Parallel reads: calls=[{tool: "file_read", args: ["a.txt"]}, {tool: "file_read", args: ["b.txt"]}]`,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["calls"],
				"properties": {
					"calls": {
						"type": "array",
						"description": "Tool calls. Fanned out in parallel when safe (ollie:parallel annotation), serially otherwise.",
						"items": {
							"type": "object",
							"properties": {
								"tool":     {"type": "string", "description": "Named tool script from the tools directory."},
								"args":     {"type": "array", "items": {"type": "string"}, "description": "Arguments for the tool script."},
								"elevated": {"type": "boolean", "description": "Run outside the sandbox via the elevation backend."},
								"parallel": {
									"type": "array",
									"description": "Explicit fan-out: calls run concurrently, outputs concatenated in submission order.",
									"items": {
										"type": "object",
										"properties": {
											"tool":     {"type": "string"},
											"args":     {"type": "array", "items": {"type": "string"}},
											"elevated": {"type": "boolean"}
										}
									}
								}
							}
						}
					},
					"timeout": {"type": "integer", "description": "Timeout in seconds per call (default: 30)."},
					"sandbox": {"type": "string",  "description": "Sandbox name (default: default)."}
				}
			}`),
		},
		{
			Name: "pipe",
			Description: `Compose execute_code and call_tool steps into a sequential pipeline.

Stages run in order; each stage's stdout becomes the next stage's stdin.
Use this when you need to chain heterogeneous code and tool steps together.

Each stage is one of:
- {code, language}              — inline code (default: bash)
- {tool, args}                  — named script from $OLLIE/s/$SID/t
- {elevated: true, code/tool}   — run outside sandbox
- {parallel: [{code/tool}...]}  — fan-out within a stage; outputs concatenated, fed to next stage

Supported inline languages: bash, python3, perl, lua, awk, sed, jq, ed, expect, bc.
timeout applies to each stage independently (default: 30s). A failed stage aborts.

Examples:
- Code → code:    stages=[{code: "grep error app.log"}, {code: "wc -l"}]
- Tool → code:    stages=[{tool: "fetch.sh", args: ["--last=1h"]}, {code: "jq .result"}]
- Fan-out → code: stages=[{parallel: [{code: "cat a.txt"}, {code: "cat b.txt"}]}, {code: "sort"}]`,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["stages"],
				"properties": {
					"stages": {
						"type": "array",
						"description": "Pipeline stages. Each stage's stdout feeds the next stage's stdin.",
						"items": {
							"type": "object",
							"properties": {
								"code":     {"type": "string", "description": "Inline code to execute."},
								"language": {"type": "string", "description": "Language interpreter (default: bash). Ignored when tool or parallel is set."},
								"tool":     {"type": "string", "description": "Named tool script from the tools directory."},
								"args":     {"type": "array", "items": {"type": "string"}, "description": "Arguments for the tool script."},
								"elevated": {"type": "boolean", "description": "Run outside the sandbox via the elevation backend."},
								"parallel": {
									"type": "array",
									"description": "Fan-out within a stage: steps run concurrently, outputs concatenated, then fed to next stage.",
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
					"timeout": {"type": "integer", "description": "Timeout in seconds per stage (default: 30)."},
					"sandbox": {"type": "string",  "description": "Sandbox name (default: default)."}
				}
			}`),
		},
	}
	if len(e.allowExecutors) > 0 {
		filtered := all[:0]
		for _, t := range all {
			if e.allowExecutors[t.Name] {
				filtered = append(filtered, t)
			}
		}
		return filtered, nil
	}
	return all, nil
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
	if e.Yolo {
		cmd = exec.CommandContext(ctx, interpreter[0], interpreter[1:]...)
	} else {
		wrapped, wrapErr := sandbox.WrapCommand(cfg, interpreter, workDir)
		if wrapErr != nil {
			return "", wrapErr
		}
		cmd = exec.CommandContext(ctx, wrapped[0], wrapped[1:]...)
	}
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
