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

func Run(ctx context.Context, cfg Config, state State) error {
	maxSteps := cfg.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 1
	}

	for step := range maxSteps {
		history := state.History()
		if cfg.SystemPrompt != "" {
			history = append([]backend.Message{{Role: "system", Content: cfg.SystemPrompt}}, history...)
		}

		// Stream the assistant's response.
		ch, err := cfg.Backend.ChatStream(ctx, cfg.Model, history, cfg.Tools)
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

func emit(cfg Config, msg OutputMsg) {
	if cfg.Output != nil {
		cfg.Output(msg)
	}
}
