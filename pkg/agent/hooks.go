package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	olog "ollie/pkg/log"
	"ollie/pkg/paths"
)

// Hook name constants for well-known agent lifecycle events.
const (
	HookAgentSpawn  = "agentSpawn"
	HookPreTurn     = "preTurn"
	HookPostTurn    = "postTurn"
	HookPreTool     = "preTool"
	HookPostTool    = "postTool"
	HookPreCompact  = "preCompact"
	HookPostCompact = "postCompact"
	HookTurnError   = "turnError"
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
	// Handled is true when all hook commands exited 0 (no warnings, not blocked).
	// Use this to check whether a hook fully handled an event (e.g. turnError).
	Handled bool
	// Blocked is true when the hook wants to prevent the action (exit 2).
	// For Stop hooks, Blocked means "don't stop, continue".
	Blocked bool
	// Context is stdout from the hook, injected into the conversation.
	Context string
	// Warning, if non-empty, is a message about hook execution problems
	// (e.g. timeout, start failure) that should be surfaced to the user.
	Warning string
}

// Run executes all commands for the named hook in order, sending payload as
// JSON on stdin for each. Returns a combined HookResult.
//
// Exit codes per command:
//   - 0: success. Stdout is appended to combined context.
//   - 2: block. Stops execution immediately and returns blocked.
//   - other: non-blocking warning (stderr logged, execution continues).
func (h Hooks) Run(ctx context.Context, name string, payload any, log *olog.Logger) HookResult {
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
	var warnings []string
	allHandled := true
	for _, cmdStr := range cmds {
		log.Debug("hook %s: cmd=%q", name, cmdStr)
		result := runHookCmd(ctx, name, cmdStr, payloadJSON, cwd, log)
		if result.Warning != "" {
			warnings = append(warnings, result.Warning)
			allHandled = false
		}
		if !result.Ran {
			allHandled = false
			continue
		}
		if result.Blocked {
			return HookResult{Ran: true, Blocked: true, Context: result.Context, Warning: strings.Join(warnings, "; ")}
		}
		if result.Context != "" {
			contextParts = append(contextParts, result.Context)
		}
	}
	return HookResult{Ran: true, Handled: allHandled, Context: strings.Join(contextParts, "\n"), Warning: strings.Join(warnings, "; ")}
}

func runHookCmd(ctx context.Context, name, cmdStr string, payloadJSON []byte, cwd string, log *olog.Logger) HookResult {
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = bytes.NewReader(payloadJSON)
	if cwd != "" {
		cmd.Dir = paths.ExpandHome(cwd)
	}

	// Inject payload map keys as OLLIE_* environment variables so hook
	// commands can use $OLLIE_SESSION_ID, $OLLIE_MODEL, etc. without
	// parsing the JSON payload on stdin.
	var payloadMap map[string]string
	if err := json.Unmarshal(payloadJSON, &payloadMap); err == nil {
		env := cmd.Environ()
		for k, v := range payloadMap {
			env = append(env, "OLLIE_"+strings.ToUpper(k)+"="+v)
		}
		cmd.Env = env
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		log.Debug("hook %s: start error: %v", name, err)
		return HookResult{Ran: true, Warning: fmt.Sprintf("hook %s: failed to start: %v", name, err)}
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
				log.Debug("hook %s: wait error: %v", name, err)
				return HookResult{}
			}
		}
		switch exitCode {
		case 0:
			out := strings.TrimSpace(stdout.String())
			log.Debug("hook %s: exit=0 context_len=%d", name, len(out))
			return HookResult{Ran: true, Context: out}
		case 2:
			msg := strings.TrimSpace(stderr.String())
			log.Debug("hook %s: exit=2 (blocked) msg=%q", name, msg)
			return HookResult{Ran: true, Blocked: true, Context: msg}
		default:
			log.Debug("hook %s: exit=%d (non-blocking error) stderr=%q", name, exitCode, stderr.String())
			return HookResult{Ran: false, Warning: fmt.Sprintf("hook %s: exit %d", name, exitCode)}
		}
	case <-ctx.Done():
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) //nolint:errcheck
		<-done
		log.Debug("hook %s: cancelled (context done)", name)
		return HookResult{}
	case <-timeout:
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) //nolint:errcheck
		<-done
		log.Debug("hook %s: timed out after %ds", name, hookTimeout)
		return HookResult{Ran: true, Warning: fmt.Sprintf("hook %s: timed out after %ds", name, hookTimeout)}
	}
}

// hooksRan returns a display string for N hooks having run, e.g. "1 hook run".
func hooksRan(n int) string {
	if n == 1 {
		return "1 hook run"
	}
	return fmt.Sprintf("%d hooks run", n)
}
