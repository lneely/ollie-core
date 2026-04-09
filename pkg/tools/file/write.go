package file

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func write(confirm func(string) bool, args json.RawMessage) (string, error) {
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
	if confirm != nil && !confirm(prompt) {
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
