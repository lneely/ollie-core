package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"ollie/pkg/backend"
)

// errStream returns a backend respond function that always returns the given error.
func errStream(err error) func(context.Context, []backend.Message, []backend.Tool, backend.GenerationParams) (<-chan backend.StreamEvent, error) {
	return func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		return nil, err
	}
}

// errThenOKStream returns an error on the first call, then a text response.
func errThenOKStream(err error) func(context.Context, []backend.Message, []backend.Tool, backend.GenerationParams) (<-chan backend.StreamEvent, error) {
	var n int32
	return func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		if atomic.AddInt32(&n, 1) == 1 {
			return nil, err
		}
		return textStream("recovered"), nil
	}
}

// TestTurnError_HookInterceptsRateLimit verifies that a turnError hook fired
// on a RateLimitError causes the loop to skip retries and return immediately.
func TestTurnError_HookInterceptsRateLimit(t *testing.T) {
	var hookCalls int32
	be := defaultBE()
	be.respond = errStream(&backend.RateLimitError{Message: "quota exceeded"})

	c := newCore(t, be, nil)
	c.cfg.TurnError = func(_ context.Context, errType, _ string) HookResult {
		atomic.AddInt32(&hookCalls, 1)
		if errType != "rate_limit" {
			t.Errorf("errType = %q; want rate_limit", errType)
		}
		return HookResult{Ran: true, Handled: true}
	}

	evs := collectEvents(context.Background(), c, "hi")

	if n := atomic.LoadInt32(&hookCalls); n != 1 {
		t.Errorf("hook called %d times; want 1 (no retries after hook intercept)", n)
	}
	errEvs := byRole(evs, "error")
	if len(errEvs) == 0 {
		t.Error("expected an error event")
	}
}

// TestTurnError_HookInterceptsToolUnsupported verifies the same skip-retry
// behaviour for ToolUnsupportedError.
func TestTurnError_HookInterceptsToolUnsupported(t *testing.T) {
	var hookCalls int32
	be := defaultBE()
	be.respond = errStream(&backend.ToolUnsupportedError{Message: "model does not support tools"})

	c := newCore(t, be, nil)
	c.cfg.TurnError = func(_ context.Context, errType, _ string) HookResult {
		atomic.AddInt32(&hookCalls, 1)
		if errType != "tool_unsupported" {
			t.Errorf("errType = %q; want tool_unsupported", errType)
		}
		return HookResult{Ran: true, Handled: true}
	}

	collectEvents(context.Background(), c, "hi")

	if n := atomic.LoadInt32(&hookCalls); n != 1 {
		t.Errorf("hook called %d times; want 1", n)
	}
}

// TestTurnError_NoHookFallsThrough verifies that when no turnError hook is
// configured, normal retry behaviour proceeds for retryable errors.
func TestTurnError_NoHookFallsThrough(t *testing.T) {
	old := retryBaseDelay
	retryBaseDelay = 10 * time.Millisecond
	defer func() { retryBaseDelay = old }()

	var attempts int32
	be := defaultBE()
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		atomic.AddInt32(&attempts, 1)
		return nil, &backend.RateLimitError{Message: "slow down"}
	}

	c := newCore(t, be, nil)
	// TurnError is nil — no hook configured.

	collectEvents(context.Background(), c, "hi")

	// Should have attempted maxTransientRetries+1 = 4 times.
	if n := atomic.LoadInt32(&attempts); n != maxTransientRetries+1 {
		t.Errorf("attempts = %d; want %d (full retry cycle)", n, maxTransientRetries+1)
	}
}

// TestTurnError_NonRetryableErrorNoHook verifies that a plain (non-retryable)
// error fires the hook once and does not retry.
func TestTurnError_NonRetryableErrorNoHook(t *testing.T) {
	var hookCalls int32
	be := defaultBE()
	be.respond = errStream(&backend.ToolUnsupportedError{Message: "no tools"})

	c := newCore(t, be, nil)
	c.cfg.TurnError = func(_ context.Context, errType, _ string) HookResult {
		atomic.AddInt32(&hookCalls, 1)
		return HookResult{Ran: true, Handled: true}
	}

	collectEvents(context.Background(), c, "hi")

	if n := atomic.LoadInt32(&hookCalls); n != 1 {
		t.Errorf("hook called %d times; want 1", n)
	}
}

// TestTurnError_HookNotRunOnSuccess verifies that the turnError hook is never
// called when the backend succeeds on the first attempt.
func TestTurnError_HookNotRunOnSuccess(t *testing.T) {
	var hookCalls int32
	be := defaultBE()
	// Default respond returns textStream("ok") — no error.

	c := newCore(t, be, nil)
	c.cfg.TurnError = func(_ context.Context, _, _ string) HookResult {
		atomic.AddInt32(&hookCalls, 1)
		return HookResult{Ran: true}
	}

	collectEvents(context.Background(), c, "hi")

	if n := atomic.LoadInt32(&hookCalls); n != 0 {
		t.Errorf("hook called %d times on success; want 0", n)
	}
}

// TestTurnError_ClassifyError verifies that classifyError returns the correct
// string for each known error type.
func TestTurnError_ClassifyError(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&backend.RateLimitError{Message: "x"}, "rate_limit"},
		{&backend.ToolUnsupportedError{Message: "x"}, "tool_unsupported"},
		{&backend.ContextOverflowError{Message: "x"}, "context_overflow"},
		{&backend.TransientError{Message: "x"}, "transient"},
	}
	for _, tc := range cases {
		got := classifyError(tc.err)
		if got != tc.want {
			t.Errorf("classifyError(%T) = %q; want %q", tc.err, got, tc.want)
		}
	}
}
