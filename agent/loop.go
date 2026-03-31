package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"ollie/backend"
)

// ToolExecutor runs a named tool with the given JSON arguments and returns
// its output. Implemented by the caller using ollie/exec.
type ToolExecutor func(name string, args json.RawMessage) (string, error)

// OutputMsg is emitted by the loop for each visible event: assistant replies,
// tool calls, tool results, and errors.
type OutputMsg struct {
	Role    string // "assistant" | "tool" | "error"
	Name    string // tool name, for Role=="tool"
	Content string
}

// Config holds everything the loop needs to run.
type Config struct {
	Backend      backend.Backend
	Model        string
	Tools        []backend.Tool
	Exec         ToolExecutor
	MaxSteps     int                 // 0 → default 1
	Output       func(msg OutputMsg) // nil → discard
	SystemPrompt string              // prepended as system message if non-empty
}

// Loop implements observe → decide → act → update → terminate.
type Loop struct {
	cfg Config
}

// New creates a Loop from the given Config.
func New(cfg Config) *Loop {
	return &Loop{cfg: cfg}
}

// Run executes the agent loop against state until the goal is complete or
// MaxSteps is exhausted.
func (l *Loop) Run(ctx context.Context, state State) error {
	maxSteps := l.cfg.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 1
	}

	for step := range maxSteps {
		// 1. Observe: history is the context; already up to date in state.

		// 2. Decide: call the model.
		history := state.History()
		if l.cfg.SystemPrompt != "" {
			history = append([]backend.Message{{Role: "system", Content: l.cfg.SystemPrompt}}, history...)
		}
		resp, err := l.cfg.Backend.Chat(ctx, l.cfg.Model, history, l.cfg.Tools)
		if err != nil {
			return fmt.Errorf("step %d decide: %w", step, err)
		}

		// 3. Act: execute any tool calls.
		var results []ToolResult
		for _, tc := range resp.Message.ToolCalls {
			var content string
			var isErr bool

			l.emit(OutputMsg{Role: "call", Name: tc.Name, Content: string(tc.Arguments)})

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

		if len(resp.Message.ToolCalls) == 0 && resp.Message.Content != "" {
			l.emit(OutputMsg{Role: "assistant", Content: resp.Message.Content})
		}

		// 4. Update: append assistant message and tool results to state.
		if err := state.Update(resp.Message, results); err != nil {
			return fmt.Errorf("step %d update: %w", step, err)
		}

		// 5. Terminate?
		if l.shouldStop(resp, step, maxSteps) {
			if resp.StopReason == "stop" {
				if err := state.MarkComplete(); err != nil {
					return fmt.Errorf("mark complete: %w", err)
				}
			}
			break
		}
	}

	return nil
}

func (l *Loop) shouldStop(resp *backend.Response, step, maxSteps int) bool {
	// Always stop at maxSteps.
	if step >= maxSteps-1 {
		return true
	}
	// Stop when the model is done and has no pending tool calls.
	if resp.StopReason == "stop" && len(resp.Message.ToolCalls) == 0 {
		return true
	}
	return false
}

func (l *Loop) emit(msg OutputMsg) {
	if l.cfg.Output != nil {
		l.cfg.Output(msg)
	}
}
