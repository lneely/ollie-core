package agent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"ollie/pkg/backend"
)

// TestResultCache_HitSkipsExec verifies that a second call to a read-safe tool
// with identical arguments returns the cached result without calling Exec again.
func TestResultCache_HitSkipsExec(t *testing.T) {
	var execCount int32
	be := defaultBE()
	be.respond = toolsStream([]backend.ToolCall{
		{ID: "1", Name: "file_read", Arguments: json.RawMessage(`{"path":"a.txt"}`)},
		{ID: "2", Name: "file_read", Arguments: json.RawMessage(`{"path":"a.txt"}`)},
	})

	c := newCore(t, be, nil)
	c.cfg.ClassifyTool = func(string) bool { return true }
	c.cfg.Exec = func(_ context.Context, name string, _ json.RawMessage) (string, error) {
		atomic.AddInt32(&execCount, 1)
		return "contents of a.txt", nil
	}

	evs := collectEvents(context.Background(), c, "dup read")

	if n := atomic.LoadInt32(&execCount); n != 1 {
		t.Errorf("Exec called %d times; want 1 (second should be cached)", n)
	}
	toolEvs := byRole(evs, "tool")
	if len(toolEvs) != 2 {
		t.Errorf("tool events = %d; want 2", len(toolEvs))
	}
	for i, ev := range toolEvs {
		if ev != "contents of a.txt" {
			t.Errorf("tool event[%d] = %q; want cached value", i, ev)
		}
	}
}

// TestResultCache_DifferentArgsMiss verifies that the same tool name with
// different arguments produces two separate Exec calls (no false cache hits).
func TestResultCache_DifferentArgsMiss(t *testing.T) {
	var execCount int32
	be := defaultBE()
	be.respond = toolsStream([]backend.ToolCall{
		{ID: "1", Name: "file_read", Arguments: json.RawMessage(`{"path":"a.txt"}`)},
		{ID: "2", Name: "file_read", Arguments: json.RawMessage(`{"path":"b.txt"}`)},
	})

	c := newCore(t, be, nil)
	c.cfg.ClassifyTool = func(string) bool { return true }
	c.cfg.Exec = func(_ context.Context, name string, args json.RawMessage) (string, error) {
		atomic.AddInt32(&execCount, 1)
		return "result for " + string(args), nil
	}

	collectEvents(context.Background(), c, "different args")

	if n := atomic.LoadInt32(&execCount); n != 2 {
		t.Errorf("Exec called %d times; want 2 (different paths)", n)
	}
}

// TestResultCache_SerialToolNotCached verifies that non-read-safe tools are
// never cached: two identical calls both hit Exec.
func TestResultCache_SerialToolNotCached(t *testing.T) {
	var execCount int32
	be := defaultBE()
	be.respond = toolsStream([]backend.ToolCall{
		{ID: "1", Name: "file_write", Arguments: json.RawMessage(`{"path":"a.txt","content":"x"}`)},
		{ID: "2", Name: "file_write", Arguments: json.RawMessage(`{"path":"a.txt","content":"x"}`)},
	})

	c := newCore(t, be, nil)
	c.cfg.ClassifyTool = func(name string) bool { return false } // all serial
	c.cfg.Exec = func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
		atomic.AddInt32(&execCount, 1)
		return "ok", nil
	}

	collectEvents(context.Background(), c, "serial tool")

	if n := atomic.LoadInt32(&execCount); n != 2 {
		t.Errorf("Exec called %d times; want 2 (serial tools not cached)", n)
	}
}

// multiTurnToolsStream returns a backend respond function that issues a
// different set of tool calls on each successive invocation, then returns
// a plain text response once all sets are exhausted.
func multiTurnToolsStream(turns [][]backend.ToolCall) func(context.Context, []backend.Message, []backend.Tool, backend.GenerationParams) (<-chan backend.StreamEvent, error) {
	var n int32
	return func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		i := int(atomic.AddInt32(&n, 1)) - 1
		if i < len(turns) {
			ch := make(chan backend.StreamEvent, 1)
			ch <- backend.StreamEvent{ToolCalls: turns[i], Done: true, StopReason: "tool_calls"}
			close(ch)
			return ch, nil
		}
		return textStream("done"), nil
	}
}

// TestResultCache_ErrorNotCached verifies that a failed Exec result is not
// stored: a second call with the same args retries Exec rather than returning
// the cached error. The two calls are issued in separate loop turns so they
// run sequentially (no batching).
func TestResultCache_ErrorNotCached(t *testing.T) {
	var execCount int32
	be := defaultBE()
	be.respond = multiTurnToolsStream([][]backend.ToolCall{
		{{ID: "1", Name: "file_read", Arguments: json.RawMessage(`{"path":"a.txt"}`)}},
		{{ID: "2", Name: "file_read", Arguments: json.RawMessage(`{"path":"a.txt"}`)}},
	})

	c := newCore(t, be, nil)
	c.cfg.ClassifyTool = func(string) bool { return true }
	c.cfg.Exec = func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
		n := atomic.AddInt32(&execCount, 1)
		if n == 1 {
			return "", &mockErr{"transient failure"}
		}
		return "ok now", nil
	}

	evs := collectEvents(context.Background(), c, "error then ok")

	if n := atomic.LoadInt32(&execCount); n != 2 {
		t.Errorf("Exec called %d times; want 2 (error must not be cached)", n)
	}
	toolEvs := byRole(evs, "tool")
	if len(toolEvs) != 2 {
		t.Fatalf("tool events = %d; want 2", len(toolEvs))
	}
	if toolEvs[1] != "ok now" {
		t.Errorf("second result = %q; want %q", toolEvs[1], "ok now")
	}
}

type mockErr struct{ msg string }

func (e *mockErr) Error() string { return e.msg }
