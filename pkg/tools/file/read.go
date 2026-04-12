package file

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ollie/pkg/tools"
)

var ToolRead = tools.ToolInfo{
	Name: "file_read",
	Description: `Read a file from the local filesystem.

Usage:
- File path must be absolute (not relative)
- Reads first 2000 lines by default
- Lines longer than 2000 characters are truncated
- Maximum file size: 8 MB
- Returns content in diff format (existing lines have no prefix)
- Can read any file on the system
- Reading a non-existent file returns an error
- Reading an empty file returns "(empty file)"

Notes:
- Use offset/limit parameters for large files
- Read directories with \`execute_code('ls <path>')\`, not this tool
- Call multiple file_read operations in parallel when exploring
- Always read a file before editing or writing to it with file_edit/file_write`,
	InputSchema: json.RawMessage(`{
		"type": "object",
		"required": ["file_path"],
		"properties": {
			"file_path": {"type": "string", "description": "The absolute path to the file to read"},
			"offset":    {"type": "number", "description": "The line number to start reading from. Only provide if the file is too large to read at once"},
			"limit":     {"type": "number", "description": "The number of lines to read. Only provide if the file is too large to read at once."}
		}
	}`),
}

const (
	maxFileSize  = 8 * 1024 * 1024 // 8 MB
	defaultLimit = 2000
	maxLineLen   = 2000
)

func (s *Server) dispatchRead(_ context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		FilePath string `json:"file_path"`
		Offset   *int   `json:"offset"`
		Limit    *int   `json:"limit"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("bad args: %w", err)
	}
	if a.FilePath == "" {
		return "", fmt.Errorf("file_path is required")
	}

	file, err := openChecked(a.FilePath)
	if err != nil {
		return err.Error(), nil // surface as text
	}
	defer file.Close()

	off := 1
	if a.Offset != nil && *a.Offset > 1 {
		off = *a.Offset
	}
	lim := defaultLimit
	if a.Limit != nil && *a.Limit > 0 {
		lim = *a.Limit
	}

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var lines []string
	lineNum := 1
	for scanner.Scan() {
		if lineNum >= off && len(lines) < lim {
			line := scanner.Text()
			if len(line) > maxLineLen {
				line = line[:maxLineLen] + "..."
			}
			lines = append(lines, line)
		}
		lineNum++
		if lineNum > off+lim {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("reading file: %w", err)
	}
	if len(lines) == 0 {
		return "(empty file)", nil
	}

	s.markRead(a.FilePath)
	return strings.Join(lines, "\n") + "\n", nil
}

// openChecked validates and opens a regular file within size limits.
func openChecked(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("error: path must be absolute, got: %s", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("error accessing file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("error: not a regular file: %s", path)
	}
	if info.Size() > maxFileSize {
		return nil, fmt.Errorf("error: file too large (%d bytes, max %d)", info.Size(), maxFileSize)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("error opening file: %w", err)
	}
	return f, nil
}

// readFileChecked reads the full content of a regular file within size limits.
func readFileChecked(path string) (string, error) {
	f, err := openChecked(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, _ := f.Stat()
	buf := make([]byte, info.Size())
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return "", fmt.Errorf("reading file: %w", err)
	}
	return string(buf[:n]), nil
}
