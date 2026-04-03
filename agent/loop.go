package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"ollie/backend"
)

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

type Loop struct {
	cfg          Config
	lastUsage    backend.Usage
	streamed     bool
	skippedCalls map[string]bool
}

func New(cfg Config) *Loop {
	return &Loop{cfg: cfg, skippedCalls: make(map[string]bool)}
}

func (l *Loop) Run(ctx context.Context, state State) error {
	maxSteps := l.cfg.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 1
	}

	sb, streaming := l.cfg.Backend.(backend.StreamingBackend)

	for step := range maxSteps {
		l.streamed = false
		history := state.History()
		if l.cfg.SystemPrompt != "" {
			history = append([]backend.Message{{Role: "system", Content: l.cfg.SystemPrompt}}, history...)
		}

		var resp *backend.Response

		if streaming {
			msg, err := l.runStreamStep(ctx, sb, l.cfg.Model, history, l.cfg.Tools)
			if err != nil {
				return fmt.Errorf("step %d decide: %w", step, err)
			}
			resp = &backend.Response{
				Message:    msg,
				StopReason: decideStopReason(msg),
				Usage:      l.lastUsage,
			}
		} else {
			var err error
			resp, err = l.cfg.Backend.Chat(ctx, l.cfg.Model, history, l.cfg.Tools)
			if err != nil {
				return fmt.Errorf("step %d decide: %w", step, err)
			}
		}

		// Act: execute tool calls.
		var results []ToolResult
		for _, tc := range resp.Message.ToolCalls {
			// In the non-streaming path, emit the call announcement here.
			// In the streaming path this was already emitted inside
			// runStreamStep (once all arguments were accumulated), so skip it.
			if !l.skippedCalls[tc.Name] {
				l.emit(OutputMsg{Role: "call", Name: tc.Name, Content: string(tc.Arguments)})
			}

			var content string
			var isErr bool

			if l.cfg.Exec != nil {
				out, err := l.cfg.Exec(tc.Name, tc.Arguments)
				if err != nil {
					content = fmt.Sprintf("error: %v", err)
					isErr = true
				} else {
					content = out
				}
			} else {
				content = "error: no tool executor configured"
				isErr = true
			}

			results = append(results, ToolResult{
				ToolCallID: tc.ID,
				Name:       tc.Name,
				Content:    content,
				IsError:    isErr,
			})

			l.emit(OutputMsg{Role: "tool", Name: tc.Name, Content: content})
		}

		// Only emit the full assistant text if we did NOT stream it.
		if !l.streamed && resp.Message.Content != "" {
			l.emit(OutputMsg{Role: "assistant", Content: resp.Message.Content})
		}

		// Emit usage only when there are actual token counts (skips
		// intermediate steps and avoids [↑0 ↓0 tokens] noise).
		if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
			l.emit(OutputMsg{
				Role:  "usage",
				Usage: resp.Usage,
			})
		}

		if err := state.Update(resp.Message, results); err != nil {
			return fmt.Errorf("step %d update: %w", step, err)
		}

		if l.shouldStop(resp, step, maxSteps) {
			if resp.StopReason == "stop" {
				if err := state.MarkComplete(); err != nil {
					return fmt.Errorf("mark complete: %w", err)
				}
			}
			break
		}
	}

	l.skippedCalls = make(map[string]bool)
	return nil
}

func (l *Loop) runStreamStep(
	ctx context.Context,
	sb backend.StreamingBackend,
	model string,
	messages []backend.Message,
	tools []backend.Tool,
) (msg backend.Message, err error) {
	ch, err := sb.ChatStream(ctx, model, messages, tools)
	if err != nil {
		return msg, err
	}

	l.skippedCalls = make(map[string]bool)
	l.streamed = true
	var content strings.Builder
	var accumulatedTcs []backend.ToolCall

	for ev := range ch {
		if ev.Content != "" {
			content.WriteString(ev.Content)
			l.emit(OutputMsg{Role: "assistant", Content: ev.Content})
		}

		for _, tc := range ev.ToolCalls {
			accumulatedTcs = append(accumulatedTcs, tc)
		}

		if ev.Done {
			l.lastUsage = ev.Usage
			msg.Content = content.String()
			msg.Role = "assistant"
			msg.ToolCalls = accumulatedTcs
			// Emit 'call' now that arguments are fully accumulated, and
			// mark each as skipped so the Act loop does not re-emit it.
			for _, tc := range accumulatedTcs {
				l.emit(OutputMsg{Role: "call", Name: tc.Name, Content: string(tc.Arguments)})
				l.skippedCalls[tc.Name] = true
			}
			return msg, nil
		}
	}

	// Stream closed without a done event — treat as a transient error so the
	// caller does not save partial/corrupt state.  Any tool calls that arrived
	// before the drop are attached to the message for diagnostic use, but
	// because we return a non-nil error the outer Run loop will NOT call
	// state.Update, keeping the session history clean for a retry.
	msg.Content = content.String()
	msg.Role = "assistant"
	msg.ToolCalls = accumulatedTcs
	return msg, fmt.Errorf("stream ended without done event")
}

func decideStopReason(m backend.Message) string {
	if len(m.ToolCalls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

func (l *Loop) emit(msg OutputMsg) {
	if l.cfg.Output != nil {
		l.cfg.Output(msg)
	}
}

func (l *Loop) shouldStop(resp *backend.Response, step, maxSteps int) bool {
	return resp.StopReason == "stop" || step >= maxSteps-1
}
