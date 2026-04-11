package execute

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ToolsPath returns the directory to search for named tool scripts.
// Resolved in order: OLLIE_TOOLS_PATH env var, then ~/.local/share/ollie/tools.
func ToolsPath() string {
	if p := os.Getenv("OLLIE_TOOLS_PATH"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "ollie", "tools")
}

// detectLanguage infers the script language from the shebang line.
// Returns "python3" for Python scripts, "bash" for everything else.
func detectLanguage(code string) string {
	line, _, _ := strings.Cut(code, "\n")
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "#!") && strings.Contains(line, "python") {
		return "python3"
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
