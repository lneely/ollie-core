package mcp

import (
	"fmt"
	"os"
	"os/exec"
)

// STDIOTransport launches a subprocess and connects a Client to its stdin/stdout.
type STDIOTransport struct {
	command string
	args    []string
	env     map[string]string
}

// NewSTDIOTransport creates a transport that will launch the given command.
func NewSTDIOTransport(command string, args []string, env map[string]string) *STDIOTransport {
	return &STDIOTransport{command: command, args: args, env: env}
}

// Connect launches the subprocess and returns a connected Client.
func (t *STDIOTransport) Connect() (*Client, error) {
	cmd := exec.Command(t.command, t.args...)
	if len(t.env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range t.env {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, os.ExpandEnv(v)))
		}
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	client := newClientWithProcess(stdout, stdin, cmd)

	// MCP initialization handshake.
	if _, err := client.Call("initialize", map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo":      map[string]interface{}{"name": "ollie", "version": "0.1"},
	}); err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if err := client.Notify("notifications/initialized", nil); err != nil {
		return nil, fmt.Errorf("notifications/initialized: %w", err)
	}

	return client, nil
}
