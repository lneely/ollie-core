// Package tools defines the Executor interface for routing tool calls,
// and provides a default MCP-backed implementation.
package tools

import (
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

// Executor routes tool calls to the server that owns them.
// Implementations may back this with MCP servers, local functions, or mocks.
type Executor interface {
	ListTools() ([]ToolInfo, error)
	Execute(server, tool string, args json.RawMessage) (json.RawMessage, error)
	Close()
}

// MCPExecutor is the default Executor backed by MCP servers.
// NewExecutor returns a *MCPExecutor so callers can call AddServer during
// setup; after setup it satisfies the Executor interface.
type MCPExecutor struct {
	servers map[string]*mcp.Client
}

// NewExecutor returns an MCP-backed executor with no servers registered.
// Call AddServer to attach MCP clients before using it.
func NewExecutor() *MCPExecutor {
	return &MCPExecutor{servers: make(map[string]*mcp.Client)}
}

// AddServer registers an MCP client under the given name.
func (e *MCPExecutor) AddServer(name string, client *mcp.Client) {
	e.servers[name] = client
}

// Close shuts down all connected MCP servers.
func (e *MCPExecutor) Close() {
	for _, client := range e.servers {
		client.Close()
	}
}

// ListTools returns all tools advertised by all connected servers.
func (e *MCPExecutor) ListTools() ([]ToolInfo, error) {
	var all []ToolInfo
	for serverName, client := range e.servers {
		result, err := client.Call("tools/list", nil)
		if err != nil {
			return nil, fmt.Errorf("server %s: %w", serverName, err)
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
		for _, t := range resp.Tools {
			all = append(all, ToolInfo{
				Server:      serverName,
				Name:        t.Name,
				Description: t.Description,
				InputSchema: t.InputSchema,
			})
		}
	}
	return all, nil
}

// Execute calls a named tool on the named server.
func (e *MCPExecutor) Execute(server, tool string, args json.RawMessage) (json.RawMessage, error) {
	client, ok := e.servers[server]
	if !ok {
		return nil, fmt.Errorf("server not found: %s", server)
	}
	return client.Call("tools/call", map[string]any{
		"name":      tool,
		"arguments": args,
	})
}
