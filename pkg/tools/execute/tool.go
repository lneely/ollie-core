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
