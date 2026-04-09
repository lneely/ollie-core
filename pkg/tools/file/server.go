package file

import (
	"context"
	"encoding/json"
	"fmt"

	"ollie/pkg/tools"
)

// Server implements tools.Server for file_read and file_write.
type Server struct {
	// Confirm is called before each operation. Return false to deny.
	Confirm func(string) bool
}

// New returns a file Server.
func New() *Server {
	return &Server{}
}

// ListTools implements tools.Server.
func (s *Server) ListTools() ([]tools.ToolInfo, error) {
	return tools.FileDefs(), nil
}

// CallTool implements tools.Server.
func (s *Server) CallTool(_ context.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
	result, err := s.dispatch(tool, args)
	if err != nil {
		return json.Marshal(map[string]string{"error": err.Error()})
	}
	return json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": result}},
	})
}

// Close implements tools.Server (no-op).
func (s *Server) Close() {}

func (s *Server) dispatch(name string, args json.RawMessage) (string, error) {
	switch name {
	case "file_read":
		return s.dispatchRead(args)
	case "file_write":
		return s.dispatchWrite(args)
	default:
		return "", fmt.Errorf("unknown file tool: %s", name)
	}
}

func (s *Server) dispatchRead(args json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("file_read: bad args: %w", err)
	}
	if a.Path == "" {
		return "", fmt.Errorf("file_read: 'path' is required")
	}
	if s.Confirm != nil && !s.Confirm("read "+a.Path) {
		return "", fmt.Errorf("file_read: denied by user")
	}
	return Read(a.Path)
}

func (s *Server) dispatchWrite(args json.RawMessage) (string, error) {
	var a struct {
		Path      string `json:"path"`
		Content   string `json:"content"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("file_write: bad args: %w", err)
	}
	if a.Path == "" {
		return "", fmt.Errorf("file_write: 'path' is required")
	}
	prompt := "write " + a.Path
	if a.StartLine > 0 {
		prompt = fmt.Sprintf("write %s lines %d-%d", a.Path, a.StartLine, a.EndLine)
	}
	if s.Confirm != nil && !s.Confirm(prompt) {
		return "", fmt.Errorf("file_write: denied by user")
	}
	return Write(a.Path, a.Content, a.StartLine, a.EndLine)
}

var _ tools.Server = (*Server)(nil) // compile-time interface check
