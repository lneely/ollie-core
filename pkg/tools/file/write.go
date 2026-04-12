package file

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"ollie/pkg/tools"
)

var ToolWrite = tools.ToolInfo{
	Name: "Write",
	Description: `Writes a file to the local filesystem.

Usage:
- This tool will overwrite the existing file if there is one at the provided path.
- If this is an existing file, you MUST use the Read tool first to read the file's contents. This tool will fail if you did not read the file first.
- ALWAYS prefer editing existing files in the codebase. NEVER write new files unless explicitly required.
- NEVER proactively create documentation files (*.md) or README files. Only create documentation files if explicitly requested by the User.
- Only use emojis if the user explicitly requests it. Avoid writing emojis to files unless asked.`,
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

	if err := os.WriteFile(a.FilePath, []byte(a.Content), 0644); err != nil { //nolint:gosec
		return "", fmt.Errorf("writing file: %w", err)
	}

	s.markRead(a.FilePath)
	return "File written successfully", nil
}
