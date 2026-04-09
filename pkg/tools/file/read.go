package file

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func read(confirm func(string) bool, args json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("file_read: bad args: %w", err)
	}
	if a.Path == "" {
		return "", fmt.Errorf("file_read: 'path' is required")
	}
	if confirm != nil && !confirm(fmt.Sprintf("read %s", a.Path)) {
		return "", fmt.Errorf("file_read: denied by user")
	}
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return "", fmt.Errorf("file_read: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	var out strings.Builder
	for i, line := range lines {
		fmt.Fprintf(&out, "%d\t%s\n", i+1, line)
	}
	return strings.TrimRight(out.String(), "\n"), nil
}
