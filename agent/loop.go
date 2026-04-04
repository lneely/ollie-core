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
const maxNudges = 2

// nudgeMsg is injected as a user turn when the model narrates intent without
// acting. It is ephemeral — added to the history slice for the next ChatStream
// call but never persisted to State, so it won't appear in future sessions.
const nudgeMsg = "Continue. Use execute_code now — do not describe what you will do."

// narrationPhrases are case-insensitive prefixes/substrings that indicate the
// model is narrating intent rather than acting.
var narrationPhrases = []string{
	"let me",
	"i'll ",
	"i will ",
	"i'm going to",
	"i am going to",
	"i need to",
	"first, i",
	"first i'll",
	"now i'll",
	"now i will",
	"to do this",
	"i'll now",
}

type ToolExecutor func(name string, args json.RawMessage) (string, error)
type OutputFn func(msg OutputMsg)

type OutputMsg struct {
	Role    string
	Name    string
	Content string
	Usage   backend.Usage
}

type Config struct {
	Backend      backend.Backend
	Model        string
	Tools        []backend.Tool
	Exec         ToolExecutor
	MaxSteps     int
	Output       OutputFn
	SystemPrompt string
}

func Run(ctx context.Context, cfg Config, state State) error {
	maxSteps := cfg.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 1
	}

	var pendingNudge string
	nudgeCount := 0

	for step := range maxSteps {
		history := state.History()
		if cfg.SystemPrompt != "" {
			history = append([]backend.Message{{Role: "system", Content: cfg.SystemPrompt}}, history...)
		}
		// Inject nudge from previous step if the model narrated instead of acted.
		if pendingNudge != "" {
			history = append(history, backend.Message{Role: "user", Content: pendingNudge})
			pendingNudge = ""
		}

		// Stream the assistant's response, retrying on HTTP 429.
		var ch <-chan backend.StreamEvent
		for attempt := range maxRateLimitRetries + 1 {
			var err error
			ch, err = cfg.Backend.ChatStream(ctx, cfg.Model, history, cfg.Tools)
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

		// Announce and execute tool calls.
		msg := backend.Message{Role: "assistant", Content: content.String(), ToolCalls: toolCalls}
		var results []ToolResult

		for _, tc := range toolCalls {
			emit(cfg, OutputMsg{Role: "call", Name: tc.Name, Content: string(tc.Arguments)})

			var result string
			var isErr bool
			if cfg.Exec != nil {
				out, err := cfg.Exec(tc.Name, tc.Arguments)
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
			// If the model narrated intent without acting, nudge it to continue
			// rather than treating the turn as completion.
			if nudgeCount < maxNudges && isNarration(content.String()) {
				nudgeCount++
				pendingNudge = nudgeMsg
				emit(cfg, OutputMsg{Role: "nudge", Content: nudgeMsg})
				continue
			}
			if err := state.MarkComplete(); err != nil {
				return fmt.Errorf("mark complete: %w", err)
			}
			break
		}
		if step >= maxSteps-1 {
			break
		}
	}

	return nil
}

// isNarration returns true when text looks like the model is describing what it
// is about to do rather than doing it. Only short responses are considered —
// a longer response likely contains actual content or analysis.
func isNarration(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 800 {
		return false
	}
	lower := strings.ToLower(text)
	for _, phrase := range narrationPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
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
