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
	HookPreTurn  = "preTurn"
	HookPostTurn = "postTurn"
	HookPreCompact  = "preCompact"
	HookPostCompact = "postCompact"
)

const defaultHookTimeout = 60

// hookTimeout is the hook execution timeout in seconds. Overridable in tests.
var hookTimeout = defaultHookTimeout

// Hooks maps hook names to one or more shell commands.
type Hooks map[string][]string

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

// Run executes all commands for the named hook in order, sending payload as
// JSON on stdin for each. Returns a combined HookResult.
//
// Exit codes per command:
//   - 0: success. Stdout is appended to combined context.
//   - 2: block. Stops execution immediately and returns blocked.
//   - other: non-blocking warning (stderr logged, execution continues).
func (h Hooks) Run(ctx context.Context, name string, payload any) HookResult {
	cmds := h[name]
	if len(cmds) == 0 {
		return HookResult{}
	}

	payloadJSON, _ := json.Marshal(payload)
	var cwd string
	if m, ok := payload.(map[string]string); ok {
		cwd = m["cwd"]
	}

	var contextParts []string
	for _, cmdStr := range cmds {
		clog.Debug("hook %s: cmd=%q", name, cmdStr)
		result := runHookCmd(ctx, name, cmdStr, payloadJSON, cwd)
		if !result.Ran {
			continue
		}
		if result.Blocked {
			return HookResult{Ran: true, Blocked: true, Context: result.Context}
		}
		if result.Context != "" {
			contextParts = append(contextParts, result.Context)
		}
	}
	return HookResult{Ran: true, Context: strings.Join(contextParts, "\n")}
}

func runHookCmd(ctx context.Context, name, cmdStr string, payloadJSON []byte, cwd string) HookResult {
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	cmd.Stdin = bytes.NewReader(payloadJSON)
	if cwd != "" {
		cmd.Dir = cwd
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		clog.Debug("hook %s: start error: %v", name, err)
		return HookResult{}
	}
	go func() { done <- cmd.Wait() }()

	timeout := time.After(time.Duration(hookTimeout) * time.Second)
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
		clog.Debug("hook %s: timed out after %ds", name, hookTimeout)
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
