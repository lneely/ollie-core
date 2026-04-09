package agent

import "ollie/pkg/backend"

// toolResult holds the output of a single tool call.
type toolResult struct {
	ToolCallID string // may be empty (Ollama does not always set this)
	Name       string
	Content    string
	IsError    bool
}

// state is the interface both ephemeral and bead-backed state must satisfy.
// The loop reads from it on Observe, writes to it on Update.
type state interface {
	// History returns the full conversation history for the current session.
	// The first entry is always the initial user message containing the goal.
	history() []backend.Message

	// Update appends the assistant's reply and any tool results to the history.
	// Called once per loop iteration after Act completes.
	update(assistant backend.Message, results []toolResult) error

	// MarkComplete records that the goal has been achieved.
	markComplete() error

	// IsComplete returns true once MarkComplete has been called.
	isComplete() bool
}
