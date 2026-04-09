// Package file provides the file_read and file_write built-in tools.
package file

import (
	"context"
	"encoding/json"
	"fmt"

	"ollie/pkg/tools"
)

// Server implements tools.Server for file_read and file_write.
type Server struct {
	// Confirm is called before each operation. Return false to deny.
	Confirm func(string) bool
}

func (s *Server) ListTools() ([]tools.ToolInfo, error) {
	return []tools.ToolInfo{
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
	}, nil
}

func (s *Server) CallTool(_ context.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
	var result string
	var err error
	switch tool {
	case "file_read":
		result, err = read(s.Confirm, args)
	case "file_write":
		result, err = write(s.Confirm, args)
	default:
		return nil, fmt.Errorf("unknown tool: %s", tool)
	}
	if err != nil {
		return json.Marshal(map[string]string{"error": err.Error()})
	}
	return json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": result}},
	})
}

func (s *Server) Close() {}
