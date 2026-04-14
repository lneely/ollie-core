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
// Returns "python3", "perl", "awk", "sed", "ed", or "bash".
func detectLanguage(code string) string {
	line, _, _ := strings.Cut(code, "\n")
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "#!") {
		return "bash"
	}
	fields := strings.Fields(strings.TrimPrefix(line, "#!"))
	if len(fields) == 0 {
		return "bash"
	}
	// When the interpreter is /usr/bin/env, the actual interpreter is the next argument.
	names := fields
	if filepath.Base(fields[0]) == "env" && len(fields) > 1 {
		names = fields[1:]
	}
	switch filepath.Base(names[0]) {
	case "python", "python3":
		return "python3"
	case "perl":
		return "perl"
	case "awk", "gawk":
		return "awk"
	case "sed", "gsed":
		return "sed"
	case "ed":
		return "ed"
	case "jq":
		return "jq"
	case "expect":
		return "expect"
	case "bc":
		return "bc"
	case "lua", "lua5.1", "lua5.2", "lua5.3", "lua5.4":
		return "lua"
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
	case "awk":
		// awk args are input filenames; produce a bash snippet: gawk -e $'prog' -- file ...
		fileArgs := make([]string, len(args))
		for i, a := range args {
			fileArgs[i] = "'" + strings.ReplaceAll(a, "'", "'\\''") + "'"
		}
		return fmt.Sprintf("gawk -e $'%s' -- %s", ansiCEscape(code), strings.Join(fileArgs, " "))
	case "sed":
		// sed args are input filenames (use -i in the script for in-place editing).
		fileArgs := make([]string, len(args))
		for i, a := range args {
			fileArgs[i] = "'" + strings.ReplaceAll(a, "'", "'\\''") + "'"
		}
		return fmt.Sprintf("sed -e $'%s' %s", ansiCEscape(code), strings.Join(fileArgs, " "))
	case "ed":
		// ed reads commands from stdin; first arg is the file to edit.
		file := ""
		if len(args) > 0 {
			file = " '" + strings.ReplaceAll(args[0], "'", "'\\''") + "'"
		}
		return fmt.Sprintf("printf '%%s' $'%s' | ed -s%s", ansiCEscape(code), file)
	case "jq":
		// jq args are JSON input files; filter is the program.
		fileArgs := make([]string, len(args))
		for i, a := range args {
			fileArgs[i] = "'" + strings.ReplaceAll(a, "'", "'\\''") + "'"
		}
		return fmt.Sprintf("jq $'%s' %s", ansiCEscape(code), strings.Join(fileArgs, " "))
	case "expect":
		// expect reads script from stdin via expect -.
		return fmt.Sprintf("printf '%%s' $'%s' | expect -", ansiCEscape(code))
	case "bc":
		// bc reads from stdin; -ql for quiet mode + math library.
		return fmt.Sprintf("printf '%%s' $'%s' | bc -ql", ansiCEscape(code))
	case "lua":
		// Inject args as the arg table, matching lua's scriptfile convention.
		quoted := make([]string, len(args))
		for i, a := range args {
			quoted[i] = fmt.Sprintf("%q", a)
		}
		return fmt.Sprintf("arg={%s}\n%s", strings.Join(quoted, ", "), code)
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
	if !e.allowed("execute_tool", fmt.Sprintf("execute_tool: %s %s", a.Tool, strings.Join(a.Args, " "))) {
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
		// injectArgs for file-based languages produces a bash command snippet;
		// run it as bash rather than trying to pass it as a raw program argument.
		switch language {
		case "awk", "sed", "jq", "ed", "expect", "bc":
			language = "bash"
		}
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
