package agent

import (
	"context"
	"encoding/json"
	"testing"

	"ollie/pkg/backend"
)

// TestMaxStepsZeroUnlimited verifies that MaxSteps=0 does not trigger the
// guardrail — the loop runs until the model stops calling tools.
func TestMaxStepsZeroUnlimited(t *testing.T) {
	var steps int
	mb := &mockBackend{
		responses: []mockResponse{
			{toolCalls: []backend.ToolCall{{ID: "1", Name: "tool", Arguments: json.RawMessage(`{}`)}}, stopReason: "tool_calls"},
			{toolCalls: []backend.ToolCall{{ID: "2", Name: "tool", Arguments: json.RawMessage(`{}`)}}, stopReason: "tool_calls"},
			{content: "done", stopReason: "stop"},
		},
	}
	cfg := agentConfig{
		Backend:  mb,
		Tools:    []backend.Tool{{Name: "tool"}},
		MaxSteps: 0,
		Exec: func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
			steps++
			return "ok", nil
		},
	}
	if err := run(context.Background(), cfg, newState()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if steps != 2 {
		t.Errorf("expected 2 tool executions, got %d", steps)
	}
}

// TestMaxStepsSoftNudge verifies that when MaxSteps is reached the loop injects
// the budget-exhausted nudge message, emits a maxsteps event, and exits cleanly
// (no error returned). The model is given one final tool-free turn.
func TestMaxStepsSoftNudge(t *testing.T) {
	var nudgeSeen bool
	var maxstepsEventSeen bool

	// Backend: two tool-calling rounds, then a final text turn.
	mb := &mockBackend{
		responses: []mockResponse{
			{toolCalls: []backend.ToolCall{{ID: "1", Name: "tool", Arguments: json.RawMessage(`{}`)}}, stopReason: "tool_calls"},
			{toolCalls: []backend.ToolCall{{ID: "2", Name: "tool", Arguments: json.RawMessage(`{}`)}}, stopReason: "tool_calls"},
			{content: "wrapping up", stopReason: "stop"},
		},
	}

	// MaxSteps=1 means the guardrail fires after completing step 0 (the first
	// tool round), before step 1 would begin.
	cfg := agentConfig{
		Backend:  mb,
		Tools:    []backend.Tool{{Name: "tool"}},
		MaxSteps: 1,
		Exec: func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
			return "ok", nil
		},
		Output: func(ev Event) {
			if ev.Role == "maxsteps" {
				maxstepsEventSeen = true
			}
		},
	}

	// Intercept state updates to detect the nudge message.
	s := newState()
	origUpdate := s.update
	_ = origUpdate // state.update is not a field; we'll check history post-run instead.

	if err := run(context.Background(), cfg, s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !maxstepsEventSeen {
		t.Error("expected maxsteps event to be emitted")
	}

	// Confirm the nudge message is present in conversation history.
	for _, m := range s.history() {
		if m.Role == "user" && len(m.Content) > 0 {
			if contains(m.Content, "step budget exhausted") {
				nudgeSeen = true
				break
			}
		}
	}
	if !nudgeSeen {
		t.Error("expected step-budget nudge message in conversation history")
	}
}

// TestMaxStepsExactBoundary checks that with MaxSteps=N the loop completes
// exactly N tool rounds before nudging.
func TestMaxStepsExactBoundary(t *testing.T) {
	var toolRounds int

	const limit = 3

	// Build limit+1 tool responses so the model would run forever without the cap.
	var responses []mockResponse
	for i := range limit + 1 {
		responses = append(responses, mockResponse{
			toolCalls:  []backend.ToolCall{{ID: fmt.Sprintf("%d", i+1), Name: "tool", Arguments: json.RawMessage(`{}`)}},
			stopReason: "tool_calls",
		})
	}
	responses = append(responses, mockResponse{content: "done", stopReason: "stop"})

	mb := &mockBackend{responses: responses}
	cfg := agentConfig{
		Backend:  mb,
		Tools:    []backend.Tool{{Name: "tool"}},
		MaxSteps: limit,
		Exec: func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
			toolRounds++
			return "ok", nil
		},
	}
	if err := run(context.Background(), cfg, newState()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if toolRounds != limit {
		t.Errorf("expected %d tool rounds, got %d", limit, toolRounds)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(func() bool {
			for i := 0; i <= len(s)-len(substr); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		})())
}
