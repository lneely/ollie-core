package file

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"ollie/pkg/tools"
)

var ToolEdit = tools.ToolInfo{
	Name: "file_edit",
	Description: `Perform exact string replacement in a file.

Usage:
- File path must be absolute (not relative)
- Must read file with file_read before editing
- old_string must be unique in file (or use replace_all)
- Preserve exact indentation from file_read output
- old_string and new_string must be different

Parameters:
- replace_all: replace all occurrences (for renaming)
- old_string: text to find (exact match)
- new_string: replacement text

Notes:
- file_read returns diff format: exclude leading "+" from old_string
- For non-unique old_string: use larger context or replace_all
- Prefer over file_write for modifications
- Line endings preserved`,
	InputSchema: json.RawMessage(`{
		"type": "object",
		"required": ["file_path", "old_string", "new_string"],
		"properties": {
			"file_path":   {"type": "string",  "description": "The absolute path to the file to modify"},
			"old_string":  {"type": "string",  "description": "The text to replace"},
			"new_string":  {"type": "string",  "description": "The text to replace it with (must be different from old_string)"},
			"replace_all": {"type": "boolean", "description": "Replace all occurences of old_string (default false)"}
		}
	}`),
}

func (s *Server) dispatchEdit(_ context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		FilePath   string `json:"file_path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("bad args: %w", err)
	}
	if a.FilePath == "" {
		return "", fmt.Errorf("file_path is required")
	}
	if err := s.checkAccess(a.FilePath, true); err != nil {
		return errText("%v", err), nil
	}
	if a.OldString == a.NewString {
		return errText("old_string and new_string must be different"), nil
	}
	if !s.wasRead(a.FilePath) {
		return errText("file must be read first. Use the Read tool to examine the file contents."), nil
	}

	original, err := readFileChecked(a.FilePath)
	if err != nil {
		return err.Error(), nil
	}

	newContent, err := robustReplace(original, a.OldString, a.NewString, a.ReplaceAll)
	if err != nil {
		return errText("%v", err), nil
	}

	if err := os.WriteFile(a.FilePath, []byte(newContent), 0644); err != nil { //nolint:gosec
		return "", fmt.Errorf("writing file: %w", err)
	}

	return unifiedDiff(a.FilePath, original, newContent), nil
}

// robustReplace tries exact → whitespace-normalized → indentation-flexible → trimmed-boundary.
func robustReplace(content, oldString, newString string, replaceAll bool) (string, error) {
	if m := findExact(content, oldString); m != "" {
		return doReplace(content, m, newString, replaceAll)
	}
	if m := findWSNormalized(content, oldString); m != "" {
		return doReplace(content, m, newString, replaceAll)
	}
	if m := findIndentFlexible(content, oldString); m != "" {
		return doReplace(content, m, newString, replaceAll)
	}
	if m := findTrimmedBoundary(content, oldString); m != "" {
		return doReplace(content, m, newString, replaceAll)
	}
	return "", fmt.Errorf("old_string not found in file")
}

func doReplace(content, match, newString string, replaceAll bool) (string, error) {
	if replaceAll {
		return strings.ReplaceAll(content, match, newString), nil
	}
	n := strings.Count(content, match)
	if n == 0 {
		return "", fmt.Errorf("match disappeared from content")
	}
	if n > 1 {
		return "", fmt.Errorf("old_string is not unique (%d occurrences). Use replace_all=true or provide more context", n)
	}
	return strings.Replace(content, match, newString, 1), nil
}

func findExact(content, find string) string {
	if strings.Contains(content, find) {
		return find
	}
	return ""
}

func findWSNormalized(content, find string) string {
	nf := strings.Join(strings.Fields(find), " ")
	lines := strings.Split(content, "\n")
	findLines := strings.Split(find, "\n")
	n := len(findLines)
	for i := 0; i <= len(lines)-n; i++ {
		block := strings.Join(lines[i:i+n], "\n")
		if strings.Join(strings.Fields(block), " ") == nf {
			return block
		}
	}
	return ""
}

func findIndentFlexible(content, find string) string {
	deindent := func(text string) string {
		ls := strings.Split(text, "\n")
		min := 10000
		for _, l := range ls {
			if strings.TrimSpace(l) == "" {
				continue
			}
			for i, r := range l {
				if r != ' ' && r != '\t' {
					if i < min {
						min = i
					}
					break
				}
			}
		}
		if min == 10000 {
			return text
		}
		out := make([]string, len(ls))
		for i, l := range ls {
			if strings.TrimSpace(l) == "" {
				out[i] = l
			} else if len(l) > min {
				out[i] = l[min:]
			} else {
				out[i] = strings.TrimLeft(l, " \t")
			}
		}
		return strings.Join(out, "\n")
	}
	nf := deindent(find)
	lines := strings.Split(content, "\n")
	findLines := strings.Split(find, "\n")
	n := len(findLines)
	for i := 0; i <= len(lines)-n; i++ {
		block := strings.Join(lines[i:i+n], "\n")
		if deindent(block) == nf {
			return block
		}
	}
	return ""
}

func findTrimmedBoundary(content, find string) string {
	trimmed := strings.TrimSpace(find)
	if trimmed == find {
		return ""
	}
	if strings.Contains(content, trimmed) {
		return trimmed
	}
	lines := strings.Split(content, "\n")
	findLines := strings.Split(find, "\n")
	n := len(findLines)
	for i := 0; i <= len(lines)-n; i++ {
		block := strings.Join(lines[i:i+n], "\n")
		if strings.TrimSpace(block) == trimmed {
			return block
		}
	}
	return ""
}
