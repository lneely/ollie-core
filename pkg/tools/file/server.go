// Package file implements the Read, Write, Edit, Grep, and Glob tools.
package file

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sync"

	"ollie/pkg/tools"
)

// Server implements tools.Server for file operations.
type Server struct {
	projectDir string
	rgPath     string

	mu        sync.Mutex
	readFiles map[string]struct{}
}

// New creates a Server for the given project directory.
func New(projectDir string) *Server {
	rg, _ := exec.LookPath("rg")
	return &Server{
		projectDir: projectDir,
		rgPath:     rg,
		readFiles:  make(map[string]struct{}),
	}
}

// Decl returns a factory for a file Server with the given project directory.
func Decl(projectDir string) func() tools.Server {
	return func() tools.Server { return New(projectDir) }
}

// SetCWD implements tools.CWDSetter.
func (s *Server) SetCWD(dir string) {
	s.mu.Lock()
	s.projectDir = dir
	s.mu.Unlock()
}

// ListTools implements tools.Server.
func (s *Server) ListTools() ([]tools.ToolInfo, error) {
	return []tools.ToolInfo{ToolRead, ToolWrite, ToolEdit, ToolGrep, ToolGlob}, nil
}

// CallTool implements tools.Server.
func (s *Server) CallTool(ctx context.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
	var (
		text string
		err  error
	)
	switch tool {
	case "file_read":
		text, err = s.dispatchRead(ctx, args)
	case "file_write":
		text, err = s.dispatchWrite(ctx, args)
	case "file_edit":
		text, err = s.dispatchEdit(ctx, args)
	case "file_grep":
		text, err = s.dispatchGrep(ctx, args)
	case "file_glob":
		text, err = s.dispatchGlob(ctx, args)
	default:
		text = "unknown tool: " + tool
	}
	if err != nil {
		text = "error: " + err.Error()
	}
	return json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
	})
}

// Close implements tools.Server (no-op).
func (s *Server) Close() {}

func (s *Server) markRead(path string) {
	s.mu.Lock()
	s.readFiles[path] = struct{}{}
	s.mu.Unlock()
}

func (s *Server) wasRead(path string) bool {
	s.mu.Lock()
	_, ok := s.readFiles[path]
	s.mu.Unlock()
	return ok
}

// Compile-time interface checks.
var _ tools.Server = (*Server)(nil)
var _ tools.CWDSetter = (*Server)(nil)

// Shared argument helpers.

func intOrZero(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func paginate(items []string, offset, headLimit int) []string {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(items) {
		return nil
	}
	items = items[offset:]
	if headLimit > 0 && headLimit < len(items) {
		items = items[:headLimit]
	}
	return items
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = s[:len(s)-countTrailingNewlines(s)]
	if s == "" {
		return nil
	}
	lines := make([]string, 0, 32)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	lines = append(lines, s[start:])
	return lines
}

func countTrailingNewlines(s string) int {
	n := 0
	for i := len(s) - 1; i >= 0 && s[i] == '\n'; i-- {
		n++
	}
	return n
}

// errText wraps an error message as a non-error tool result (model sees error text).
func errText(format string, args ...any) string {
	return "error: " + fmt.Sprintf(format, args...)
}
