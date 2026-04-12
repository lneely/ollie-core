package file

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"ollie/pkg/tools"
)

var ToolWrite = tools.ToolInfo{
	Name: "file_write",
	Description: `Write or overwrite a file.

Usage:
- Creates new file or overwrites existing file
- File path must be absolute (not relative)
- For existing files: must read with file_read first
- Overwrites entire file content

Notes:
- Prefer file_edit for modifying existing files
- Only create new files when explicitly required
- Write permissions required for target location
- Creates parent directories if needed`,
	InputSchema: json.RawMessage(`{
		"type": "object",
		"required": ["file_path", "content"],
		"properties": {
			"file_path": {"type": "string", "description": "The absolute path to the file to write (must be absolute, not relative)"},
			"content":   {"type": "string", "description": "The content to write to the file"}
		}
	}`),
}

func (s *Server) dispatchWrite(_ context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("bad args: %w", err)
	}
	if a.FilePath == "" {
		return "", fmt.Errorf("file_path is required")
	}

	_, statErr := os.Stat(a.FilePath)
	exists := statErr == nil
	if exists && !s.wasRead(a.FilePath) {
		return errText("existing file must be read first. Use the Read tool to examine the file contents."), nil
	}

	var oldContent string
	if exists {
		oldContent, _ = readFileChecked(a.FilePath)
	}

	if err := os.WriteFile(a.FilePath, []byte(a.Content), 0644); err != nil { //nolint:gosec
		return "", fmt.Errorf("writing file: %w", err)
	}

	s.markRead(a.FilePath)

	if !exists {
		return plusLines(a.Content), nil
	}
	return unifiedDiff(a.FilePath, oldContent, a.Content), nil
}
