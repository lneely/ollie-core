// Package tools defines the Executor interface for routing tool calls,
// and provides a default MCP-backed implementation.
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"ollie/pkg/mcp"
)

// ToolInfo describes a tool provided by a tool server.
type ToolInfo struct {
	Server      string
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Server is the interface satisfied by any tool server registered with an Executor.
// Both MCP servers and built-in tool sets implement this interface.
type Server interface {
	ListTools() ([]ToolInfo, error)
	CallTool(ctx context.Context, tool string, args json.RawMessage) (json.RawMessage, error)
	Close()
}

// Executor routes tool calls to the server that owns them.
// Implementations may back this with MCP servers, local functions, or mocks.
type Executor interface {
	ListTools() ([]ToolInfo, error)
	Execute(ctx context.Context, server, tool string, args json.RawMessage) (json.RawMessage, error)
	Close()
}

// MCPExecutor is the default Executor backed by registered Server instances.
// NewExecutor returns a *MCPExecutor so callers can call AddServer during
// setup; after setup it satisfies the Executor interface.
type MCPExecutor struct {
	servers map[string]Server
}

// NewExecutor returns an executor with no servers registered.
// Call AddServer to attach servers before using it.
func NewExecutor() *MCPExecutor {
	return &MCPExecutor{servers: make(map[string]Server)}
}

// AddServer registers a Server under the given name.
func (e *MCPExecutor) AddServer(name string, s Server) {
	e.servers[name] = s
}

// NewMCPServer wraps an mcp.Client as a Server.
func NewMCPServer(client *mcp.Client) Server {
	return &mcpServer{client: client}
}

// Close shuts down all registered servers.
func (e *MCPExecutor) Close() {
	for _, s := range e.servers {
		s.Close()
	}
}

// ListTools returns all tools advertised by all registered servers.
func (e *MCPExecutor) ListTools() ([]ToolInfo, error) {
	var all []ToolInfo
	for serverName, s := range e.servers {
		tools, err := s.ListTools()
		if err != nil {
			return nil, fmt.Errorf("server %s: %w", serverName, err)
		}
		for _, t := range tools {
			t.Server = serverName
			all = append(all, t)
		}
	}
	return all, nil
}

// Execute calls a named tool on the named server.
func (e *MCPExecutor) Execute(ctx context.Context, server, tool string, args json.RawMessage) (json.RawMessage, error) {
	s, ok := e.servers[server]
	if !ok {
		return nil, fmt.Errorf("server not found: %s", server)
	}
	return s.CallTool(ctx, tool, args)
}

// mcpServer wraps an mcp.Client as a Server.
type mcpServer struct {
	client *mcp.Client
}

func (m *mcpServer) ListTools() ([]ToolInfo, error) {
	result, err := m.client.Call("tools/list", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, err
	}
	var tools []ToolInfo
	for _, t := range resp.Tools {
		tools = append(tools, ToolInfo{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	return tools, nil
}

func (m *mcpServer) CallTool(_ context.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
	return m.client.Call("tools/call", map[string]any{
		"name":      tool,
		"arguments": args,
	})
}

func (m *mcpServer) Close() {
	m.client.Close()
}
