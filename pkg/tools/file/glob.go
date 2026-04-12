package file

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ollie/pkg/tools"
)

var ToolGlob = tools.ToolInfo{
	Name: "Glob",
	Description: `Fast file pattern matching. Find files by name/path patterns.

Usage:
- Works with any codebase size
- Results sorted by modification time
- ripgrep respects .gitignore and .ignore files by default
- Use ** for recursive path matches (for example, **/*.js)
- Use instead of find or ls in Bash
- Can speculatively launch multiple Glob calls in parallel`,
	InputSchema: json.RawMessage(`{
		"type": "object",
		"required": ["pattern"],
		"properties": {
			"pattern": {"type": "string", "description": "Glob pattern, e.g. '**/*.js', 'src/**/*.ts'"},
			"path":    {"type": "string", "description": "Directory to search in. Omit for current working directory. Do NOT enter 'undefined' or 'null'."}
		}
	}`),
}

func (s *Server) dispatchGlob(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
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

	searchPath := a.Path
	if searchPath == "" {
		searchPath = s.projectDir
	}
	searchPath = filepath.Clean(searchPath)

	if err := ensureSearchPath(searchPath); err != nil {
		return "", err
	}

	cmd := exec.CommandContext(ctx, s.rgPath, "--no-config", "--files", "-g", a.Pattern, searchPath)
	cmd.Dir = s.projectDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return "", nil
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", msg)
	}

	type entry struct {
		path    string
		modTime time.Time
	}
	lines := splitLines(stdout.String())
	entries := make([]entry, 0, len(lines))
	for _, p := range lines {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		entries = append(entries, entry{p, info.ModTime()})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].modTime.Equal(entries[j].modTime) {
			return entries[i].path < entries[j].path
		}
		return entries[i].modTime.After(entries[j].modTime)
	})

	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = e.path
	}
	return strings.Join(paths, "\n"), nil
}

// statPath wraps os.Stat for use by grep.go and glob.go.
func statPath(path string) (os.FileInfo, error) {
	return os.Stat(path)
}
