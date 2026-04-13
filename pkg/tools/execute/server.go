package execute

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"syscall"
	"time"

	"ollie/internal/sandbox"
	"ollie/pkg/tools"
)

// Server runs code in a sandboxed environment.
type Server struct {
	// Confirm is an optional function called before executing sensitive operations.
	// If it returns false, the operation is denied.
	Confirm func(string) bool

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
			Description: `Run inline shell code in a sandboxed environment.

Usage:
- Run shell commands: grep, cat, sed, find, etc.
- Default timeout: 30 seconds
- Default language: bash
- Output includes stdout, stderr, and exit code

Security:
- Dangerous commands blocked: rm -rf, fork bombs, sudo, etc.
- File system access limited to sandbox
- Network access restricted

Examples:
- List files: code='ls -la'
- Search content: code='grep -r "TODO" ./src/'
- Count lines: code='wc -l file.txt'`,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["code"],
				"properties": {
					"code":     {"type": "string",  "description": "Code to execute."},
					"language": {"type": "string",  "description": "Language interpreter (default: bash)."},
					"timeout":  {"type": "integer", "description": "Timeout in seconds (default: 30)."},
					"sandbox":  {"type": "string",  "description": "Sandbox name (default: default)."}
				}
			}`),
		},
		{
			Name: "execute_tool",
			Description: `Run a named tool script from the tools directory.

Usage:
- Scripts located in: ` + ToolsPath() + `
- Supported languages: bash, python3 (detected from shebang)
- Use for named scripts, not inline shell commands
- Default timeout: 30 seconds

Tool Discovery:
- List tools: execute_code with 'ls ` + ToolsPath() + `'
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
			Description: `Run a pipeline of commands, chaining stdout to stdin.

Usage:
- Pipe output between multiple commands
- Each step: {code: "cmd"} or {tool: "name", args: [...]}
- Default timeout: 30 seconds per pipeline
- Steps execute sequentially

Structure:
- pipe: array of step objects
- Step with code: shell command string
- Step with tool: named script from tools directory
- Use code for shell commands, tool for scripts

Examples:
- Filter and count: pipe=[{code: "grep error log.txt"}, {code: "wc -l"}]
- Process with script: pipe=[{tool: "parse.py", args: ["data.json"]}, {code: "jq .result"}]`,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["pipe"],
				"properties": {
					"pipe": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"tool": {"type": "string"},
								"args": {"type": "array", "items": {"type": "string"}},
								"code": {"type": "string"}
							}
						}
					},
					"timeout": {"type": "integer", "description": "Timeout in seconds (default: 30)."},
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
		return json.Marshal(map[string]string{"error": err.Error()})
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

// SetEnv adds a per-session environment variable to all subsequent commands.
func (e *Server) SetEnv(key, value string) {
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
	if timeout <= 0 {
		timeout = 30
	}

	if !trusted {
		if err := e.ValidateCode(code); err != nil {
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
	switch language {
	case "bash", "":
		interpreter = []string{"bash", "-c", code}
	case "python3", "python":
		interpreter = []string{"python3", "-c", code}
	default:
		return "", fmt.Errorf("unsupported language: %s (supported: bash, python3)", language)
	}
	wrapped := sandbox.WrapCommand(cfg, interpreter, workDir)
	cmd = exec.CommandContext(ctx, wrapped[0], wrapped[1:]...)
	cmd.Dir = workDir

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
