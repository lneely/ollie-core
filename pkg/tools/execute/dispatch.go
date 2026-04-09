package execute

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"ollie/pkg/tools"
)

// builtinToolDefs returns the tool definitions for all built-in tools.
func builtinToolDefs() []tools.ToolInfo {
	tp := ToolsPath()
	return []tools.ToolInfo{
		{
			Name:        "execute_code",
			Description: "Run inline code in a sandboxed environment.",
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
			Name:        "execute_tool",
			Description: "Run a named tool script from " + tp + " in a sandboxed environment. Use this only for named scripts, not for inline shell commands.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["tool"],
				"properties": {
					"tool":     {"type": "string", "description": "Name of the tool script (e.g. discover_skill.sh)."},
					"args":     {"type": "array",  "items": {"type": "string"}, "description": "Arguments for the tool script."},
					"timeout":  {"type": "integer", "description": "Timeout in seconds (default: 30)."},
					"sandbox":  {"type": "string",  "description": "Sandbox name (default: default)."}
				}
			}`),
		},
		{
			Name:        "execute_pipe",
			Description: "Run a pipeline of steps, piping stdout of each into stdin of the next. Use {code: \"cmd --flags\"} for shell commands; use {tool, args} only for named scripts in " + tp + ".",
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
	}
}

// ListTools implements tools.Server.
func (e *Executor) ListTools() ([]tools.ToolInfo, error) {
	return builtinToolDefs(), nil
}

// CallTool implements tools.Server.
func (e *Executor) CallTool(ctx context.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
	result, err := e.dispatch(ctx, tool, args)
	if err != nil {
		return json.Marshal(map[string]string{"error": err.Error()})
	}
	return json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": result}},
	})
}

// Close implements tools.Server (no-op).
func (e *Executor) Close() {}

func (e *Executor) dispatch(ctx context.Context, name string, args json.RawMessage) (string, error) {
	switch name {
	case "execute_pipe":
		return dispatchExecutePipe(ctx, e, args)
	case "execute_tool":
		return dispatchExecuteTool(ctx, e, args)
	case "execute_code":
		return dispatchExecuteCode(ctx, e, args)
	default:
		return "", fmt.Errorf("unknown built-in tool: %s", name)
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

func dispatchExecuteCode(ctx context.Context, e *Executor, args json.RawMessage) (string, error) {
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

func dispatchExecuteTool(ctx context.Context, e *Executor, args json.RawMessage) (string, error) {
	var a struct {
		Tool     string   `json:"tool"`
		Args     []string `json:"args"`
		Language string   `json:"language"`
		Timeout  int      `json:"timeout"`
		Sandbox  string   `json:"sandbox"`
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
	code := toolCode
	if len(a.Args) > 0 {
		var escaped []string
		for _, arg := range a.Args {
			escaped = append(escaped, "'"+strings.ReplaceAll(arg, "'", "'\\''")+"'")
		}
		code = fmt.Sprintf("set -- %s\n%s", strings.Join(escaped, " "), code)
	}
	language := a.Language
	if language == "" {
		language = "bash"
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

func dispatchExecutePipe(ctx context.Context, e *Executor, args json.RawMessage) (string, error) {
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
