package agent

import (
	"context"
	"testing"

	"ollie/pkg/backend"
)

// TestTextToolCall_DetectedAsUnsupported verifies that when a model emits a tool
// call as plain text (no API-level tool calls) the loop returns a
// ToolUnsupportedError so freeloader can blacklist and cycle.
func TestTextToolCall_DetectedAsUnsupported(t *testing.T) {
	be := defaultBE()
	// Model responds with text that mimics a tool invocation instead of using
	// the function-calling API.
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		return textStream(`execute_code: steps=[{code: "print('hi')"}]`), nil
	}

	c := newCore(t, be, nil)
	c.cfg.Tools = []backend.Tool{{Name: "execute_code", Description: "run code"}}

	evs := collectEvents(context.Background(), c, "run something")

	errEvs := byRole(evs, "error")
	if len(errEvs) == 0 {
		t.Fatal("expected an error event; got none")
	}
	// The error must mention tool_unsupported behaviour (not a plain backend error).
	if got := errEvs[0]; got == "" {
		t.Error("error event content is empty")
	}
}

// TestTextToolCall_NoFalsePositive verifies that a normal text response that
// does not contain a tool-call pattern is not mistakenly flagged.
func TestTextToolCall_NoFalsePositive(t *testing.T) {
	be := defaultBE()
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		return textStream("Here is how execute_code works in general."), nil
	}

	c := newCore(t, be, nil)
	c.cfg.Tools = []backend.Tool{{Name: "execute_code", Description: "run code"}}

	evs := collectEvents(context.Background(), c, "explain tools")

	errEvs := byRole(evs, "error")
	if len(errEvs) != 0 {
		t.Errorf("unexpected error events: %v", errEvs)
	}
	assistEvs := byRole(evs, "assistant")
	if len(assistEvs) == 0 {
		t.Error("expected assistant response; got none")
	}
}

// TestTextToolCall_NoToolsConfigured verifies that when no tools are configured
// the detection does not fire (nothing to detect against).
func TestTextToolCall_NoToolsConfigured(t *testing.T) {
	be := defaultBE()
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		return textStream(`execute_code: steps=[{code: "print('hi')"}]`), nil
	}

	c := newCore(t, be, nil)
	// No tools configured.

	evs := collectEvents(context.Background(), c, "run something")

	errEvs := byRole(evs, "error")
	if len(errEvs) != 0 {
		t.Errorf("unexpected error with no tools configured: %v", errEvs)
	}
}
