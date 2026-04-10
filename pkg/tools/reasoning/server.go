package reasoning

import (
	"context"
	"encoding/json"

	"ollie/pkg/tools"
)

// Decl returns a factory for a reasoning Server.
func Decl() func() tools.Server { return func() tools.Server { return &Server{} } }

// Server implements tools.Server for reasoning tools.
type Server struct{}

func (s *Server) ListTools() ([]tools.ToolInfo, error) {
	return tools.ReasoningDefs(), nil
}

func (s *Server) CallTool(_ context.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
	switch tool {
	case "reasoning_think":
		var a struct {
			Thought string `json:"thought"`
		}
		if json.Unmarshal(args, &a) != nil || a.Thought == "" {
			return json.Marshal(map[string]any{
				"content": []map[string]string{{"type": "text", "text": "error: missing required field 'thought'"}},
			})
		}
		// No-op: the thought is recorded in conversation history by the loop.
		return json.Marshal(map[string]any{
			"content": []map[string]string{{"type": "text", "text": ""}},
		})
	default:
		return json.Marshal(map[string]string{"error": "unknown tool: " + tool})
	}
}

func (s *Server) Close() {}

var _ tools.Server = (*Server)(nil)
