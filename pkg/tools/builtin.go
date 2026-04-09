// Package tools defines the Executor and Server interfaces, the MCPExecutor
// implementation, and the BuiltinServer that provides the built-in file and
// execute tools.
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"ollie/pkg/tools/execute"
	"ollie/pkg/tools/file"
)

// BuiltinServer implements Server for the built-in execute and file tools.
// Confirm is read from Exec.Confirm for all operations.
type BuiltinServer struct {
	Exec *execute.Executor
}

// ListTools returns definitions for all five built-in tools.
func (b *BuiltinServer) ListTools() ([]ToolInfo, error) {
	return append(ExecuteDefs(execute.ToolsPath()), FileDefs()...), nil
}

// CallTool dispatches a built-in tool call and wraps the result in MCP format.
func (b *BuiltinServer) CallTool(ctx context.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
	result, err := b.dispatch(ctx, tool, args)
	if err != nil {
		return json.Marshal(map[string]string{"error": err.Error()})
	}
	return json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": result}},
	})
}

// Close is a no-op.
func (b *BuiltinServer) Close() {}

func (b *BuiltinServer) dispatch(ctx context.Context, name string, args json.RawMessage) (string, error) {
	switch name {
	case "execute_code", "execute_tool", "execute_pipe":
		return b.Exec.Dispatch(ctx, name, args)
	case "file_read":
		return b.dispatchFileRead(args)
	case "file_write":
		return b.dispatchFileWrite(args)
	default:
		return "", fmt.Errorf("unknown built-in tool: %s", name)
	}
}

func (b *BuiltinServer) confirm(prompt string) bool {
	if b.Exec.Confirm == nil {
		return true
	}
	return b.Exec.Confirm(prompt)
}

func (b *BuiltinServer) dispatchFileRead(args json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("file_read: bad args: %w", err)
	}
	if a.Path == "" {
		return "", fmt.Errorf("file_read: 'path' is required")
	}
	if !b.confirm("read " + a.Path) {
		return "", fmt.Errorf("file_read: denied by user")
	}
	return file.Read(a.Path)
}

func (b *BuiltinServer) dispatchFileWrite(args json.RawMessage) (string, error) {
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
	prompt := "write " + a.Path
	if a.StartLine > 0 {
		prompt = fmt.Sprintf("write %s lines %d-%d", a.Path, a.StartLine, a.EndLine)
	}
	if !b.confirm(prompt) {
		return "", fmt.Errorf("file_write: denied by user")
	}
	return file.Write(a.Path, a.Content, a.StartLine, a.EndLine)
}

// ExecuteDefs returns the ToolInfo definitions for the execute_* built-in tools.
// toolsPath is embedded in the descriptions.
func ExecuteDefs(toolsPath string) []ToolInfo {
	return []ToolInfo{
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
			Description: "Run a named tool script from " + toolsPath + " in a sandboxed environment. Use this only for named scripts, not for inline shell commands.",
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
			Description: "Run a pipeline of steps, piping stdout of each into stdin of the next. Use {code: \"cmd --flags\"} for shell commands; use {tool, args} only for named scripts in " + toolsPath + ".",
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

// FileDefs returns the ToolInfo definitions for the file_* built-in tools.
func FileDefs() []ToolInfo {
	return []ToolInfo{
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

