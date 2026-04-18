package mcp

import (
	"fmt"
	"os"
	"os/exec"
)

// STDIOTransport launches a subprocess and connects a Client to its stdin/stdout.
type STDIOTransport struct {
	command  string
	args     []string
	env      map[string]string
	extraEnv map[string]string // session-scoped vars used to expand ${VAR} in env values
}

// NewSTDIOTransport creates a transport that will launch the given command.
// sessionEnv provides values for ${VAR} expansion in env values, taking
// precedence over the process environment.
func NewSTDIOTransport(command string, args []string, env map[string]string, sessionEnv map[string]string) *STDIOTransport {
	return &STDIOTransport{command: command, args: args, env: env, extraEnv: sessionEnv}
}

// Connect launches the subprocess and returns a connected Client.
func (t *STDIOTransport) Connect() (*Client, error) {
	cmd := exec.Command(t.command, t.args...)
	if len(t.env) > 0 {
		cmd.Env = os.Environ()
		lookup := func(name string) string {
			if v, ok := t.extraEnv[name]; ok {
				return v
			}
			return os.Getenv(name)
		}
		for k, v := range t.env {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, os.Expand(v, lookup)))
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
