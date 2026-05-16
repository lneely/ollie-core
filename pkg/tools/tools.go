// Package tools defines the Server and Dispatcher interfaces and their
// default implementations.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
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
}

// Dispatcher routes tool calls to the server that owns them.
type Dispatcher interface {
	AddServer(name string, s Server)
	GetServer(name string) (Server, bool)
	ListTools() ([]ToolInfo, error)
	Dispatch(ctx context.Context, server, tool string, args json.RawMessage) (json.RawMessage, error)
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

// GetServer returns the Server registered under the given name, if any.
func (d *dispatcher) GetServer(name string) (Server, bool) {
	s, ok := d.servers[name]
	return s, ok
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

// CWDSetter is implemented by tool servers that accept a dynamic working
// directory. SetCWD updates the directory used for subsequent tool calls.
type CWDSetter interface {
	SetCWD(string)
}

// EnvSetter is implemented by tool servers that accept per-session environment
// variables. SetEnv adds a key=value pair to the command environment.
type EnvSetter interface {
	SetEnv(key, value string)
}

// TrustedToolsSetter is implemented by tool servers that support a trusted-tool
// list. Tools in the list bypass the Confirm callback.
type TrustedToolsSetter interface {
	SetTrustedTools(tools []string)
}

// LockDirSetter is implemented by tool servers that use advisory flock files
// for parallel-step coordination. SetLockDir must be called once at session init.
type LockDirSetter interface {
	SetLockDir(dir string)
}

// ToolRestrictionSetter is implemented by tool servers that support restricting
// which executors and tool scripts are available.
type ToolRestrictionSetter interface {
	SetAllowExecutors(names []string)
	SetAllowTools(names []string)
}

// ParallelClassifier is implemented by tool servers that can report whether a
// named tool is safe to run concurrently with other read-class tools.
// Returns false for unknown tools (conservative default).
type ParallelClassifier interface {
	IsParallelRead(name string) bool
}
