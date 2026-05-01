package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"ollie/pkg/backend"
)

// alwaysFailStream returns a backend that issues a single tool call on every
// invocation, never producing a text-only (stop) response.
func alwaysFailStream() func(context.Context, []backend.Message, []backend.Tool, backend.GenerationParams) (<-chan backend.StreamEvent, error) {
	return func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		ch := make(chan backend.StreamEvent, 1)
		ch <- backend.StreamEvent{
			ToolCalls:  []backend.ToolCall{{ID: "c1", Name: "bad_tool", Arguments: json.RawMessage(`{}`)}},
			Done:       true,
			StopReason: "tool_calls",
			Usage:      backend.Usage{InputTokens: 10, OutputTokens: 5},
		}
		close(ch)
		return ch, nil
	}
}

func TestConsecutiveErrors_HardLimit(t *testing.T) {
	be := defaultBE()
	be.respond = alwaysFailStream()

	c := newCore(t, be, nil)
	c.cfg.Exec = func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
		return "", fmt.Errorf("always fails")
	}

	evs := collectEvents(context.Background(), c, "do something")

	// Should see an error event about consecutive tool errors.
	errs := byRole(evs, "error")
	found := false
	for _, e := range errs {
		if strings.Contains(e, "consecutive tool errors") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'consecutive tool errors' in error events; got %v", errs)
	}
}

func TestConsecutiveErrors_SoftLimitNudge(t *testing.T) {
	var rounds atomic.Int32
	be := defaultBE()
	be.respond = func(_ context.Context, msgs []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		n := int(rounds.Add(1))
		// After soft limit, check that the nudge was injected into the
		// conversation history. Stop the loop by returning text only.
		if n > consecutiveErrorSoftLimit {
			for _, m := range msgs {
				if m.Role == "user" && strings.Contains(m.Content, "your last several tool calls all failed") {
					return textStream("giving up"), nil
				}
			}
			// Nudge not found — keep going (will hit hard limit if broken).
			ch := make(chan backend.StreamEvent, 1)
			ch <- backend.StreamEvent{
				ToolCalls:  []backend.ToolCall{{ID: "c1", Name: "bad_tool", Arguments: json.RawMessage(`{}`)}},
				Done:       true,
				StopReason: "tool_calls",
				Usage:      backend.Usage{InputTokens: 10, OutputTokens: 5},
			}
			close(ch)
			return ch, nil
		}
		ch := make(chan backend.StreamEvent, 1)
		ch <- backend.StreamEvent{
			ToolCalls:  []backend.ToolCall{{ID: "c1", Name: "bad_tool", Arguments: json.RawMessage(`{}`)}},
			Done:       true,
			StopReason: "tool_calls",
			Usage:      backend.Usage{InputTokens: 10, OutputTokens: 5},
		}
		close(ch)
		return ch, nil
	}

	c := newCore(t, be, nil)
	c.cfg.Exec = func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
		return "", fmt.Errorf("always fails")
	}

	evs := collectEvents(context.Background(), c, "do something")

	// The model should have seen the nudge and responded with text, ending the loop.
	texts := byRole(evs, "assistant")
	found := false
	for _, txt := range texts {
		if strings.Contains(txt, "giving up") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected model to receive nudge and respond with 'giving up'")
	}
}

func TestConsecutiveErrors_ResetOnSuccess(t *testing.T) {
	var rounds atomic.Int32
	be := defaultBE()
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		n := int(rounds.Add(1))
		// Rounds 1-4: fail. Round 5: succeed. Rounds 6-9: fail. Round 10: succeed. Round 11: text.
		// This ensures the counter resets and we never hit the soft limit.
		if n == 5 || n == 10 {
			ch := make(chan backend.StreamEvent, 1)
			ch <- backend.StreamEvent{
				ToolCalls:  []backend.ToolCall{{ID: "c1", Name: "good_tool", Arguments: json.RawMessage(`{}`)}},
				Done:       true,
				StopReason: "tool_calls",
				Usage:      backend.Usage{InputTokens: 10, OutputTokens: 5},
			}
			close(ch)
			return ch, nil
		}
		if n >= 11 {
			return textStream("done"), nil
		}
		ch := make(chan backend.StreamEvent, 1)
		ch <- backend.StreamEvent{
			ToolCalls:  []backend.ToolCall{{ID: "c1", Name: "bad_tool", Arguments: json.RawMessage(`{}`)}},
			Done:       true,
			StopReason: "tool_calls",
			Usage:      backend.Usage{InputTokens: 10, OutputTokens: 5},
		}
		close(ch)
		return ch, nil
	}

	c := newCore(t, be, nil)
	c.cfg.Exec = func(_ context.Context, name string, _ json.RawMessage) (string, error) {
		if name == "good_tool" {
			return "ok", nil
		}
		return "", fmt.Errorf("fails")
	}

	evs := collectEvents(context.Background(), c, "do something")

	// Should complete normally — no consecutive-error abort.
	errs := byRole(evs, "error")
	for _, e := range errs {
		if strings.Contains(e, "consecutive tool errors") {
			t.Errorf("unexpected hard limit error; counter should have reset: %s", e)
		}
	}
	texts := byRole(evs, "assistant")
	found := false
	for _, txt := range texts {
		if strings.Contains(txt, "done") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected loop to complete normally with 'done' response")
	}
}
