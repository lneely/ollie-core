package execute

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"ollie/internal/sandbox"
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
	wdMu    sync.RWMutex
	cwd string

	// envExtra holds per-session environment variables injected via SetEnv.
	envMu    sync.RWMutex
	envExtra map[string]string

	// rate limiting state (per-Server)
	rateLimitMu        sync.Mutex
	validationFailures int
	lastFailure        time.Time
	blockedUntil       time.Time
}

// Decl returns a factory for an execute Server with the given working directory.
func Decl(cwd string) func() tools.Server {
	return func() tools.Server { return New(cwd) }
}

// ListTools implements tools.Server, returning the ToolInfo definitions for the execute_* built-in tools.
func (e *Server) ListTools() ([]tools.ToolInfo, error) {
	return []tools.ToolInfo{
		{
			Name: "execute_code",
			Description: `Run one or more code snippets in a sandboxed environment.

Steps run in parallel when more than one is provided; results are returned in
submission order under === step N === headers. Single-step calls return raw output.

Usage:
- Default language: bash. Supported: bash, python3, perl, lua, awk, sed, jq, ed, expect, bc.
- Default timeout: 30 seconds (applied per step).
- Output includes stdout, stderr, and exit code.

Security:
- Dangerous commands blocked: rm -rf, fork bombs, sudo, etc.
- File system access limited to sandbox.
- Network access restricted.

Examples:
- Single step:  steps=[{code: "ls -la"}]
- Parallel:     steps=[{code: "wc -l a.txt"}, {code: "wc -l b.txt"}]
- Mixed langs:  steps=[{code: "...", language: "python3"}, {code: "...", language: "bash"}]`,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["steps"],
				"properties": {
					"steps": {
						"type": "array",
						"description": "One or more steps. Multiple steps run in parallel. Each step is either inline code or a named tool.",
						"items": {
							"type": "object",
							"properties": {
								"code":     {"type": "string", "description": "Inline code to execute."},
								"language": {"type": "string", "description": "Language interpreter (default: bash). Ignored when tool is set."},
								"tool":     {"type": "string", "description": "Named tool script from the tools directory. Takes precedence over code."},
								"args":     {"type": "array", "items": {"type": "string"}, "description": "Arguments for the tool script."}
							}
						}
					},
					"timeout":  {"type": "integer", "description": "Timeout in seconds per step (default: 30)."},
					"sandbox":  {"type": "string",  "description": "Sandbox name (default: default)."}
				}
			}`),
		},
		{
			Name: "execute_tool",
			Description: `Run a named tool script from the tools directory.

Usage:
- Scripts located in: ollie/t (default: $HOME/mnt/ollie/t)
- Supported languages: bash, python3, perl, awk, sed, ed, jq, expect, bc, lua (detected from shebang)
- Use for named scripts, not inline shell commands
- Default timeout: 30 seconds

Tool Discovery:
- List tools: execute_code with 'ls ollie/t'
- Check script permissions before execution

Examples:
- Run bash tool: tool='script.sh', args=['arg1', 'arg2']
- Run python tool: tool='process.py', args=['--input', 'data.txt']`,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["tool"],
				"properties": {
					"tool":     {"type": "string", "description": "Name of the tool script in the tools directory."},
					"args":     {"type": "array",  "items": {"type": "string"}, "description": "Arguments for the tool script."},
					"timeout":  {"type": "integer", "description": "Timeout in seconds (default: 30)."},
					"sandbox":  {"type": "string",  "description": "Sandbox name (default: default)."}
				}
			}`),
		},
		{
			Name: "execute_pipe",
			Description: `Run a sequential pipeline, chaining each stage's stdout to the next stage's stdin.

Each stage is one of:
- {code: "..."}               — inline bash
- {tool: "name", args: [...]} — named script from the tools directory
- {parallel: [{code,language}, ...]} — fan-out: N steps run concurrently, outputs
                                        concatenated in submission order, result fed to next stage

Use parallel stages when N independent operations produce the same output schema and
their combined output should flow into the next stage as a single stream. For disparate
schemas, normalize with an inner pipe stage before merging.

Timeout applies per stage. Stages execute sequentially; a failed stage aborts the pipeline.

Examples:
- Filter and count:    pipe=[{code: "grep error log.txt"}, {code: "wc -l"}]
- Parallel then sort:  pipe=[{parallel: [{code:"cat a.txt"},{code:"cat b.txt"}]}, {code:"sort"}]
- Tool transform:      pipe=[{tool: "parse.py", args: ["data.json"]}, {code: "jq .result"}]`,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["pipe"],
				"properties": {
					"pipe": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"tool":     {"type": "string"},
								"args":     {"type": "array", "items": {"type": "string"}},
								"code":     {"type": "string"},
								"parallel": {
									"type": "array",
									"description": "Steps to run in parallel; outputs concatenated in submission order. Each step is inline code or a named tool.",
									"items": {
										"type": "object",
										"properties": {
											"code":     {"type": "string", "description": "Inline code to execute."},
											"language": {"type": "string", "description": "Interpreter for inline code (default: bash). Ignored when tool is set."},
											"tool":     {"type": "string", "description": "Named tool script; language detected from shebang."},
											"args":     {"type": "array", "items": {"type": "string"}, "description": "Arguments for the tool script."}
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
func New(cwd string) *Server { return &Server{cwd: cwd} }

// SetCWD updates the working directory used for subsequent command executions.
func (e *Server) SetCWD(dir string) {
	e.wdMu.Lock()
	e.cwd = dir
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

// SetEnv adds a per-session environment variable to all subsequent commands.
// It also exports the variable to the process environment so it is inherited
// by sandboxed subprocesses that read os.Environ().
func (e *Server) SetEnv(key, value string) {
	os.Setenv(key, value) //nolint:errcheck
	e.envMu.Lock()
	if e.envExtra == nil {
		e.envExtra = make(map[string]string)
	}
	e.envExtra[key] = value
	e.envMu.Unlock()
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

	cfg, err := loadLayeredConfig(sandboxName)
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
		interpreter = []string{"bc", "-ql"}
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

	// Inject per-session env vars so they're visible inside the sandbox.
	e.envMu.RLock()
	if len(e.envExtra) > 0 {
		cmd.Env = os.Environ()
		for k, v := range e.envExtra {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
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

// Close implements tools.Server (no-op).
func (e *Server) Close() {}

var _ tools.Server = (*Server)(nil) // compile-time interface check
