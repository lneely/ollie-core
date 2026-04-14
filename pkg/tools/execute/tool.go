package execute

// execute_tool is a specific case of execute_code, such that the name of the
// script passed to `tool` is loaded into memory from `$OLLIE_9MOUNT/t/` and
// run with Execute(...).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ToolsPath returns the directory to search for named tool scripts.
// Resolved in order: first entry of OLLIE_TOOLS_PATH (colon-separated),
// then ~/.config/ollie/tools.
func ToolsPath() string {
	if p := os.Getenv("OLLIE_TOOLS_PATH"); p != "" {
		if i := strings.Index(p, ":"); i >= 0 {
			p = p[:i]
		}
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "ollie", "tools")
}

// detectLanguage infers the script language from the shebang line.
// Returns "python3", "perl", or "bash".
func detectLanguage(code string) string {
	line, _, _ := strings.Cut(code, "\n")
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "#!") {
		if strings.Contains(line, "python") {
			return "python3"
		}
		if strings.Contains(line, "perl") {
			return "perl"
		}
	}
	return "bash"
}

// injectArgs prepends language-appropriate argument binding to code.
func injectArgs(language, name string, args []string, code string) string {
	switch language {
	case "python3":
		quoted := make([]string, len(args))
		for i, a := range args {
			quoted[i] = fmt.Sprintf("%q", a)
		}
		return fmt.Sprintf("import sys\nsys.argv = [%q, %s]\n%s", name, strings.Join(quoted, ", "), code)
	case "perl":
		quoted := make([]string, len(args))
		for i, a := range args {
			quoted[i] = "'" + strings.ReplaceAll(a, "'", "\\'") + "'"
		}
		return fmt.Sprintf("@ARGV = (%s);\n%s", strings.Join(quoted, ", "), code)
	default: // bash
		escaped := make([]string, len(args))
		for i, a := range args {
			escaped[i] = "'" + strings.ReplaceAll(a, "'", "'\\''") + "'"
		}
		return fmt.Sprintf("set -- %s\n%s", strings.Join(escaped, " "), code)
	}
}

// ansiCEscape escapes a string for embedding in a bash $'...' literal.
func ansiCEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\'':
			b.WriteString(`\'`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ReadTool reads a named tool script from the tools directory.
func ReadTool(name string) (string, error) {
	if strings.Contains(name, "/") || strings.Contains(name, "..") {
		return "", fmt.Errorf("invalid tool name")
	}

	path := filepath.Join(ToolsPath(), name)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("tool not found: %s", name)
		}
		return "", fmt.Errorf("read tool %s: %w", name, err)
	}
	return string(data), nil
}

func dispatchExecuteTool(ctx context.Context, e *Server, args json.RawMessage) (string, error) {
	var a struct {
		Tool    string   `json:"tool"`
		Args    []string `json:"args"`
		Timeout int      `json:"timeout"`
		Sandbox string   `json:"sandbox"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("execute_tool: bad args: %w", err)
	}
	if a.Tool == "" {
		return "", fmt.Errorf("execute_tool: 'tool' is required")
	}
	if e.Confirm != nil && !e.Confirm(fmt.Sprintf("execute_tool: %s %s", a.Tool, strings.Join(a.Args, " "))) {
		return "", fmt.Errorf("execute_tool: denied by user")
	}
	toolCode, err := ReadTool(a.Tool)
	if err != nil {
		return "", err
	}
	language := detectLanguage(toolCode)
	code := toolCode
	if len(a.Args) > 0 {
		code = injectArgs(language, a.Tool, a.Args, toolCode)
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	sandbox := a.Sandbox
	if sandbox == "" {
		sandbox = "default"
	}
	return e.Execute(ctx, code, language, timeout, sandbox, true)
}
