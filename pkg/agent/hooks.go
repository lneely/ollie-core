package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Hook name constants for well-known agent lifecycle events.
const (
	HookAgentSpawn       = "agentSpawn"
	HookUserPromptSubmit = "userPromptSubmit"
	HookStop             = "stop"
	HookPreCompact       = "preCompact"
	HookPostCompact      = "postCompact"
	HookPreClear         = "preClear"
	HookPostClear        = "postClear"
)

const defaultHookTimeout = 60

// Hooks maps hook names to shell command strings.
type Hooks map[string]string

// HookResult holds the outcome of running a hook.
type HookResult struct {
	// Ran is true when a hook command was configured and executed.
	Ran bool
	// Blocked is true when the hook wants to prevent the action (exit 2).
	// For Stop hooks, Blocked means "don't stop, continue".
	Blocked bool
	// Context is stdout from the hook, injected into the conversation.
	Context string
}

// Run executes the hook for the named event, sending payload as JSON on stdin.
// Returns a HookResult describing whether the hook ran, blocked, and any context.
//
// Exit codes:
//   - 0: success. Stdout is added as conversation context.
//   - 2: block. For UserPromptSubmit: don't send the prompt.
//     For Stop: don't stop, continue with stderr as the next prompt.
//   - other: non-blocking warning (stderr logged, execution continues).
func (h Hooks) Run(ctx context.Context, name string, payload any) HookResult {
	cmdStr := h[name]
	clog.Debug("hook %s: cmd=%q", name, cmdStr)
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
		clog.Debug("hook %s: start error: %v", name, err)
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
				clog.Debug("hook %s: wait error: %v", name, err)
				return HookResult{}
			}
		}

		switch exitCode {
		case 0:
			out := strings.TrimSpace(stdout.String())
			clog.Debug("hook %s: exit=0 context_len=%d", name, len(out))
			return HookResult{Ran: true, Context: out}
		case 2:
			msg := strings.TrimSpace(stderr.String())
			clog.Debug("hook %s: exit=2 (blocked) msg=%q", name, msg)
			return HookResult{Ran: true, Blocked: true, Context: msg}
		default:
			clog.Debug("hook %s: exit=%d (non-blocking error) stderr=%q", name, exitCode, stderr.String())
			return HookResult{Ran: true}
		}

	case <-ctx.Done():
		cmd.Process.Kill() //nolint:errcheck
		<-done
		clog.Debug("hook %s: cancelled (context done)", name)
		return HookResult{}

	case <-timeout:
		cmd.Process.Kill() //nolint:errcheck
		<-done
		clog.Debug("hook %s: timed out after %ds", name, defaultHookTimeout)
		return HookResult{}
	}
}

// hooksRan returns a display string for N hooks having run, e.g. "1 hook run".
func hooksRan(n int) string {
	if n == 1 {
		return "1 hook run"
	}
	return fmt.Sprintf("%d hooks run", n)
}
