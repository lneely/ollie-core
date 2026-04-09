package core

import "os/exec"

// Hook name constants for well-known agent lifecycle events.
const (
	HookAgentSpawn       = "agentSpawn"
	HookUserPromptSubmit = "userPromptSubmit"
	HookStop             = "stop"
)

// Hooks maps hook names to shell command strings. Any program that has
// lifecycle events can use this type to let users attach shell commands
// to those events.
type Hooks map[string]string

// Run executes the shell command registered for the named hook, if any.
func (h Hooks) Run(name string) {
	if cmd := h[name]; cmd != "" {
		exec.Command("sh", "-c", cmd).Run() //nolint:errcheck
	}
}
