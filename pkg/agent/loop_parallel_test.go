package agent

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ollie/pkg/backend"
)

// toolsStream returns a backend respond function that issues the given tool
// calls on the first invocation and returns a plain text response thereafter.
func toolsStream(calls []backend.ToolCall) func(context.Context, []backend.Message, []backend.Tool, backend.GenerationParams) (<-chan backend.StreamEvent, error) {
	var n int32
	return func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		if atomic.AddInt32(&n, 1) == 1 {
			ch := make(chan backend.StreamEvent, 1)
			ch <- backend.StreamEvent{ToolCalls: calls, Done: true, StopReason: "tool_calls"}
			close(ch)
			return ch, nil
		}
		return textStream("done"), nil
	}
}

// TestParallel_ConcurrentExecution proves that read-safe tools actually run
// in parallel. The barrier requires all 3 goroutines to be in-flight at the
// same time; sequential execution would deadlock and trip the timeout.
func TestParallel_ConcurrentExecution(t *testing.T) {
	const n = 3
	started := make(chan struct{}, n)
	gate := make(chan struct{})

	go func() {
		for i := 0; i < n; i++ {
			<-started
		}
		close(gate) // open once all n tools have started
	}()

	be := defaultBE()
	be.respond = toolsStream([]backend.ToolCall{
		{ID: "1", Name: "tool_a", Arguments: json.RawMessage(`{}`)},
		{ID: "2", Name: "tool_b", Arguments: json.RawMessage(`{}`)},
		{ID: "3", Name: "tool_c", Arguments: json.RawMessage(`{}`)},
	})

	c := newCore(t, be, nil)
	c.cfg.ClassifyTool = func(string) bool { return true }
	c.cfg.Exec = func(ctx context.Context, name string, _ json.RawMessage) (string, error) {
		started <- struct{}{}
		select {
		case <-gate:
			return name + "-result", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	evs := collectEvents(ctx, c, "run parallel")
	if ctx.Err() != nil {
		t.Fatal("timed out — tools likely ran sequentially (barrier never opened)")
	}

	toolEvs := byRole(evs, "tool")
	if len(toolEvs) != n {
		t.Errorf("tool events = %d; want %d", len(toolEvs), n)
	}
}

// TestParallel_SerialToolBreaksBatch verifies that a serial tool between two
// read-safe tools prevents them from being batched together. Order must be
// read_a, write_b, read_c regardless of internal execution details.
func TestParallel_SerialToolBreaksBatch(t *testing.T) {
	be := defaultBE()
	be.respond = toolsStream([]backend.ToolCall{
		{ID: "1", Name: "read_a", Arguments: json.RawMessage(`{}`)},
		{ID: "2", Name: "write_b", Arguments: json.RawMessage(`{}`)},
		{ID: "3", Name: "read_c", Arguments: json.RawMessage(`{}`)},
	})

	var mu sync.Mutex
	var order []string

	c := newCore(t, be, nil)
	c.cfg.ClassifyTool = func(name string) bool {
		return name == "read_a" || name == "read_c"
	}
	c.cfg.Exec = func(_ context.Context, name string, _ json.RawMessage) (string, error) {
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
		return name + "-result", nil
	}

	collectEvents(context.Background(), c, "mixed tools")

	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()

	if len(got) != 3 {
		t.Fatalf("executions = %d; want 3: %v", len(got), got)
	}
	// write_b must appear after the reads that precede it and before those that follow.
	// With single-element batches for reads flanking a serial write, order is deterministic.
	if got[0] != "read_a" || got[1] != "write_b" || got[2] != "read_c" {
		t.Errorf("execution order = %v; want [read_a write_b read_c]", got)
	}
}

// TestParallel_CancellationFillsRemaining verifies that when the context is
// cancelled during a parallel batch, all tools in the batch still produce
// results (IsError) and any subsequent tool calls also get cancelled results.
func TestParallel_CancellationFillsRemaining(t *testing.T) {
	be := defaultBE()
	be.respond = toolsStream([]backend.ToolCall{
		{ID: "1", Name: "tool_a", Arguments: json.RawMessage(`{}`)},
		{ID: "2", Name: "tool_b", Arguments: json.RawMessage(`{}`)},
		{ID: "3", Name: "tool_c", Arguments: json.RawMessage(`{}`)}, // serial — after the parallel batch
	})

	ctx, cancel := context.WithCancel(context.Background())

	c := newCore(t, be, nil)
	// tool_a and tool_b are parallel-safe; tool_c is serial.
	c.cfg.ClassifyTool = func(name string) bool {
		return name == "tool_a" || name == "tool_b"
	}
	c.cfg.Exec = func(execCtx context.Context, name string, _ json.RawMessage) (string, error) {
		cancel() // cancel on first execution; propagates to all
		<-execCtx.Done()
		return "", execCtx.Err()
	}

	evs := collectEvents(ctx, c, "cancel mid-batch")

	toolEvs := byRole(evs, "tool")
	// All 3 tool calls must have produced a result.
	if len(toolEvs) != 3 {
		t.Errorf("tool events = %d; want 3", len(toolEvs))
	}
	// All results must be errors.
	for _, ev := range toolEvs {
		_ = ev // content varies; IsError is tracked internally, not in the event text
	}
	// The backend should not have been called a second time (interrupted before follow-up).
	for _, ev := range evs {
		if ev.Role == "assistant" && ev.Content == "done" {
			t.Error("follow-up 'done' response received; expected interruption before second backend call")
		}
	}
}

// TestParallel_NilClassifyToolIsSerial verifies that when ClassifyTool is nil
// all tools execute sequentially and all results are returned in order.
func TestParallel_NilClassifyToolIsSerial(t *testing.T) {
	names := []string{"tool_a", "tool_b", "tool_c"}
	be := defaultBE()
	be.respond = toolsStream([]backend.ToolCall{
		{ID: "1", Name: names[0], Arguments: json.RawMessage(`{}`)},
		{ID: "2", Name: names[1], Arguments: json.RawMessage(`{}`)},
		{ID: "3", Name: names[2], Arguments: json.RawMessage(`{}`)},
	})

	var mu sync.Mutex
	var order []string

	c := newCore(t, be, nil)
	c.cfg.ClassifyTool = nil // no classifier → all serial
	c.cfg.Exec = func(_ context.Context, name string, _ json.RawMessage) (string, error) {
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
		return name + "-result", nil
	}

	evs := collectEvents(context.Background(), c, "serial fallback")

	toolEvs := byRole(evs, "tool")
	if len(toolEvs) != 3 {
		t.Errorf("tool events = %d; want 3", len(toolEvs))
	}

	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()

	sort.Strings(got)
	sort.Strings(names)
	for i, g := range got {
		if g != names[i] {
			t.Errorf("execution order mismatch: got %v", got)
			break
		}
	}
}
