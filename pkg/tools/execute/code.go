package execute

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"ollie/internal/sandbox"
	"regexp"
	"strings"
)

// execute_code is implemented directly by Executor.Execute with trusted=false.
// See executor.go for the implementation.

var dangerousPatterns = []*regexp.Regexp{
	regexp.MustCompile(`rm\s+(-[a-z]*r[a-z]*\s+)*-[a-z]*f[a-z]*\s*/(home|var|usr|etc|boot|root|bin|sbin|lib|opt|srv)?`),
	regexp.MustCompile(`rm\s+(-[a-z]*f[a-z]*\s+)*-[a-z]*r[a-z]*\s*/(home|var|usr|etc|boot|root|bin|sbin|lib|opt|srv)?`),
	regexp.MustCompile(`rm\s+.*--recursive.*--force`),
	regexp.MustCompile(`rm\s+.*--force.*--recursive`),
	regexp.MustCompile(`rm\s+(-[a-z]*r[a-z]*\s+)*-[a-z]*f[a-z]*\s+\.\.?(/|$)`),
	regexp.MustCompile(`rm\s+(-[a-z]*r[a-z]*\s+)*-[a-z]*f[a-z]*\s+~`),
	regexp.MustCompile(`rm\s+(-[a-z]*r[a-z]*\s+)*-[a-z]*f[a-z]*\s+\*`),
	regexp.MustCompile(`:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&`),
	regexp.MustCompile(`\bmkfs\b`),
	regexp.MustCompile(`\bdd\b.*\bif=/dev/`),
	regexp.MustCompile(`>\s*/dev/sd`),
	regexp.MustCompile(`\beval\s+".*\$`),
	regexp.MustCompile(`\b(sudo|su)\s`),
	regexp.MustCompile(`/etc/(shadow|sudoers)`),
}

// Dispatch routes a named execute tool call. Called by tools.BuiltinServer.
func (e *Server) Dispatch(ctx context.Context, name string, args json.RawMessage) (string, error) {
	switch name {
	case "execute_code":
		return dispatchExecuteCode(ctx, e, args)
	case "execute_tool":
		return dispatchExecuteTool(ctx, e, args)
	case "execute_pipe":
		return dispatchExecutePipe(ctx, e, args)
	default:
		return "", fmt.Errorf("unknown execute tool: %s", name)
	}
}

// ValidateCode checks code against dangerous patterns.
func (e *Server) ValidateCode(code string) error {
	if err := e.checkRateLimit(); err != nil {
		return err
	}

	normalized := strings.ToLower(code)
	normalized = whitespacePattern.ReplaceAllString(normalized, " ")

	for _, pattern := range dangerousPatterns {
		if pattern.MatchString(normalized) {
			e.recordValidationFailure()
			return fmt.Errorf("dangerous pattern detected")
		}
	}
	return nil
}

func loadLayeredConfig(name string) (*sandbox.Config, error) {
	return sandbox.LoadMerged(name)
}

type limitedWriter struct {
	w         io.Writer
	written   int
	limit     int
	truncated bool
}

func (lw *limitedWriter) Write(p []byte) (n int, err error) {
	if lw.written >= lw.limit {
		lw.truncated = true
		return len(p), nil
	}

	remaining := lw.limit - lw.written
	toWrite := p
	if len(p) > remaining {
		toWrite = p[:remaining]
		lw.truncated = true
	}

	written, err := lw.w.Write(toWrite)
	lw.written += written
	if err != nil {
		return written, err
	}
	return len(p), nil
}

func dispatchExecuteCode(ctx context.Context, e *Server, args json.RawMessage) (string, error) {
	code, language, sandbox, timeout, err := execArgs(args)
	if err != nil {
		return "", fmt.Errorf("execute_code: bad args: %w", err)
	}
	if code == "" {
		return "", fmt.Errorf("execute_code: 'code' is required")
	}
	if e.Confirm != nil && !e.Confirm(fmt.Sprintf("execute_code: %s", code)) {
		return "", fmt.Errorf("execute_code: denied by user")
	}
	return e.Execute(ctx, code, language, timeout, sandbox, false)
}

func execArgs(args json.RawMessage) (code, language, sandbox string, timeout int, err error) {
	var a struct {
		Code     string `json:"code"`
		Language string `json:"language"`
		Timeout  int    `json:"timeout"`
		Sandbox  string `json:"sandbox"`
	}
	if err = json.Unmarshal(args, &a); err != nil {
		return
	}
	code = a.Code
	language = a.Language
	if language == "" {
		language = "bash"
	}
	timeout = a.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	sandbox = a.Sandbox
	if sandbox == "" {
		sandbox = "default"
	}
	return
}
