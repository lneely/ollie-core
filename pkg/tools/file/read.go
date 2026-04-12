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
	Name: "Read",
	Description: `Reads a file from the local filesystem. You can access any file directly by using this tool.
Assume this tool is able to read all files on the machine. If the User provides a path to a file assume that path is valid. It is okay to read a file that does not exist; an error will be returned.

Usage:
- The file_path parameter must be an absolute path, not a relative path
- By default, it reads up to 2000 lines starting from the beginning of the file
- You can optionally specify a line offset and limit (especially handy for long files), but it's recommended to read the whole file by not providing these parameters
- Any lines longer than 2000 characters will be truncated
- Results are returned with line numbers in the format: line number + "| " + content, starting at 1
- This tool can only read files, not directories. To read a directory, use an ls command via the Bash tool.
- You can call multiple tools in a single response. It is always better to speculatively read multiple potentially useful files in parallel.
- If you read a file that exists but has empty contents "(empty file)" will be returned.`,
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
			lines = append(lines, fmt.Sprintf("%05d| %s", lineNum, line))
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
	return strings.Join(lines, "\n"), nil
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
