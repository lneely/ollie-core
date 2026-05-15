package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ollie/pkg/backend"
)

// TestMaxStepsZeroUnlimited verifies that MaxSteps=0 does not trigger the
// guardrail — the loop runs until the model stops calling tools.
func TestMaxStepsZeroUnlimited(t *testing.T) {
	var steps int
	mb := &mockBackend{
		respond: sequentialStream([]mockResponse{
			{toolCalls: []backend.ToolCall{{ID: "1", Name: "tool", Arguments: json.RawMessage(`{}`)}}, stopReason: "tool_calls"},
			{toolCalls: []backend.ToolCall{{ID: "2", Name: "tool", Arguments: json.RawMessage(`{}`)}}, stopReason: "tool_calls"},
			{content: "done", stopReason: "stop"},
		}),
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
		respond: sequentialStream([]mockResponse{
			{toolCalls: []backend.ToolCall{{ID: "1", Name: "tool", Arguments: json.RawMessage(`{}`)}}, stopReason: "tool_calls"},
			{toolCalls: []backend.ToolCall{{ID: "2", Name: "tool", Arguments: json.RawMessage(`{}`)}}, stopReason: "tool_calls"},
			{content: "wrapping up", stopReason: "stop"},
		}),
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
		if m.Role == "user" && strings.Contains(m.Content, "step budget exhausted") {
			nudgeSeen = true
			break
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

	mb := &mockBackend{respond: sequentialStream(responses)}
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

// TestPlanReinjection verifies that the plan file contents are re-surfaced
// into the conversation every planReinjectInterval tool rounds.
func TestPlanReinjection(t *testing.T) {
	// Write a plan file to a temp location.
	planPath := filepath.Join(t.TempDir(), "plan")
	os.WriteFile(planPath, []byte("- [ ] step one\n- [ ] step two\n"), 0644)

	// We need planReinjectInterval+1 tool rounds so the re-injection fires
	// at step == planReinjectInterval (0-indexed, checked after increment).
	// The check is: step > 0 && step%planReinjectInterval == 0, and step is
	// incremented at the end of the loop, so it fires after the 10th round.
	n := planReinjectInterval + 1
	var responses []mockResponse
	for i := range n {
		responses = append(responses, mockResponse{
			toolCalls:  []backend.ToolCall{{ID: fmt.Sprintf("%d", i+1), Name: "tool", Arguments: json.RawMessage(`{}`)}},
			stopReason: "tool_calls",
		})
	}
	responses = append(responses, mockResponse{content: "done", stopReason: "stop"})

	mb := &mockBackend{respond: sequentialStream(responses)}
	cfg := agentConfig{
		Backend:  mb,
		Tools:    []backend.Tool{{Name: "tool"}},
		PlanFile: planPath,
		Exec: func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
			return "ok", nil
		},
	}

	s := newState()
	if err := run(context.Background(), cfg, s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var planSeen bool
	for _, m := range s.history() {
		if m.Role == "user" && strings.Contains(m.Content, "step one") && strings.Contains(m.Content, "your current plan") {
			planSeen = true
			break
		}
	}
	if !planSeen {
		t.Error("expected plan re-injection message in conversation history")
	}
}
