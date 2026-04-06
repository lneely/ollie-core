package tools

import (
	"encoding/json"
	"fmt"
	"ollie/mcp"
)

// ToolInfo describes a tool provided by an MCP server.
type ToolInfo struct {
	Server      string
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Executor routes tool calls to the MCP server that owns them.
type Executor struct {
	servers map[string]*mcp.Client
}

// NewExecutor creates an empty Executor.
func NewExecutor() *Executor {
	return &Executor{servers: make(map[string]*mcp.Client)}
}

// AddServer registers an MCP client under the given name.
func (e *Executor) AddServer(name string, client *mcp.Client) {
	e.servers[name] = client
}

// Close shuts down all connected MCP servers.
func (e *Executor) Close() {
	for _, client := range e.servers {
		client.Close()
	}
}

// ListTools returns all tools advertised by all connected servers.
func (e *Executor) ListTools() ([]ToolInfo, error) {
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
func (e *Executor) Execute(serverName, toolName string, args json.RawMessage) (json.RawMessage, error) {
	client, ok := e.servers[serverName]
	if !ok {
		return nil, fmt.Errorf("server not found: %s", serverName)
	}
	return client.Call("tools/call", map[string]any{
		"name":      toolName,
		"arguments": args,
	})
}
