// Package tools defines the Server and Dispatcher interfaces and their
// default implementations.
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"ollie/pkg/mcp"
)

// ToolInfo describes a tool provided by a server.
type ToolInfo struct {
	Server      string
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Server is the interface satisfied by any tool server.
type Server interface {
	ListTools() ([]ToolInfo, error)
	CallTool(ctx context.Context, tool string, args json.RawMessage) (json.RawMessage, error)
	Close()
}

// Dispatcher routes tool calls to the server that owns them.
type Dispatcher interface {
	AddServer(name string, s Server)
	ListTools() ([]ToolInfo, error)
	Dispatch(ctx context.Context, server, tool string, args json.RawMessage) (json.RawMessage, error)
	Close()
}

// dispatcher is the default Dispatcher backed by registered Server instances.
type dispatcher struct {
	servers map[string]Server
}

// NewDispatcher returns a Dispatcher with no servers registered.
func NewDispatcher() Dispatcher {
	return &dispatcher{servers: make(map[string]Server)}
}

// NewDispatcherFunc returns a factory that builds a fresh Dispatcher on each
// call, invoking each decl to create its Server. Pass the result to
// agent.AgentCoreConfig.NewDispatcher.
func NewDispatcherFunc(decls map[string]func() Server) func() Dispatcher {
	return func() Dispatcher {
		d := NewDispatcher()
		for name, decl := range decls {
			d.AddServer(name, decl())
		}
		return d
	}
}

// AddServer registers a Server under the given name.
func (d *dispatcher) AddServer(name string, s Server) {
	d.servers[name] = s
}

// Close shuts down all registered servers.
func (d *dispatcher) Close() {
	for _, s := range d.servers {
		s.Close()
	}
}

// ListTools returns all tools advertised by all registered servers.
func (d *dispatcher) ListTools() ([]ToolInfo, error) {
	var all []ToolInfo
	for serverName, s := range d.servers {
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

// Dispatch calls a named tool on the named server.
func (d *dispatcher) Dispatch(ctx context.Context, server, tool string, args json.RawMessage) (json.RawMessage, error) {
	s, ok := d.servers[server]
	if !ok {
		return nil, fmt.Errorf("server not found: %s", server)
	}
	return s.CallTool(ctx, tool, args)
}

// NewServer wraps an mcp.Client as a Server.
func NewServer(client *mcp.Client) Server {
	return &mcpServer{client: client}
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
