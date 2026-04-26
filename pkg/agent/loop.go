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

type agentConfig struct {
	Backend          backend.Backend
	Tools            []backend.Tool
	Exec             toolExecutor
	Output           EventHandler
	preamble         string // compiled system+agent prompt sent as the system role
	GenerationParams backend.GenerationParams
	PopInject        func() string                                                      // returns and clears pending inject, or ""
	AutoCompact      func(ctx context.Context)                                           // called after each tool round; may compact in-place
	PreTool          func(ctx context.Context, name string, args json.RawMessage) HookResult  // called before each tool; exit 2 blocks execution
	PostTool         func(ctx context.Context, name string, args json.RawMessage, result string) HookResult // called after each tool; exit 0 appends, exit 2 replaces result
}

func run(ctx context.Context, cfg agentConfig, state state) error {
	var step int

	for {
		history := state.history()
		if cfg.preamble != "" {
			history = append([]backend.Message{{Role: "system", Content: cfg.preamble}}, history...)
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
		var hadReasoning bool

		for ev := range ch {
			if ev.Reasoning != "" {
				if !hadReasoning {
					emit(cfg, Event{Role: "reasoning", Content: "<think>\n"})
					hadReasoning = true
				}
				emit(cfg, Event{Role: "reasoning", Content: ev.Reasoning})
			}
			if ev.Content != "" {
				if hadReasoning {
					emit(cfg, Event{Role: "reasoning", Content: "\n</think>\n"})
					hadReasoning = false
				}
				content.WriteString(ev.Content)
				emit(cfg, Event{Role: "assistant", Content: ev.Content})
			}
			toolCalls = append(toolCalls, ev.ToolCalls...)
			if ev.Done {
				if hadReasoning {
					emit(cfg, Event{Role: "reasoning", Content: "\n</think>\n"})
					hadReasoning = false
				}
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
			state.update(msg, results)
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

			if cfg.PreTool != nil {
				hr := cfg.PreTool(ctx, tc.Name, tc.Arguments)
				if hr.Blocked {
					msg := hr.Context
					if msg == "" {
						msg = fmt.Sprintf("tool %q blocked by hook", tc.Name)
					}
					results = append(results, toolResult{
						ToolCallID: tc.ID,
						Name:       tc.Name,
						Content:    msg,
						IsError:    true,
					})
					emit(cfg, Event{Role: "tool", Name: tc.Name, Content: msg})
					continue
				}
			}

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
				if cfg.PostTool != nil {
					hr := cfg.PostTool(ctx, tc.Name, tc.Arguments, result)
					if hr.Blocked {
						result = hr.Context
					} else if hr.Context != "" {
						result += "\n" + hr.Context
					}
				}
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

		state.update(msg, results)

		if interrupted {
			return ctx.Err()
		}

		if len(toolCalls) == 0 {
			break
		}

		if cfg.AutoCompact != nil {
			cfg.AutoCompact(ctx)
		}

		step++
	}

	return nil
}


func emit(cfg agentConfig, msg Event) {
	if cfg.Output != nil {
		cfg.Output(msg)
	}
}

func retryCountdown(ctx context.Context, cfg agentConfig, wait time.Duration) error {
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
