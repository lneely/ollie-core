package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// Hook name constants for well-known agent lifecycle events.
const (
	HookAgentSpawn       = "agentSpawn"
	HookUserPromptSubmit = "userPromptSubmit"
	HookStop             = "stop"
)

const defaultHookTimeout = 60

// Hooks maps hook names to shell command strings.
type Hooks map[string]string

// HookResult holds the outcome of running a hook.
type HookResult struct {
	// Blocked is true when the hook wants to prevent the action (exit 2).
	// For Stop hooks, Blocked means "don't stop, continue".
	Blocked bool
	// Context is stdout from the hook, injected into the conversation.
	Context string
}

// Run executes the hook for the named event, sending payload as JSON on stdin.
// Returns a HookResult describing whether the hook blocked and any context.
//
// Exit codes:
//   - 0: success. Stdout is added as conversation context.
//   - 2: block. For UserPromptSubmit: don't send the prompt.
//     For Stop: don't stop, continue with stderr as the next prompt.
//   - other: non-blocking warning (stderr logged, execution continues).
func (h Hooks) Run(ctx context.Context, name string, payload any) HookResult {
	cmdStr := h[name]
	if cmdStr == "" {
		return HookResult{}
	}

	payloadJSON, _ := json.Marshal(payload)

	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	cmd.Stdin = bytes.NewReader(payloadJSON)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return HookResult{}
	}
	go func() { done <- cmd.Wait() }()

	timeout := time.After(time.Duration(defaultHookTimeout) * time.Second)
	select {
	case err := <-done:
		exitCode := 0
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				exitCode = exitErr.ExitCode()
			} else {
				return HookResult{}
			}
		}

		switch exitCode {
		case 0:
			return HookResult{Context: strings.TrimSpace(stdout.String())}
		case 2:
			return HookResult{
				Blocked: true,
				Context: strings.TrimSpace(stderr.String()),
			}
		default:
			// Non-blocking error — stderr is just a warning, ignore.
			return HookResult{}
		}

	case <-ctx.Done():
		cmd.Process.Kill() //nolint:errcheck
		<-done
		return HookResult{}

	case <-timeout:
		cmd.Process.Kill() //nolint:errcheck
		<-done
		return HookResult{}
	}
}
