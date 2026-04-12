package file

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"ollie/pkg/tools"
)

var ToolGrep = tools.ToolInfo{
	Name: "file_grep",
	Description: `Search file contents using ripgrep. The primary content search tool.

Usage:
- Always use Grep instead of grep or rg in Bash
- Uses ripgrep syntax (not GNU grep); literal braces need escaping as \{ and \}
- ripgrep respects .gitignore and .ignore files by default
- files_with_matches is the default mode (just file paths)
- multiline: true is needed for patterns spanning lines
- Supports pagination via offset + head_limit`,
	InputSchema: json.RawMessage(`{
		"type": "object",
		"required": ["pattern"],
		"properties": {
			"pattern":     {"type": "string", "description": "Regex pattern to search for (ripgrep syntax)"},
			"path":        {"type": "string", "description": "File or directory to search in. Defaults to cwd."},
			"output_mode": {"type": "string", "description": "Default: 'files_with_matches'. 'content' shows matching lines, 'count' shows match counts.", "enum": ["content", "files_with_matches", "count"]},
			"glob":        {"type": "string", "description": "Glob filter, e.g. '*.js', '*.{ts,tsx}'"},
			"type":        {"type": "string", "description": "File type filter, e.g. 'js', 'py', 'rust'. More efficient than glob for standard types."},
			"-i":          {"type": "boolean", "description": "Case insensitive search"},
			"-n":          {"type": "boolean", "description": "Show line numbers (default true). Requires output_mode: 'content'."},
			"-A":          {"type": "number", "description": "Lines after match"},
			"-B":          {"type": "number", "description": "Lines before match"},
			"-C":          {"type": "number", "description": "Lines before and after match (short flag alias)"},
			"context":     {"type": "number", "description": "Lines before and after match"},
			"multiline":   {"type": "boolean", "description": "Enable multiline mode where . matches newlines. Default: false."},
			"head_limit":  {"type": "number", "description": "Limit output to first N lines/entries (like | head -N). Default: 0 (unlimited)."},
			"offset":      {"type": "number", "description": "Skip first N entries before applying head_limit (like | tail -n +N | head -N). Default: 0."}
		}
	}`),
}

func (s *Server) dispatchGrep(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		OutputMode string `json:"output_mode"`
		Glob       string `json:"glob"`
		Type       string `json:"type"`
		IgnoreCase bool   `json:"-i"`
		LineNumber *bool  `json:"-n"`
		After      *int   `json:"-A"`
		Before     *int   `json:"-B"`
		ContextC   *int   `json:"-C"`
		Context    *int   `json:"context"`
		Multiline  bool   `json:"multiline"`
		HeadLimit  *int   `json:"head_limit"`
		Offset     *int   `json:"offset"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("bad args: %w", err)
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return "", fmt.Errorf("pattern is required")
	}
	if s.rgPath == "" {
		return "", fmt.Errorf("ripgrep (rg) not found in PATH")
	}

	outputMode := a.OutputMode
	if outputMode == "" {
		outputMode = "files_with_matches"
	}

	searchPath := a.Path
	if searchPath == "" {
		searchPath = s.projectDir
	}
	searchPath = filepath.Clean(searchPath)

	if err := ensureSearchPath(searchPath); err != nil {
		return "", err
	}

	cmdArgs := []string{"--no-config", "--color=never"}
	switch outputMode {
	case "files_with_matches":
		cmdArgs = append(cmdArgs, "-l")
	case "count":
		cmdArgs = append(cmdArgs, "--count")
	case "content":
		showLineNums := true
		if a.LineNumber != nil {
			showLineNums = *a.LineNumber
		}
		if showLineNums {
			cmdArgs = append(cmdArgs, "-n")
		}
	}
	if a.IgnoreCase {
		cmdArgs = append(cmdArgs, "-i")
	}
	if a.Multiline {
		cmdArgs = append(cmdArgs, "-U", "--multiline-dotall")
	}
	if a.Glob != "" {
		cmdArgs = append(cmdArgs, "-g", a.Glob)
	}
	if a.Type != "" {
		cmdArgs = append(cmdArgs, "-t", a.Type)
	}
	ctx_ := a.Context
	if ctx_ == nil {
		ctx_ = a.ContextC
	}
	if ctx_ != nil {
		cmdArgs = append(cmdArgs, "-C", fmt.Sprintf("%d", *ctx_))
	}
	if a.Before != nil {
		cmdArgs = append(cmdArgs, "-B", fmt.Sprintf("%d", *a.Before))
	}
	if a.After != nil {
		cmdArgs = append(cmdArgs, "-A", fmt.Sprintf("%d", *a.After))
	}
	cmdArgs = append(cmdArgs, "-e", a.Pattern, searchPath)

	cmd := exec.CommandContext(ctx, s.rgPath, cmdArgs...)
	cmd.Dir = s.projectDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return "", nil // no matches
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", msg)
	}

	lines := splitLines(stdout.String())
	lines = paginate(lines, intOrZero(a.Offset), intOrZero(a.HeadLimit))

	if outputMode == "content" {
		lines = formatContentLines(lines)
	}

	return strings.Join(lines, "\n"), nil
}

func formatContentLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if line == "--" {
			out = append(out, line)
			continue
		}
		if p, rest, ok := strings.Cut(line, ":"); ok {
			if ln, content, ok2 := strings.Cut(rest, ":"); ok2 && isDigits(ln) {
				out = append(out, fmt.Sprintf("%s:%05s| %s", p, ln, content))
				continue
			}
		}
		if p, rest, ok := strings.Cut(line, "-"); ok {
			if ln, content, ok2 := strings.Cut(rest, "-"); ok2 && isDigits(ln) {
				out = append(out, fmt.Sprintf("%s:%05s| %s", p, ln, content))
				continue
			}
		}
		out = append(out, line)
	}
	return out
}

func ensureSearchPath(path string) error {
	info, err := statPath(path)
	if err != nil {
		return fmt.Errorf("error accessing path: %w", err)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return fmt.Errorf("not a searchable path: %s", path)
	}
	return nil
}
