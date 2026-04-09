package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"ollie/pkg/backend"
)

const maxRateLimitRetries = 3

type toolExecutor func(ctx context.Context, name string, args json.RawMessage) (string, error)

type loopConfig struct {
	Backend          backend.Backend
	Tools            []backend.Tool
	Exec             toolExecutor
	Confirm          confirmFn
	MaxSteps         int
	Output           EventHandler
	systemPrompt     string
	GenerationParams backend.GenerationParams
}

// confirmFn requests user confirmation for an action. Returns true if approved.
type confirmFn func(prompt string) bool

func run(ctx context.Context, cfg loopConfig, state state) error {
	maxSteps := cfg.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 1
	}

	var totalToolCalls int
	hitLimit := false

	for step := range maxSteps {
		history := state.history()
		if cfg.systemPrompt != "" {
			history = append([]backend.Message{{Role: "system", Content: cfg.systemPrompt}}, history...)
		}

		// Stream the assistant's response, retrying on HTTP 429.
		var ch <-chan backend.StreamEvent
		for attempt := range maxRateLimitRetries + 1 {
			var err error
			ch, err = cfg.Backend.ChatStream(ctx, history, cfg.Tools, cfg.GenerationParams)
			if err == nil {
				break
			}
			var rlErr *backend.RateLimitError
			if !errors.As(err, &rlErr) || attempt >= maxRateLimitRetries {
				return fmt.Errorf("step %d: %w", step, err)
			}
			// Exponential backoff: 5s, 10s, 20s — unless the server told us exactly.
			wait := rlErr.RetryAfter
			if wait == 0 {
				wait = time.Duration(5<<attempt) * time.Second
			}
			if err := retryCountdown(ctx, cfg, wait); err != nil {
				return fmt.Errorf("step %d: %w", step, err)
			}
		}

		var content strings.Builder
		var toolCalls []backend.ToolCall
		var stopReason string
		var done bool

		for ev := range ch {
			if ev.Content != "" {
				content.WriteString(ev.Content)
				emit(cfg, Event{Role: "assistant", Content: ev.Content})
			}
			toolCalls = append(toolCalls, ev.ToolCalls...)
			if ev.Done {
				stopReason = ev.StopReason
				done = true
				break
			}
		}

		if !done {
			return fmt.Errorf("step %d: stream ended without done event", step)
		}
		switch stopReason {
		case "stop", "tool_calls", "length", "":
			// normal
		default:
			return fmt.Errorf("step %d: %s", step, stopReason)
		}
		totalToolCalls += len(toolCalls)

		// Announce and execute tool calls.
		msg := backend.Message{Role: "assistant", Content: content.String(), ToolCalls: toolCalls}
		var results []toolResult

		for _, tc := range toolCalls {
			if tc.Name == "" {
				results = append(results, toolResult{
					ToolCallID: tc.ID,
					Name:       tc.Name,
					Content:    "error: empty tool name",
					IsError:    true,
				})
				continue
			}
			emit(cfg, Event{Role: "call", Name: tc.Name, Content: string(tc.Arguments)})

			var result string
			var isErr bool
			if cfg.Exec != nil {
				out, err := cfg.Exec(ctx, tc.Name, tc.Arguments)
				if err != nil {
					result = fmt.Sprintf("error: %v", err)
					isErr = true
				} else {
					result = out
				}
			} else {
				result = "error: no tool executor configured"
				isErr = true
			}

			results = append(results, toolResult{
				ToolCallID: tc.ID,
				Name:       tc.Name,
				Content:    result,
				IsError:    isErr,
			})
			emit(cfg, Event{Role: "tool", Name: tc.Name, Content: result})
		}



		if err := state.update(msg, results); err != nil {
			return fmt.Errorf("step %d update: %w", step, err)
		}

		if len(toolCalls) == 0 {
			if err := state.markComplete(); err != nil {
				return fmt.Errorf("mark complete: %w", err)
			}
			break
		}
		if step >= maxSteps-1 {
			hitLimit = true
			break
		}
	}

	// Surface stall: only when the step limit was hit without completing.
	if hitLimit {
		emit(cfg, Event{Role: "stalled", Content: "max steps"})
	}

	return nil
}

func emit(cfg loopConfig, msg Event) {
	if cfg.Output != nil {
		cfg.Output(msg)
	}
}

// retryCountdown emits one "retry" OutputMsg per second, counting down from
// wait, so the UI can display a live countdown. Returns ctx.Err() if the
// context is cancelled before the wait elapses.
func retryCountdown(ctx context.Context, cfg loopConfig, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		secs := int(remaining.Seconds()) + 1
		emit(cfg, Event{Role: "retry", Content: fmt.Sprintf("%d", secs)})
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(remaining, time.Second)):
		}
	}
}
