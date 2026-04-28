package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"ollie/pkg/backend"
)

const maxTransientRetries = 3

type toolExecutor func(ctx context.Context, name string, args json.RawMessage) (string, error)

type agentConfig struct {
	Backend          backend.Backend
	Tools            []backend.Tool
	Exec             toolExecutor
	ClassifyTool     func(name string) bool // nil=treat all as serial; true=parallel-read-safe
	Output           EventHandler
	preamble         string // compiled system+agent prompt sent as the system role
	GenerationParams backend.GenerationParams
	PopInject        func() string                                                                         // returns and clears pending inject, or ""
	AutoCompact      func(ctx context.Context)                                                             // called after each tool round; may compact in-place
	SaveSession      func()                                                                                // called after each state.update(); persists mid-turn progress
	PreTool          func(ctx context.Context, name string, args json.RawMessage) HookResult              // called before each tool; exit 2 blocks execution
	PostTool         func(ctx context.Context, name string, args json.RawMessage, result string) HookResult // called after each tool; exit 0 appends, exit 2 replaces result
}

func run(ctx context.Context, cfg agentConfig, state state) error {
	var step int

	for {
		history := state.history()
		if cfg.preamble != "" {
			history = append([]backend.Message{{Role: "system", Content: cfg.preamble}}, history...)
		}

		// Stream the assistant's response, retrying on rate limits, transient
		// backend errors (5xx, network), and mid-stream drops.
		var content strings.Builder
		var toolCalls []backend.ToolCall
		var stopReason string

		for attempt := range maxTransientRetries + 1 {
			content.Reset()
			toolCalls = nil

			ch, err := cfg.Backend.ChatStream(ctx, history, cfg.Tools, cfg.GenerationParams)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				wait, retryable := transientWait(err, attempt)
				if !retryable || attempt >= maxTransientRetries {
					return fmt.Errorf("step %d: %w", step, err)
				}
				if err := retryCountdown(ctx, cfg, wait); err != nil {
					return fmt.Errorf("step %d: %w", step, err)
				}
				continue
			}

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
							Content: fmt.Sprintf("%d %d 0 %g %d %d", ev.Usage.InputTokens, ev.Usage.OutputTokens, ev.Usage.CostUSD, ev.Usage.CachedInputTokens, ev.Usage.CacheCreationTokens),
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
							Content: fmt.Sprintf("%d %d 1 0", inChars/4, content.Len()/4),
						})
					}
					break
				}
			}

			if done {
				break
			}

			if ctx.Err() != nil {
				// User pause: record partial state and return.
				if hadReasoning {
					emit(cfg, Event{Role: "reasoning", Content: "\n</think>\n"})
				}
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
				if cfg.SaveSession != nil {
					cfg.SaveSession()
				}
				return ctx.Err()
			}

			// Pure stream drop — retry if attempts remain.
			if attempt >= maxTransientRetries {
				return fmt.Errorf("step %d: stream dropped (no more retries)", step)
			}
			if hadReasoning {
				emit(cfg, Event{Role: "reasoning", Content: "\n</think>\n"})
			}
			wait := time.Duration(2<<attempt) * time.Second
			if err := retryCountdown(ctx, cfg, wait); err != nil {
				return fmt.Errorf("step %d: %w", step, err)
			}
		}

		switch stopReason {
		case "stop", "tool_calls", "length", "":
			// normal
		default:
			return fmt.Errorf("step %d: %s", step, stopReason)
		}

		// Execute tool calls, running consecutive parallel-read-safe tools concurrently.
		msg := backend.Message{Role: "assistant", Content: content.String(), ToolCalls: toolCalls}
		results := make([]toolResult, 0, len(toolCalls))
		interrupted := false

		cancelledResult := func(tc backend.ToolCall) toolResult {
			return toolResult{
				ToolCallID: tc.ID,
				Name:       tc.Name,
				Content:    `{"status":"cancelled","error":"interrupted"}`,
				IsError:    true,
			}
		}

		// execOne runs a single tool call end-to-end and reports whether the
		// context was cancelled during execution.
		execOne := func(tc backend.ToolCall) (toolResult, bool) {
			if tc.Name == "" {
				return toolResult{ToolCallID: tc.ID, Name: tc.Name, Content: "error: empty tool name", IsError: true}, false
			}
			if ctx.Err() != nil {
				return cancelledResult(tc), true
			}
			emit(cfg, Event{Role: "call", Name: tc.Name, Content: string(tc.Arguments)})
			if cfg.PreTool != nil {
				hr := cfg.PreTool(ctx, tc.Name, tc.Arguments)
				if hr.Blocked {
					blocked := hr.Context
					if blocked == "" {
						blocked = fmt.Sprintf("tool %q blocked by hook", tc.Name)
					}
					emit(cfg, Event{Role: "tool", Name: tc.Name, Content: blocked})
					return toolResult{ToolCallID: tc.ID, Name: tc.Name, Content: blocked, IsError: true}, false
				}
			}
			var result string
			var isErr bool
			if cfg.Exec != nil {
				out, err := cfg.Exec(ctx, tc.Name, tc.Arguments)
				if err != nil {
					result = fmt.Sprintf("error: %v", err)
					isErr = true
					if ctx.Err() != nil {
						if cfg.PopInject != nil {
							if injected := cfg.PopInject(); injected != "" {
								result += "\n\n<system-user-interruption>\n" + injected + "\n</system-user-interruption>"
							}
						}
						emit(cfg, Event{Role: "tool", Name: tc.Name, Content: result})
						return toolResult{ToolCallID: tc.ID, Name: tc.Name, Content: result, IsError: true}, true
					}
				} else {
					result = out
				}
			} else {
				result = "error: no tool executor configured"
				isErr = true
			}
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
			emit(cfg, Event{Role: "tool", Name: tc.Name, Content: result})
			return toolResult{ToolCallID: tc.ID, Name: tc.Name, Content: result, IsError: isErr}, false
		}

		isParallelSafe := func(name string) bool {
			return name != "" && cfg.ClassifyTool != nil && cfg.ClassifyTool(name)
		}

		for i := 0; i < len(toolCalls) && !interrupted; {
			if ctx.Err() != nil {
				for _, remaining := range toolCalls[i:] {
					results = append(results, cancelledResult(remaining))
				}
				interrupted = true
				break
			}

			// Collect a run of consecutive parallel-safe calls.
			j := i + 1
			if isParallelSafe(toolCalls[i].Name) {
				for j < len(toolCalls) && isParallelSafe(toolCalls[j].Name) {
					j++
				}
			}
			batch := toolCalls[i:j]

			if len(batch) == 1 {
				tr, wasInt := execOne(batch[0])
				results = append(results, tr)
				if wasInt {
					for _, remaining := range toolCalls[j:] {
						results = append(results, cancelledResult(remaining))
					}
					interrupted = true
				}
			} else {
				// Fan out the batch concurrently; preserve submission order in results.
				batchResults := make([]toolResult, len(batch))
				batchInt := make([]bool, len(batch))
				var wg sync.WaitGroup
				for k, tc := range batch {
					wg.Add(1)
					go func(k int, tc backend.ToolCall) {
						defer wg.Done()
						batchResults[k], batchInt[k] = execOne(tc)
					}(k, tc)
				}
				wg.Wait()
				for k, tr := range batchResults {
					results = append(results, tr)
					if batchInt[k] {
						for _, remaining := range toolCalls[j:] {
							results = append(results, cancelledResult(remaining))
						}
						interrupted = true
						break
					}
				}
			}
			i = j
		}

		state.update(msg, results)
		if cfg.SaveSession != nil {
			cfg.SaveSession()
		}

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

// transientWait returns the retry wait for a retryable error and whether it is
// retryable. Rate limits use longer waits; transient/network errors use shorter.
func transientWait(err error, attempt int) (time.Duration, bool) {
	var rlErr *backend.RateLimitError
	if errors.As(err, &rlErr) {
		wait := rlErr.RetryAfter
		if wait == 0 {
			wait = time.Duration(5<<attempt) * time.Second
		}
		return wait, true
	}
	var tErr *backend.TransientError
	if errors.As(err, &tErr) {
		return time.Duration(2<<attempt) * time.Second, true
	}
	return 0, false
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
