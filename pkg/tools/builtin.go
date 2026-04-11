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
					"tool":     {"type": "string", "description": "Name of the tool script in the tools directory."},
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

// ReasoningDefs returns the ToolInfo definitions for the reasoning_* built-in tools.
func ReasoningDefs() []ToolInfo {
	return []ToolInfo{
		{
			Name:        "reasoning_think",
			Description: "Internal reasoning tool for breaking down problems, planning approaches, analyzing constraints, and reasoning through multi-step solutions before acting. The thought content is not shown to the user.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["thought"],
				"properties": {
					"thought": {"type": "string", "description": "A reflective note or intermediate reasoning step."}
				}
			}`),
		},
		{
			Name:        "reasoning_plan",
			Description: "Executive planning tool. Before beginning multi-step work, decompose the goal into ordered steps with explicit dependencies. If a task backend is available (task_create), steps are committed to persistent storage and assigned IDs. Otherwise the plan exists in context only. Use reasoning_think for moment-to-moment reflection; use reasoning_plan to lay out a complete work breakdown before acting. Steps may only depend on earlier steps (lower index).",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["goal", "steps"],
				"properties": {
					"goal": {"type": "string", "description": "Top-level goal or objective."},
					"steps": {
						"type": "array",
						"description": "Ordered work steps. Each step may only list earlier steps (by 0-based index) in 'after'.",
						"items": {
							"type": "object",
							"required": ["title"],
							"properties": {
								"title": {"type": "string", "description": "Short step title."},
								"body":  {"type": "string", "description": "Optional detail or acceptance criteria."},
								"after": {
									"type": "array",
									"items": {"type": "integer"},
									"description": "0-based indices of steps that must complete before this one."
								}
							}
						}
					}
				}
			}`),
		},
	}
}
