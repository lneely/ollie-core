package execute

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

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

func dispatchExecuteCode(ctx context.Context, e *Server, args json.RawMessage) (string, error) {
	code, language, sandbox, timeout, err := execArgs(args)
	if err != nil {
		return "", fmt.Errorf("execute_code: bad args: %w", err)
	}
	if code == "" {
		return "", fmt.Errorf("execute_code: 'code' is required")
	}
	if e.Confirm != nil && !e.Confirm(fmt.Sprintf("execute_code: %s", squashWhitespace(code))) {
		return "", fmt.Errorf("execute_code: denied by user")
	}
	return e.Execute(ctx, code, language, timeout, sandbox, false)
}

func dispatchExecuteTool(ctx context.Context, e *Server, args json.RawMessage) (string, error) {
	var a struct {
		Tool    string   `json:"tool"`
		Args    []string `json:"args"`
		Timeout int      `json:"timeout"`
		Sandbox string   `json:"sandbox"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("execute_tool: bad args: %w", err)
	}
	if a.Tool == "" {
		return "", fmt.Errorf("execute_tool: 'tool' is required")
	}
	if e.Confirm != nil && !e.Confirm(fmt.Sprintf("execute_tool: %s %s", a.Tool, strings.Join(a.Args, " "))) {
		return "", fmt.Errorf("execute_tool: denied by user")
	}
	toolCode, err := ReadTool(a.Tool)
	if err != nil {
		return "", err
	}
	language := detectLanguage(toolCode)
	code := toolCode
	if len(a.Args) > 0 {
		code = injectArgs(language, a.Tool, a.Args, toolCode)
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	sandbox := a.Sandbox
	if sandbox == "" {
		sandbox = "default"
	}
	return e.Execute(ctx, code, language, timeout, sandbox, true)
}

func dispatchExecutePipe(ctx context.Context, e *Server, args json.RawMessage) (string, error) {
	var a struct {
		Pipe    []PipeStep `json:"pipe"`
		Timeout int        `json:"timeout"`
		Sandbox string     `json:"sandbox"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("execute_pipe: bad args: %w", err)
	}
	if len(a.Pipe) == 0 {
		return "", fmt.Errorf("execute_pipe: 'pipe' is required")
	}
	code, _, err := BuildPipeline(a.Pipe)
	if err != nil {
		return "", err
	}
	if e.Confirm != nil && !e.Confirm(fmt.Sprintf("execute_pipe: %s", squashWhitespace(code))) {
		return "", fmt.Errorf("execute_pipe: denied by user")
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	sandbox := a.Sandbox
	if sandbox == "" {
		sandbox = "default"
	}
	return e.Execute(ctx, code, "bash", timeout, sandbox, true)
}

func squashWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
