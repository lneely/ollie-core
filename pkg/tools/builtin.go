package tools

import "encoding/json"

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
