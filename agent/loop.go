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
	cfg Config
}

func New(cfg Config) *Loop {
	return &Loop{cfg: cfg}
}

func (l *Loop) Run(ctx context.Context, state State) error {
	maxSteps := l.cfg.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 1
	}

	for step := range maxSteps {
		history := state.History()
		if l.cfg.SystemPrompt != "" {
			history = append([]backend.Message{{Role: "system", Content: l.cfg.SystemPrompt}}, history...)
		}

		// Stream the assistant's response.
		ch, err := l.cfg.Backend.ChatStream(ctx, l.cfg.Model, history, l.cfg.Tools)
		if err != nil {
			return fmt.Errorf("step %d: %w", step, err)
		}

		var content strings.Builder
		var toolCalls []backend.ToolCall
		var usage backend.Usage
		var done bool

		for ev := range ch {
			if ev.Content != "" {
				content.WriteString(ev.Content)
				l.emit(OutputMsg{Role: "assistant", Content: ev.Content})
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
			l.emit(OutputMsg{Role: "call", Name: tc.Name, Content: string(tc.Arguments)})

			var result string
			var isErr bool
			if l.cfg.Exec != nil {
				out, err := l.cfg.Exec(tc.Name, tc.Arguments)
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
			l.emit(OutputMsg{Role: "tool", Name: tc.Name, Content: result})
		}

		// Emit usage when we have real token counts.
		if usage.InputTokens > 0 || usage.OutputTokens > 0 {
			l.emit(OutputMsg{Role: "usage", Usage: usage})
		}

		if err := state.Update(msg, results); err != nil {
			return fmt.Errorf("step %d update: %w", step, err)
		}

		// Stop when the model has nothing more to call.
		if len(toolCalls) == 0 || step >= maxSteps-1 {
			if len(toolCalls) == 0 {
				if err := state.MarkComplete(); err != nil {
					return fmt.Errorf("mark complete: %w", err)
				}
			}
			break
		}
	}

	return nil
}

func (l *Loop) emit(msg OutputMsg) {
	if l.cfg.Output != nil {
		l.cfg.Output(msg)
	}
}
