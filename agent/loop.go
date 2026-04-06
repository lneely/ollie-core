package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"ollie/backend"
)

const maxRateLimitRetries = 3

type ToolExecutor func(ctx context.Context, name string, args json.RawMessage) (string, error)
type OutputFn func(msg OutputMsg)

type OutputMsg struct {
	Role    string
	Name    string
	Content string
	Usage   backend.Usage
}

type Config struct {
	Backend          backend.Backend
	Model            string
	Tools            []backend.Tool
	Exec             ToolExecutor
	Confirm          ConfirmFn
	MaxSteps         int
	Output           OutputFn
	SystemPrompt     string
	GenerationParams backend.GenerationParams
}

// ConfirmFn requests user confirmation for an action. Returns true if approved.
type ConfirmFn func(prompt string) bool

func Run(ctx context.Context, cfg Config, state State) error {
	maxSteps := cfg.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 1
	}

	var totalToolCalls int
	var hadContent bool
	hitLimit := false

	for step := range maxSteps {
		history := state.History()
		if cfg.SystemPrompt != "" {
			history = append([]backend.Message{{Role: "system", Content: cfg.SystemPrompt}}, history...)
		}

		// Stream the assistant's response, retrying on HTTP 429.
		var ch <-chan backend.StreamEvent
		for attempt := range maxRateLimitRetries + 1 {
			var err error
			ch, err = cfg.Backend.ChatStream(ctx, cfg.Model, history, cfg.Tools, cfg.GenerationParams)
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
		var usage backend.Usage
		var done bool

		for ev := range ch {
			if ev.Content != "" {
				content.WriteString(ev.Content)
				emit(cfg, OutputMsg{Role: "assistant", Content: ev.Content})
			}
			toolCalls = append(toolCalls, ev.ToolCalls...)
			if ev.Done {
				usage = ev.Usage
				done = true
				break
			}
		}

		if !done {
			return fmt.Errorf("step %d: stream ended without done event", step)
		}
		if content.Len() > 0 {
			hadContent = true
		}
		totalToolCalls += len(toolCalls)

		// Announce and execute tool calls.
		msg := backend.Message{Role: "assistant", Content: content.String(), ToolCalls: toolCalls}
		var results []ToolResult

		for _, tc := range toolCalls {
			if tc.Name == "" {
				results = append(results, ToolResult{
					ToolCallID: tc.ID,
					Name:       tc.Name,
					Content:    "error: empty tool name",
					IsError:    true,
				})
				continue
			}
			emit(cfg, OutputMsg{Role: "call", Name: tc.Name, Content: string(tc.Arguments)})

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

			results = append(results, ToolResult{
				ToolCallID: tc.ID,
				Name:       tc.Name,
				Content:    result,
				IsError:    isErr,
			})
			emit(cfg, OutputMsg{Role: "tool", Name: tc.Name, Content: result})
		}

		// Emit usage when we have real token counts.
		if usage.InputTokens > 0 || usage.OutputTokens > 0 {
			emit(cfg, OutputMsg{Role: "usage", Usage: usage})
		}

		if err := state.Update(msg, results); err != nil {
			return fmt.Errorf("step %d update: %w", step, err)
		}

		if len(toolCalls) == 0 {
			if err := state.MarkComplete(); err != nil {
				return fmt.Errorf("mark complete: %w", err)
			}
			break
		}
		if step >= maxSteps-1 {
			hitLimit = true
			break
		}
	}

	// Surface stall conditions so the UI can indicate them.
	if hitLimit {
		emit(cfg, OutputMsg{Role: "stalled", Content: "max steps"})
	} else if totalToolCalls == 0 && hadContent {
		emit(cfg, OutputMsg{Role: "stalled", Content: "no tools"})
	}

	return nil
}

func emit(cfg Config, msg OutputMsg) {
	if cfg.Output != nil {
		cfg.Output(msg)
	}
}

// retryCountdown emits one "retry" OutputMsg per second, counting down from
// wait, so the UI can display a live countdown. Returns ctx.Err() if the
// context is cancelled before the wait elapses.
func retryCountdown(ctx context.Context, cfg Config, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		secs := int(remaining.Seconds()) + 1
		emit(cfg, OutputMsg{Role: "retry", Content: fmt.Sprintf("%d", secs)})
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(remaining, time.Second)):
		}
	}
}
