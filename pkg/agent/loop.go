package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"ollie/pkg/backend"
)

const maxTransientRetries = 3

// retryBaseDelay is the base delay for rate-limit retries (5<<attempt seconds).
// Tests can override this to speed up retry tests.
var retryBaseDelay = 5 * time.Second

// streamDropBaseDelay is the base delay for stream-drop retries (2<<attempt seconds).
var streamDropBaseDelay = 2 * time.Second

// Consecutive tool-error thresholds. At the soft limit the model is nudged
// to try a different approach; at the hard limit the loop aborts.
const (
	consecutiveErrorSoftLimit = 5
	consecutiveErrorHardLimit = 10
)

// planReinjectInterval is the number of tool-call rounds between periodic
// plan re-injection. Every N steps the current contents of the plan file
// are surfaced back into the conversation to keep the model on track.
const planReinjectInterval = 10

type toolExecutor func(ctx context.Context, name string, args json.RawMessage) (string, error)

type agentConfig struct {
	Backend              backend.Backend
	Tools                []backend.Tool
	Exec                 toolExecutor
	ClassifyTool         func(name string) bool // nil=treat all as serial; true=parallel-read-safe
	ToolResultMaxBytes   int                    // 0=unlimited; truncate tool results beyond this size
	Output               EventHandler
	preamble             string // compiled system+agent prompt sent as the system role
	GenerationParams     backend.GenerationParams
	PopInject            func() string                                                                         // returns and clears pending inject, or ""
	AutoCompact          func(ctx context.Context)                                                             // called after each tool round; may compact in-place
	SaveSession          func()                                                                                // called after each state.update(); persists mid-turn progress
	PreTool              func(ctx context.Context, name string, args json.RawMessage) HookResult              // called before each tool; exit 2 blocks execution
	PostTool             func(ctx context.Context, name string, args json.RawMessage, result string) HookResult // called after each tool; exit 0 appends, exit 2 replaces result
	TurnError            func(ctx context.Context, errType, errMsg string) HookResult                          // called on first backend error; if ran, skips retries
	// MaxSteps is the maximum number of tool-call rounds per turn.
	// When reached, a soft nudge is injected and the loop exits cleanly.
	// 0 means unlimited.
	MaxSteps int
	// PlanFile is the path to the session plan file (e.g. s/<id>/plan).
	// When set, the loop re-surfaces its contents every planReinjectInterval
	// tool-call rounds. Empty string disables plan re-injection.
	PlanFile string
	// IncrToolCallCount increments and returns the session-wide tool call counter.
	IncrToolCallCount func() int64
}

// readPlanFile returns the contents of the plan file, or empty string if
// the file does not exist, is empty, or cannot be read.
func readPlanFile(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		return ""
	}
	return string(b)
}

func run(ctx context.Context, cfg agentConfig, state state) error {
	var step int
	var consecutiveErrors int // rounds where every tool call returned an error
	// resultCache stores outputs of read-safe tool calls keyed by name+args.
	// Only tools classified as parallel-safe (immutable reads) are cached.
	// sync.Map is required because execOne may run in concurrent goroutines.
	var resultCache sync.Map

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
				// On the first error, fire the turnError hook. If it handles
				// the error (exit 0), return immediately — the hook is
				// responsible for recovery (e.g. switching model and resubmitting).
				if attempt == 0 && cfg.TurnError != nil {
					errType := classifyError(err)
					if r := cfg.TurnError(ctx, errType, err.Error()); r.Handled {
						return fmt.Errorf("step %d: %w", step, err)
					}
				}
				wait, retryable := transientWait(err, attempt)
				if !retryable || attempt >= maxTransientRetries {
					return fmt.Errorf("step %d: %w", step, err)
				}
				var rlErr *backend.RateLimitError
				if errors.As(err, &rlErr) {
					emit(cfg, Event{Role: "limitretry"})
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
			wait := streamDropBaseDelay << attempt
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

		// Detect models that emit tool calls as plain text instead of using the
		// function-calling API. If the response contains no API-level tool calls
		// but the text matches a registered tool name followed by ':' or '(',
		// the model does not support function calling — treat as ToolUnsupportedError
		// so freeloader can blacklist and cycle to the next model.
		if len(toolCalls) == 0 && len(cfg.Tools) > 0 && hasTextToolCall(content.String(), cfg.Tools) {
			return fmt.Errorf("step %d: %w", step, &backend.ToolUnsupportedError{
				Message: "model emitted tool call as text (function calling API not supported)",
			})
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
				cr := cancelledResult(tc)
				emit(cfg, Event{Role: "tool", Name: tc.Name, Content: cr.Content})
				return cr, true
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
			readSafe := cfg.ClassifyTool != nil && cfg.ClassifyTool(tc.Name)
			if readSafe {
				key := tc.Name + "\x00" + string(tc.Arguments)
				if v, ok := resultCache.Load(key); ok {
					cached := v.(string)
					emit(cfg, Event{Role: "tool", Name: tc.Name, Content: cached})
					return toolResult{ToolCallID: tc.ID, Name: tc.Name, Content: cached}, false
				}
			}
			var result string
			var isErr bool
			if cfg.Exec != nil {
				out, err := cfg.Exec(ctx, tc.Name, tc.Arguments)
				if err != nil {
					isErr = true
					if ctx.Err() != nil {
						result = "error: tool execution interrupted by user"
						if cfg.PopInject != nil {
							if injected := cfg.PopInject(); injected != "" {
								result += "\n\n<system-user-interruption>\n" + injected + "\n</system-user-interruption>"
							}
						}
						emit(cfg, Event{Role: "tool", Name: tc.Name, Content: result})
						return toolResult{ToolCallID: tc.ID, Name: tc.Name, Content: result, IsError: true}, true
					}
					result = fmt.Sprintf("error: %v", err)
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
			if !isErr && cfg.ToolResultMaxBytes > 0 && len(result) > cfg.ToolResultMaxBytes {
				orig := len(result)
				result = result[:cfg.ToolResultMaxBytes]
				result += fmt.Sprintf("\n\n[result truncated: first %d of %d bytes shown; reissue with offset=%d to read more]",
					cfg.ToolResultMaxBytes, orig, cfg.ToolResultMaxBytes)
			}
			if readSafe && !isErr {
				resultCache.Store(tc.Name+"\x00"+string(tc.Arguments), result)
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
					cr := cancelledResult(remaining)
					emit(cfg, Event{Role: "tool", Name: remaining.Name, Content: cr.Content})
					results = append(results, cr)
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

			fillCancelled := func(from int) {
				for _, remaining := range toolCalls[from:] {
					cr := cancelledResult(remaining)
					emit(cfg, Event{Role: "tool", Name: remaining.Name, Content: cr.Content})
					results = append(results, cr)
				}
			}

			if len(batch) == 1 {
				tr, wasInt := execOne(batch[0])
				results = append(results, tr)
				if wasInt {
					fillCancelled(j)
					interrupted = true
				}
			} else {
				// Fan out the batch concurrently; preserve submission order in results.
				// Deduplicate identical calls so only one executes per unique key.
				type inflightResult struct {
					tr     toolResult
					wasInt bool
				}
				inflight := make(map[string]int) // key -> index of first occurrence
				batchResults := make([]toolResult, len(batch))
				batchInt := make([]bool, len(batch))
				var wg sync.WaitGroup
				uniqueResults := make([]inflightResult, len(batch))
				for k, tc := range batch {
					key := tc.Name + "\x00" + string(tc.Arguments)
					if first, dup := inflight[key]; dup {
						// Will copy result from first occurrence after wg.Wait.
						batchResults[k] = toolResult{} // placeholder
						_ = first                      // used below
						continue
					}
					inflight[key] = k
					wg.Add(1)
					go func(k int, tc backend.ToolCall) {
						defer wg.Done()
						uniqueResults[k].tr, uniqueResults[k].wasInt = execOne(tc)
					}(k, tc)
				}
				wg.Wait()
				// Fill results: unique calls get their own result, duplicates copy from first.
				for k, tc := range batch {
					key := tc.Name + "\x00" + string(tc.Arguments)
					first := inflight[key]
					if k == first {
						batchResults[k] = uniqueResults[k].tr
						batchInt[k] = uniqueResults[k].wasInt
					} else {
						batchResults[k] = toolResult{
							ToolCallID: tc.ID,
							Name:       tc.Name,
							Content:    uniqueResults[first].tr.Content,
							IsError:    uniqueResults[first].tr.IsError,
						}
						batchInt[k] = uniqueResults[first].wasInt
						emit(cfg, Event{Role: "call", Name: tc.Name, Content: string(tc.Arguments)})
						emit(cfg, Event{Role: "tool", Name: tc.Name, Content: batchResults[k].Content})
					}
				}
				// Add all batch results — model needs a result for every tool call.
				results = append(results, batchResults...)
				for _, wasInt := range batchInt {
					if wasInt {
						fillCancelled(j)
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

		// Track consecutive rounds where every tool call errored.
		allErrors := len(results) > 0
		for _, r := range results {
			if !r.IsError {
				allErrors = false
				break
			}
		}
		if allErrors {
			consecutiveErrors++
		} else {
			consecutiveErrors = 0
		}
		if consecutiveErrors >= consecutiveErrorHardLimit {
			emit(cfg, Event{Role: "error", Content: fmt.Sprintf("%d consecutive tool errors — aborting", consecutiveErrors)})
			return fmt.Errorf("step %d: %d consecutive tool errors", step, consecutiveErrors)
		}
		if consecutiveErrors == consecutiveErrorSoftLimit {
			// Nudge the model to try a different approach and keep the plan current.
			state.update(backend.Message{
				Role:    "user",
				Content: "[system: your last several tool calls all failed. Try a different approach, or ask the user for help. Keep your plan current — update s/$OLLIE_SESSION_ID/plan to reflect where you are and what remains.]",
			}, nil)
		}

		// Soft step-budget guardrail: when MaxSteps is set and the budget is
		// exhausted, nudge the model to wrap up and exit cleanly. This is not
		// a hard abort — the model gets one final turn without tools to emit
		// a summary or hand-off message.
		if cfg.MaxSteps > 0 && step >= cfg.MaxSteps-1 {
			emit(cfg, Event{Role: "maxsteps", Content: fmt.Sprintf("%d", step+1)})
			state.update(backend.Message{
				Role:    "user",
				Content: fmt.Sprintf("[system: step budget exhausted (%d/%d steps used). Stop calling tools. Summarize what you have done and what remains, then stop.]", step+1, cfg.MaxSteps),
			}, nil)
			break
		}

		// Periodically re-surface the plan file so it stays visible as tool
		// results accumulate in the context. Only fires when PlanFile is set
		// and the file has content.
		if cfg.PlanFile != "" && step > 0 && step%planReinjectInterval == 0 {
			if plan := readPlanFile(cfg.PlanFile); plan != "" {
				state.update(backend.Message{
					Role: "user",
					Content: fmt.Sprintf(
						"[system: your current plan:\n\n%s\n\nKeep this plan current — mark completed steps and revise if your approach has changed. Continue.]",
						plan,
					),
				}, nil)
			}
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

// hasTextToolCall returns true when the assistant text contains a registered
// tool name immediately followed by ':' or '(', indicating the model tried to
// invoke a tool by writing the call as plain text rather than via the API.
func hasTextToolCall(text string, tools []backend.Tool) bool {
	for _, t := range tools {
		if strings.Contains(text, t.Name+":") || strings.Contains(text, t.Name+"(") {
			return true
		}
	}
	return false
}

// classifyError returns a short string identifying the error type for the
// turnError hook payload.
func classifyError(err error) string {
	var rlErr *backend.RateLimitError
	if errors.As(err, &rlErr) {
		return "rate_limit"
	}
	var tuErr *backend.ToolUnsupportedError
	if errors.As(err, &tuErr) {
		return "tool_unsupported"
	}
	var coErr *backend.ContextOverflowError
	if errors.As(err, &coErr) {
		return "context_overflow"
	}
	var tErr *backend.TransientError
	if errors.As(err, &tErr) {
		return "transient"
	}
	return "unknown"
}

// transientWait returns the retry wait for a retryable error and whether it is
// retryable. Rate limits use longer waits; transient/network errors use shorter.
func transientWait(err error, attempt int) (time.Duration, bool) {
	var rlErr *backend.RateLimitError
	if errors.As(err, &rlErr) {
		wait := rlErr.RetryAfter
		if wait == 0 {
			wait = retryBaseDelay << attempt
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
