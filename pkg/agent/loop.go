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
	Output           EventHandler
	systemPrompt     string
	GenerationParams backend.GenerationParams
	PopInject        func() string // returns and clears pending inject, or ""
}

func run(ctx context.Context, cfg loopConfig, state state) error {
	var step int

	for {
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
			if ctx.Err() != nil {
				recordInterruption(state, "request")
				return ctx.Err()
			}
			var rlErr *backend.RateLimitError
			if !errors.As(err, &rlErr) || attempt >= maxRateLimitRetries {
				return fmt.Errorf("step %d: %w", step, err)
			}
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
				if ev.Usage.InputTokens > 0 || ev.Usage.OutputTokens > 0 {
					emit(cfg, Event{
						Role:    "usage",
						Content: fmt.Sprintf("%d %d 0", ev.Usage.InputTokens, ev.Usage.OutputTokens),
					})
				} else {
					// Backend didn't report usage; estimate from content.
					inChars := 0
					for _, m := range history {
						inChars += len(m.Content)
						for _, tc := range m.ToolCalls {
							inChars += len(tc.Name) + len(tc.Arguments)
						}
					}
					emit(cfg, Event{
						Role:    "usage",
						Content: fmt.Sprintf("%d %d 1", inChars/4, content.Len()/4),
					})
				}
				break
			}
		}

		if !done {
			// Stream interrupted. Record whatever we got.
			msg := backend.Message{Role: "assistant", Content: content.String(), ToolCalls: toolCalls}
			var results []toolResult
			for _, tc := range toolCalls {
				results = append(results, toolResult{
					ToolCallID: tc.ID,
					Name:       tc.Name,
					Content:    `{"status":"cancelled","error":"stream interrupted"}`,
					IsError:    true,
				})
			}
			state.update(msg, results) //nolint:errcheck
			recordInterruption(state, "stream")
			return fmt.Errorf("step %d: stream interrupted: %w", step, ctx.Err())
		}

		switch stopReason {
		case "stop", "tool_calls", "length", "":
			// normal
		default:
			return fmt.Errorf("step %d: %s", step, stopReason)
		}

		// Execute tool calls, handling mid-execution interruption.
		msg := backend.Message{Role: "assistant", Content: content.String(), ToolCalls: toolCalls}
		var results []toolResult
		interrupted := false

		for i, tc := range toolCalls {
			if tc.Name == "" {
				results = append(results, toolResult{
					ToolCallID: tc.ID,
					Name:       tc.Name,
					Content:    "error: empty tool name",
					IsError:    true,
				})
				continue
			}

			// Check for cancellation before executing.
			if ctx.Err() != nil {
				// Fill synthetic results for this and all remaining tool calls.
				for _, remaining := range toolCalls[i:] {
					results = append(results, toolResult{
						ToolCallID: remaining.ID,
						Name:       remaining.Name,
						Content:    `{"status":"cancelled","error":"interrupted"}`,
						IsError:    true,
					})
				}
				interrupted = true
				break
			}

			emit(cfg, Event{Role: "call", Name: tc.Name, Content: string(tc.Arguments)})

			var result string
			var isErr bool
			if cfg.Exec != nil {
				out, err := cfg.Exec(ctx, tc.Name, tc.Arguments)
				if err != nil {
					result = fmt.Sprintf("error: %v", err)
					isErr = true
					// If cancelled during execution, fill remaining and break.
					if ctx.Err() != nil {
						if cfg.PopInject != nil {
							if injected := cfg.PopInject(); injected != "" {
								result += "\n\n<system-user-interruption>\n" + injected + "\n</system-user-interruption>"
							}
						}
						results = append(results, toolResult{
							ToolCallID: tc.ID,
							Name:       tc.Name,
							Content:    result,
							IsError:    true,
						})
						for _, remaining := range toolCalls[i+1:] {
							results = append(results, toolResult{
								ToolCallID: remaining.ID,
								Name:       remaining.Name,
								Content:    `{"status":"cancelled","error":"interrupted"}`,
								IsError:    true,
							})
						}
						interrupted = true
						break
					}
				} else {
					result = out
				}
			} else {
				result = "error: no tool executor configured"
				isErr = true
			}

			if !interrupted {
				if cfg.PopInject != nil {
					if injected := cfg.PopInject(); injected != "" {
						result += "\n\n<system-user-interruption>\n" + injected + "\n</system-user-interruption>"
					}
				}
				results = append(results, toolResult{
					ToolCallID: tc.ID,
					Name:       tc.Name,
					Content:    result,
					IsError:    isErr,
				})
				emit(cfg, Event{Role: "tool", Name: tc.Name, Content: result})
			}
		}

		if err := state.update(msg, results); err != nil {
			return fmt.Errorf("step %d update: %w", step, err)
		}

		if interrupted {
			recordInterruption(state, "tools")
			return ctx.Err()
		}

		if len(toolCalls) == 0 {
			if err := state.markComplete(); err != nil {
				return fmt.Errorf("mark complete: %w", err)
			}
			break
		}

		step++
	}

	return nil
}

// recordInterruption appends a note to history so the model knows what happened.
func recordInterruption(state state, phase string) {
	var note string
	switch phase {
	case "stream":
		note = "Note: generation was interrupted before completion. Treat the previous assistant output as partial."
	case "tools":
		note = "Note: tool execution was interrupted before completion. Tool outputs may be missing or cancelled."
	case "request":
		note = "Note: the previous assistant turn was interrupted before any response was received."
	default:
		note = "Note: the previous assistant turn was interrupted."
	}
	state.update(backend.Message{Role: "assistant", Content: note}, nil) //nolint:errcheck
}

func emit(cfg loopConfig, msg Event) {
	if cfg.Output != nil {
		cfg.Output(msg)
	}
}

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
