package execute

import (
	"context"
	"encoding/json"

	"ollie/pkg/tools"
)

// Decl returns a factory for an execute Server with the given working directory.
func Decl(workdir string) func() tools.Server {
	return func() tools.Server { return New(workdir) }
}

// ListTools implements tools.Server.
func (e *Server) ListTools() ([]tools.ToolInfo, error) {
	return tools.ExecuteDefs(ToolsPath()), nil
}

// CallTool implements tools.Server.
func (e *Server) CallTool(ctx context.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
	result, err := e.Dispatch(ctx, tool, args)
	if err != nil {
		return json.Marshal(map[string]string{"error": err.Error()})
	}
	return json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": result}},
	})
}

// Close implements tools.Server (no-op).
func (e *Server) Close() {}

var _ tools.Server = (*Server)(nil) // compile-time interface check
