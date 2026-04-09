package execute

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"9fans.net/go/plan9/client"
)

// ReadTool reads a named tool script from the 9P tools directory.
func ReadTool(name string) (string, error) {
	if strings.Contains(name, "/") || strings.Contains(name, "..") {
		return "", fmt.Errorf("invalid tool name")
	}

	ns := fmt.Sprintf("/tmp/ns.%s.:0", os.Getenv("USER"))
	fsys, err := client.Mount("unix", filepath.Join(ns, "anvillm"))
	if err != nil {
		return "", fmt.Errorf("failed to mount 9P: %v", err)
	}
	defer fsys.Close()

	fid, err := fsys.Open("/tools/"+name, 0)
	if err != nil {
		return "", fmt.Errorf("tool not found: %s", name)
	}
	defer fid.Close()

	var buf []byte
	tmp := make([]byte, 8192)
	for {
		n, err := fid.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil || n < len(tmp) {
			break
		}
	}
	return string(buf), nil
}
