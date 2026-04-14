package reasoning

import (
	"context"
	"encoding/json"

	"ollie/pkg/tools"
)

// Decl returns a factory for a reasoning Server.
func Decl() func() tools.Server { return func() tools.Server { return &Server{} } }

// Server implements tools.Server for reasoning_think.
type Server struct{}

// ListTools implements tools.Server.
func (s *Server) ListTools() ([]tools.ToolInfo, error) {
	return []tools.ToolInfo{ToolThink}, nil
}

// CallTool implements tools.Server.
func (s *Server) CallTool(_ context.Context, tool string, _ json.RawMessage) (json.RawMessage, error) {
	var text string
	switch tool {
	case ToolThink.Name:
		// No-op: the thought is recorded in conversation history by the loop.
	default:
		text = "error: unknown tool: " + tool
	}
	result, _ := json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
	})
	return result, nil
}

// Close implements tools.Server (no-op).
func (s *Server) Close() {}
