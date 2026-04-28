package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ollie/pkg/backend"
)

// TestTruncation_LargeResultTruncated verifies that a tool result exceeding
// ToolResultMaxBytes is truncated and a continuation hint is appended.
func TestTruncation_LargeResultTruncated(t *testing.T) {
	const maxBytes = 64
	large := strings.Repeat("x", 200)

	be := defaultBE()
	be.respond = toolsStream([]backend.ToolCall{
		{ID: "1", Name: "execute_code", Arguments: json.RawMessage(`{}`)},
	})

	c := newCore(t, be, nil)
	c.cfg.ClassifyTool = nil
	c.cfg.ToolResultMaxBytes = maxBytes
	c.cfg.Exec = func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
		return large, nil
	}

	evs := collectEvents(context.Background(), c, "big result")

	toolEvs := byRole(evs, "tool")
	if len(toolEvs) != 1 {
		t.Fatalf("tool events = %d; want 1", len(toolEvs))
	}
	result := toolEvs[0]
	if !strings.HasPrefix(result, strings.Repeat("x", maxBytes)) {
		t.Errorf("result does not start with %d x's: %q", maxBytes, result[:min(len(result), 80)])
	}
	if !strings.Contains(result, "result truncated") {
		t.Errorf("truncation hint missing from result: %q", result)
	}
	if !strings.Contains(result, "offset=64") {
		t.Errorf("offset hint missing from result: %q", result)
	}
}

// TestTruncation_SmallResultNotTruncated verifies that results within the limit
// pass through unchanged.
func TestTruncation_SmallResultNotTruncated(t *testing.T) {
	be := defaultBE()
	be.respond = toolsStream([]backend.ToolCall{
		{ID: "1", Name: "execute_code", Arguments: json.RawMessage(`{}`)},
	})

	c := newCore(t, be, nil)
	c.cfg.ToolResultMaxBytes = 8192
	c.cfg.Exec = func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
		return "short result", nil
	}

	evs := collectEvents(context.Background(), c, "small result")

	toolEvs := byRole(evs, "tool")
	if len(toolEvs) != 1 {
		t.Fatalf("tool events = %d; want 1", len(toolEvs))
	}
	if toolEvs[0] != "short result" {
		t.Errorf("result = %q; want %q", toolEvs[0], "short result")
	}
}

// TestTruncation_ZeroDisabled verifies that ToolResultMaxBytes=0 disables
// truncation entirely regardless of result size.
func TestTruncation_ZeroDisabled(t *testing.T) {
	large := strings.Repeat("y", 100_000)

	be := defaultBE()
	be.respond = toolsStream([]backend.ToolCall{
		{ID: "1", Name: "execute_code", Arguments: json.RawMessage(`{}`)},
	})

	c := newCore(t, be, nil)
	c.cfg.ToolResultMaxBytes = 0 // disabled
	c.cfg.Exec = func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
		return large, nil
	}

	evs := collectEvents(context.Background(), c, "no limit")

	toolEvs := byRole(evs, "tool")
	if len(toolEvs) != 1 {
		t.Fatalf("tool events = %d; want 1", len(toolEvs))
	}
	if toolEvs[0] != large {
		t.Errorf("result was truncated when limit=0 (len=%d, want %d)", len(toolEvs[0]), len(large))
	}
}

// TestTruncation_ErrorNotTruncated verifies that error results are never
// truncated (the model needs the full error message).
func TestTruncation_ErrorNotTruncated(t *testing.T) {
	const maxBytes = 10
	longErr := strings.Repeat("e", 200)

	be := defaultBE()
	be.respond = toolsStream([]backend.ToolCall{
		{ID: "1", Name: "execute_code", Arguments: json.RawMessage(`{}`)},
	})

	c := newCore(t, be, nil)
	c.cfg.ToolResultMaxBytes = maxBytes
	c.cfg.Exec = func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
		return "", &mockErr{longErr}
	}

	evs := collectEvents(context.Background(), c, "error result")

	toolEvs := byRole(evs, "tool")
	if len(toolEvs) != 1 {
		t.Fatalf("tool events = %d; want 1", len(toolEvs))
	}
	if strings.Contains(toolEvs[0], "result truncated") {
		t.Errorf("error result was truncated; should be preserved in full")
	}
}

// TestTruncation_CachedResultAlreadyTruncated verifies that a cache hit on a
// previously-truncated result returns the truncated form, not the original.
func TestTruncation_CachedResultAlreadyTruncated(t *testing.T) {
	const maxBytes = 64
	large := strings.Repeat("z", 200)

	be := defaultBE()
	be.respond = multiTurnToolsStream([][]backend.ToolCall{
		{{ID: "1", Name: "file_read", Arguments: json.RawMessage(`{"path":"a.txt"}`)}},
		{{ID: "2", Name: "file_read", Arguments: json.RawMessage(`{"path":"a.txt"}`)}},
	})

	c := newCore(t, be, nil)
	c.cfg.ClassifyTool = func(string) bool { return true } // read-safe → cacheable
	c.cfg.ToolResultMaxBytes = maxBytes
	c.cfg.Exec = func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
		return large, nil
	}

	evs := collectEvents(context.Background(), c, "cached truncated")

	toolEvs := byRole(evs, "tool")
	if len(toolEvs) != 2 {
		t.Fatalf("tool events = %d; want 2", len(toolEvs))
	}
	// Both should be the truncated form.
	for i, ev := range toolEvs {
		if !strings.Contains(ev, "result truncated") {
			t.Errorf("event[%d] missing truncation hint: %q", i, ev[:min(len(ev), 80)])
		}
	}
}

