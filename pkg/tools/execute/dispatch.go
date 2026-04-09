package execute

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
		{
			Name:        "file_read",
			Description: "Read a file in full. Output includes line numbers. Use grep/execute_code to search before reading. Prefer file_read only when you need to write — use grep or execute_code for exploration.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["path"],
				"properties": {
					"path": {"type": "string", "description": "Path to the file."}
				}
			}`),
		},
		{
			Name:        "file_write",
			Description: "Write content to a file. For existing files, start_line and end_line are required — whole-file overwrites are not permitted. For new files (not yet on disk), omit start_line/end_line to write the full content. Always use file_read or grep -n to identify the exact line range before writing. Never guess line numbers. Preserve original formatting and indentation.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["path", "content"],
				"properties": {
					"path":       {"type": "string",  "description": "Path to the file."},
					"content":    {"type": "string",  "description": "Content to write."},
					"start_line": {"type": "integer", "description": "First line of range to replace, 1-based."},
					"end_line":   {"type": "integer", "description": "Last line of range to replace, inclusive."}
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
	case "file_read":
		return dispatchFileRead(e, args)
	case "file_write":
		return dispatchFileWrite(e, args)
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

func dispatchFileRead(e *Executor, args json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("file_read: bad args: %w", err)
	}
	if a.Path == "" {
		return "", fmt.Errorf("file_read: 'path' is required")
	}
	if e.Confirm != nil && !e.Confirm(fmt.Sprintf("read %s", a.Path)) {
		return "", fmt.Errorf("file_read: denied by user")
	}
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return "", fmt.Errorf("file_read: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	var out strings.Builder
	for i, line := range lines {
		fmt.Fprintf(&out, "%d\t%s\n", i+1, line)
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

func dispatchFileWrite(e *Executor, args json.RawMessage) (string, error) {
	var a struct {
		Path      string `json:"path"`
		Content   string `json:"content"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("file_write: bad args: %w", err)
	}
	if a.Path == "" {
		return "", fmt.Errorf("file_write: 'path' is required")
	}
	prompt := fmt.Sprintf("write %s", a.Path)
	if a.StartLine > 0 {
		prompt = fmt.Sprintf("write %s lines %d-%d", a.Path, a.StartLine, a.EndLine)
	}
	if e.Confirm != nil && !e.Confirm(prompt) {
		return "", fmt.Errorf("file_write: denied by user")
	}
	if a.StartLine == 0 && a.EndLine == 0 {
		if err := os.WriteFile(a.Path, []byte(a.Content), 0644); err != nil {
			return "", fmt.Errorf("file_write: %w", err)
		}
		return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), a.Path), nil
	}
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return "", fmt.Errorf("file_write: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	start, end := a.StartLine, a.EndLine
	if start < 1 {
		start = 1
	}
	if end > len(lines) {
		end = len(lines)
	}
	if start > end {
		return "", fmt.Errorf("file_write: start_line %d > end_line %d", start, end)
	}
	newLines := strings.Split(a.Content, "\n")
	result := append(lines[:start-1], append(newLines, lines[end:]...)...)
	if err := os.WriteFile(a.Path, []byte(strings.Join(result, "\n")), 0644); err != nil {
		return "", fmt.Errorf("file_write: %w", err)
	}
	return fmt.Sprintf("replaced lines %d-%d in %s", start, end, a.Path), nil
}

func squashWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
